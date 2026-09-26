package storage

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestACLEncodingRoundTrip(t *testing.T) {
	in := []aclEntry{
		{aclOther, 0, aclUndefined},
		{aclGroup, aclRead, 1900000002},
		{aclUserObj, aclRead, aclUndefined},
		{aclMask, aclRead, aclUndefined},
		{aclGroupObj, 0, aclUndefined},
	}
	out, err := decodeACL(encodeACL(in))
	if err != nil {
		t.Fatal(err)
	}
	// The kernel requires entries sorted by tag, then id.
	want := []aclEntry{
		{aclUserObj, aclRead, aclUndefined},
		{aclGroupObj, 0, aclUndefined},
		{aclGroup, aclRead, 1900000002},
		{aclMask, aclRead, aclUndefined},
		{aclOther, 0, aclUndefined},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %+v", out)
	}
	if _, err := decodeACL([]byte{1, 2, 3}); err == nil {
		t.Fatal("accepted a malformed ACL")
	}
}

// sealedFile creates a sealed-mode file, skipping the test where the temp
// filesystem has no POSIX ACLs.
func sealedFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(p, []byte("memory"), snapshotSealedMode); err != nil {
		t.Fatal(err)
	}
	if _, err := readACL(p); errors.Is(err, ErrACLUnsupported) {
		t.Skip("no POSIX ACL support on the temp filesystem")
	}
	return p
}

// A grant is read-only and per group; revoking one keeps the others, and
// revoking the last one leaves no ACL and the sealed mode.
func TestGrantRevokeGroupRead(t *testing.T) {
	p := sealedFile(t)
	a, b := 1900000001, 1900000002
	if err := grantGroupRead(p, a); err != nil {
		if errors.Is(err, ErrACLUnsupported) {
			t.Skip("no POSIX ACL support")
		}
		t.Fatal(err)
	}
	if err := grantGroupRead(p, b); err != nil {
		t.Fatal(err)
	}
	entries, err := readACL(p)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[uint32]bool{}
	for _, e := range entries {
		if e.perm&^aclRead != 0 {
			t.Fatalf("entry %+v grants more than read", e)
		}
		if e.tag == aclGroup {
			groups[e.id] = true
		}
	}
	if !groups[uint32(a)] || !groups[uint32(b)] {
		t.Fatalf("groups = %v", groups)
	}
	if err := revokeGroupRead(p, a); err != nil {
		t.Fatal(err)
	}
	entries, _ = readACL(p)
	for _, e := range entries {
		if e.tag == aclGroup && e.id == uint32(a) {
			t.Fatal("revoked group still present")
		}
	}
	if err := revokeGroupRead(p, b); err != nil {
		t.Fatal(err)
	}
	if entries, _ := readACL(p); entries != nil {
		t.Fatalf("ACL left behind: %+v", entries)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != snapshotSealedMode {
		t.Fatalf("mode %v after last revoke", fi.Mode().Perm())
	}
}

// ShareSnapshotFile links the shared inode (no copy) and its revoke removes
// the grant; a symlink planted at the destination is refused.
func TestShareSnapshotFile(t *testing.T) {
	src := sealedFile(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "snap.mem")
	revoke, err := ShareSnapshotFile(src, dst, 1900000003)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := os.Stat(src)
	fd, _ := os.Stat(dst)
	if !os.SameFile(fs, fd) {
		t.Fatal("restore did not share the snapshot's inode")
	}
	if err := revoke(); err != nil {
		t.Fatal(err)
	}
	if err := revoke(); err != nil { // idempotent
		t.Fatal(err)
	}
	if entries, _ := readACL(src); entries != nil {
		t.Fatalf("grant outlived revoke: %+v", entries)
	}

	planted := filepath.Join(dir, "planted")
	if err := os.Symlink(filepath.Join(dir, "victim"), planted); err != nil {
		t.Fatal(err)
	}
	if _, err := ShareSnapshotFile(src, planted, 1900000004); err == nil {
		t.Fatal("linked over a planted symlink")
	}
	if entries, _ := readACL(src); entries != nil {
		t.Fatalf("failed share left a grant: %+v", entries)
	}
}

// SealSnapshotDir clears a grant a crashed daemon left behind.
func TestSealClearsLeftoverGrant(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{SnapshotStateFile, SnapshotMemFile, SnapshotDiskFile} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	mem := filepath.Join(dir, SnapshotMemFile)
	if err := grantGroupRead(mem, 1900000005); err != nil {
		if errors.Is(err, ErrACLUnsupported) {
			t.Skip("no POSIX ACL support")
		}
		t.Fatal(err)
	}
	if err := SealSnapshotDir(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := readACL(mem); entries != nil {
		t.Fatalf("leftover grant survived sealing: %+v", entries)
	}
}
