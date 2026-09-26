package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCaptureSnapshotFileNewInode: the captured artifact is a different inode
// from the one Firecracker wrote, so a descriptor the VMM kept open on its own
// output reaches nothing in the snapshot.
func TestCaptureSnapshotFileNewInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "snap.mem")
	if err := os.WriteFile(src, []byte("memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	dst := filepath.Join(dir, "mem")
	if err := CaptureSnapshotFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
	if _, err := held.WriteAt([]byte("EVIL!!"), 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "memory" {
		t.Fatalf("sealed artifact = %q, %v", b, err)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != snapshotSealedMode {
		t.Fatalf("mode %v, want %v", fi.Mode().Perm(), os.FileMode(snapshotSealedMode))
	}
}

// TestCaptureSnapshotFileRefusesNonRegular: a symlink or FIFO planted where
// Firecracker's output should be is refused, never followed or waited on.
func TestCaptureSnapshotFileRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "host-secret")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mem")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := CaptureSnapshotFile(link, filepath.Join(dir, "a")); err == nil {
		t.Fatal("followed a symlink")
	}
	fifo := filepath.Join(dir, "fifo.mem")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CaptureSnapshotFile(fifo, filepath.Join(dir, "b")); err == nil {
		t.Fatal("accepted a FIFO")
	}
	for _, n := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("left %s behind", n)
		}
	}
}

// TestSealSnapshotDirTightensModes covers the startup migration's mode part.
func TestSealSnapshotDirTightensModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{SnapshotStateFile, SnapshotMemFile, SnapshotDiskFile} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := SealSnapshotDir(dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{
		"":                snapshotDirMode,
		SnapshotStateFile: snapshotSealedMode,
		SnapshotMemFile:   snapshotSealedMode,
		SnapshotDiskFile:  snapshotDiskMode,
	}
	for n, m := range want {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != m {
			t.Errorf("%q mode %v, want %v", n, fi.Mode().Perm(), m)
		}
	}
	// Idempotent.
	if err := SealSnapshotDir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestSealSnapshotDirRefusesSymlinkedArtifact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(dir), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, SnapshotMemFile)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{SnapshotStateFile, SnapshotDiskFile} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SealSnapshotDir(dir); err == nil {
		t.Fatal("sealed a snapshot whose memory image is a symlink")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target's mode changed to %v", fi.Mode().Perm())
	}
}

// TestInstallSnapshotFilePrivateCopy: each restore gets its own inode, so a
// write through one fork's copy (were it writable) reaches neither the
// snapshot nor a sibling; a symlink planted at the destination is refused.
func TestInstallSnapshotFilePrivateCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mem")
	if err := os.WriteFile(src, []byte("clean"), 0o400); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, dst := range []string{a, b} {
		if err := InstallSnapshotFile(src, dst, os.Getgid()); err != nil {
			t.Fatal(err)
		}
	}
	fa, _ := os.Stat(a)
	fb, _ := os.Stat(b)
	fs, _ := os.Stat(src)
	if os.SameFile(fa, fs) || os.SameFile(fa, fb) {
		t.Fatal("restore copies share an inode")
	}
	if fa.Mode().Perm() != snapshotInstalledMode {
		t.Fatalf("mode %v", fa.Mode().Perm())
	}
	planted := filepath.Join(dir, "planted")
	if err := os.Symlink(filepath.Join(dir, "victim"), planted); err != nil {
		t.Fatal(err)
	}
	if err := InstallSnapshotFile(src, planted, os.Getgid()); err == nil {
		t.Fatal("wrote through a planted symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, "victim")); !os.IsNotExist(err) {
		t.Fatal("symlink target created")
	}
}
