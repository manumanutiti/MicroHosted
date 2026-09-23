package types

// Template describes a reusable "micro-machine": a golden kernel + rootfs pair
// plus default sizing, that VMs are cloned from. The catalog is the seed of
// what will later become a full image library (Stage 5).
type Template struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	KernelPath  string `json:"kernel_path"`
	RootfsPath  string `json:"rootfs_path"`
	VCPUs       int64  `json:"vcpus"`
	MemMB       int64  `json:"mem_mb"`
	// DiskMB is the disk size, in MiB, VMs cloned from this template get. The
	// golden rootfs is grown to it at clone time (storage.CloneRootfs), so it
	// must be >= the golden image's own size — a smaller value is ignored,
	// disks are only ever grown, never shrunk. 0 means "leave the clone at the
	// golden's size", which is almost always too tight for a guest that wants
	// to apt-install anything (see the No-space-left errors that motivated it).
	DiskMB int64 `json:"disk_mb"`

	// Ready reports whether this template's golden kernel AND rootfs are
	// actually on THIS host. The catalog is a static list of what the project
	// knows how to boot, and most goldens are built by `make prepare-image`:
	// on a fresh store a template is listed and still not creatable. The
	// daemon computes this on every read (it is the side that can stat the
	// store) and never writes it back to the catalog file.
	Ready bool `json:"ready"`
	// Missing says what Ready is false for — the absent golden's path — so a
	// listing can explain itself without a second round trip.
	Missing string `json:"missing,omitempty"`
}
