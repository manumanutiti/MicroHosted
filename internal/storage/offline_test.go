package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sparseImage makes a small volume holding a sparse file of size bytes at
// guestPath — what a guest gets from `truncate -s`: no blocks in the image,
// any i_size. It is written with debugfs from a sparse host file, which debugfs
// copies hole for hole.
func sparseImage(t *testing.T, guestPath string, size int64) (img string, o OfflineIO) {
	t.Helper()
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")
	dir := t.TempDir()
	img, err := CreateVolume(dir, "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	src := filepath.Join(dir, "sparse")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, size); err != nil {
		t.Skipf("host filesystem cannot hold a %d-byte sparse file: %v", size, err)
	}
	o = OfflineIO{StagingDir: filepath.Join(dir, "staging")}
	script, err := debugfsLine("write", src, guestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.runDebugfs(img, true, script); err != nil {
		t.Fatal(err)
	}
	return img, o
}

func stagedFiles(t *testing.T, o OfflineIO) []string {
	t.Helper()
	ents, err := os.ReadDir(o.StagingDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// The audit's repro, scaled to what a 16 MB image can hold (1 KB blocks cap a
// file at 4 TB): a 1 TB sparse file costs the guest nothing, and dump
// would write all of it to the store. admit sees its real i_size before
// anything is staged, and a refusal stages nothing.
func TestExtractAdmitsSparseFileAtFullSize(t *testing.T) {
	const size = 1 << 40
	img, o := sparseImage(t, "/huge", size)

	refused := errors.New("refused")
	var got int64
	_, _, err := o.ExtractFileStream(img, "/huge", func(n int64) error {
		got = n
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("ExtractFileStream = %v, want admit's refusal", err)
	}
	if got != size {
		t.Errorf("admit saw %d bytes, want the file's i_size %d", got, int64(size))
	}
	if left := stagedFiles(t, o); len(left) != 0 {
		t.Errorf("a refused extract staged %v", left)
	}
}

// Holes are part of the file: an admitted sparse file comes out at its full
// size, zeros and all, and the size check does not mistake it for a short dump.
func TestExtractSmallSparseFile(t *testing.T) {
	const size = 3<<20 + 5
	img, o := sparseImage(t, "/holes", size)

	rc, n, err := o.ExtractFileStream(img, "/holes", func(int64) error { return nil })
	if err != nil {
		t.Fatalf("ExtractFileStream: %v", err)
	}
	defer rc.Close()
	if n != size {
		t.Fatalf("size = %d, want %d", n, size)
	}
	b, err := io.ReadAll(rc)
	if err != nil || int64(len(b)) != size || strings.Trim(string(b), "\x00") != "" {
		t.Fatalf("read %d bytes (err %v), want %d zeros", len(b), err, size)
	}
}

// The kernel backstop: whatever stat said, debugfs cannot write past the
// RLIMIT_FSIZE it runs under.
func TestDebugfsFileSizeCap(t *testing.T) {
	img, o := sparseImage(t, "/huge", 1<<40)
	if err := os.MkdirAll(o.StagingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(o.StagingDir, "out")
	script, err := debugfsLine("dump", "/huge", dst)
	if err != nil {
		t.Fatal(err)
	}
	out := &cappedBuffer{max: maxToolOutput}
	const limit = 1 << 20
	if err := o.debugfs(img, false, script, limit, out, out); err == nil {
		t.Errorf("dump past RLIMIT_FSIZE succeeded: %s", out)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if fi.Size() > limit {
		t.Fatalf("debugfs wrote %d bytes under a %d-byte cap", fi.Size(), limit)
	}
}

// The tail of a stat block is the guest's: an xattr value, or a symlink target
// printed raw with its newlines, can read like the header fields or even a
// command echo. Only the head of the first block for a path counts.
func TestParseStatsIgnoresGuestTail(t *testing.T) {
	out := `debugfs: stat "/f"
Inode: 12   Type: symlink    Mode:  0777   Flags: 0x0
Generation: 0    Version: 0x00000000:00000000
User:     0   Group:     0   Project:     0   Size: 70
File ACL: 0
Links: 1   Blockcount: 0
Fragment:  Address: 0    Number: 0    Size: 0
Extended attributes:
  user.x (30) = "User: 0 Group: 0 Size: 999999"
Fast link dest: "a
User:     0   Group:     0   Size: 1
debugfs: stat "/f"
Inode: 13   Type: regular    Mode:  0644   Flags: 0x0
Generation: 0    Version: 0x00000000:00000000
User:     0   Group:     0   Project:     0   Size: 5
File not found"
`
	st, ok := parseStats(out)["/f"]
	if !ok || st.typ != "symlink" || st.size != 70 {
		t.Fatalf("parsed %+v (present %v), want the symlink's own header: size 70", st, ok)
	}

	// A path whose first line is not the Inode header has no type — not found —
	// however header-like the lines after it are.
	out = "debugfs: stat \"/g\"\nnot a header\nInode: 1   Type: regular    Mode:  0644\nUser: 0 Group: 0 Size: 5\n"
	if st := parseStats(out)["/g"]; st.typ != "" || st.size != 0 {
		t.Fatalf("parsed %+v from a block without a header", st)
	}
}
