package types

// CreateVMRequest is the payload accepted by POST /v1/vms.
type CreateVMRequest struct {
	Template string `json:"template"`

	// Name and Labels: see VMConfig.Name and VMConfig.Labels. Both optional.
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`

	VCPUs int64 `json:"vcpus,omitempty"`
	MemMB int64 `json:"mem_mb,omitempty"`

	// DiskMB overrides the template's default disk size (in MiB) for this VM.
	// Only ever grows the disk past the golden image — a value smaller than the
	// image (or 0, meaning "use the template default") is ignored. See
	// types.Template.DiskMB and storage.CloneRootfs.
	DiskMB int64 `json:"disk_mb,omitempty"`

	// Network is the name of the segmented network to attach the VM to (see
	// docs/networking.md). Empty means the built-in "default" network. Ignored
	// when NoNetwork is set.
	Network string `json:"network,omitempty"`

	// NoNetwork skips the TAP device/IP allocation entirely, giving the VM
	// zero network path to the host. Sandboxed/ephemeral workloads (the
	// main use case in PROJECT.md) often shouldn't need one at all — the
	// intended channel for those is vsock, not SSH over a TAP link. Network
	// stays on by default so existing callers keep working unchanged.
	NoNetwork bool `json:"no_network,omitempty"`

	// GuestIP asks for one specific address on Network instead of the next
	// free one — how a replacement takes over the address of the VM it
	// replaces, and the only way to get an address an ingress rule points at
	// (those are never handed out automatically). 409 if someone holds it.
	GuestIP string `json:"guest_ip,omitempty"`

	// Volumes attaches existing persistent volumes to the VM at boot: a sample
	// mounted read-only to analyse without altering it, plus a writable volume
	// to collect artifacts that survive the VM's destruction. Each is mounted at
	// /vol/<name> (or its guest_path) after boot. See VolumeAttachRequest.
	Volumes []VolumeAttachRequest `json:"volumes,omitempty"`

	// Autostart boots the VM again when the daemon starts and finds it dead
	// (host reboot, crash while the daemon was down). See VMConfig.Autostart.
	Autostart bool `json:"autostart,omitempty"`
}

// UpdateVMAutostartRequest is the payload accepted by
// PUT /v1/vms/{id}/autostart: flips whether the VM comes back on its own after
// a host reboot.
type UpdateVMAutostartRequest struct {
	Autostart bool `json:"autostart"`
}

// ReplaceVMRequest is the payload accepted by POST /v1/vms/{id}/replace: a new
// VM takes over the old one's function — its network address and labels —
// and the old one is dealt with as Old says.
type ReplaceVMRequest struct {
	// Where the replacement comes from: a snapshot (forked at the function's
	// address, so it must have been taken there) or a template (a cold boot
	// with the old VM's shape). Neither: the old VM's template. Never both.
	Snapshot string `json:"snapshot,omitempty"`
	Template string `json:"template,omitempty"`
	// Old is what happens to the VM being replaced, which is always cut off
	// its network first: "quarantine" (default — left running, reachable over
	// vsock), "stop" (quarantined and powered off: the disk is kept for
	// forensics, the RAM freed before the replacement boots) or "destroy".
	Old string `json:"old,omitempty"`
	// Name for the replacement (names belong to instances; it does not inherit
	// the old one's). Labels are merged over the ones it inherits.
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Old-VM dispositions for ReplaceVMRequest.Old.
const (
	ReplaceOldQuarantine = "quarantine"
	ReplaceOldStop       = "stop"
	ReplaceOldDestroy    = "destroy"
)

// ReplaceVMResponse is what POST /v1/vms/{id}/replace returns: the new VM and
// the old one as it was left (absent when destroyed).
type ReplaceVMResponse struct {
	Replacement VMResponse  `json:"replacement"`
	Old         *VMResponse `json:"old,omitempty"`
}

// UpdateVMLabelsRequest is the payload accepted by PATCH /v1/vms/{id}/labels,
// a merge patch: a key with a value sets it, a key with null removes it, and
// keys not mentioned are left alone.
type UpdateVMLabelsRequest struct {
	Labels map[string]*string `json:"labels"`
}

// VMResponse is the JSON representation of a VM returned by the API.
type VMResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Template string            `json:"template"`
	State    VMState           `json:"state"`
	PID      int               `json:"pid,omitempty"`
	// Shape: what the VM was promised at create time.
	VCPUs  int64 `json:"vcpus"`
	MemMB  int64 `json:"mem_mb"`
	DiskMB int64 `json:"disk_mb,omitempty"`
	// Live consumption of the Firecracker process, present only while
	// running (filled by the API layer from /proc, not by NewVMResponse):
	// MemRSSMB is what the VM costs the host RIGHT NOW (guest RAM faults in
	// on demand, so usually well under mem_mb); CPUSeconds is cumulative —
	// diff between two polls for a usage rate; UptimeSeconds counts from the
	// process's start (boot/restore), not from created_at.
	UptimeSeconds int64   `json:"uptime_seconds,omitempty"`
	MemRSSMB      int64   `json:"mem_rss_mb,omitempty"`
	CPUSeconds    float64 `json:"cpu_seconds,omitempty"`
	Network       string  `json:"network,omitempty"`
	GuestIP       string  `json:"guest_ip,omitempty"`
	HostIP        string  `json:"host_ip,omitempty"`
	TapDevice     string  `json:"tap_device,omitempty"`
	// RootfsPath is the VM's disk (the rootfs clone) on the host — with
	// LogPath, the two per-VM artifacts an operator inspects directly.
	RootfsPath string `json:"rootfs_path,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
	// Quarantine: TAP on no bridge (a quarantined fork, or a VM quarantined in
	// place) — guest_ip is the address the guest believes it has, not a live
	// reservation.
	Quarantine bool `json:"quarantine,omitempty"`
	// QuarantinedFrom: the network it served on before a quarantine in place.
	QuarantinedFrom string `json:"quarantined_from,omitempty"`
	// Replaces: the VM this one took over from (POST /v1/vms/{id}/replace).
	Replaces string `json:"replaces,omitempty"`
	// RestoredFrom is the snapshot this VM was forked/restored from, if any.
	RestoredFrom string `json:"restored_from,omitempty"`
	// Volumes are the persistent volumes attached to this VM.
	Volumes   []MountResponse `json:"volumes,omitempty"`
	Autostart bool            `json:"autostart,omitempty"`
	CreatedAt string          `json:"created_at"`
	// LastExit: the VM's process last died on its own (see VM.LastExit).
	LastExit *VMExitResponse `json:"last_exit,omitempty"`
}

