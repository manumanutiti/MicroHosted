package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"

	mhlabels "microhosted/internal/labels"
	"microhosted/pkg/types"
)

// replacement is what Replace asks the launcher for: a new VM serving the
// function at its network and address.
type replacement struct {
	snapshot, template string
	image              string
	network, ip        string
	name               string
	labels             map[string]string
	vcpus, memMB       int64
	diskMB             int64
	// ioLimits are the old VM's, kept by its replacement (within the ceiling
	// in force now, which may have been lowered since).
	ioLimits *types.IOLimits
	// files are written into a template or image replacement's disk.
	files []types.FileSpec
}

// Replace hands a VM's function — its network address and labels — over to a
// new VM, and deals with the old one as req.Old says. Deciding THAT a VM must
// be replaced (it died, it stopped answering, its data looks compromised) is
// the orchestrator's call; this makes the handover itself safe:
//
//  1. Everything that can be checked is checked before anything changes: the
//     source exists and serves at the function's address, the old VM carries
//     no volumes, and no other VM serves the function already — so a replace
//     repeated by a confused or looping caller is a 409, not one more VM.
//  2. The old VM is cut off its network (Quarantine; skipped if it already
//     is), which frees the function's address.
//  3. With old=stop or old=destroy it is powered off before the replacement
//     boots, so its RAM is free for it — on a small host that is often the difference
//     between a replacement and a 503.
//  4. The replacement boots claiming the function's address (a fork of a
//     snapshot taken there, or a template boot with guest_ip), inheriting the
//     old VM's labels (minus lease=quarantined) and autostart.
//  5. Only once it is up is the old VM destroyed (old=destroy).
//
// If the replacement fails, the old VM stays quarantined — never put back on
// its network: a suspect is not reconnected because its successor did not
// boot. The function is down, the error says so, and the same replace can be
// retried (a quarantined VM remembers its function's network). The daemon
// dying midway leaves the same state: the half-created replacement is undone
// at startup like any create.
func (m *Manager) Replace(ctx context.Context, id string, req types.ReplaceVMRequest) (replacementVM, old *types.VM, err error) {
	disposition := req.Old
	if disposition == "" {
		disposition = types.ReplaceOldQuarantine
	}
	switch disposition {
	case types.ReplaceOldQuarantine, types.ReplaceOldStop, types.ReplaceOldDestroy:
	default:
		return nil, nil, fmt.Errorf("%w: old %q: want quarantine, stop or destroy", ErrInvalid, req.Old)
	}
	if sources := countSet(req.Snapshot, req.Template, req.Image); sources > 1 {
		return nil, nil, fmt.Errorf("%w: give one of snapshot, template or image", ErrInvalid)
	}
	if err := errors.Join(ValidateName(req.Name), ValidateLabels(req.Labels)); err != nil {
		return nil, nil, err
	}
	if req.Snapshot != "" && len(req.Files) > 0 {
		return nil, nil, fmt.Errorf("%w: files cannot be written into a snapshot's disk: its memory already holds that filesystem", ErrInvalid)
	}
	// Checked here, before the old VM is cut off: a bad file must not leave
	// the function without either VM.
	if _, _, err := prepareFiles(req.Files); err != nil {
		return nil, nil, err
	}

	if err := m.begin(id, "replace"); err != nil {
		return nil, nil, err
	}
	defer m.end(id)

	m.mu.Lock()
	record, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrVMNotFound, id)
	}
	cfg := record.Config
	fnNet, fnIP := cfg.NetworkName, cfg.GuestIP
	if cfg.Quarantine {
		fnNet = cfg.QuarantinedFrom
	}
	var holder string
	for oid, v := range m.vms {
		if oid != id && fnNet != "" && v.Config.NetworkName == fnNet && v.Config.GuestIP == fnIP {
			holder = oid
		}
	}
	m.mu.Unlock()

	if fnNet == "" || fnIP == "" {
		return nil, nil, fmt.Errorf("%w: vm %s serves on no network: there is no address to hand over", ErrConflict, id)
	}
	if holder != "" {
		return nil, nil, fmt.Errorf("%w: the function of vm %s (%s on %s) is already served by vm %s", ErrConflict, id, fnIP, fnNet, holder)
	}
	if len(cfg.Volumes) > 0 {
		return nil, nil, fmt.Errorf("%w: vm %s has volumes attached; replace does not hand volumes over yet", ErrConflict, id)
	}
	// The daemon keeps no file contents, so it cannot give a replacement the
	// old VM's files by itself; booting one without them would bring the
	// function up unconfigured. The caller says which files, or none.
	if req.Snapshot == "" && req.Files == nil && len(cfg.Files) > 0 {
		paths := make([]string, len(cfg.Files))
		for i, f := range cfg.Files {
			paths[i] = f.Path
		}
		return nil, nil, fmt.Errorf("%w: vm %s was created with files (%s): give them again for its replacement, or an empty list to replace it without", ErrInvalid, id, strings.Join(paths, ", "))
	}

	if v, ok := req.Labels[mhlabels.ManagedBy]; ok && v != cfg.Labels[mhlabels.ManagedBy] {
		return nil, nil, fmt.Errorf("%w: label %s is inherited by the replacement and cannot be changed (it decides ownership and quota)", ErrInvalid, mhlabels.ManagedBy)
	}
	labels := maps.Clone(cfg.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	delete(labels, LeaseLabel)
	maps.Copy(labels, req.Labels)
	if err := ValidateLabels(labels); err != nil {
		return nil, nil, err
	}
	if len(labels) == 0 {
		labels = nil
	}
	spec := replacement{
		snapshot: req.Snapshot, network: fnNet, ip: fnIP,
		name: req.Name, labels: labels,
		vcpus: cfg.VCPUs, memMB: cfg.MemMB, diskMB: cfg.DiskMB,
		ioLimits: clampIOLimits(cfg.IOLimits, m.Limits().IO),
		files:    req.Files,
	}
	if req.Snapshot != "" {
		m.mu.Lock()
		snap, ok := m.snaps[req.Snapshot]
		m.mu.Unlock()
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, req.Snapshot)
		}
		// The fork wakes up with the snapshot's address frozen in its memory.
		// Anywhere else it would not be serving the function at all.
		if snap.NetworkName != fnNet || snap.GuestIP != fnIP {
			return nil, nil, fmt.Errorf("%w: snapshot %s was taken at %s on %q, the function is at %s on %q", ErrConflict, snap.ID, snap.GuestIP, snap.NetworkName, fnIP, fnNet)
		}
	} else {
		spec.template, spec.image = req.Template, req.Image
		if spec.template == "" && spec.image == "" {
			// The old VM's own source. For an image that is its digest, not
			// its tag: the replacement boots exactly the bytes that were
			// validated, never a later build.
			if cfg.Image != "" {
				spec.image = cfg.Image
			} else {
				spec.template = cfg.TemplateName
			}
		}
		if spec.template == "" && spec.image == "" {
			return nil, nil, fmt.Errorf("%w: vm %s has no template to replace it from; give a template, an image or a snapshot", ErrInvalid, id)
		}
		if m.catalog != nil || m.images != nil {
			if _, _, err := m.resolveSource(spec.template, spec.image); err != nil {
				if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
					err = fmt.Errorf("%w: %v", ErrInvalid, err)
				}
				return nil, nil, err
			}
		}
	}

	// From here on the old VM only ever moves away from its network.
	if !cfg.Quarantine {
		if _, err := m.quarantine(id); err != nil {
			return nil, nil, fmt.Errorf("cutting vm %s off its network: %w", id, err)
		}
	}
	if disposition != types.ReplaceOldQuarantine {
		m.mu.Lock()
		running := record.State != types.VMStateStopped
		m.mu.Unlock()
		if running {
			// A failed power-off still leaves it cut off; the replacement
			// matters more than a clean stop. A VM about to be destroyed is
			// killed outright: its disk goes next, so an orderly shutdown
			// would only keep the function down for its whole window.
			if _, err := m.stop(ctx, id, disposition != types.ReplaceOldDestroy); err != nil {
				log.Printf("replace: vm %s: powering it off: %v", id, err)
			}
		}
	}

	launch := m.replaceLaunch
	if launch == nil {
		launch = m.launchReplacement
	}
	nv, err := launch(ctx, spec)
	if err != nil {
		old, _ = m.Get(id)
		if old != nil {
			m.emit(types.EventVMReplaceFailed, old, err.Error(), map[string]string{"old": disposition})
		}
		return nil, old, fmt.Errorf("vm %s is cut off but its replacement failed, the function (%s on %s) is down — retry the replace: %w", id, fnIP, fnNet, err)
	}

	// Lineage and autostart: not create/fork parameters, set once it exists.
	m.mu.Lock()
	var cp types.VM
	if rec, ok := m.vms[nv.Config.ID]; ok {
		rec.Config.Replaces = id
		rec.Config.Autostart = cfg.Autostart
		cp = *rec
	}
	m.mu.Unlock()
	if cp.Config.ID != "" {
		if err := m.store.SaveVM(&cp); err != nil {
			log.Printf("replace: vm %s: persisting its lineage: %v", cp.Config.ID, err)
		}
		nv = &cp
	}

	// Before the destroy: the orchestrator learns where the function went
	// ahead of the old VM's vm.destroyed.
	m.emit(types.EventVMReplaced, &types.VM{Config: cfg}, "", map[string]string{"replacement": nv.Config.ID, "old": disposition})
	if disposition == types.ReplaceOldDestroy {
		if err := m.destroy(ctx, id); err != nil {
			log.Printf("replace: vm %s: destroying it: %v", id, err)
		}
	} else {
		old, _ = m.Get(id)
	}
	log.Printf("replace: vm %s (%s) replaced by vm %s at %s on %s", id, disposition, nv.Config.ID, fnIP, fnNet)
	return nv, old, nil
}

// launchReplacement boots a replacement: a fork of the snapshot (which claims
// the snapshot's address, checked to be the function's), or a template boot
// claiming the function's address explicitly.
func (m *Manager) launchReplacement(ctx context.Context, r replacement) (*types.VM, error) {
	if r.snapshot != "" {
		return m.Fork(ctx, r.snapshot, types.ForkVMRequest{Name: r.name, Labels: r.labels, IOLimits: r.ioLimits})
	}
	return m.Create(ctx, types.CreateVMRequest{
		Template: r.template, Image: r.image, Name: r.name, Labels: r.labels,
		VCPUs: r.vcpus, MemMB: r.memMB, DiskMB: r.diskMB,
		Network: r.network, GuestIP: r.ip, IOLimits: r.ioLimits, Files: r.files,
	})
}

// countSet counts the non-empty strings.
func countSet(ss ...string) int {
	n := 0
	for _, s := range ss {
		if s != "" {
			n++
		}
	}
	return n
}
