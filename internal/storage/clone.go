package storage

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"microhosted/pkg/types"
)

// bytesPerMiB is the multiplier used to turn the MiB sizes carried on
// templates/requests into the byte sizes os.Truncate wants.
const bytesPerMiB = 1024 * 1024

// CloneRootfs makes a private, writable copy of a template's golden rootfs
// for a single VM. The golden image in the catalog is never opened for
// writing — every VM gets its own file under instancesDir.
//
// It prefers `cp --reflink=auto`, a copy-on-write clone that's instant and
// space-free on filesystems that support it (btrfs, XFS with reflink) and
// transparently falls back to a full copy otherwise. If the `cp` binary
// doesn't understand --reflink at all (e.g. non-GNU coreutils), it falls
// back to a plain Go io.Copy.
//
// The clone is chowned to uid/gid — Jailer hard-links it as-is into the
// chroot (it never chowns drive files, only its own fifos), and Firecracker
// runs as that uid/gid once inside, so without this it can open the disk for
// read but not write.
//
// diskMB grows the clone (and its ext4 filesystem) to that size in MiB so the
// guest has room to write — a golden image is usually near-full, and a clone
// left at its size runs out of space the moment the guest apt-installs
// anything. Growth is additive only: a diskMB of 0, or one no larger than the
// golden, leaves the clone as-is (CloneRootfs never shrinks a disk). See
// growRootfs for why an offline resize is all it takes.
func CloneRootfs(tpl types.Template, vmID string, instancesDir string, uid, gid int, diskMB int64) (string, error) {
	if err := os.MkdirAll(instancesDir, 0o755); err != nil {
		return "", fmt.Errorf("creating instances dir %s: %w", instancesDir, err)
	}

	dst := filepath.Join(instancesDir, vmID+".ext4")

	if err := ReflinkFile(tpl.RootfsPath, dst); err != nil {
		return "", fmt.Errorf("cloning rootfs for %s: %w", vmID, err)
	}

	if err := growRootfs(dst, diskMB); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("growing clone for %s to %dMB: %w", vmID, diskMB, err)
	}

	if err := os.Chown(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("chowning clone %s to %d:%d: %w", dst, uid, gid, err)
	}

	return dst, nil
}

// growRootfs enlarges an ext4 image file to sizeMB MiB and expands its
// filesystem to fill the new space. It's a no-op when sizeMB is 0 or no larger
// than the file already is — CloneRootfs only ever grows disks, never shrinks
// them (an ext4 shrink is slow, needs a full fsck, and risks the guest's data).
//
// It works offline, with nothing running in the guest, because the golden
// images are a bare ext4 written straight onto the whole device (mkfs.ext4 on
// the file, no partition table — see scripts/build-rootfs.sh and the
// LABEL=rootfs fstab): so truncating the file to the target size and letting
// resize2fs grow the filesystem into it is the whole job, and the guest sees
// the full disk at first boot.
func growRootfs(path string, sizeMB int64) error {
	if sizeMB <= 0 {
		return nil
	}
	target := sizeMB * bytesPerMiB

	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() >= target {
		return nil // already at/above the requested size — never shrink
	}

	if err := os.Truncate(path, target); err != nil {
		return fmt.Errorf("resizing image file: %w", err)
	}

	// resize2fs refuses to touch a filesystem that isn't marked clean, so force
	// a check first: a golden left dirty (or one whose clean bit we can't vouch
	// for) would otherwise make the grow bail out. e2fsck exits 1/2 when it
	// corrected something, which is fine for an offline image we're about to
	// grow; only 4+ (uncorrected errors / usage) is a real failure.
	if out, err := exec.Command("e2fsck", "-fy", path).CombinedOutput(); err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() >= 4 {
			return fmt.Errorf("e2fsck before resize: %v: %s", err, out)
		}
	}

	// No size argument: resize2fs grows the filesystem to fill the whole device
	// (here, the freshly-truncated file).
	if out, err := exec.Command("resize2fs", path).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %v: %s", err, out)
	}
	return nil
}

// SupportsReflink reports whether dir sits on a filesystem that can make
// copy-on-write clones (btrfs, XFS with reflink, ...). It's how the daemon
// warns at startup when the instances store is on a plain filesystem (ext4
// without reflink): there CloneRootfs's cp --reflink=auto silently falls back
// to a *full* copy, so N VMs of a 1GB image cost N GB — the storage blow-up
// scripts/setup-host.sh's CoW store exists to avoid. It answers by actually
// attempting a reflink between two temp files in dir (support depends on the
// concrete filesystem, not just its type) and cleaning both up.
func SupportsReflink(dir string) bool {
	src, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	srcPath := src.Name()
	// A byte or two so the clone exercises real block sharing rather than an
	// empty-file shortcut.
	_, _ = src.WriteString("reflink probe")
	_ = src.Close()
	defer os.Remove(srcPath)

	dstPath := srcPath + ".clone"
	defer os.Remove(dstPath)

	return exec.Command("cp", "--reflink=always", srcPath, dstPath).Run() == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// DeleteClone removes a VM's private rootfs copy. Safe to call even if the
// file doesn't exist.
func DeleteClone(instancesDir, vmID string) error {
	path := filepath.Join(instancesDir, vmID+".ext4")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing clone %s: %w", path, err)
	}
	return nil
}
