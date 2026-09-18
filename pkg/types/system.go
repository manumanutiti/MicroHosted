package types

// Observability DTOs — the wire shapes of GET /v1/system (full report) and
// GET /v1/health (cheap probe). Designed for a panel/monitor to answer, in
// one call each: is the platform healthy, does another VM fit (CPU/RAM), is
// the store filling up, and where on the host does everything live.

// HealthCheck is one named probe of the platform's ability to do its job.
// Detail carries the human-readable reason on failure (and, for capacity
// checks, the numbers even on success).
type HealthCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// HealthResponse is returned by GET /v1/health: "ok" when every check passes,
// "degraded" otherwise (the endpoint then answers 503, so anything that can
// read an HTTP status can monitor the platform without parsing JSON).
type HealthResponse struct {
	Status string        `json:"status"`
	Checks []HealthCheck `json:"checks"`
}

// SystemResponse is the full observability report of GET /v1/system.
type SystemResponse struct {
	// Status/Checks mirror GET /v1/health, embedded so a dashboard needs one
	// call, not two.
	Status  string        `json:"status"`
	Checks  []HealthCheck `json:"checks"`
	Daemon  DaemonInfo    `json:"daemon"`
	Host    HostInfo      `json:"host"`
	Storage StorageInfo   `json:"storage"`
	Fleet   FleetInfo     `json:"fleet"`
}

// DaemonInfo describes the microhosted process itself and where every
// platform artifact lives on the host.
type DaemonInfo struct {
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	// FirecrackerVersion as reported by the binary (e.g. "v1.10.1"); empty if
	// the probe failed (the firecracker health check will be failing too).
	FirecrackerVersion string `json:"firecracker_version,omitempty"`
	// NetworkOverrides: the firecracker binary supports network_overrides on
	// snapshot load (>= v1.12) — the capability that allows simultaneous
	// forks from one snapshot. Surfaced because its absence turns fork
	// requests into 409s that are otherwise puzzling.
	NetworkOverrides bool        `json:"network_overrides"`
	Paths            DaemonPaths `json:"paths"`
}

// DaemonPaths is the "where is everything" map: every host-side location an
// operator may need to inspect, back up or clean.
type DaemonPaths struct {
	Store       string `json:"store"`       // CoW store root: clones + console logs live directly here
	Goldens     string `json:"goldens"`     // golden template rootfs images
	Kernels     string `json:"kernels"`     // guest kernels
	Snapshots   string `json:"snapshots"`   // vmstate/mem/disk per snapshot
	Volumes     string `json:"volumes"`     // persistent volumes (<id>.ext4)
	ChrootBase  string `json:"chroot_base"` // Jailer chroots, one dir per VM
	Database    string `json:"database"`    // SQLite state
	Catalog     string `json:"catalog"`     // template catalog JSON
	Firecracker string `json:"firecracker"` // firecracker binary
	Jailer      string `json:"jailer"`      // jailer binary
}

// HostInfo is the physical host's live resource picture.
type HostInfo struct {
	Hostname string `json:"hostname,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	CPUs     int    `json:"cpus"`
	// Load averages; judge against CPUs.
	Load1  float64    `json:"load1"`
	Load5  float64    `json:"load5"`
	Load15 float64    `json:"load15"`
	Memory MemoryInfo `json:"memory"`
}

// MemoryInfo is host RAM in MiB. UsedMB is Total-Available (reclaimable cache
// doesn't count as used), so AvailableMB is what new VMs can actually claim.
type MemoryInfo struct {
	TotalMB     int64 `json:"total_mb"`
	UsedMB      int64 `json:"used_mb"`
	AvailableMB int64 `json:"available_mb"`
}

// StorageInfo is the capacity picture of the instances store: the filesystem
// totals plus a per-category breakdown of what the platform itself occupies.
type StorageInfo struct {
	Path   string `json:"path"`
	FSType string `json:"fs_type"`
	// COW: the store supports reflink clones. False means every VM costs a
	// full rootfs copy (the daemon also warns at startup).
	COW     bool  `json:"cow"`
	TotalMB int64 `json:"total_mb"`
	UsedMB  int64 `json:"used_mb"`
	FreeMB  int64 `json:"free_mb"`
	// Breakdown: allocated (du-style) size per category. On a CoW store,
	// reflink-shared extents are counted once per file, so categories can sum
	// to more than UsedMB — each number answers "what would deleting this
	// free at most", not "what does it exclusively own".
	Breakdown []StorageEntry `json:"breakdown"`
}

// StorageEntry is one category of the store breakdown.
type StorageEntry struct {
	What   string `json:"what"`
	Path   string `json:"path"`
	SizeMB int64  `json:"size_mb"`
}

// FleetInfo summarizes everything the daemon manages, with the aggregate
// resources the running VMs have been promised (overcommit visibility: judge
// Allocated against HostInfo).
type FleetInfo struct {
	VMs       VMCounts  `json:"vms"`
	Allocated Allocated `json:"allocated"`
	Networks  int       `json:"networks"`
	// ManagedIfaces are the host interfaces this daemon owns the whole
	// nftables policy for. Reported because "which interfaces am I
	// responsible for" should be answerable from the API, not only from
	// the unit file.
	ManagedIfaces []string     `json:"managed_ifaces,omitempty"`
	Snapshots     int          `json:"snapshots"`
	Volumes       VolumeCounts `json:"volumes"`
	Templates     int          `json:"templates"`
}

// VMCounts breaks the fleet down by state.
type VMCounts struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Stopped int `json:"stopped"`
}

// Allocated is what the RUNNING VMs are promised in aggregate. Actual
// consumption is usually lower (guest RAM faults in on demand) — per-VM
// reality is in VMResponse.MemRSSMB.
type Allocated struct {
	VCPUs int64 `json:"vcpus"`
	MemMB int64 `json:"mem_mb"`
}

// VolumeCounts splits volumes into attached (in use by a VM) and the rest.
type VolumeCounts struct {
	Total    int `json:"total"`
	Attached int `json:"attached"`
}
