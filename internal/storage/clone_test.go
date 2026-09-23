package storage

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"microhosted/pkg/types"
)

func checksum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return sha256.Sum256(data)
}

func TestCloneRootfsDoesNotModifyGolden(t *testing.T) {
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.ext4")
	content := []byte("fake rootfs contents for testing clone behaviour")
	if err := os.WriteFile(golden, content, 0o644); err != nil {
		t.Fatalf("writing golden file: %v", err)
	}

	before := checksum(t, golden)

	tpl := types.Template{Name: "test", RootfsPath: golden}
	instancesDir := filepath.Join(dir, "instances")

	clonePath, err := CloneRootfs(tpl, "vm-1", instancesDir, os.Getuid(), os.Getgid(), 0)
	if err != nil {
		t.Fatalf("CloneRootfs: %v", err)
	}

	if checksum(t, golden) != before {
		t.Fatalf("golden file changed after clone")
	}

	if err := os.WriteFile(clonePath, []byte("modified by the guest"), 0o644); err != nil {
		t.Fatalf("writing to clone: %v", err)
	}
	if checksum(t, golden) != before {
		t.Fatalf("golden file changed after writing to the clone")
	}

	if err := DeleteClone(instancesDir, "vm-1"); err != nil {
		t.Fatalf("DeleteClone: %v", err)
	}
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Fatalf("expected clone to be removed, stat err=%v", err)
	}
}

// A real ext4 golden cloned with a larger diskMB must come out grown to that
// size — the whole point of the disk-sizing path (guests need room to write).
// Skips where mkfs.ext4/resize2fs aren't installed, since it can't build a real
// filesystem to grow without them.
func TestCloneRootfsGrows(t *testing.T) {
	for _, bin := range []string{"mkfs.ext4", "resize2fs", "e2fsck"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}

	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.ext4")

	// A small (16MiB) whole-device ext4, same shape as the real goldens.
	const goldenMB, targetMB = 16, 64
	if err := os.WriteFile(golden, nil, 0o644); err != nil {
		t.Fatalf("creating golden file: %v", err)
	}
	if err := os.Truncate(golden, goldenMB*bytesPerMiB); err != nil {
		t.Fatalf("sizing golden file: %v", err)
	}
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", golden).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}

	tpl := types.Template{Name: "test", RootfsPath: golden}
	instancesDir := filepath.Join(dir, "instances")

	clonePath, err := CloneRootfs(tpl, "vm-grow", instancesDir, os.Getuid(), os.Getgid(), targetMB)
	if err != nil {
		t.Fatalf("CloneRootfs: %v", err)
	}

	fi, err := os.Stat(clonePath)
	if err != nil {
		t.Fatalf("stat clone: %v", err)
	}
	if fi.Size() != int64(targetMB)*bytesPerMiB {
		t.Fatalf("clone size = %d bytes, want %d", fi.Size(), int64(targetMB)*bytesPerMiB)
	}

	// The golden itself must be untouched — grow acts on the clone only.
	gfi, err := os.Stat(golden)
	if err != nil {
		t.Fatalf("stat golden: %v", err)
	}
	if gfi.Size() != int64(goldenMB)*bytesPerMiB {
		t.Fatalf("golden size changed to %d bytes, want %d", gfi.Size(), int64(goldenMB)*bytesPerMiB)
	}
}

// A diskMB no larger than the golden must leave the clone alone — CloneRootfs
// grows disks but never shrinks them.
func TestCloneRootfsNeverShrinks(t *testing.T) {
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.ext4")
	if err := os.WriteFile(golden, make([]byte, 8*bytesPerMiB), 0o644); err != nil {
		t.Fatalf("writing golden: %v", err)
	}

	tpl := types.Template{Name: "test", RootfsPath: golden}
	instancesDir := filepath.Join(dir, "instances")

	// Ask for a smaller disk than the golden — must be ignored, not shrunk.
	clonePath, err := CloneRootfs(tpl, "vm-small", instancesDir, os.Getuid(), os.Getgid(), 4)
	if err != nil {
		t.Fatalf("CloneRootfs: %v", err)
	}
	fi, err := os.Stat(clonePath)
	if err != nil {
		t.Fatalf("stat clone: %v", err)
	}
	if fi.Size() != 8*bytesPerMiB {
		t.Fatalf("clone size = %d bytes, want unchanged %d", fi.Size(), 8*bytesPerMiB)
	}
}

func TestCatalog(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	data := `[{"name":"base","description":"test","kernel_path":"/k","rootfs_path":"/r","vcpus":1,"mem_mb":128}]`
	if err := os.WriteFile(catalogPath, []byte(data), 0o644); err != nil {
		t.Fatalf("writing catalog: %v", err)
	}

	cat, err := LoadCatalog(catalogPath)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	tpl, err := cat.Get("base")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tpl.VCPUs != 1 || tpl.MemMB != 128 {
		t.Fatalf("unexpected template: %+v", tpl)
	}

	if _, err := cat.Get("missing"); err == nil {
		t.Fatalf("expected error for missing template")
	}

	if len(cat.List()) != 1 {
		t.Fatalf("expected 1 template in list, got %d", len(cat.List()))
	}
}

