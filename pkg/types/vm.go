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

	// Red
	TapDevice string
	GuestIP   string
	HostIP    string
	GatewayIP string
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
