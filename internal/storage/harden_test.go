package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHardenStoreMigratesModes: a store laid out by an older daemon (0755 dirs,
// 0644 disks and logs) ends up private, and a symlink planted among the disks
// is not followed.
func TestHardenStoreMigratesModes(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	for _, d := range []string{store, VolumesDir(store), filepath.Join(store, "snapshots"), filepath.Join(store, "rootfs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		filepath.Join(store, "aaaa1111.ext4"),
		filepath.Join(store, "aaaa1111.log"),
		filepath.Join(store, "aaaa1111.log.1"),
		VolumePath(store, "vol1"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := filepath.Join(store, "rootfs", "golden.ext4")
	if err := os.WriteFile(golden, []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store, "bbbb2222.ext4")); err != nil {
		t.Fatal(err)
	}

	if err := HardenStore(store); err != nil {
		t.Fatal(err)
	}
	check := func(p string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
	check(store, StoreDirMode)
	check(VolumesDir(store), StoreDirMode)
	check(filepath.Join(store, "snapshots"), snapshotDirMode)
	for _, f := range files {
		check(f, privateFileMode)
	}
	check(golden, 0o644)  // not guest data: left alone
	check(outside, 0o644) // symlink not followed
}