// Growing clones of one golden to one size must run the grow once: every later
// clone reflinks the same pre-grown copy, each clone stays private, and a
// rebuilt golden gets a fresh copy while the stale one is pruned.
func TestCloneRootfsReusesSizedGolden(t *testing.T) {
	for _, bin := range []string{"mkfs.ext4", "resize2fs", "e2fsck"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}

	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.ext4")
	mkGolden := func() {
		t.Helper()
		_ = os.Remove(golden)
		if err := os.WriteFile(golden, nil, 0o644); err != nil {
			t.Fatalf("creating golden file: %v", err)
		}
		if err := os.Truncate(golden, 16*bytesPerMiB); err != nil {
			t.Fatalf("sizing golden file: %v", err)
		}
		if out, err := exec.Command("mkfs.ext4", "-F", "-q", golden).CombinedOutput(); err != nil {
			t.Fatalf("mkfs.ext4: %v: %s", err, out)
		}
	}
	sized := func() []string {
		t.Helper()
		m, err := filepath.Glob(filepath.Join(dir, "instances", sizedDir, "*.ext4"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		return m
	}
	mkGolden()

	tpl := types.Template{Name: "test", RootfsPath: golden}
	instancesDir := filepath.Join(dir, "instances")
	const targetMB = 64

	a, err := CloneRootfs(tpl, "vm-a", instancesDir, os.Getuid(), os.Getgid(), targetMB)
	if err != nil {
		t.Fatalf("CloneRootfs a: %v", err)
	}
	first := sized()
	if len(first) != 1 {
		t.Fatalf("pre-grown copies after first clone = %v, want exactly one", first)
	}
	before, err := os.Stat(first[0])
	if err != nil {
		t.Fatalf("stat pre-grown copy: %v", err)
	}

	b, err := CloneRootfs(tpl, "vm-b", instancesDir, os.Getuid(), os.Getgid(), targetMB)
	if err != nil {
		t.Fatalf("CloneRootfs b: %v", err)
	}
	after, err := os.Stat(first[0])
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("second clone rebuilt the pre-grown copy (err=%v)", err)
	}
	for _, p := range []string{a, b} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() != targetMB*bytesPerMiB {
			t.Fatalf("clone %s: size/err = %v/%v, want %d bytes", p, fi.Size(), err, targetMB*bytesPerMiB)
		}
	}

	// A guest writing to its disk must not reach the shared copy or a sibling.
	sharedSum, bSum := checksum(t, first[0]), checksum(t, b)
	if err := os.WriteFile(a, []byte("written by guest a"), 0o644); err != nil {
		t.Fatalf("writing clone a: %v", err)
	}
	if checksum(t, first[0]) != sharedSum || checksum(t, b) != bSum {
		t.Fatalf("writing one clone changed the pre-grown copy or another clone")
	}

	// Rebuild the golden: the next clone must come from a new copy, and the
	// stale one must be gone.
	mkGolden()
	if _, err := CloneRootfs(tpl, "vm-c", instancesDir, os.Getuid(), os.Getgid(), targetMB); err != nil {
		t.Fatalf("CloneRootfs c: %v", err)
	}
	now := sized()
	if len(now) != 1 || now[0] == first[0] {
		t.Fatalf("pre-grown copies after golden rebuild = %v, want one new copy replacing %s", now, first[0])
	}
}

// Startup pruning keeps the pre-grown copies of the goldens in the catalog as
// they are now, and removes the rest: a template no longer listed, a golden
// rebuilt since its copy was made, and a half-built temporary file.
func TestPruneSizedGoldens(t *testing.T) {
	dir := t.TempDir()
	instancesDir := filepath.Join(dir, "instances")
	sd := filepath.Join(instancesDir, sizedDir)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Nothing to prune yet, and no sized dir at all, is not an error.
	if removed, err := PruneSizedGoldens(filepath.Join(dir, "none"), nil); err != nil || len(removed) != 0 {
		t.Fatalf("prune of a missing dir = %v, %v; want nothing, nil", removed, err)
	}

	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	kept := filepath.Join(dir, "base-alpine.ext4")
	gone := filepath.Join(dir, "retired.ext4")
	write(kept)
	write(gone)
	keyOf := func(g string) string {
		t.Helper()
		k, _, err := goldenKey(g)
		if err != nil {
			t.Fatalf("goldenKey(%s): %v", g, err)
		}
		return k
	}

	keep := filepath.Join(sd, "base-alpine-512m-"+keyOf(kept)+".ext4")
	keepOtherSize := filepath.Join(sd, "base-alpine-1024m-"+keyOf(kept)+".ext4")
	retired := filepath.Join(sd, "retired-512m-"+keyOf(gone)+".ext4")
	stale := filepath.Join(sd, "base-alpine-512m-000000000000.ext4")
	tmp := keep + ".tmp"
	for _, p := range []string{keep, keepOtherSize, retired, stale, tmp} {
		write(p)
	}

	removed, err := PruneSizedGoldens(instancesDir, []string{kept})
	if err != nil {
		t.Fatalf("PruneSizedGoldens: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %v, want the retired, stale and tmp files", removed)
	}
	for _, p := range []string{keep, keepOtherSize} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("current copy %s was removed: %v", p, err)
		}
	}
	for _, p := range []string{retired, stale, tmp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("pruning touched a golden: %v", err)
	}
}
