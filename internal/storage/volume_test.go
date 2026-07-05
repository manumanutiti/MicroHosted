package storage

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireTool skips the test when an e2fsprogs binary the volume/offline layer
// shells out to isn't installed — CI without e2fsprogs shouldn't hard-fail.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed, skipping", name)
	}
}

func TestCreateVolumeMakesMountableExt4(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	path, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if path != VolumePath(dir, "vol1") {
		t.Fatalf("unexpected path %s", path)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat volume: %v", err)
	}
	if got := fi.Size(); got != 16*bytesPerMiB {
		t.Fatalf("volume size = %d, want %d", got, int64(16*bytesPerMiB))
	}

	// A valid ext4 image reports a clean filesystem to debugfs.
	if out, err := exec.Command("debugfs", "-R", "show_super_stats -h", path).CombinedOutput(); err != nil {
		t.Fatalf("debugfs on new volume: %v: %s", err, out)
	}
}

func TestInjectAndExtractFileRoundTrip(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	path, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	want := []byte("sample artifact bytes \x00\x01\x02 not-just-text")
	if err := InjectFile(path, "/out/sub/artifact.bin", bytes.NewReader(want), dir); err != nil {
		t.Fatalf("InjectFile: %v", err)
	}

	got := extractBytes(t, path, "/out/sub/artifact.bin", dir)
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip mismatch: got %q want %q", got, want)
	}
}

// extractBytes streams a file out of an image and reads it fully — the test
// helper mirror of ExtractFileStream for assertions.
func extractBytes(t *testing.T, imagePath, guestPath, stagingDir string) []byte {
	t.Helper()
	f, _, err := ExtractFileStream(imagePath, guestPath, stagingDir)
	if err != nil {
		t.Fatalf("ExtractFileStream(%s): %v", guestPath, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading extracted %s: %v", guestPath, err)
	}
	return data
}

func TestInjectFileOverwrites(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	path, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if err := InjectFile(path, "/f", bytes.NewReader([]byte("first-longer-content")), dir); err != nil {
		t.Fatalf("first InjectFile: %v", err)
	}
	if err := InjectFile(path, "/f", bytes.NewReader([]byte("second")), dir); err != nil {
		t.Fatalf("second InjectFile: %v", err)
	}
	if got := extractBytes(t, path, "/f", dir); string(got) != "second" {
		t.Fatalf("overwrite failed: got %q", got)
	}
}

func TestExtractMissingFileErrors(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	path, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, _, err := ExtractFileStream(path, "/nope", dir); err == nil {
		t.Fatal("expected error extracting a missing file, got nil")
	}
}

func TestExtractDirPullsTree(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	path, err := CreateVolume(dir, "vol1", 32, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := InjectFile(path, "/arts/a.txt", bytes.NewReader([]byte("aaa")), dir); err != nil {
		t.Fatalf("inject a: %v", err)
	}
	if err := InjectFile(path, "/arts/b.txt", bytes.NewReader([]byte("bbb")), dir); err != nil {
		t.Fatalf("inject b: %v", err)
	}

	dest := filepath.Join(dir, "extracted")
	if err := ExtractDir(path, "/arts", dest, dir); err != nil {
		t.Fatalf("ExtractDir: %v", err)
	}
	// rdump reproduces the tree under dest/arts.
	if b, err := os.ReadFile(filepath.Join(dest, "arts", "a.txt")); err != nil || string(b) != "aaa" {
		t.Fatalf("extracted a.txt = %q, err %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "arts", "b.txt")); err != nil || string(b) != "bbb" {
		t.Fatalf("extracted b.txt = %q, err %v", b, err)
	}
}

// TestInjectExtractLargeFileStreams round-trips a file bigger than any internal
// buffer through inject+extract, checked by hash. It's the regression guard for
// the whole point of this layer: constant memory via streaming, not io.ReadAll.
func TestInjectExtractLargeFileStreams(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	// 64 MiB volume, 40 MiB payload — big enough to prove nothing buffers the
	// whole thing, small enough to keep the test quick.
	path, err := CreateVolume(dir, "vol1", 64, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	const size = 40 << 20
	src := filepath.Join(dir, "payload")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("create payload: %v", err)
	}
	h := sha256.New()
	rng := rand.New(rand.NewSource(1))
	if _, err := io.CopyN(io.MultiWriter(f, h), rng, size); err != nil {
		t.Fatalf("writing payload: %v", err)
	}
	_ = f.Close()
	want := h.Sum(nil)

	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	defer in.Close()
	if err := InjectFile(path, "/big.bin", in, dir); err != nil {
		t.Fatalf("InjectFile: %v", err)
	}

	rc, gotSize, err := ExtractFileStream(path, "/big.bin", dir)
	if err != nil {
		t.Fatalf("ExtractFileStream: %v", err)
	}
	defer rc.Close()
	if gotSize != size {
		t.Fatalf("extracted size = %d, want %d", gotSize, size)
	}
	gh := sha256.New()
	if _, err := io.Copy(gh, rc); err != nil {
		t.Fatalf("streaming extracted: %v", err)
	}
	if !bytes.Equal(gh.Sum(nil), want) {
		t.Fatal("large-file round trip hash mismatch")
	}
}

func TestCleanGuestPathRejectsTraversal(t *testing.T) {
	// "/x/.." cleans to "/" (rejected as root). "/a/../../etc" cleans to "/etc",
	// a legitimate in-image path — debugfs can't escape the image to the host, so
	// normalising .. within the image is fine, not a traversal.
	for _, bad := range []string{"", "relative/path", "/", "/x/.."} {
		if _, err := cleanGuestPath(bad); err == nil {
			t.Errorf("cleanGuestPath(%q) = nil error, want rejection", bad)
		}
	}
	for _, good := range []string{"/a", "/a/b/c.bin", "/vol/out/x"} {
		if _, err := cleanGuestPath(good); err != nil {
			t.Errorf("cleanGuestPath(%q) = %v, want ok", good, err)
		}
	}
}
