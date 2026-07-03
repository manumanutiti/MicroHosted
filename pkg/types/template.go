package types

// Template describes a reusable "micromáquina": a golden kernel + rootfs pair
// plus default sizing, that VMs are cloned from. The catalog is the seed of
// what will later become a full image library (Sesión 5).
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
}
