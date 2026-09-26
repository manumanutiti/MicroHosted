package vm

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"microhosted/internal/hostinfo"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// Disks are thin: a clone is a reflink that costs nothing until the guest
// writes, a volume is a sparse file. That is what lets a small store hold
// hundreds of VMs, and also what lets them, together, promise more than the
// store has. When the store fills, every VM on it gets I/O errors at once.
//
// The policy is a reserve, not an accounting of promises (which would cap
// density at the sum of disk sizes): an operation that writes to the store is
// refused while it would leave less than DiskReserveMB free, and the monitor
// raises host.disk_low while the store is under it. A VM already running can
// still write into the reserve — its throughput limit (IOLimits) bounds how
// fast — so the reserve is the time the operator has to react, not a wall.

// DefaultDiskReserveMB is the store space kept free when Limits leaves it 0.
const DefaultDiskReserveMB = 1024

// diskState is the admission and watch state of the store's filesystem.
type diskState struct {
	// usage reads the filesystem holding the store; a variable so tests can
	// set the host they need.
	usage func(dir string) (hostinfo.DiskUsage, error)
	// inflightMB is what admitted operations are about to write and statfs
	// does not show yet (a full copy on a store without reflink, a snapshot's
	// memory file). Guarded by Manager.mu.
	inflightMB int64
	// low is the last state the watch reported, so an event marks the change
	// rather than every tick. Guarded by Manager.mu.
	low bool

	cowOnce sync.Once
	cow     bool
}

// copyCostMB is what copying a sizeMB file into the store writes now: nothing
// on a reflink store, all of it on one that falls back to a full copy.
// Probed once: the store does not change filesystem under a live daemon.
func (m *Manager) copyCostMB(sizeMB int64) int64 {
	m.disk.cowOnce.Do(func() { m.disk.cow = storage.SupportsReflink(m.instancesDir) })
	if m.disk.cow {
		return 0
	}
	return sizeMB
}

// fileMB is the apparent size of path in MB, rounded up; 0 if it cannot be
// read (the operation that needs it will fail on its own).
func fileMB(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return (fi.Size() + 1<<20 - 1) >> 20
}

// snapDiskMB is the size of the disk captured in snap.
func snapDiskMB(instancesDir string, snap *types.Snapshot) int64 {
	return max(snap.DiskMB, fileMB(filepath.Join(storage.SnapshotDir(instancesDir, snap.ID), storage.SnapshotDiskFile)))
}

// reserveDisk admits an operation about to write needMB to the store (0 when
// it writes nothing up front but will grow: a boot, a sparse volume). It is
// refused with ErrCapacity when the store's free space, minus what operations
// in flight will write, minus needMB, would drop under the reserve — or when
// the free space cannot be read at all. The returned release must be called
// once the operation's writes are done or abandoned.
func (m *Manager) reserveDisk(what string, needMB int64) (release func(), err error) {
	du, uerr := m.disk.usage(m.instancesDir)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkDiskLocked(what, du, uerr, needMB); err != nil {
		return nil, err
	}
	m.disk.inflightMB += needMB
	return func() {
		m.mu.Lock()
		m.disk.inflightMB -= needMB
		m.mu.Unlock()
	}, nil
}

// checkDiskLocked is reserveDisk's decision, for callers already holding m.mu
// (admit, which decides memory and disk under one lock).
func (m *Manager) checkDiskLocked(what string, du hostinfo.DiskUsage, uerr error, needMB int64) error {
	if uerr != nil {
		// Like memory: admission that cannot see the store admits nothing.
		return fmt.Errorf("%w: reading the store's free space: %v", ErrCapacity, uerr)
	}
	reserve := m.limits.DiskReserveMB
	if left := du.FreeMB - m.disk.inflightMB - needMB; left < reserve {
		return fmt.Errorf("%w: %s: the store has %d MB free, %d MB promised to operations in progress; writing %d MB would leave %d MB, under the %d MB reserve (--disk-reserve-mb)",
			ErrCapacity, what, du.FreeMB, m.disk.inflightMB, needMB, left, reserve)
	}
	return nil
}

// watchDisk is the monitor's check of the store: it logs and publishes
// host.disk_low when the free space falls under the reserve, and host.disk_ok
// once it is back above it with a margin (a tenth of the reserve), so a store
// hovering at the line does not flap. A store that cannot be read counts as
// low: the operator must hear about it.
func (m *Manager) watchDisk() {
	du, err := m.disk.usage(m.instancesDir)

	m.mu.Lock()
	reserve := m.limits.DiskReserveMB
	was := m.disk.low
	now := err != nil || du.FreeMB < reserve
	if was && !now && du.FreeMB < reserve+reserve/10 {
		now = true
	}
	m.disk.low = now
	m.mu.Unlock()
	if now == was {
		return
	}

	typ, reason := types.EventHostDiskOK, fmt.Sprintf("the store has %d MB free again, above the %d MB reserve", du.FreeMB, reserve)
	if now {
		typ = types.EventHostDiskLow
		if err != nil {
			reason = fmt.Sprintf("the store's free space cannot be read: %v", err)
		} else {
			reason = fmt.Sprintf("the store has %d MB free, under the %d MB reserve: new VMs, forks, snapshots, volumes and imports are refused, and running VMs are eating into the reserve", du.FreeMB, reserve)
		}
	}
	log.Printf("monitor: %s: %s", typ, reason)
	if m.events != nil {
		m.events.Publish(types.Event{Type: typ, Reason: reason, Data: map[string]string{
			"free_mb": fmt.Sprint(du.FreeMB), "reserve_mb": fmt.Sprint(reserve),
		}})
	}
}

// DiskLow reports whether the last watch found the store under its reserve.
func (m *Manager) DiskLow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disk.low
}

// ReserveCopy is reserveDisk for writers outside the manager that copy sizeMB
// of files into the store (the image store's imports): they share the store
// and its reserve.
func (m *Manager) ReserveCopy(what string, sizeMB int64) (release func(), err error) {
	return m.reserveDisk(what, m.copyCostMB(sizeMB))
}

// uploadBudget bounds an upload of unknown size that lands on the store: data
// is cut off with ErrCapacity once it has passed what the store can take above
// its reserve. copies is how many times the bytes are written to the store —
// 2 for an offline write, staged and then written into the disk image. The
// budget is taken when the upload starts; it is not reserved against other
// operations, which the reserve absorbs.
func (m *Manager) uploadBudget(what string, data io.Reader, copies int64) (io.Reader, error) {
	du, err := m.disk.usage(m.instancesDir)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkDiskLocked(what, du, err, 0); err != nil {
		return nil, err
	}
	allowMB := (du.FreeMB - m.disk.inflightMB - m.limits.DiskReserveMB) / copies
	return &budgetReader{r: data, left: allowMB << 20, what: what}, nil
}

type budgetReader struct {
	r    io.Reader
	left int64
	what string
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// Probe one byte: an upload that ends exactly at the budget is fine.
		var one [1]byte
		if n, err := b.r.Read(one[:]); n == 0 {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s: the upload is larger than the store can take above its reserve (--disk-reserve-mb)", ErrCapacity, b.what)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}
