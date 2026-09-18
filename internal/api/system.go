package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"microhosted/internal/firecracker"
	"microhosted/internal/hostinfo"
	"microhosted/internal/network"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// Observability endpoints.
//
//   - GET /v1/health — cheap probe: can the platform do its job right now
//     (KVM, database, store writable+CoW, disk space, firecracker binary,
//     every stored egress rule actually enforceable).
//     Answers 200/503 by status so monitors don't need to parse the body.
//   - GET /v1/system — the full report a panel renders in one call: the
//     health above, plus daemon identity and every important host path, host
//     CPU/RAM/load, store capacity with a per-category breakdown, and fleet
//     counts with aggregate allocated resources.
//
// Everything is computed at request time from /proc, statfs and the
// manager's in-memory state — no background collectors, no history. History
// is a client concern (poll and diff); the platform's job is to answer "now"
// cheaply and honestly.

// SystemConfig carries the daemon-level facts only main knows: where state
// and catalog live, and when the process started (uptime's zero point).
type SystemConfig struct {
	DBPath      string
	CatalogPath string
	StartedAt   time.Time
}

// freeSpaceFloor: below max(5% of the store, 1 GiB) free, the disk_space
// check fails — a full store makes every create/snapshot/upload fail in
// worse, less obvious ways, so the health endpoint says it first.
const freeSpaceFloorMB = 1024

