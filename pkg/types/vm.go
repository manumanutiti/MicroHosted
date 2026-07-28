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
	ID           string
	TemplateName string
	Kernel       string
	Rootfs       string
	VCPUs        int64
	MemMB        int64
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

	// Quarantine marks a VM forked from a snapshot with its TAP deliberately
	// enslaved to no bridge: the guest believes it has GuestIP (frozen in the
	// restored memory) but its packets go nowhere. NetworkName is empty for
	// such a VM — it holds no IP reservation on any network — yet GuestIP is
	// kept populated as the address the guest *thinks* it has. Reconcile and
	// cleanup must not touch IPAM for it, only the TAP.
	Quarantine bool

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
}
