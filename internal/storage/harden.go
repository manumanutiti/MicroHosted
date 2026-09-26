package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store permissions. The store holds every VM's disk, every volume, console
// logs and snapshots — guest data, secrets included — so no local user but
// root (and, for what it must open, the jailer identity) may read any of it.
//
// Directories are 0711: the jailer identity has to traverse them (debugfs runs
// as it and opens images by path) but nobody can list them. Guest data files
// are 0600, owned by whoever must open them: the owning VM's (or volume's)
// identity for disks and volumes, root for logs.
const (
	// StoreDirMode is the mode of the store root and its traversable subdirs.
	StoreDirMode = 0o711
	// privateFileMode is the mode of every file holding guest data.
	privateFileMode = 0o600
)

// HardenStore brings an existing store to the modes new files are created
// with, so hosts installed before the store was locked down are not left
// exposed: the directories 0711 (snapshots 0700, see SealSnapshotDir), VM
// disks, volumes and console logs 0600. Only regular files are touched, through
// an O_NOFOLLOW descriptor, never a symlink. Goldens, kernels and the image
// store hold no guest data and keep their own modes.
//
// Errors are collected rather than stopping at the first: the caller logs them,
// and one stubborn file must not leave the rest world-readable.
func HardenStore(instancesDir string) error {
	var errs []error
	chmodDir := func(dir string, mode os.FileMode) {
		fi, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err == nil && !fi.IsDir() {
			err = fmt.Errorf("not a directory")
		}
		if err == nil {
			err = os.Chmod(dir, mode)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}
	privateFiles := func(dir string, suffixes ...string) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !hasAnySuffix(e.Name(), suffixes) {
				continue
			}
			if err := chmodRegular(filepath.Join(dir, e.Name()), privateFileMode); err != nil {
				errs = append(errs, err)
			}
		}
	}

	chmodDir(instancesDir, StoreDirMode)
	chmodDir(VolumesDir(instancesDir), StoreDirMode)
	chmodDir(filepath.Join(instancesDir, "staging"), StoreDirMode)
	chmodDir(filepath.Join(instancesDir, "snapshots"), snapshotDirMode)
	privateFiles(instancesDir, ".ext4", ".log", ".log.1")
	privateFiles(VolumesDir(instancesDir), ".ext4")
	return errors.Join(errs...)
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// chmodRegular sets mode on path only if it is a regular file, without
// following a symlink.
func chmodRegular(path string, mode os.FileMode) error {
	f, err := openUntrustedRegular(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// OwnPrivate gives the regular file at path to uid:gid with mode 0600, through
// an O_NOFOLLOW descriptor so a symlink is never followed. It is how a disk or
// volume changes hands between identities. Ownership only changes when the
// daemon runs as root.
func OwnPrivate(path string, uid, gid int) error {
	f, err := openUntrustedRegular(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	if err := sealFile(f, uid, gid, privateFileMode); err != nil {
		return fmt.Errorf("giving %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}
