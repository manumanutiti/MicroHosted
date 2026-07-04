package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotDirLifecycle(t *testing.T) {
	instances := t.TempDir()

	dir, err := CreateSnapshotDir(instances, "snap1234")
	if err != nil {
		t.Fatalf("CreateSnapshotDir: %v", err)
	}
	if dir != SnapshotDir(instances, "snap1234") {
		t.Fatalf("dir = %q, want %q", dir, SnapshotDir(instances, "snap1234"))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("snapshot dir not created: %v", err)
	}

	// A snapshot ID collision is a logic bug and must fail loudly, not merge.
	if _, err := CreateSnapshotDir(instances, "snap1234"); err == nil {
		t.Fatal("second CreateSnapshotDir with same ID must fail")
	}

	if err := DeleteSnapshotDir(instances, "snap1234"); err != nil {
		t.Fatalf("DeleteSnapshotDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot dir removed, stat err=%v", err)
	}
	// Deleting an already-absent snapshot dir is not an error.
	if err := DeleteSnapshotDir(instances, "snap1234"); err != nil {
		t.Fatalf("DeleteSnapshotDir on absent dir: %v", err)
	}
}

// CloneFromSnapshot must produce a private copy: writing to the clone can't
// touch the snapshot's captured disk (reflink where supported, full copy
// elsewhere — either way the contents must diverge safely).
func TestCloneFromSnapshotIsPrivate(t *testing.T) {
	instances := t.TempDir()
	snapDir, err := CreateSnapshotDir(instances, "snapabcd")
	if err != nil {
		t.Fatalf("CreateSnapshotDir: %v", err)
	}

	captured := []byte("disk contents captured at snapshot time")
	if err := os.WriteFile(filepath.Join(snapDir, SnapshotDiskFile), captured, 0o644); err != nil {
		t.Fatalf("writing captured disk: %v", err)
	}
	before := checksum(t, filepath.Join(snapDir, SnapshotDiskFile))

	clone, err := CloneFromSnapshot(snapDir, "fork0001", instances, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CloneFromSnapshot: %v", err)
	}
	if clone != filepath.Join(instances, "fork0001.ext4") {
		t.Fatalf("clone path = %q", clone)
	}
	if checksum(t, clone) != before {
		t.Fatal("clone contents differ from the captured disk")
	}

	if err := os.WriteFile(clone, []byte("guest wrote after fork"), 0o644); err != nil {
		t.Fatalf("writing to clone: %v", err)
	}
	if checksum(t, filepath.Join(snapDir, SnapshotDiskFile)) != before {
		t.Fatal("writing to the fork's clone modified the snapshot's captured disk")
	}
}