func registerSystemRoutes(mux *http.ServeMux, mgr *vm.Manager, netmgr *network.Manager, cfg SystemConfig) {
	facts := mgr.Facts()
	// The binary's version doesn't change under a live daemon — probe once.
	fcVersion := firecracker.Version(facts.FirecrackerBin)

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		resp := types.HealthResponse{Checks: runHealthChecks(mgr, netmgr, facts)}
		resp.Status = healthStatus(resp.Checks)
		code := http.StatusOK
		if resp.Status != "ok" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	})

	mux.HandleFunc("GET /v1/system", func(w http.ResponseWriter, r *http.Request) {
		checks := runHealthChecks(mgr, netmgr, facts)
		resp := types.SystemResponse{
			Status:  healthStatus(checks),
			Checks:  checks,
			Daemon:  daemonInfo(cfg, facts, fcVersion),
			Host:    hostReport(),
			Storage: storageReport(mgr, facts),
			Fleet:   fleetReport(mgr, netmgr),
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func healthStatus(checks []types.HealthCheck) string {
	for _, c := range checks {
		if !c.OK {
			return "degraded"
		}
	}
	return "ok"
}

// runHealthChecks probes, in order of how fatal a failure is: no KVM means no
// VMs at all; a dead database loses state on restart; an unwritable or full
// store fails every create; a CoW-less store still works but fills the disk
// (same warning the daemon logs at startup, made pollable); egress rules the
// daemon cannot enforce mean the policy the API reports is not the one in force.
func runHealthChecks(mgr *vm.Manager, netmgr *network.Manager, facts vm.Facts) []types.HealthCheck {
	checks := make([]types.HealthCheck, 0, 7)

	kvm := types.HealthCheck{Name: "kvm", OK: true}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		kvm.OK, kvm.Detail = false, "/dev/kvm not available: "+err.Error()
	}
	checks = append(checks, kvm)

	db := types.HealthCheck{Name: "database", OK: true}
	if err := mgr.CheckDB(); err != nil {
		db.OK, db.Detail = false, err.Error()
	}
	checks = append(checks, db)

	// Writable store: create-and-remove a probe file where clones would go.
	storeW := types.HealthCheck{Name: "store_writable", OK: true}
	probe := filepath.Join(facts.StoreDir, ".healthprobe")
	if f, err := os.Create(probe); err != nil {
		storeW.OK, storeW.Detail = false, err.Error()
	} else {
		f.Close()
		os.Remove(probe)
	}
	checks = append(checks, storeW)

	space := types.HealthCheck{Name: "disk_space", OK: true}
	if du, err := hostinfo.ReadDiskUsage(facts.StoreDir); err != nil {
		space.OK, space.Detail = false, err.Error()
	} else {
		floor := max(du.TotalMB*5/100, freeSpaceFloorMB)
		space.Detail = fmt.Sprintf("%d MiB free of %d MiB", du.FreeMB, du.TotalMB)
		if du.FreeMB < floor {
			space.OK = false
			space.Detail += fmt.Sprintf(" (below the %d MiB floor)", floor)
		}
	}
	checks = append(checks, space)

	cow := types.HealthCheck{Name: "store_cow", OK: true}
	if !mgr.StoreIsCoW() {
		cow.OK = false
		cow.Detail = "store does not support reflink: every VM is a full rootfs copy (provision with scripts/setup-host.sh)"
	}
	checks = append(checks, cow)

	fc := types.HealthCheck{Name: "firecracker", OK: true}
	if firecracker.Version(facts.FirecrackerBin) == "" {
		fc.OK, fc.Detail = false, "cannot run "+facts.FirecrackerBin+" --version"
	}
	checks = append(checks, fc)

	egress := types.HealthCheck{Name: "egress_policy", OK: true}
	if bad := netmgr.UnenforcedRules(); len(bad) > 0 {
		egress.OK = false
		egress.Detail = fmt.Sprintf("%d rule(s) NOT applied, their interface is not managed by this daemon: %s — "+
			"reinstall with MANAGED_IFACE=<iface> or remove them with mh network update --rm-out / --rm-in",
			len(bad), strings.Join(bad, "; "))
	}
	checks = append(checks, egress)

	return checks
}

func daemonInfo(cfg SystemConfig, facts vm.Facts, fcVersion string) types.DaemonInfo {
	return types.DaemonInfo{
		PID:                os.Getpid(),
		StartedAt:          cfg.StartedAt.Format(time.RFC3339),
		UptimeSeconds:      int64(time.Since(cfg.StartedAt).Seconds()),
		FirecrackerVersion: fcVersion,
		NetworkOverrides:   facts.NetworkOverridesOK,
		Paths: types.DaemonPaths{
			Store:       facts.StoreDir,
			Goldens:     filepath.Join(facts.StoreDir, "rootfs"),
			Kernels:     filepath.Join(facts.StoreDir, "kernels"),
			Snapshots:   filepath.Join(facts.StoreDir, "snapshots"),
			Volumes:     filepath.Join(facts.StoreDir, "volumes"),
			ChrootBase:  facts.ChrootBase,
			Database:    cfg.DBPath,
			Catalog:     cfg.CatalogPath,
			Firecracker: facts.FirecrackerBin,
			Jailer:      facts.JailerBin,
		},
	}
}

// hostReport reads the host's live resources. Individual read failures leave
// zero values rather than failing the report — a partially-informative report
// beats a 500 on the endpoint meant for diagnosing trouble.
func hostReport() types.HostInfo {
	info := types.HostInfo{
		Hostname: hostinfo.Hostname(),
		Kernel:   hostinfo.KernelVersion(),
		CPUs:     hostinfo.NumCPU(),
	}
	if load, err := hostinfo.ReadLoad(); err == nil {
		info.Load1, info.Load5, info.Load15 = load.Load1, load.Load5, load.Load15
	}
	if mem, err := hostinfo.ReadMemory(); err == nil {
		info.Memory = types.MemoryInfo{
			TotalMB:     mem.TotalMB,
			UsedMB:      mem.UsedMB,
			AvailableMB: mem.AvailableMB,
		}
	}
	return info
}

func storageReport(mgr *vm.Manager, facts vm.Facts) types.StorageInfo {
	info := types.StorageInfo{Path: facts.StoreDir, COW: mgr.StoreIsCoW()}
	if du, err := hostinfo.ReadDiskUsage(facts.StoreDir); err == nil {
		info.FSType = du.FSType
		info.TotalMB, info.UsedMB, info.FreeMB = du.TotalMB, du.UsedMB, du.FreeMB
	}
	// The store root holds the per-VM loose files (rootfs clones + console
	// logs) next to the named subdirectories — FilesSizeMB counts just those.
	info.Breakdown = []types.StorageEntry{
		{What: "vm_disks_and_logs", Path: facts.StoreDir, SizeMB: hostinfo.FilesSizeMB(facts.StoreDir)},
		{What: "goldens", Path: filepath.Join(facts.StoreDir, "rootfs"), SizeMB: hostinfo.DirSizeMB(filepath.Join(facts.StoreDir, "rootfs"))},
		{What: "kernels", Path: filepath.Join(facts.StoreDir, "kernels"), SizeMB: hostinfo.DirSizeMB(filepath.Join(facts.StoreDir, "kernels"))},
		{What: "snapshots", Path: filepath.Join(facts.StoreDir, "snapshots"), SizeMB: hostinfo.DirSizeMB(filepath.Join(facts.StoreDir, "snapshots"))},
		{What: "volumes", Path: filepath.Join(facts.StoreDir, "volumes"), SizeMB: hostinfo.DirSizeMB(filepath.Join(facts.StoreDir, "volumes"))},
		{What: "jailer_chroots", Path: facts.ChrootBase, SizeMB: hostinfo.DirSizeMB(facts.ChrootBase)},
	}
	return info
}

func fleetReport(mgr *vm.Manager, netmgr *network.Manager) types.FleetInfo {
	info := mgr.FleetStats()
	info.Networks = len(netmgr.List())
	info.ManagedIfaces = netmgr.ManagedIfaceNames()
	return info
}

// liveVMResponse builds a VM's wire DTO and, when it's running, adds the
// Firecracker process's live consumption (RSS, cumulative CPU, uptime) read
// from /proc. Best-effort: if the process vanished between listing and
// reading, the VM is returned without stats rather than failing the request.
func liveVMResponse(v *types.VM) types.VMResponse {
	resp := types.NewVMResponse(v)
	if v.State == types.VMStateRunning && v.PID > 0 {
		if s, err := hostinfo.ReadProcStats(v.PID); err == nil {
			resp.UptimeSeconds = s.UptimeSeconds
			resp.MemRSSMB = s.RSSMB
			resp.CPUSeconds = s.CPUSeconds
		}
	}
	return resp
}
