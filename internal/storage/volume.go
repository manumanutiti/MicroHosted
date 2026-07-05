package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Volumes live under <instancesDir>/volumes/ — the same CoW store as clones,
// kernels and the Jailer chroot, on purpose: Jailer hardlinks a VM's drives
// into its chroot and a hardlink can't cross filesystems (the L3 invariant).

// VolumesDir returns the directory holding all volume images.
func VolumesDir(instancesDir string) string {
	return filepath.Join(instancesDir, "volumes")
}

// VolumePath returns the host-side ext4 file for one volume.
func VolumePath(instancesDir, volID string) string {
	return filepath.Join(VolumesDir(instancesDir), volID+".ext4")
}

// CreateVolume provisions a fresh, empty ext4 volume image of sizeMB MiB and
// chowns it to the jailer uid/gid so the jailed Firecracker can open it for
// writing. Same shape as a golden rootfs — a bare ext4 written straight onto the
// file with no partition table (mkfs.ext4 -F on the truncated file) — so a
// future grow path is the same offline resize2fs the clones already use.
func CreateVolume(instancesDir, volID string, sizeMB int64, uid, gid int) (string, error) {
	if sizeMB <= 0 {
		return "", fmt.Errorf("volume size must be positive, got %d MiB", sizeMB)
	}
	dir := VolumesDir(instancesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating volumes dir %s: %w", dir, err)
	}

	path := VolumePath(instancesDir, volID)
	if err := os.Truncate(path, 0); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("preparing volume file %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating volume file %s: %w", path, err)
	}
	_ = f.Close()

	if err := os.Truncate(path, sizeMB*bytesPerMiB); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("sizing volume file %s: %w", path, err)
	}

	// -F: force mkfs on a plain file (not a block device) without prompting.
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", path).CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("mkfs.ext4 on volume %s: %v: %s", path, err, out)
	}

	if err := os.Chown(path, uid, gid); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("chowning volume %s to %d:%d: %w", path, uid, gid, err)
	}

	return path, nil
}

// DeleteVolume removes a volume's image file. Safe to call if it doesn't exist.
func DeleteVolume(instancesDir, volID string) error {
	path := VolumePath(instancesDir, volID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing volume %s: %w", path, err)
	}
	return nil
}
