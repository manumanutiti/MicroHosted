package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

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
// growRootfs for why an offline resize is all it takes. The grow runs once per
// golden and size, not once per VM: see sizedGolden.
func CloneRootfs(tpl types.Template, vmID string, instancesDir string, uid, gid int, diskMB int64) (string, error) {
	if err := os.MkdirAll(instancesDir, 0o755); err != nil {
		return "", fmt.Errorf("creating instances dir %s: %w", instancesDir, err)
	}

	dst := filepath.Join(instancesDir, vmID+".ext4")

	// Reflink from an already-grown copy of the golden when one can be had:
	// same bytes as growing this clone, without an e2fsck+resize2fs per VM.
	// Any trouble preparing it falls back to growing the clone itself.
	src := tpl.RootfsPath
	sized, err := sizedGolden(tpl.RootfsPath, instancesDir, diskMB)
	if err != nil {
		log.Printf("storage: no pre-grown copy of %s at %dMB, growing the clone instead: %v", tpl.RootfsPath, diskMB, err)
	} else if sized != "" {
		src = sized
	}

	if err := ReflinkFile(src, dst); err != nil {
		return "", fmt.Errorf("cloning rootfs for %s: %w", vmID, err)
	}

	if src == tpl.RootfsPath {
		if err := growRootfs(dst, diskMB); err != nil {
			_ = os.Remove(dst)
			return "", fmt.Errorf("growing clone for %s to %dMB: %w", vmID, diskMB, err)
		}
	}

	if err := os.Chown(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("chowning clone %s to %d:%d: %w", dst, uid, gid, err)
	}

	return dst, nil
}

// sizedDir holds the pre-grown copies of the goldens (see sizedGolden). A
// subdirectory, so the doctor's scan of the store's top level for orphaned
// <vm id>.ext4 clones never mistakes one for a VM disk.
const sizedDir = "sized"

// sizedMu serialises building pre-grown goldens, so concurrent creates of the
// same template and size grow it once instead of racing to do the same work.
var sizedMu sync.Mutex

// sizedGolden returns a copy of golden already grown to diskMB, building it on
// first use, or "" when no growth is needed (diskMB 0 or not above the
// golden's size). Growing a clone runs e2fsck and resize2fs, tens of
// milliseconds on every create, and the result depends only on the golden and
// the size, so it's done once and every VM reflinks the grown copy instead.
//
// The file name carries a key derived from the golden's identity (path,
// inode, size, mtime), so rebuilding a golden yields a new copy rather than
// serving a stale one; building it prunes the previous copies of that golden
// at that size. The copy is written under a temporary name and renamed into
// place, so a reader never sees a half-grown file.
func sizedGolden(golden, instancesDir string, diskMB int64) (string, error) {
	if diskMB <= 0 {
		return "", nil
	}
	key, size, err := goldenKey(golden)
	if err != nil {
		return "", err
	}
	if size >= diskMB*bytesPerMiB {
		return "", nil
	}

	prefix := fmt.Sprintf("%s-%dm-", goldenBase(golden), diskMB)
	dir := filepath.Join(instancesDir, sizedDir)
	path := filepath.Join(dir, prefix+key+".ext4")

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	sizedMu.Lock()
	defer sizedMu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil // built by a concurrent create while we waited
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := ReflinkFile(golden, tmp); err != nil {
		return "", err
	}
	if err := growRootfs(tmp, diskMB); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	if stale, err := filepath.Glob(filepath.Join(dir, prefix+"*.ext4")); err == nil {
		for _, s := range stale {
			if s != path {
				_ = os.Remove(s)
			}
		}
	}
	return path, nil
}

// goldenKey identifies a golden as it is right now (path, inode, size, mtime),
// so a rebuilt golden gets a different key; it also returns the golden's size.
func goldenKey(golden string) (key string, size int64, err error) {
	fi, err := os.Stat(golden)
	if err != nil {
		return "", 0, err
	}
	abs, err := filepath.Abs(golden)
	if err != nil {
		return "", 0, err
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d|%d", abs, ino, fi.Size(), fi.ModTime().UnixNano()))
	return hex.EncodeToString(sum[:6]), fi.Size(), nil
}

// goldenBase is the golden's file name without .ext4: the leading part of its
// pre-grown copies' names.
func goldenBase(golden string) string {
	return strings.TrimSuffix(filepath.Base(golden), ".ext4")
}

// PruneSizedGoldens removes the pre-grown copies (see sizedGolden) that match
// none of goldens as they are now: their template left the catalog, or its
// golden was rebuilt or deleted. Leftover temporary files from an interrupted
// build go too. They are a cache, rebuilt by the next create that needs one,
// so this is safe whenever no create is in flight. Returns what it removed.
func PruneSizedGoldens(instancesDir string, goldens []string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(instancesDir, sizedDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// A copy is named <golden base>-<size>m-<golden key>.ext4.
	current := make(map[string]bool)
	for _, g := range goldens {
		if key, _, err := goldenKey(g); err == nil {
			current[goldenBase(g)+"|"+key] = true
		}
	}
	keep := func(name string) bool {
		rest, ok := strings.CutSuffix(name, ".ext4")
		if !ok {
			return false
		}
		i := strings.LastIndexByte(rest, '-')
		if i < 0 {
			return false
		}
		rest, key := rest[:i], rest[i+1:]
		j := strings.LastIndexByte(rest, '-')
		if j < 0 {
			return false
		}
		base, size := rest[:j], rest[j+1:]
		if n, ok := strings.CutSuffix(size, "m"); !ok || n == "" || strings.Trim(n, "0123456789") != "" {
			return false
		}
		return current[base+"|"+key]
	}

	var removed []string
	var errs []error
	for _, e := range entries {
		if e.IsDir() || keep(e.Name()) {
			continue
		}
		path := filepath.Join(instancesDir, sizedDir, e.Name())
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(errs...)
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
