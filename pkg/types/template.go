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
}
