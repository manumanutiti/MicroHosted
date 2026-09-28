package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"microhosted/internal/labels"
	"microhosted/internal/store"
	"microhosted/pkg/types"
)

type fixture struct {
	dir    string // instances dir
	st     *store.Store
	s      *Store
	kernel string
	rootfs string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := Open(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{dir: dir, st: st, s: s,
		kernel: filepath.Join(dir, "src-vmlinux"), rootfs: filepath.Join(dir, "src-rootfs.ext4")}
	f.write(t, f.kernel, "kernel v1")
	f.write(t, f.rootfs, "rootfs v1")
	return f
}

func (f *fixture) write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) req(name string) types.ImportImageRequest {
	return types.ImportImageRequest{Name: name, KernelPath: f.kernel, RootfsPath: f.rootfs, VCPUs: 1, MemMB: 64}
}

func (f *fixture) blobs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.s.blobDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestImportStoresReadOnlyCopies(t *testing.T) {
	f := newFixture(t)
	img, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !validDigest(img.Digest) || !validDigest(img.Manifest.Kernel) || !validDigest(img.Manifest.Rootfs) {
		t.Fatalf("digests %+v", img)
	}
	tpl := f.s.Template(img)
	for _, p := range []string{tpl.KernelPath, tpl.RootfsPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o444 {
			t.Errorf("%s mode %v, want 0444", p, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(f.s.dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("store dir mode %v, want 0700", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(tpl.RootfsPath); string(b) != "rootfs v1" {
		t.Errorf("stored rootfs = %q", b)
	}

	// The source changing afterwards changes nothing in the store: the digest
	// was taken over the store's own copy.
	f.write(t, f.rootfs, "rootfs TAMPERED")
	if b, _ := os.ReadFile(tpl.RootfsPath); string(b) != "rootfs v1" {
		t.Errorf("stored rootfs followed its source: %q", b)
	}
	if err := f.s.Verify(img); err != nil {
		t.Errorf("Verify: %v", err)
	}
	if p := f.s.Integrity(); len(p) != 0 {
		t.Errorf("Integrity: %v", p)
	}
}

func TestTagsNeverMove(t *testing.T) {
	f := newFixture(t)
	first, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	// Same files, same tag: idempotent.
	again, err := f.s.Import(f.req("parser:1.0"))
	if err != nil || again.Digest != first.Digest {
		t.Fatalf("re-import = %v, %v", again, err)
	}
	// Same files, another tag: the same image, two tags.
	if img, err := f.s.Import(f.req("parser:stable")); err != nil || img.Digest != first.Digest || len(img.Tags) != 2 {
		t.Fatalf("second tag = %+v, %v", img, err)
	}
	// A rebuild under a taken tag is refused, and leaves no file behind.
	before := f.blobs(t)
	f.write(t, f.rootfs, "rootfs v2")
	if _, err := f.s.Import(f.req("parser:1.0")); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebuild under a taken tag = %v, want ErrConflict", err)
	}
	if after := f.blobs(t); len(after) != len(before) {
		t.Errorf("a refused import left files: %v → %v", before, after)
	}
	if img, _ := f.s.Resolve("parser:1.0"); img.Digest != first.Digest {
		t.Error("the tag moved")
	}
	// Different defaults are a different image, too.
	f.write(t, f.rootfs, "rootfs v1")
	r := f.req("parser:1.0")
	r.MemMB = 128
	if _, err := f.s.Import(r); !errors.Is(err, ErrConflict) {
		t.Errorf("same files, other defaults, taken tag = %v, want ErrConflict", err)
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	img, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("0", 64)
	for ref, want := range map[string]error{
		"parser:1.0":                    nil,
		img.Digest:                      nil,
		"parser:1.0@" + img.Digest:      nil,
		"parser:1.0@" + other:           ErrConflict, // tag and digest disagree
		"parser:2.0":                    ErrNotFound,
		other:                           ErrNotFound,
		"sha256:ABC":                    labels.ErrInvalid,
		"Parser:1.0":                    labels.ErrInvalid,
		"parser:":                       labels.ErrInvalid,
		"parser:1.0@" + img.Digest[:20]: labels.ErrInvalid,
	} {
		got, err := f.s.Resolve(ref)
		if want == nil {
			if err != nil || got.Digest != img.Digest {
				t.Errorf("Resolve(%q) = %v, %v", ref, got, err)
			}
		} else if !errors.Is(err, want) {
			t.Errorf("Resolve(%q) = %v, want %v", ref, err, want)
		}
	}
}

func TestImportRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	dev := f.req("parser:1.0")
	dev.RootfsPath = "/dev/null"
	rel := f.req("parser:1.0")
	rel.KernelPath = "vmlinux"
	empty := f.req("parser:1.0")
	f.write(t, filepath.Join(f.dir, "empty"), "")
	empty.KernelPath = filepath.Join(f.dir, "empty")
	noCPU := f.req("parser:1.0")
	noCPU.VCPUs = 0
	for name, r := range map[string]types.ImportImageRequest{
		"no version": f.req("parser"), "bad name": f.req("Parser:1"), "a device": dev,
		"relative path": rel, "empty file": empty, "no vcpus": noCPU,
	} {
		if _, err := f.s.Import(r); !errors.Is(err, labels.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if b := f.blobs(t); len(b) != 0 {
		t.Errorf("refused imports stored files: %v", b)
	}
}

// A copy that fails midway leaves nothing: no image, no file.
func TestFailedImportLeavesNothing(t *testing.T) {
	f := newFixture(t)
	calls := 0
	old := reflink
	reflink = func(src, dst string) error {
		if calls++; calls == 2 {
			return errors.New("disk full")
		}
		return old(src, dst)
	}
	t.Cleanup(func() { reflink = old })
	if _, err := f.s.Import(f.req("parser:1.0")); err == nil {
		t.Fatal("import succeeded")
	}
	if b := f.blobs(t); len(b) != 0 {
		t.Errorf("files left: %v", b)
	}
	if tmp, _ := os.ReadDir(f.s.tmpDir()); len(tmp) != 0 {
		t.Errorf("temp files left: %d", len(tmp))
	}
	if len(f.s.List()) != 0 {
		t.Error("image recorded")
	}
}

func TestDeleteAndReopen(t *testing.T) {
	f := newFixture(t)
	a, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	// Same kernel, other rootfs: the kernel file is shared.
	f.write(t, f.rootfs, "rootfs v2")
	b, err := f.s.Import(f.req("parser:2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.blobs(t)); n != 3 {
		t.Fatalf("%d files, want 3 (one shared kernel)", n)
	}

	if _, err := f.s.Delete("parser:1.0", func(string) string { return "vm a1b2c3d4" }); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete in use = %v, want ErrConflict", err)
	}
	if _, err := f.s.Delete("parser:1.0", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if n := len(f.blobs(t)); n != 2 {
		t.Errorf("%d files after delete, want 2 (the kernel is still needed)", n)
	}
	if _, err := os.Stat(f.s.BlobPath(a.Manifest.Kernel)); err != nil {
		t.Error("the shared kernel was removed")
	}

	// A restart sees the same images and the same tags.
	s2, err := Open(f.dir, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Resolve("parser:2.0"); err != nil || got.Digest != b.Digest {
		t.Errorf("after reopen: %v, %v", got, err)
	}
	if _, err := s2.Resolve("parser:1.0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted tag after reopen = %v", err)
	}
}

// Integrity notices a file made writable or removed behind the store's back.
func TestIntegrity(t *testing.T) {
	f := newFixture(t)
	img, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	tpl := f.s.Template(img)
	if err := os.Chmod(tpl.RootfsPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if p := f.s.Integrity(); len(p) != 1 || p[0].Kind != "image_file_writable" {
		t.Errorf("Integrity = %v", p)
	}
	if err := os.WriteFile(tpl.RootfsPath, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Verify(img); err == nil {
		t.Error("Verify missed changed content")
	}
	if err := os.Remove(tpl.KernelPath); err != nil {
		t.Fatal(err)
	}
	if f.s.Missing(img) != img.Manifest.Kernel {
		t.Error("Missing did not report the kernel")
	}
}

// Only files under the instances dir can be imported: anything else would let
// an API caller copy any file root can read into a VM.
func TestImportConfinedToStore(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	f.write(t, outside, "root-only data")
	link := filepath.Join(f.dir, "innocent.ext4")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	img, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	inStore := f.s.Template(img).RootfsPath
	up := filepath.Join(f.dir, "sub", "..", "..", filepath.Base(filepath.Dir(outside)), "secret")

	for name, src := range map[string]string{
		"outside the store":   outside,
		"/etc/passwd":         "/etc/passwd",
		"a symlink out":       link,
		"a .. path out":       up,
		"a file of the store": inStore,
	} {
		r := f.req("parser:2.0")
		r.RootfsPath = src
		if _, err := f.s.Import(r); !errors.Is(err, labels.ErrInvalid) {
			t.Errorf("%s (%s): %v, want ErrInvalid", name, src, err)
		}
	}
	// A symlink that stays inside is fine: its target is what gets copied.
	inside := filepath.Join(f.dir, "alias.ext4")
	if err := os.Symlink(f.rootfs, inside); err != nil {
		t.Fatal(err)
	}
	r := f.req("parser:1.0")
	r.RootfsPath = inside
	if got, err := f.s.Import(r); err != nil || got.Digest != img.Digest {
		t.Errorf("symlink inside: %v, %v", got, err)
	}
}

// A bare tag of an image with other tags removes only that tag, even while
// a VM boots from the image; its last tag deletes the image.
func TestDeleteUntags(t *testing.T) {
	f := newFixture(t)
	a, err := f.s.Import(f.req("parser:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Import(f.req("parser:stable")); err != nil {
		t.Fatal(err)
	}
	before := f.blobs(t)

	res, err := f.s.Delete("parser:stable", func(string) string { return "vm a1b2c3d4" })
	if err != nil {
		t.Fatalf("untag in use = %v, want it allowed", err)
	}
	if res.Deleted || res.Digest != a.Digest || len(res.Untagged) != 1 || res.Untagged[0] != "parser:stable" {
		t.Errorf("untag = %+v", res)
	}
	if _, err := f.s.Resolve("parser:stable"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed tag resolves: %v", err)
	}
	if img, err := f.s.Resolve("parser:1.0"); err != nil || len(img.Tags) != 1 || img.Tags[0] != "parser:1.0" {
		t.Errorf("remaining tag = %+v, %v", img, err)
	}
	if after := f.blobs(t); len(after) != len(before) {
		t.Errorf("untag removed files: %v → %v", before, after)
	}

	// The untag is persisted.
	s2, err := Open(f.dir, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if img, err := s2.Resolve("parser:1.0"); err != nil || len(img.Tags) != 1 {
		t.Errorf("after reopen = %+v, %v", img, err)
	}
	if _, err := s2.Resolve("parser:stable"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed tag after reopen = %v", err)
	}

	// The last tag deletes the image, and is refused while it is in use.
	if _, err := f.s.Delete("parser:1.0", func(string) string { return "vm a1b2c3d4" }); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete last tag in use = %v, want ErrConflict", err)
	}
	res, err = f.s.Delete("parser:1.0", func(string) string { return "" })
	if err != nil || !res.Deleted || len(res.Untagged) != 1 {
		t.Fatalf("delete last tag = %+v, %v", res, err)
	}
	if n := len(f.blobs(t)); n != 0 {
		t.Errorf("%d files after delete, want 0", n)
	}
}

// A reference with a digest names the image: it goes with all its tags.
func TestDeleteByDigestRemovesAllTags(t *testing.T) {
	for _, ref := range []string{"digest", "parser:1.0@digest"} {
		f := newFixture(t)
		a, err := f.s.Import(f.req("parser:1.0"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.Import(f.req("parser:stable")); err != nil {
			t.Fatal(err)
		}
		ref = strings.Replace(ref, "digest", a.Digest, 1)
		res, err := f.s.Delete(ref, func(string) string { return "" })
		if err != nil || !res.Deleted || len(res.Untagged) != 2 {
			t.Fatalf("delete %s = %+v, %v", ref, res, err)
		}
		if len(f.s.List()) != 0 {
			t.Errorf("delete %s left images", ref)
		}
	}
}

// An image's default command and health check are part of what its digest
// covers; spelling a duration differently is the same image, and an image
// without defaults hashes as it did before they existed.
func TestDefaultsAreCovered(t *testing.T) {
	f := newFixture(t)
	plain, err := f.s.Import(f.req("web:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := manifestDigest(types.ImageManifest{Schema: manifestSchema, Kernel: plain.Manifest.Kernel,
		Rootfs: plain.Manifest.Rootfs, VCPUs: 1, MemMB: 64})
	if plain.Digest != want {
		t.Errorf("an image without defaults hashes to %s, want %s", plain.Digest, want)
	}

	r := f.req("web:1.1")
	r.Command = "nginx -g 'daemon off;'"
	r.Health = &types.ImageHealth{Command: "wget -qO- http://127.0.0.1/", Every: "60s", Timeout: "2000ms", Failures: 3}
	img, err := f.s.Import(r)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest == plain.Digest {
		t.Error("a default command did not change the digest")
	}
	if h := img.Manifest.Health; h.Every != "1m0s" || h.Timeout != "2s" {
		t.Errorf("health durations stored as %q/%q, want canonical 1m0s/2s", h.Every, h.Timeout)
	}
	r.Name = "web:1.1-again"
	r.Health = &types.ImageHealth{Command: "wget -qO- http://127.0.0.1/", Every: "1m", Timeout: "2s", Failures: 3}
	if again, err := f.s.Import(r); err != nil || again.Digest != img.Digest {
		t.Errorf("same defaults spelled differently = %v, %v; want digest %s", again, err, img.Digest)
	}
	if resp := f.s.Response(img); resp.Command != r.Command || resp.Health == nil {
		t.Errorf("Response lost the defaults: %+v", resp)
	}

	for name, mut := range map[string]func(*types.ImportImageRequest){
		"multi-line command": func(r *types.ImportImageRequest) { r.Command = "a\nb" },
		"health w/o command": func(r *types.ImportImageRequest) { r.Health = &types.ImageHealth{Every: "10s"} },
		"bad duration":       func(r *types.ImportImageRequest) { r.Health = &types.ImageHealth{Command: "true", Every: "often"} },
		"timeout over every": func(r *types.ImportImageRequest) {
			r.Health = &types.ImageHealth{Command: "true", Every: "5s", Timeout: "5s"}
		},
		"negative failures": func(r *types.ImportImageRequest) { r.Health = &types.ImageHealth{Command: "true", Failures: -1} },
	} {
		r := f.req("bad:1.0")
		mut(&r)
		if _, err := f.s.Import(r); !errors.Is(err, labels.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}
