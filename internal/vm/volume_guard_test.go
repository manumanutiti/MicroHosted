package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"microhosted/pkg/types"
)

// These tests exercise the concurrency guards that keep offline debugfs I/O from
// racing an attach/boot on the same ext4 (which would corrupt a mounted
// filesystem). They stop at the guard — no real image or debugfs needed — since
// a rejected op never reaches storage.

func TestOfflineIORejectedWhileVolumeAttached(t *testing.T) {
	m := newTestManager(t)
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "v", Path: "/nope.ext4", AttachedTo: "somevm"}

	if err := m.InjectToVolume("vol1", "/x", strings.NewReader("data")); !errors.Is(err, ErrConflict) {
		t.Fatalf("InjectToVolume on attached volume = %v, want ErrConflict", err)
	}
	if _, _, err := m.ExtractFromVolumeStream("vol1", "/x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ExtractFromVolumeStream on attached volume = %v, want ErrConflict", err)
	}
}

func TestOfflineIORejectedWhileVolumeBusy(t *testing.T) {
	m := newTestManager(t)
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "v", Path: "/nope.ext4"}
	m.volIO["vol1"] = true // another op in flight

	if err := m.InjectToVolume("vol1", "/x", strings.NewReader("data")); !errors.Is(err, ErrConflict) {
		t.Fatalf("InjectToVolume on busy volume = %v, want ErrConflict", err)
	}
}

func TestAttachRejectedWhileVolumeBusy(t *testing.T) {
	m := newTestManager(t)
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "v", Path: "/nope.ext4"}
	m.volIO["vol1"] = true

	if _, err := m.attachVolumes("vm1", []types.VolumeAttachRequest{{Name: "v"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("attachVolumes on busy volume = %v, want ErrConflict", err)
	}
	// The failed attach must not have left the volume marked attached.
	if m.vols["vol1"].AttachedTo != "" {
		t.Fatalf("busy-rejected volume left AttachedTo=%q", m.vols["vol1"].AttachedTo)
	}
}

func TestBeginVolumeIOReservesAndReleases(t *testing.T) {
	m := newTestManager(t)
	m.vols["vol1"] = &types.Volume{ID: "vol1", Name: "v", Path: "/nope.ext4"}

	path, err := m.beginVolumeIO("vol1")
	if err != nil {
		t.Fatalf("beginVolumeIO: %v", err)
	}
	if path != "/nope.ext4" {
		t.Fatalf("path = %q", path)
	}
	// A second reservation must fail while the first is held.
	if _, err := m.beginVolumeIO("vol1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second beginVolumeIO = %v, want ErrConflict", err)
	}
	m.endVolumeIO("vol1")
	// After release it's reservable again.
	if _, err := m.beginVolumeIO("vol1"); err != nil {
		t.Fatalf("beginVolumeIO after release: %v", err)
	}
}

func TestStartRejectedDuringVMDiskIO(t *testing.T) {
	m := newTestManager(t)
	m.vms["vm1"] = &types.VM{
		Config:    types.VMConfig{ID: "vm1"},
		State:     types.VMStateStopped,
		CreatedAt: time.Now(),
	}
	m.vmIO["vm1"] = true // an offline inject/extract is writing the disk

	if _, err := m.Start(context.Background(), "vm1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Start during offline disk I/O = %v, want ErrConflict", err)
	}
}

func TestOfflineIOUnknownVolume(t *testing.T) {
	m := newTestManager(t)
	if err := m.InjectToVolume("ghost", "/x", strings.NewReader("d")); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("InjectToVolume unknown = %v, want ErrVMNotFound", err)
	}
}
