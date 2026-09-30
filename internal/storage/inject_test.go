package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newExt4(t *testing.T, size string) (string, OfflineIO) {
	t.Helper()
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.ext4")
	if out, err := exec.Command("truncate", "-s", size, img).CombinedOutput(); err != nil {
		t.Fatalf("truncate: %v %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, out)
	}
	return img, OfflineIO{StagingDir: filepath.Join(dir, "staging")}
}

// Files land with their content, mode and owner, parents created, an existing
// file replaced — and nothing is left staged.
func TestInjectFiles(t *testing.T) {
	img, o := newExt4(t, "16M")
	files := []InjectSpec{
		{Path: "/etc/app/parser.conf", Data: []byte("port=502\n"), Mode: 0o644},
		{Path: "/etc/app/key", Data: []byte("s3cret"), Mode: 0o400, UID: 1000, GID: 1001},
		{Path: "/empty", Data: nil, Mode: 0o600},
	}
	if err := o.InjectFiles(img, files); err != nil {
		t.Fatal(err)
	}
	// Again, with new content: replaced in place.
	if err := o.InjectFiles(img, []InjectSpec{{Path: "/etc/app/parser.conf", Data: []byte("port=503\n"), Mode: 0o640}}); err != nil {
		t.Fatal(err)
	}

	r, size, err := o.ExtractFileStream(img, "/etc/app/parser.conf", nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, size)
	_, _ = r.Read(buf)
	r.Close()
	if string(buf) != "port=503\n" {
		t.Errorf("content %q", buf)
	}
	out, err := o.runDebugfsStdout(img, "stat \"/etc/app/key\"\nstat \"/etc/app/parser.conf\"\nstat \"/nope\"\n")
	if err != nil {
		t.Fatal(err)
	}
	st := parseStats(out)
	if k := st["/etc/app/key"]; k.typ != "regular" || k.mode != 0o400 || k.uid != 1000 || k.gid != 1001 || k.size != 6 {
		t.Errorf("key: %+v", k)
	}
	if c := st["/etc/app/parser.conf"]; c.mode != 0o640 || c.uid != 0 {
		t.Errorf("conf: %+v", c)
	}
	if n, ok := st["/nope"]; !ok || n.typ != "" {
		t.Errorf("missing file parsed as %+v (present %v)", n, ok)
	}
	if left, _ := os.ReadDir(o.StagingDir); len(left) != 0 {
		t.Errorf("staging not cleaned: %v", left)
	}
}

// A write debugfs could not do is an error, not a silent success.
func TestInjectFilesFailures(t *testing.T) {
	img, o := newExt4(t, "16M")
	if err := o.InjectFiles(img, []InjectSpec{{Path: "/etc", Data: []byte("x"), Mode: 0o644}, {Path: "/etc/x", Data: []byte("y"), Mode: 0o644}}); err == nil {
		t.Error("a file where a directory is needed was accepted")
	}

	img, o = newExt4(t, "16M")
	// Not zeros: debugfs writes zero blocks as holes, and a sparse file fits.
	big := []byte(strings.Repeat("x", 20<<20))
	err := o.InjectFiles(img, []InjectSpec{{Path: "/big", Data: big, Mode: 0o644}})
	if err == nil {
		t.Error("a file larger than the image was accepted")
	}
	if err != nil && strings.Contains(err.Error(), "xxxx") {
		t.Error("the error carries file content")
	}

	for _, bad := range []InjectSpec{
		{Path: "relative", Mode: 0o644},
		{Path: "/a\nwrite /etc/shadow /x", Mode: 0o644},
		{Path: "/suid", Mode: 0o4755},
	} {
		if err := o.InjectFiles(img, []InjectSpec{bad}); err == nil {
			t.Errorf("%q mode %o accepted", bad.Path, bad.Mode)
		}
	}
}
