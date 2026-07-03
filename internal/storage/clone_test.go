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
