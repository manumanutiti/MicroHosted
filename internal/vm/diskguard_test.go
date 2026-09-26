package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"microhosted/internal/events"
	"microhosted/internal/hostinfo"
	"microhosted/pkg/types"
)

func setFree(m *Manager, freeMB int64, err error) {
	m.disk.usage = func(string) (hostinfo.DiskUsage, error) { return hostinfo.DiskUsage{FreeMB: freeMB}, err }
}

// A launch that would leave the store under its reserve is refused, what
// admitted launches are about to write counts although statfs does not show
// it yet, and a store that cannot be read admits nothing.
func TestAdmitDiskReserve(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{DiskReserveMB: 1000, MaxParallelBoots: 4})
	m.memAvailable = func() (int64, error) { return 1 << 20, nil }
	setFree(m, 2000, nil)

	rel, err := m.admit(context.Background(), "disk0001", "", 64, 600) // leaves 1400
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// 2000 - 600 in flight - 600 = 800 < 1000.
	if _, err := m.admit(context.Background(), "disk0002", "", 64, 600); !errors.Is(err, ErrCapacity) {
		t.Errorf("second admit = %v, want ErrCapacity", err)
	}
	rel()
	rel2, err := m.admit(context.Background(), "disk0002", "", 64, 600)
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	rel2()

	// A boot that writes nothing up front is still refused under the reserve:
	// the VM will grow into a store that has no room left.
	setFree(m, 999, nil)
	if _, err := m.admit(context.Background(), "disk0003", "", 64, 0); !errors.Is(err, ErrCapacity) {
		t.Errorf("admit under the reserve = %v, want ErrCapacity", err)
	}
	if _, err := m.reserveDisk("creating volume v", 0); !errors.Is(err, ErrCapacity) {
		t.Errorf("volume under the reserve = %v, want ErrCapacity", err)
	}

	setFree(m, 0, errors.New("statfs: EIO"))
	if _, err := m.reserveDisk("snapshot", 1); !errors.Is(err, ErrCapacity) {
		t.Errorf("reserve without a reading = %v, want ErrCapacity (fail closed)", err)
	}
	if m.disk.inflightMB != 0 || m.inflightMB != 0 {
		t.Errorf("reservations leaked: disk %d MB, mem %d MB", m.disk.inflightMB, m.inflightMB)
	}
}

// The watch publishes one event per change, with a margin before it clears.
func TestWatchDiskEvents(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{DiskReserveMB: 1000})
	bus := events.NewBus(0)
	m.SetEvents(bus)

	step := func(free int64, err error) {
		setFree(m, free, err)
		m.watchDisk()
	}
	step(5000, nil) // fine: nothing
	step(900, nil)  // low
	step(800, nil)  // still low: no repeat
	step(1050, nil) // above the reserve but within the margin: still low
	step(1200, nil) // ok
	step(0, errors.New("statfs: EIO"))

	backlog, _, _, sub := bus.Subscribe("", 0, true)
	sub.Close()
	var got []string
	for _, ev := range backlog {
		got = append(got, ev.Type)
	}
	want := []string{types.EventHostDiskLow, types.EventHostDiskOK, types.EventHostDiskLow}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	if !m.DiskLow() {
		t.Error("an unreadable store is not reported low")
	}
}

// An upload of unknown size is cut off once it passes what the store can take
// above its reserve; one that fits exactly goes through.
func TestUploadBudget(t *testing.T) {
	m := newTestManager(t)
	m.SetLimits(Limits{DiskReserveMB: 1000})
	setFree(m, 1002, nil) // 2 MB above the reserve

	r, err := m.uploadBudget("upload", bytes.NewReader(make([]byte, 2<<20)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, r); err != nil || n != 2<<20 {
		t.Errorf("upload that fits: %d bytes, %v", n, err)
	}

	r, _ = m.uploadBudget("upload", bytes.NewReader(make([]byte, 2<<20+1)), 1)
	if _, err := io.Copy(io.Discard, r); !errors.Is(err, ErrCapacity) {
		t.Errorf("upload over the budget: %v, want ErrCapacity", err)
	}

	// Offline writes land twice: staged, then into the image.
	r, _ = m.uploadBudget("upload", bytes.NewReader(make([]byte, 2<<20)), 2)
	if _, err := io.Copy(io.Discard, r); !errors.Is(err, ErrCapacity) {
		t.Errorf("offline upload over half the budget: %v, want ErrCapacity", err)
	}

	setFree(m, 999, nil)
	if _, err := m.uploadBudget("upload", bytes.NewReader(nil), 1); !errors.Is(err, ErrCapacity) {
		t.Errorf("upload under the reserve: %v, want ErrCapacity", err)
	}
}
