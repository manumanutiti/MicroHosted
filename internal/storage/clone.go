package storage

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"microhosted/pkg/types"
)

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
func CloneRootfs(tpl types.Template, vmID string, instancesDir string, uid, gid int) (string, error) {
	if err := os.MkdirAll(instancesDir, 0o755); err != nil {
		return "", fmt.Errorf("creating instances dir %s: %w", instancesDir, err)
	}

	dst := filepath.Join(instancesDir, vmID+".ext4")

	cmd := exec.Command("cp", "--reflink=auto", tpl.RootfsPath, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(dst) // partial output from the failed cp, if any
		if copyErr := copyFile(tpl.RootfsPath, dst); copyErr != nil {
			return "", fmt.Errorf("cloning rootfs for %s: cp failed (%s), fallback copy failed: %w", vmID, out, copyErr)
		}
	}

	if err := os.Chown(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("chowning clone %s to %d:%d: %w", dst, uid, gid, err)
	}

	return dst, nil
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
