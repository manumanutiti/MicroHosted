package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Snapshot artifacts live under <instancesDir>/snapshots/<snapshotID>/ — the
// same CoW store as clones, kernels and the Jailer chroot, on purpose: the
// disk is captured with a reflink (instant, shares blocks with the live clone)
// and restores hardlink everything into a chroot, and neither operation
// crosses filesystems.

// snapshotFileNames are the fixed names of a snapshot's artifacts inside its
// directory.
const (
	SnapshotStateFile = "vmstate"
	SnapshotMemFile   = "mem"
	SnapshotDiskFile  = "disk.ext4"
)

// SnapshotDir returns the directory holding one snapshot's artifacts.
func SnapshotDir(instancesDir, snapshotID string) string {
	return filepath.Join(instancesDir, "snapshots", snapshotID)
}

// CreateSnapshotDir makes the snapshot's directory, failing if it already
// exists (snapshot IDs are fresh UUIDs; a collision means a logic bug, not
// something to silently merge into).
func CreateSnapshotDir(instancesDir, snapshotID string) (string, error) {
	dir := SnapshotDir(instancesDir, snapshotID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("creating snapshots dir: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating snapshot dir %s: %w", dir, err)
	}
	return dir, nil
}

// DeleteSnapshotDir removes a snapshot's directory and artifacts. Safe on an
// already-absent directory. VMs previously restored from this snapshot are
// unaffected: their chroots hold hardlinks to the mem/vmstate inodes (which
// survive until those VMs are destroyed) and their disks are private reflink
// copies.
func DeleteSnapshotDir(instancesDir, snapshotID string) error {
	dir := SnapshotDir(instancesDir, snapshotID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing snapshot dir %s: %w", dir, err)
	}
	return nil
}

// ReflinkFile clones src to dst copy-on-write where the filesystem supports it
// (the CoW store's whole point), transparently falling back to a full copy.
// This is CloneRootfs's copy step, extracted: snapshots need it twice more —
// capturing a paused VM's disk into the snapshot dir, and stamping out a
// private disk for each restored VM.
func ReflinkFile(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(dst) // partial output from the failed cp, if any
		if copyErr := copyFile(src, dst); copyErr != nil {
			return fmt.Errorf("cp --reflink failed (%s), fallback copy failed: %w", out, copyErr)
		}
	}
	return nil
}

// CloneFromSnapshot stamps out a VM's private disk from a snapshot's captured
// disk: a reflink copy chowned to the jailer uid/gid so the jailed Firecracker
// can write it. No resize — the captured disk already has the size (and
// filesystem state) the snapshotted guest had; growing it here would desync it
// from the memory image, which remembers the old block device size.
func CloneFromSnapshot(snapDir, vmID, instancesDir string, uid, gid int) (string, error) {
	dst := filepath.Join(instancesDir, vmID+".ext4")
	if err := ReflinkFile(filepath.Join(snapDir, SnapshotDiskFile), dst); err != nil {
		return "", fmt.Errorf("cloning snapshot disk for %s: %w", vmID, err)
	}
	if err := os.Chown(dst, uid, gid); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("chowning clone %s to %d:%d: %w", dst, uid, gid, err)
	}
	return dst, nil
}
