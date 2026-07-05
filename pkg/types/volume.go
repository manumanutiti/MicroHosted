package types

import "time"

// Volume is a persistent ext4 disk that lives independently of any VM. Unlike a
// VM's rootfs clone (erased on Destroy), a volume survives — it's the data plane
// of the platform: a read-only sample to analyse without altering it, or a
// writable scratch disk that collects a detonation's artifacts and can be read
// back afterwards.
//
// A volume is attached to at most one VM at a time (AttachedTo). Attaching it to
// a second while the first holds it would let two guests write the same ext4
// concurrently and corrupt it — the manager rejects that with ErrConflict.
type Volume struct {
	ID   string
	Name string
	// SizeMB is the ext4 image size in MiB, fixed at creation (mkfs.ext4 over a
	// truncated file, no partition table — same shape as the golden rootfs, so a
	// future grow path is the same offline resize2fs the clones use).
	SizeMB int64
	// Path is the host-side ext4 file, under <instancesDir>/volumes/<id>.ext4 —
	// the same CoW store as clones/kernels/chroot, by the L3 invariant, so Jailer
	// can hardlink it into a VM's chroot (a hardlink can't cross filesystems).
	Path string
	// AttachedTo is the ID of the VM currently holding this volume, or "" when
	// free. Set at Create (attach) and cleared at Destroy; a free volume is the
	// only one that host-side offline I/O (debugfs) may touch, since a mounted
	// ext4 must not be written underneath a running guest.
	AttachedTo string
	CreatedAt  time.Time
}

// VolumeMount records one volume attached to a VM: how it's exposed to the guest
// and under what drive identity. Stored on VMConfig so a restart's Reconcile can
// re-establish the attachment and the auto-mount survives in the record.
type VolumeMount struct {
	VolumeID   string
	VolumeName string
	// ReadOnly maps to the Firecracker drive's is_read_only. The read-only mode
	// is what lets a sample be analysed without the analysis mutating it — the
	// block device itself rejects writes, not just a mount option.
	ReadOnly bool
	// GuestPath is where the daemon mounts the volume inside the guest after boot
	// (default "/vol/<name>"). Empty means the volume is attached as a raw block
	// device and the guest mounts it itself.
	GuestPath string
	// DriveID is the Firecracker drive id ("vol-<volumeID>"), stable per volume.
	// Firecracker exposes secondary drives as /dev/vdb, /dev/vdc… in the order
	// they appear in the config, which is the order this slice is built in.
	DriveID string
	// HostPath is the volume's ext4 file on the host, passed to Firecracker as
	// the drive's path_on_host. Recorded here so BuildConfig has it without
	// needing the storage layer, the same way VMConfig.Rootfs carries the root
	// disk's path.
	HostPath string
}

// CreateVolumeRequest is the payload accepted by POST /v1/volumes.
type CreateVolumeRequest struct {
	Name   string `json:"name"`
	SizeMB int64  `json:"size_mb"`
}

// VolumeAttachRequest attaches an existing volume to a VM at create time. It's
// an element of CreateVMRequest.Volumes.
type VolumeAttachRequest struct {
	// Name of an existing volume (created via POST /v1/volumes).
	Name string `json:"name"`
	// ReadOnly attaches the volume as a read-only block device — the mode for a
	// sample that must not be altered by the guest that inspects it.
	ReadOnly bool `json:"read_only,omitempty"`
	// GuestPath overrides the default mount point (/vol/<name>) inside the guest.
	GuestPath string `json:"guest_path,omitempty"`
}

// VolumeResponse is the JSON representation of a volume returned by the API.
type VolumeResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SizeMB     int64  `json:"size_mb"`
	AttachedTo string `json:"attached_to,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// NewVolumeResponse builds the API DTO from an internal volume record.
func NewVolumeResponse(v *Volume) VolumeResponse {
	return VolumeResponse{
		ID:         v.ID,
		Name:       v.Name,
		SizeMB:     v.SizeMB,
		AttachedTo: v.AttachedTo,
		CreatedAt:  v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// MountResponse describes one attached volume inside a VMResponse.
type MountResponse struct {
	Volume    string `json:"volume"`
	ReadOnly  bool   `json:"read_only"`
	GuestPath string `json:"guest_path,omitempty"`
}
