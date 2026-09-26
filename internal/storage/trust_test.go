package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// The chain checks run against a temp tree the test owns; the directories
// above it (/tmp is 1777) are deliberately left out.
func TestCheckChain(t *testing.T) {
	me := uint32(os.Getuid())
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "microhosted.db")

	// A database not created yet, in a private directory: fine.
	if err := checkChain([]string{db, dir}, me); err != nil {
		t.Fatalf("missing file in a private dir: %v", err)
	}
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkChain([]string{db, dir}, me); err != nil {
		t.Fatalf("private file: %v", err)
	}
	// Someone else's file is refused.
	if err := checkChain([]string{db, dir}, me+1); err == nil {
		t.Fatal("accepted a file owned by another uid")
	}
	// A group/other-writable file, or directory above it, is refused.
	if err := os.Chmod(db, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := checkChain([]string{db, dir}, me); err == nil {
		t.Fatal("accepted a group-writable file")
	}
	if err := os.Chmod(db, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkChain([]string{db, dir}, me); err == nil {
		t.Fatal("accepted a world-writable directory")
	}
	// A missing directory is an error, not a pass.
	if err := checkChain([]string{db, filepath.Join(dir, "nope")}, me); err == nil {
		t.Fatal("accepted a missing directory")
	}
}

func TestRestrictDB(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	for _, p := range []string{db, db + "-wal"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := RestrictDB(db); err != nil { // -shm/-journal absent: not an error
		t.Fatal(err)
	}
	for _, p := range []string{db, db + "-wal"} {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", p, fi.Mode().Perm())
		}
	}
}
