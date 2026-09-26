package vm

import (
	"fmt"
	"log"

	"microhosted/internal/jailer"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// idPool hands out the per-VM (and per-volume) identities of the reserved
// range — see internal/jailer/identity.go for what they isolate. Guarded by
// Manager.mu.
//
// Two rules keep a reused identity from inheriting anything: an identity
// returns to the pool only once its owner's record is gone, and the allocator
// skips any identity a live process still runs as. Allocation walks the range
// round-robin from the last identity handed out, so a released one is reused
// as late as possible.
type idPool struct {
	base, count int
	owner       map[int]string // identity → "vm:<id>" / "vol:<id>"
	next        int            // offset of the next candidate
	// live returns the uids processes run as right now; a variable so tests
	// need not depend on the host's process table.
	live func() (map[int]bool, error)
}

func newIDPool(d jailer.Defaults) *idPool {
	return &idPool{base: d.IDBase, count: d.IDCount, owner: make(map[int]string), live: jailer.LiveUIDs}
}

func vmOwner(id string) string  { return "vm:" + id }
func volOwner(id string) string { return "vol:" + id }

// take records an identity already assigned to owner in a persisted record.
// It reports false — and records nothing — when the identity is outside the
// range or held by someone else; the caller then assigns a fresh one.
func (p *idPool) take(uid int, owner string) bool {
	if uid < p.base || uid >= p.base+p.count {
		return false
	}
	if cur, ok := p.owner[uid]; ok && cur != owner {
		return false
	}
	p.owner[uid] = owner
	return true
}

// alloc assigns a fresh identity to owner. It fails closed: without a readable
// process table it can't rule out a live process on a candidate, so it refuses
// rather than guess.
func (p *idPool) alloc(owner string) (int, error) {
	live, err := p.live()
	if err != nil {
		return 0, fmt.Errorf("reading the process table to allocate an identity: %w", err)
	}
	for i := 0; i < p.count; i++ {
		off := (p.next + i) % p.count
		uid := p.base + off
		if _, used := p.owner[uid]; used || live[uid] {
			continue
		}
		p.owner[uid] = owner
		p.next = (off + 1) % p.count
		return uid, nil
	}
	return 0, fmt.Errorf("%w: all %d identities of the reserved range are in use", ErrConflict, p.count)
}

// release returns owner's identity to the pool; a no-op if owner doesn't hold
// it.
func (p *idPool) release(uid int, owner string) {
	if p.owner[uid] == owner {
		delete(p.owner, uid)
	}
}

func (m *Manager) allocID(owner string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ids.alloc(owner)
}

func (m *Manager) releaseID(uid int, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids.release(uid, owner)
}

// adoptVMIdentity re-registers a persisted VM's identity at startup and makes
// its disk belong to it. A record from before per-VM identities (or one whose
// identity is unusable) gets a fresh identity, persisted, and its disk is
// handed over: from then on it is that VM's alone. A VM still running as the
// old shared identity keeps its open disk and TAP until its next boot, which
// runs as the new one.
func (m *Manager) adoptVMIdentity(rec *types.VM) {
	id := rec.Config.ID
	m.mu.Lock()
	ok := m.ids.take(rec.Config.JailUID, vmOwner(id))
	m.mu.Unlock()
	if !ok {
		uid, err := m.allocID(vmOwner(id))
		if err != nil {
			log.Printf("reconcile: vm %s: %v — it cannot boot until an identity frees up", id, err)
			return
		}
		old := rec.Config.JailUID
		rec.Config.JailUID = uid
		if err := m.store.SaveVM(rec); err != nil {
			log.Printf("reconcile: vm %s: persisting its new identity %d: %v", id, uid, err)
		}
		if rec.State == types.VMStateRunning {
			log.Printf("reconcile: vm %s moved from identity %d to %d; its running process keeps the old one until it restarts", id, old, uid)
		}
	}
	if rec.Config.Rootfs == "" {
		return
	}
	if err := storage.OwnPrivate(rec.Config.Rootfs, rec.Config.JailUID, rec.Config.JailUID); err != nil {
		log.Printf("reconcile: vm %s: %v", id, err)
	}
}

// adoptVolumeIdentity is adoptVMIdentity for a volume: re-register its own
// identity, or assign one to a record that predates them.
func (m *Manager) adoptVolumeIdentity(vol *types.Volume) {
	m.mu.Lock()
	ok := m.ids.take(vol.UID, volOwner(vol.ID))
	m.mu.Unlock()
	if ok {
		return
	}
	uid, err := m.allocID(volOwner(vol.ID))
	if err != nil {
		log.Printf("reconcile: volume %s: %v", vol.ID, err)
		return
	}
	vol.UID = uid
	if err := m.store.SaveVolume(vol); err != nil {
		log.Printf("reconcile: volume %s: persisting its identity %d: %v", vol.ID, uid, err)
	}
}

// volumeOwnerLocked returns the identity a volume's file must belong to: the
// VM holding it, or its own when free. Caller holds m.mu.
func (m *Manager) volumeOwnerLocked(vol *types.Volume) int {
	if vol.AttachedTo != "" {
		if rec, ok := m.vms[vol.AttachedTo]; ok && rec.Config.JailUID > 0 {
			return rec.Config.JailUID
		}
	}
	return vol.UID
}

// giveVolume hands a volume's file to uid. A volume that can't be handed over
// stays with its previous owner, which is never less restrictive: it is either
// the volume's own identity or the VM that last held it.
func giveVolume(vol *types.Volume, uid int) error {
	if uid <= 0 {
		return fmt.Errorf("volume %s has no identity to belong to", vol.ID)
	}
	return storage.OwnPrivate(vol.Path, uid, uid)
}

// enforceVolumeOwners makes every volume's file belong to whoever holds it
// (see volumeOwnerLocked). Run at startup, after VMs and volumes have their
// identities.
func (m *Manager) enforceVolumeOwners() {
	m.mu.Lock()
	type job struct {
		vol *types.Volume
		uid int
	}
	jobs := make([]job, 0, len(m.vols))
	for _, v := range m.vols {
		jobs = append(jobs, job{v, m.volumeOwnerLocked(v)})
	}
	m.mu.Unlock()
	for _, j := range jobs {
		if err := giveVolume(j.vol, j.uid); err != nil {
			log.Printf("reconcile: %v", err)
		}
	}
}
