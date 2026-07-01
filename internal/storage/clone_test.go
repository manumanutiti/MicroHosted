package storage

import (
	"crypto/sha256"
	"os"
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

	clonePath, err := CloneRootfs(tpl, "vm-1", instancesDir, os.Getuid(), os.Getgid())
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
