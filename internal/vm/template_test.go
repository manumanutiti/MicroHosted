package vm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// The catalog is a static list of what the project knows how to boot, while the
// goldens are built per host by `make prepare-image`. So a template can be
// listed and still be uncreatable, and Create must say that BEFORE it takes a
// name, an admission and a record — the error it used to give came from `cp`
// inside the clone and named a path with no way to act on it.
func TestCheckTemplateGoldensRejectsAbsentRootfs(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o644); err != nil {
		t.Fatalf("writing kernel: %v", err)
	}
	rootfs := filepath.Join(dir, "base-alpine.ext4") // deliberately not created

	err := checkTemplateGoldens(types.Template{Name: "base-alpine", KernelPath: kernel, RootfsPath: rootfs})
	if err == nil {
		t.Fatal("a template whose rootfs is absent was accepted")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error is not ErrInvalid (so the API would answer 500, not 400): %v", err)
	}
	// The message has to carry both halves: which file is missing, and the one
	// command that produces it.
	for _, want := range []string{"base-alpine", rootfs, "make prepare-image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestCheckTemplateGoldensAcceptsCompleteTemplate(t *testing.T) {
	dir := t.TempDir()
	tpl := types.Template{Name: "ok", KernelPath: filepath.Join(dir, "vmlinux"), RootfsPath: filepath.Join(dir, "root.ext4")}
	for _, p := range []string{tpl.KernelPath, tpl.RootfsPath} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	if err := checkTemplateGoldens(tpl); err != nil {
		t.Errorf("a template with both goldens present was rejected: %v", err)
	}
}

// A listing that shows every catalog entry alike is what sends an operator to
// `mh run` on a template this host cannot boot.
func TestTemplatesMarksWhatIsNotBuilt(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux-6.1.102")
	readyRootfs := filepath.Join(dir, "ubuntu.ext4")
	for _, p := range []string{kernel, readyRootfs} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	absentRootfs := filepath.Join(dir, "base-alpine.ext4")

	catalogPath := filepath.Join(dir, "catalog.json")
	entries := []types.Template{
		{Name: "base-ubuntu", KernelPath: kernel, RootfsPath: readyRootfs},
		{Name: "base-alpine", KernelPath: kernel, RootfsPath: absentRootfs},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshalling catalog: %v", err)
	}
	if err := os.WriteFile(catalogPath, data, 0o644); err != nil {
		t.Fatalf("writing catalog: %v", err)
	}
	cat, err := storage.LoadCatalog(catalogPath)
	if err != nil {
		t.Fatalf("loading catalog: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := NewManager(cat, jailer.Defaults{}, dir, st, network.NewManager(st, nil))

	got := make(map[string]types.Template)
	for _, tpl := range m.Templates() {
		got[tpl.Name] = tpl
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(got))
	}
	if !got["base-ubuntu"].Ready {
		t.Errorf("base-ubuntu has both goldens on disk but is not marked ready")
	}
	if got["base-ubuntu"].Missing != "" {
		t.Errorf("a ready template names a missing file: %q", got["base-ubuntu"].Missing)
	}
	if got["base-alpine"].Ready {
		t.Errorf("base-alpine has no rootfs on disk but is marked ready")
	}
	if got["base-alpine"].Missing != absentRootfs {
		t.Errorf("missing = %q, want the absent rootfs %q", got["base-alpine"].Missing, absentRootfs)
	}
}
