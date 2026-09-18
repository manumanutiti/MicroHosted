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
	oio := OfflineIO{StagingDir: dir}
	if err := oio.InjectFile(path, "/out/sub/artifact.bin", bytes.NewReader(want)); err != nil {
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
	f, _, err := OfflineIO{StagingDir: stagingDir}.ExtractFileStream(imagePath, guestPath)
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

	oio := OfflineIO{StagingDir: dir}
	if err := oio.InjectFile(path, "/f", bytes.NewReader([]byte("first-longer-content"))); err != nil {
		t.Fatalf("first InjectFile: %v", err)
	}
	if err := oio.InjectFile(path, "/f", bytes.NewReader([]byte("second"))); err != nil {
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
	if _, _, err := (OfflineIO{StagingDir: dir}).ExtractFileStream(path, "/nope"); err == nil {
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
	oio := OfflineIO{StagingDir: dir}
	if err := oio.InjectFile(path, "/arts/a.txt", bytes.NewReader([]byte("aaa"))); err != nil {
		t.Fatalf("inject a: %v", err)
	}
	if err := oio.InjectFile(path, "/arts/b.txt", bytes.NewReader([]byte("bbb"))); err != nil {
		t.Fatalf("inject b: %v", err)
	}

	dest := filepath.Join(dir, "extracted")
	if err := oio.ExtractDir(path, "/arts", dest); err != nil {
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
	oio := OfflineIO{StagingDir: dir}
	if err := oio.InjectFile(path, "/big.bin", in); err != nil {
		t.Fatalf("InjectFile: %v", err)
	}

	rc, gotSize, err := oio.ExtractFileStream(path, "/big.bin")
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
	// a legitimate in-image path — debugfs resolves paths inside the image, so
	// normalising .. there is fine, not a traversal. (What CAN reach the host is
	// a smuggled debugfs command — see TestGuestPathRefusesDebugfsInjection.)
	for _, bad := range []string{"", "relative/path", "/", "/x/.."} {
		if _, err := cleanGuestPath(bad); err == nil {
			t.Errorf("cleanGuestPath(%q) = nil error, want rejection", bad)
		}
	}
	for _, good := range []string{"/a", "/a/b/c.bin", "/vol/out/x", "/my dir/a file.txt", "/ñandú/x's.bin"} {
		if _, err := cleanGuestPath(good); err != nil {
			t.Errorf("cleanGuestPath(%q) = %v, want ok", good, err)
		}
	}
}

// TestGuestPathRefusesDebugfsInjection pins the boundary that keeps a path from
// becoming debugfs commands: debugfs reads one command per line, so a newline in
// a path would run a command of the caller's (or the guest's) choosing as the
// jailer uid — "dump" writes a host file, "write" reads one.
func TestGuestPathRefusesDebugfsInjection(t *testing.T) {
	for _, bad := range []string{
		"/x\ndump /etc/shadow /tmp/out",
		"/x\rstat /",
		"/x\x00y",
		"/tab\there",
		"/x\x7f",
		`/a" "/b`,
	} {
		if _, err := ValidateGuestPath(bad); err == nil {
			t.Errorf("ValidateGuestPath(%q) accepted", bad)
		}
	}
	if _, err := debugfsLine("rdump", "/ok", "/host\ndir"); err == nil {
		t.Error("debugfsLine accepted a host path with a newline")
	}
	got, err := debugfsLine("write", "/stage/inject-1", "/my file")
	if err != nil || got != "write \"/stage/inject-1\" \"/my file\"\n" {
		t.Errorf("debugfsLine = %q, %v", got, err)
	}
}

// TestInjectionAttemptReachesNoHostFile runs the attack against real debugfs:
// a file planted in the image, then an inject whose path tries to dump it to
// the host. Nothing may appear on the host, and the call must fail.
func TestInjectionAttemptReachesNoHostFile(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	img, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	oio := OfflineIO{StagingDir: dir}
	if err := oio.InjectFile(img, "/secret", bytes.NewReader([]byte("in-image secret"))); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	leak := filepath.Join(dir, "leaked")
	evil := "/x\ndump /secret " + leak
	if err := oio.InjectFile(img, evil, bytes.NewReader([]byte("p"))); err == nil {
		t.Error("InjectFile accepted a path carrying a newline")
	}
	if _, _, err := oio.ExtractFileStream(img, evil); err == nil {
		t.Error("ExtractFileStream accepted a path carrying a newline")
	}
	if err := oio.ExtractDir(img, evil, filepath.Join(dir, "out")); err == nil {
		t.Error("ExtractDir accepted a path carrying a newline")
	}
	if _, err := os.Stat(leak); !os.IsNotExist(err) {
		t.Fatalf("a smuggled debugfs command wrote %s on the host (stat: %v)", leak, err)
	}
}

// TestPathsWithSpacesRoundTrip checks the quoting against real debugfs: before
// it, "/my file" reached debugfs as two arguments.
func TestPathsWithSpacesRoundTrip(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	dir := t.TempDir()
	img, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	oio := OfflineIO{StagingDir: dir}
	const p = "/out dir/my artifact.bin"
	if err := oio.InjectFile(img, p, bytes.NewReader([]byte("spaced"))); err != nil {
		t.Fatalf("InjectFile(%q): %v", p, err)
	}
	if got := extractBytes(t, img, p, dir); string(got) != "spaced" {
		t.Fatalf("extract %q = %q", p, got)
	}

	dest := filepath.Join(dir, "dest with space")
	if err := oio.ExtractDir(img, "/out dir", dest); err != nil {
		t.Fatalf("ExtractDir: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "out dir", "my artifact.bin")); err != nil || string(b) != "spaced" {
		t.Fatalf("rdump result: %q, %v", b, err)
	}
}
