package vm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"microhosted/internal/jailer"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// extractVolume sets m up with a detached volume "vol1" holding a sparse file
// of size bytes at /f — a guest's `truncate -s`, no blocks, any i_size — and
// the store (and so the staging dir) in a temp dir.
func extractVolume(t *testing.T, m *Manager, size int64) {
	t.Helper()
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed, skipping", tool)
		}
	}
	m.instancesDir = t.TempDir()
	img, err := storage.CreateVolume(t.TempDir(), "vol1", 16, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	src := filepath.Join(t.TempDir(), "sparse")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, size); err != nil {
		t.Skipf("host filesystem cannot hold a %d-byte sparse file: %v", size, err)
	}
	if out, err := exec.Command("debugfs", "-w", "-R", `write "`+src+`" "/f"`, img).CombinedOutput(); err != nil {
		t.Fatalf("debugfs write: %v: %s", err, out)
	}
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "v", Path: img, UID: jailer.DefaultIDBase}
}

func assertNothingStaged(t *testing.T, m *Manager) {
	t.Helper()
	ents, _ := os.ReadDir(m.stagingDir())
	if len(ents) != 0 {
		t.Errorf("staging holds %d entries after a refused extract", len(ents))
	}
	if m.disk.inflightMB != 0 {
		t.Errorf("%d MB still reserved after a refused extract", m.disk.inflightMB)
	}
}

// A sparse file larger than --max-extract-mb is refused before debugfs dumps a
// byte of it, however little it occupies in the image.
func TestExtractOverLimitRefused(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{MaxExtractMB: 64})
	extractVolume(t, m, 1<<40)

	if _, _, err := m.ExtractFromVolumeStream("vol1", "/f"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("extracting a 1 TB sparse file = %v, want ErrCapacity", err)
	}
	assertNothingStaged(t, m)
}

// Under the limit, the file must still fit on the store above its reserve.
func TestExtractOverFreeSpaceRefused(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{DiskReserveMB: 1000, MaxExtractMB: 1 << 20})
	extractVolume(t, m, 512<<20)
	setFree(m, 1200, nil) // 1200 - 512 < 1000

	if _, _, err := m.ExtractFromVolumeStream("vol1", "/f"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("extracting past the reserve = %v, want ErrCapacity", err)
	}
	assertNothingStaged(t, m)
}

// An admitted extract holds its reservation while the staged copy exists — as
// long as the reader is open — and gives it back once, on the first Close.
func TestExtractReservationHeldUntilClose(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{DiskReserveMB: 1000})
	const size = 3<<20 + 1 // rounds up to 4 MB
	extractVolume(t, m, size)
	setFree(m, 2000, nil)

	rc, n, err := m.ExtractFromVolumeStream("vol1", "/f")
	if err != nil {
		t.Fatalf("ExtractFromVolumeStream: %v", err)
	}
	if n != size {
		t.Errorf("size = %d, want %d", n, size)
	}
	if m.disk.inflightMB != 4 {
		t.Errorf("reserved %d MB while open, want 4", m.disk.inflightMB)
	}
	if got, err := io.Copy(io.Discard, rc); err != nil || got != size {
		t.Errorf("read %d bytes (%v), want %d", got, err, size)
	}
	_ = rc.Close()
	_ = rc.Close()
	if m.disk.inflightMB != 0 {
		t.Errorf("reserved %d MB after Close, want 0", m.disk.inflightMB)
	}
}
