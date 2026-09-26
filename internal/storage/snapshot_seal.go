package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Snapshot artifacts are sealed: owned by root and immutable to every VMM.
//
// The memory image and vmstate are written by the source VM's Firecracker,
// assumed compromisable. Left as Firecracker's files, that VMM — or any VMM
// later given the same inode — could rewrite the "clean" snapshot (persistence
// across every future fork) or, since a restored VM maps the memory file
// MAP_PRIVATE, change pages a fork has not faulted in yet.
//
// So the snapshot directory holds root-only inodes (root:root 0400 for memory
// and vmstate, 0600 for the disk, 0700 for the directory) that no VMM can ever
// write. Restores hard-link memory and vmstate into their chroot and read them
// through a grant that lasts only for the load (ShareSnapshotFile, acl.go), so
// all forks share one inode — and its page cache. Without ACL support each
// restore gets a private copy instead (InstallSnapshotFile).
//
// Ownership and mode alone are not enough for the files Firecracker just wrote:
// permissions are checked at open(2), so a VMM that kept a writable descriptor
// on its own output could keep writing after a chmod. The daemon therefore
// never adopts Firecracker's inode — CaptureSnapshotFile copies the bytes into
// a fresh root-owned inode and deletes the original.

const (
	// snapshotDirMode keeps snapshot directories root-only: nothing but the
	// daemon ever reaches an artifact through them.
	snapshotDirMode = 0o700
	// snapshotSealedMode is the mode of the memory image and vmstate at rest:
	// root reads them; a restoring VM only through its temporary grant; no
	// one writes them.
	snapshotSealedMode = 0o400
	// snapshotInstalledMode is a restore's private copy in its chroot (the
	// fallback without ACLs): readable by root and by that VM's Firecracker
	// through its group.
	snapshotInstalledMode = 0o440
	// snapshotDiskMode is the captured disk's: only root reads it, to reflink
	// private copies for forks and restores.
	snapshotDiskMode = 0o600
)

// ficlone is the FICLONE ioctl (_IOW(0x94, 9, int)): make dst share src's
// extents copy-on-write.
const ficlone = 0x40049409

// openUntrustedRegular opens path read-only without following a symlink in its
// last component and refuses anything but a regular file. O_NONBLOCK keeps a
// FIFO planted in the path from blocking the open forever.
func openUntrustedRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file (%v)", path, fi.Mode().Type())
	}
	return f, nil
}

// CaptureSnapshotFile moves a snapshot artifact Firecracker wrote at src into
// dst as a new, sealed inode (root:root, 0400) and removes src. dst must not
// exist. See the comment at the top of this file for why this copies instead
// of renaming.
func CaptureSnapshotFile(src, dst string) error {
	if err := copyInto(src, dst, 0, snapshotSealedMode); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", src, err)
	}
	return nil
}

// InstallSnapshotFile gives a restore its private copy of a sealed snapshot
// artifact: a new inode at dst (inside the VM's chroot) owned by root and
// readable by gid, the VM's identity. dst must not exist; a symlink planted
// there is refused, not followed.
func InstallSnapshotFile(src, dst string, gid int) error {
	return copyInto(src, dst, gid, snapshotInstalledMode)
}

// copyInto copies src into a new root:gid inode at dst with mode, leaving src
// alone.
func copyInto(src, dst string, gid int, mode os.FileMode) error {
	in, err := openUntrustedRegular(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd()); errno != 0 {
		// No reflink here (not a CoW filesystem): a plain copy.
		if _, err := io.Copy(out, in); err != nil {
			return fmt.Errorf("copying %s: %w", src, err)
		}
	}
	if err := sealFile(out, 0, gid, mode); err != nil {
		return fmt.Errorf("sealing %s: %w", dst, err)
	}
	// Durable only when the copy outlives the call's purpose: a snapshot at
	// rest must survive a power cut. A restore's copy in its chroot is gone
	// with the VM, and syncing it would write the whole memory image to disk
	// on every fork of a store without reflink.
	if mode == snapshotSealedMode {
		if err := out.Sync(); err != nil {
			return fmt.Errorf("syncing %s: %w", dst, err)
		}
	}
	ok = true
	return nil
}

// sealFile gives an open file to uid:gid with mode. Ownership only changes when
// the daemon runs as root (tests run unprivileged and own what they create).
func sealFile(f *os.File, uid, gid int, mode os.FileMode) error {
	if os.Geteuid() == 0 {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

// SealSnapshotDir enforces the sealed layout on a snapshot directory: the
// directory root-only, the memory image and vmstate root:root 0400 in
// root-owned inodes, the captured disk root 0600. Called on every new snapshot
// and, at startup, on every existing one, which migrates snapshots taken by
// older daemons: a memory image or vmstate still owned by someone else is
// re-captured into a fresh root inode (so no descriptor an old VMM kept open
// can reach it) and renamed over the old name. VMs already forked from the old
// inode keep it — nothing the migration can change — but no future restore
// gets it.
func SealSnapshotDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("snapshot dir %s is not a directory", dir)
	}
	if err := os.Chmod(dir, snapshotDirMode); err != nil {
		return fmt.Errorf("restricting %s: %w", dir, err)
	}
	for _, name := range []string{SnapshotStateFile, SnapshotMemFile} {
		if err := sealSnapshotArtifact(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	disk, err := openUntrustedRegular(filepath.Join(dir, SnapshotDiskFile))
	if err != nil {
		return fmt.Errorf("opening snapshot disk: %w", err)
	}
	defer disk.Close()
	if err := sealFile(disk, 0, 0, snapshotDiskMode); err != nil {
		return fmt.Errorf("sealing snapshot disk in %s: %w", dir, err)
	}
	return nil
}

func sealSnapshotArtifact(path string) error {
	f, err := openUntrustedRegular(path)
	if err != nil {
		return fmt.Errorf("opening snapshot artifact: %w", err)
	}
	fi, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		// Written by Firecracker under an older daemon: re-capture into a
		// fresh inode, then atomically replace the name — the artifact is
		// never missing, even if the daemon dies half-way.
		tmp := path + ".seal"
		_ = os.Remove(tmp)
		if err := copyInto(path, tmp, 0, snapshotSealedMode); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replacing %s: %w", path, err)
		}
		return nil
	}
	f, err = openUntrustedRegular(path)
	if err != nil {
		return fmt.Errorf("opening snapshot artifact: %w", err)
	}
	defer f.Close()
	if err := sealFile(f, 0, 0, snapshotSealedMode); err != nil {
		return fmt.Errorf("sealing %s: %w", path, err)
	}
	// A read grant left by a daemon that died mid-restore (see acl.go) must
	// not outlive it. path was just verified to be a regular file in a
	// root-only directory.
	return clearACL(path)
}