// VMExitResponse is the wire form of VMExit.
type VMExitResponse struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// BulkDeleteResponse is returned by bulk-delete endpoints (DELETE /v1/vms,
// DELETE /v1/networks/{name}/vms). Individual VM failures don't abort the
// batch — each VM is destroyed independently, so partial success is the
// normal case, not an error condition; the caller inspects Failed to see
// what, if anything, needs a retry.
type BulkDeleteResponse struct {
	Deleted []string          `json:"deleted"`
	Failed  map[string]string `json:"failed,omitempty"`
}

// ExecRequest is the payload accepted by POST /v1/vms/{id}/exec.
type ExecRequest struct {
	Cmd string `json:"cmd"`
}

// ExecResponse is the combined stdout+stderr and exit code of a command run
// inside a VM via the vsock exec channel (see internal/vsock).
type ExecResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// NewVMResponse builds the API DTO from an internal VM record.
func NewVMResponse(vm *VM) VMResponse {
	var lastExit *VMExitResponse
	if vm.LastExit != nil {
		lastExit = &VMExitResponse{At: vm.LastExit.At.Format("2006-01-02T15:04:05Z07:00"), Reason: vm.LastExit.Reason}
	}
	return VMResponse{
		LastExit:        lastExit,
		ID:              vm.Config.ID,
		Name:            vm.Config.Name,
		Labels:          vm.Config.Labels,
		Template:        vm.Config.TemplateName,
		State:           vm.State,
		PID:             vm.PID,
		VCPUs:           vm.Config.VCPUs,
		MemMB:           vm.Config.MemMB,
		DiskMB:          vm.Config.DiskMB,
		Network:         vm.Config.NetworkName,
		GuestIP:         vm.Config.GuestIP,
		HostIP:          vm.Config.HostIP,
		TapDevice:       vm.Config.TapDevice,
		RootfsPath:      vm.Config.Rootfs,
		LogPath:         vm.LogPath,
		Quarantine:      vm.Config.Quarantine,
		QuarantinedFrom: vm.Config.QuarantinedFrom,
		Replaces:        vm.Config.Replaces,
		RestoredFrom:    vm.Config.RestoredFrom,
		Volumes:         newMountResponses(vm.Config.Volumes),
		Autostart:       vm.Config.Autostart,
		CreatedAt:       vm.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// newMountResponses maps a VM's attached volumes to their wire DTOs, returning
// nil (omitted from JSON) when the VM has none.
func newMountResponses(mounts []VolumeMount) []MountResponse {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]MountResponse, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, MountResponse{
			Volume:    m.VolumeName,
			ReadOnly:  m.ReadOnly,
			GuestPath: m.GuestPath,
		})
	}
	return out
}
