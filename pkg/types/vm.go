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

	// Red. En el modelo segmentado (Fase 1) el TAP se enslava al bridge de la
	// red; GuestIP sale del IPAM de la red y GatewayIP es el gateway del bridge.
	// HostIP queda del modelo /30 anterior y ya no se usa en redes segmentadas.
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
