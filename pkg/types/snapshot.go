package types

import "time"

// Snapshot is a frozen, restorable point-in-time of a running VM: guest memory
// + device state (Firecracker's vmstate/mem pair) plus a copy-on-write clone of
// the VM's disk taken at the same instant, while the VM was paused — so memory
// and disk are mutually consistent.
//
// A snapshot is an independent entity: it survives its source VM being stopped
// or destroyed (that's the point — "detonate, then revert to clean" requires
// the clean state to outlive whatever happened after it). Restoring never
// mutates the snapshot's files: forks get reflink copies of the disk and
// hardlinks to the (read-only) memory/vmstate files.
type Snapshot struct {
	ID string
	// Name is an optional operator label ("clean", "post-infection", ...).
	Name string
	// SourceVMID is the VM this snapshot was taken from. Kept for lineage and
	// because in-place restore (POST /v1/vms/{id}/restore) only accepts
	// snapshots taken from that same VM — the guest's identity (IP, MAC,
	// in-chroot drive filename) is frozen inside the snapshot's memory.
	SourceVMID   string
	TemplateName string

	// Sizing at snapshot time. A restored VM always comes back with exactly
	// this shape — Firecracker restores the machine config from vmstate, so
	// these are recorded for visibility and disk bookkeeping, not as knobs.
	VCPUs  int64
	MemMB  int64
	DiskMB int64

	// Network identity frozen inside the guest's memory. The guest will wake
	// up believing it still has this address — it can't be changed at restore
	// time (it lives in the snapshotted RAM), which is why forking back onto
	// the origin network requires this exact IP to be free.
	NetworkName string
	GuestIP     string
	GatewayIP   string
	PrefixLen   int
	// HadNetwork records whether the source VM had a TAP at snapshot time.
	// Firecracker refuses to restore a snapshot with a network device unless
	// a host TAP is provided for it, so every restore of such a snapshot must
	// create one (enslaved or quarantined).
	HadNetwork bool
	// TapDevice is the host TAP name recorded inside the vmstate. Restoring
	// onto a TAP with this exact name needs no network_overrides — which
	// matters because that load field only exists in Firecracker >= 1.12, so
	// reusing this name is the only restore path on older hosts.
	TapDevice string

	// DriveBase is the drive's filename inside the chroot at snapshot time
	// (e.g. "<source-vm-id>.ext4"). Firecracker's vmstate records the drive by
	// this path, so every restore must hardlink the disk into the new chroot
	// under this exact name, whatever the new VM's own ID is.
	DriveBase string

	// Dir is the host directory holding the snapshot's files: vmstate, mem,
	// disk.ext4. Lives under the CoW store (<instances-dir>/snapshots/<id>) so
	// disk reflinks and chroot hardlinks never cross filesystems.
	Dir string

	CreatedAt time.Time
}

// CreateSnapshotRequest is the payload accepted by POST /v1/vms/{id}/snapshot.
type CreateSnapshotRequest struct {
	Name string `json:"name,omitempty"`
}

// ForkVMRequest is the payload accepted by the two fork actions —
// POST /v1/snapshots/{id}/fork (fork from a snapshot) and
// POST /v1/vms/{id}/fork (direct fork of a running VM).
type ForkVMRequest struct {
	// Quarantine detaches the fork from any bridge: the TAP device Firecracker
	// needs is created but enslaved to nothing, so the guest wakes up believing
	// it has its old network while every packet it sends goes nowhere. vsock
	// exec keeps working. This is the mode for poking at a forked
	// point-of-infection without it talking to anything — and it's the only
	// mode that allows N simultaneous forks of one snapshot (they all share
	// the same in-memory IP/MAC, so at most one of them may rejoin the origin
	// network).
	Quarantine bool `json:"quarantine,omitempty"`
}

// RestoreVMRequest is the payload accepted by POST /v1/vms/{id}/restore.
type RestoreVMRequest struct {
	Snapshot string `json:"snapshot"`
}

// SnapshotResponse is the JSON representation of a snapshot returned by the API.
type SnapshotResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	SourceVM  string `json:"source_vm"`
	Template  string `json:"template"`
	VCPUs     int64  `json:"vcpus"`
	MemMB     int64  `json:"mem_mb"`
	DiskMB    int64  `json:"disk_mb"`
	Network   string `json:"network,omitempty"`
	GuestIP   string `json:"guest_ip,omitempty"`
	CreatedAt string `json:"created_at"`
}

// NewSnapshotResponse builds the API DTO from an internal snapshot record.
func NewSnapshotResponse(s *Snapshot) SnapshotResponse {
	return SnapshotResponse{
		ID:        s.ID,
		Name:      s.Name,
		SourceVM:  s.SourceVMID,
		Template:  s.TemplateName,
		VCPUs:     s.VCPUs,
		MemMB:     s.MemMB,
		DiskMB:    s.DiskMB,
		Network:   s.NetworkName,
		GuestIP:   s.GuestIP,
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
