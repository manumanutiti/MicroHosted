package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"microhosted/internal/images"
	"microhosted/pkg/types"
)

// withImage gives m an image store holding one image, parser:1.0.
func withImage(t *testing.T, m *Manager) *types.Image {
	t.Helper()
	dir := t.TempDir()
	s, err := images.Open(dir, m.store)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"vmlinux": "kernel", "rootfs.ext4": "rootfs"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img, err := s.Import(types.ImportImageRequest{
		Name: "parser:1.0", KernelPath: filepath.Join(dir, "vmlinux"), RootfsPath: filepath.Join(dir, "rootfs.ext4"),
		VCPUs: 1, MemMB: 64, DiskMB: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetImages(s)
	return img
}

func TestResolveSource(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.resolveSource("", "parser:1.0"); !errors.Is(err, ErrInvalid) {
		t.Errorf("image without a store = %v, want ErrInvalid", err)
	}
	img := withImage(t, m)

	tpl, digest, err := m.resolveSource("", "parser:1.0")
	if err != nil {
		t.Fatal(err)
	}
	if digest != img.Digest || tpl.Name != "parser:1.0" || tpl.MemMB != 64 || tpl.DiskMB != 256 {
		t.Errorf("resolved %+v, %s", tpl, digest)
	}
	if !strings.HasSuffix(tpl.RootfsPath, strings.TrimPrefix(img.Manifest.Rootfs, "sha256:")) {
		t.Errorf("rootfs %s is not the stored file", tpl.RootfsPath)
	}
	if _, d, err := m.resolveSource("", img.Digest); err != nil || d != img.Digest {
		t.Errorf("by digest: %s, %v", d, err)
	}

	other := "sha256:" + strings.Repeat("0", 64)
	for _, c := range []struct {
		template, image string
		want            error
	}{
		{"", "", ErrInvalid},
		{"alpine-py", "parser:1.0", ErrInvalid},
		{"parser:1.0", "", ErrInvalid}, // an image ref in the template field
		{"", "parser:9.9", ErrInvalid},
		{"", "parser:1.0@" + other, ErrConflict},
	} {
		if _, _, err := m.resolveSource(c.template, c.image); !errors.Is(err, c.want) {
			t.Errorf("resolveSource(%q, %q) = %v, want %v", c.template, c.image, err, c.want)
		}
	}

	// A file gone from the store is refused up front, not deep in the clone.
	if err := os.Chmod(filepath.Dir(tpl.RootfsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tpl.RootfsPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.resolveSource("", "parser:1.0"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing rootfs = %v", err)
	}
}

// A create naming a bad source fails before any record or side effect.
func TestCreateByImageRefusedFirst(t *testing.T) {
	m := newTestManager(t)
	withImage(t, m)
	for _, req := range []types.CreateVMRequest{
		{Image: "parser:9.9"},
		{Template: "alpine-py", Image: "parser:1.0"},
		{},
	} {
		if _, err := m.Create(context.Background(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if recs, _ := m.store.ListVMs(); len(recs) != 0 {
		t.Errorf("a refused create left records: %d", len(recs))
	}
}

// A replacement boots the old VM's own digest, not whatever its tag or a
// template of the same name points at now.
func TestReplaceBootsTheSameDigest(t *testing.T) {
	cfg := sensorVM()
	m, f := newReplaceManager(t, cfg)
	img := withImage(t, m)
	m.mu.Lock()
	m.vms[cfg.ID].Config.TemplateName = "parser:1.0"
	m.vms[cfg.ID].Config.Image = img.Digest
	m.mu.Unlock()

	if _, _, err := m.Replace(context.Background(), cfg.ID, types.ReplaceVMRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0]; got.image != img.Digest || got.template != "" {
		t.Errorf("replacement source: template %q image %q, want image %s", got.template, got.image, img.Digest)
	}
}

// An image the replacement cannot boot is refused before the old VM is cut off.
func TestReplaceRefusesUnbootableImageFirst(t *testing.T) {
	m, f := newReplaceManager(t, sensorVM())
	withImage(t, m)
	for _, req := range []types.ReplaceVMRequest{
		{Image: "parser:9.9"},
		{Image: "parser:1.0", Template: "alpine-py"},
	} {
		if _, _, err := m.Replace(context.Background(), "a0000001", req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Replace(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if old, _ := m.Get("a0000001"); old.Config.Quarantine {
		t.Error("the old VM was cut off although the replacement could not boot")
	}
	if len(f.calls) != 0 {
		t.Errorf("%d launches", len(f.calls))
	}
}

func TestImageInUse(t *testing.T) {
	m := newTestManager(t)
	img := withImage(t, m)
	if u := m.ImageInUse(img.Digest); u != "" {
		t.Errorf("unused image in use by %s", u)
	}
	m.mu.Lock()
	m.snaps["s0000001"] = &types.Snapshot{ID: "s0000001", Image: img.Digest}
	m.mu.Unlock()
	if u := m.ImageInUse(img.Digest); u != "snapshot s0000001" {
		t.Errorf("in use by %q, want the snapshot", u)
	}
	m.mu.Lock()
	m.vms["a0000001"] = &types.VM{Config: types.VMConfig{ID: "a0000001", Image: img.Digest}, State: types.VMStateStopped}
	m.mu.Unlock()
	if u := m.ImageInUse(img.Digest); !strings.HasPrefix(u, "vm ") && !strings.HasPrefix(u, "snapshot ") {
		t.Errorf("in use by %q", u)
	}
}
