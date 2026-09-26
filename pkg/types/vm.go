package types

import "time"

type VMState string

const (
	VMStateCreating VMState = "creating"
	VMStateRunning  VMState = "running"
	VMStatePaused   VMState = "paused"
	VMStateStopped  VMState = "stopped"
	VMStateFailed   VMState = "failed"
)

type VMConfig struct {
	ID string
	// Name is an optional alias for THIS instance, unique among the VMs the
	// daemon tracks and fixed for its lifetime (see vm.ValidateName). It is not
	// the identity of what the VM serves: a replacement VM gets a new name, and
	// the durable identity (the sensor, the tenant) belongs in Labels.
	Name string
	// Labels are free key=value metadata, changeable at any time
	// (PATCH /v1/vms/{id}/labels) and selectable (GET /v1/vms?label=k=v). An
	// orchestrator marks the VMs it owns with one and never touches the rest.
	// Copy-on-write: record copies share this map, so a change replaces it and
	// never writes into it.
	Labels       map[string]string
	TemplateName string
	// Image is the digest of the store image the VM was created from
	// ("sha256:…"), empty for a catalog template. TemplateName then holds the
	// image's tag, for display. A replacement boots this same digest.
	Image  string
	Kernel string
	Rootfs string
	VCPUs  int64
	MemMB  int64
	// DiskMB is the size the clone was grown to at Create time. Recorded for
	// visibility (List/Get) and so a future resize path can tell what a VM
	// already has; the actual growth happens once, in storage.CloneRootfs.
	DiskMB int64

	// Networking. In the segmented model (Phase 1) the TAP is enslaved to the
	// network's bridge; GuestIP comes from the network's IPAM and GatewayIP is
	// the bridge's gateway. HostIP is a leftover of the older /30 model and is no
	// longer used in segmented networks.
	NetworkName string
	Bridge      string
	TapDevice   string
	GuestIP     string
	HostIP      string
	GatewayIP   string
	// PrefixLen is the guest's subnet prefix (the network's, e.g. 24). It must
	// match the network so the guest computes the right broadcast/route; a
	// wrong value (the old hardcoded /30) makes same-subnet peers look like
	// broadcast and unicast between VMs fails.
	PrefixLen int

	// Quarantine marks a VM with its TAP deliberately enslaved to no bridge —
	// forked that way from a snapshot, or cut off in place while running
	// (vm.Manager.Quarantine): the guest believes it has GuestIP (frozen in
	// its memory) but its packets go nowhere. NetworkName is empty for
	// such a VM — it holds no IP reservation on any network — yet GuestIP is
	// kept populated as the address the guest *thinks* it has. Reconcile and
	// cleanup must not touch IPAM for it, only the TAP.
	Quarantine bool
	// QuarantinedFrom is the network a VM was on when it was quarantined in
	// place — the function's network, which a later replace hands over to the
	// replacement (GuestIP is the function's address). Empty for a VM forked
	// straight into quarantine: it never served on any network.
	QuarantinedFrom string

	// Replaces is the VM this one took over from (vm.Manager.Replace) —
	// lineage, so the suspect can be found from its successor.
	Replaces string

	// RestoredFrom is the snapshot ID this VM was forked/restored from, empty
	// for VMs booted from a template. Lineage only — deleting the snapshot
	// later doesn't affect a VM already restored from it (the restore took
	// reflink copies / extra hardlinks, never a live dependency).
	RestoredFrom string

	// Volumes are the persistent volumes attached to this VM, in the order they
	// were requested — which is the order Firecracker assigns /dev/vdb, /dev/vdc…
	// so it must stay stable across a Stop/Start (the auto-mount relies on it).
	// A VM with any volume attached cannot be snapshotted/forked/restored (the
	// snapshotted RAM holds the volume mounted; restoring over a since-mutated
	// volume corrupts it) — those paths reject it with ErrConflict.
	Volumes []VolumeMount

	// Autostart asks the daemon to boot this VM again at startup when it finds
	// it dead — the host rebooted, or Firecracker died while the daemon was
	// down. Only such involuntary deaths: a VM powered off on purpose (Stop)
	// stays off. It is not a crash supervisor — nothing restarts a VM that dies
	// while the daemon is up. See vm.Manager.Reconcile.
	Autostart bool

	// IOLimits is the disk and network throughput this VM asked for, below the
	// daemon's ceiling; nil takes the ceiling. What is in force is worked out
	// at every boot as the lower of the two (see vm.EffectiveIO), so lowering
	// the ceiling reaches every VM at its next boot.
	IOLimits *IOLimits `json:",omitempty"`

	// Files are the files written into the disk before the first boot, as
	// metadata only: the daemon never keeps a file's content.
	Files []InjectedFile `json:",omitempty"`

	// JailUID is the identity this VM's Firecracker runs as (uid, and a gid
	// with the same number), from the daemon's reserved range and never
	// shared with another VM or volume. It owns the VM's disk and the volumes
	// attached to it. Zero only in records written before per-VM identities;
	// the daemon assigns one at startup. See internal/jailer/identity.go.
	JailUID int
}

type VM struct {
	Config     VMConfig
	State      VMState
	PID        int
	SocketPath string
	// LogPath is where the guest's serial console + Jailer/Firecracker's own
	// logs are written. The console is never attached to the daemon's
	// terminal (see internal/jailer.Build) — this file is how an operator
	// opts into looking at a VM after the fact, e.g. `tail -f <LogPath>`.
	LogPath string
	// VsockPath is the host-side Unix socket Firecracker exposes for this
	// VM's vsock device — the programmatic exec channel (internal/vsock).
	// Present regardless of whether the VM has network/TAP configured.
	VsockPath string
	CreatedAt time.Time

	// LastExit is the last time this VM's process died on its own — not a
	// Stop, Destroy or Restore — and why, as far as the host can tell. The
	// daemon marks such a VM stopped and does not restart it: whether to is
	// policy, the orchestrator's call. Kept until the next such death.
	LastExit *VMExit
}

// IOLimits caps a VM's throughput with Firecracker's rate limiters. Disk
// limits apply to each of its drives (rootfs and every volume) separately;
// the network limit to each direction of its NIC. In a request, a zero field
// means "the daemon's ceiling"; as a ceiling, zero means no limit.
type IOLimits struct {
	// DiskMiBs is bytes per second per drive, in MiB/s.
	DiskMiBs int64 `json:"disk_mib_s,omitempty"`
	// DiskIOPS is operations per second per drive.
	DiskIOPS int64 `json:"disk_iops,omitempty"`
	// NetMbit is bits per second in each direction, in Mbit/s.
	NetMbit int64 `json:"net_mbit,omitempty"`
}

// VMExit describes an involuntary VM death (see VM.LastExit).
type VMExit struct {
	At     time.Time
	Reason string
}

// InjectedFile records a FileSpec written into a VM's disk: what it was, never
// its content. SHA256 (hex) lets a caller tell whether the VM has the file it
// wants without reading the disk; it is empty for a secret, whose hash would
// let a guessable secret be recovered from the record.
type InjectedFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}
