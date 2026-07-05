// Package hostinfo reads host- and process-level metrics for the
// observability endpoints (/v1/system, /v1/health, per-VM stats): memory and
// load from /proc, filesystem usage via statfs, and per-PID RSS/CPU from
// /proc/<pid>. Everything is a point-in-time read with no daemons, no
// sampling loops and no external dependencies — cheap enough to run on every
// request. Linux-only, like the rest of the platform (Firecracker requires
// KVM anyway).
package hostinfo

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Memory is the host's RAM picture in MiB, from /proc/meminfo. Used is
// derived as Total-Available (what `free` calls "used" minus reclaimable
// cache), which is the number an operator actually wants when deciding
// whether another VM fits.
type Memory struct {
	TotalMB     int64
	AvailableMB int64
	UsedMB      int64
}

// ReadMemory reads the host's memory usage from /proc/meminfo.
func ReadMemory() (Memory, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	return parseMeminfo(string(data))
}

func parseMeminfo(data string) (Memory, error) {
	var totalKB, availKB int64
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseInt(fields[1], 10, 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if totalKB == 0 {
		return Memory{}, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	return Memory{
		TotalMB:     totalKB / 1024,
		AvailableMB: availKB / 1024,
		UsedMB:      (totalKB - availKB) / 1024,
	}, nil
}

// Load is the host's load averages from /proc/loadavg.
type Load struct {
	Load1  float64
	Load5  float64
	Load15 float64
}

// ReadLoad reads the host's 1/5/15-minute load averages.
func ReadLoad() (Load, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return Load{}, err
	}
	return parseLoadavg(string(data))
}

func parseLoadavg(data string) (Load, error) {
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("unexpected /proc/loadavg content: %q", data)
	}
	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err2 := strconv.ParseFloat(fields[1], 64)
	l15, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return Load{}, fmt.Errorf("unexpected /proc/loadavg content: %q", data)
	}
	return Load{Load1: l1, Load5: l5, Load15: l15}, nil
}

// NumCPU is the number of logical CPUs available to the daemon — the
// denominator for judging load averages and vCPU overcommit.
func NumCPU() int { return runtime.NumCPU() }

// KernelVersion returns the host kernel release (e.g. "6.17.0-35-generic"),
// or "" if unreadable.
func KernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Hostname returns the host's name, or "" if unavailable.
func Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// DiskUsage is the capacity picture of the filesystem holding path (the whole
// filesystem, not the directory): what statfs reports. FSType is a human name
// decoded from the filesystem magic — the store being "btrfs" vs "ext4" is
// exactly the CoW question, so it's surfaced rather than left as a hex code.
type DiskUsage struct {
	TotalMB int64
	FreeMB  int64
	UsedMB  int64
	FSType  string
}

// Statfs magics for the filesystems a store plausibly sits on. From
// linux/magic.h; anything else is reported as the raw hex value.
var fsTypeNames = map[int64]string{
	0x9123683e: "btrfs",
	0xef53:     "ext4",
	0x58465342: "xfs",
	0x01021994: "tmpfs",
	0x2fc12fc1: "zfs",
	0x794c7630: "overlayfs",
	0x6969:     "nfs",
}

// ReadDiskUsage reports the usage of the filesystem containing path.
func ReadDiskUsage(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize / (1 << 20)
	// Bavail (space usable by non-root), not Bfree: it's what a new clone or
	// volume can actually consume.
	free := int64(st.Bavail) * bsize / (1 << 20)
	fsType, ok := fsTypeNames[int64(st.Type)]
	if !ok {
		fsType = fmt.Sprintf("0x%x", st.Type)
	}
	return DiskUsage{TotalMB: total, FreeMB: free, UsedMB: total - free, FSType: fsType}, nil
}

// DirSizeMB walks dir summing the ALLOCATED size of every regular file
// (st_blocks, what du reports) — not the apparent size, which on the sparse
// files this store is full of (truncated volumes, snapshot mem files) can be
// wildly larger than the disk they occupy. Best-effort: unreadable entries
// are skipped, a missing dir is 0. Caveat, documented in docs/api.md: on a
// CoW store, extents shared by reflink are counted once per file, so the
// per-directory numbers can add up to more than the filesystem's real usage
// (`btrfs filesystem du` is authoritative there).
func DirSizeMB(dir string) int64 {
	var bytes int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable entries, keep walking
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			bytes += st.Blocks * 512
		}
		return nil
	})
	return bytes / (1 << 20)
}

// FilesSizeMB sums the allocated size of the regular files directly inside
// dir (non-recursive) — the store's root holds the per-VM artifacts (rootfs
// clones and console logs) as loose files next to the named subdirectories.
func FilesSizeMB(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var bytes int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			bytes += st.Blocks * 512
		}
	}
	return bytes / (1 << 20)
}

// ProcStats is a live process's resource consumption: resident memory,
// cumulative CPU time and how long it has been running. For a VM this is the
// Firecracker process, so RSS is what the guest actually costs the host right
// now (guest RAM is faulted in on demand — typically well under mem_mb), and
// UptimeSeconds is time since boot/restore, not since the record was created.
type ProcStats struct {
	RSSMB         int64
	CPUSeconds    float64
	UptimeSeconds int64
}

// Kernel USER_HZ. Fixed at 100 on Linux for userspace-visible values
// regardless of the scheduler's internal HZ.
const clockTicks = 100

// ReadProcStats reads a process's stats from /proc/<pid>. An error (process
// gone, permission) just means "no stats", the caller shows the VM without
// them.
func ReadProcStats(pid int) (ProcStats, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcStats{}, err
	}
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ProcStats{}, err
	}
	fields := strings.Fields(string(uptimeData))
	if len(fields) < 1 {
		return ProcStats{}, fmt.Errorf("unexpected /proc/uptime content")
	}
	hostUptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return ProcStats{}, err
	}
	stats, err := parseProcStat(string(stat), hostUptime)
	if err != nil {
		return ProcStats{}, err
	}

	// RSS from statm (in pages). The stat file has an rss field too, but statm
	// is the conventional source and avoids counting past field 24.
	statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return ProcStats{}, err
	}
	mfields := strings.Fields(string(statm))
	if len(mfields) >= 2 {
		pages, _ := strconv.ParseInt(mfields[1], 10, 64)
		stats.RSSMB = pages * int64(os.Getpagesize()) / (1 << 20)
	}
	return stats, nil
}

// parseProcStat extracts utime+stime and starttime from a /proc/<pid>/stat
// line. The comm field (2) can contain spaces and parentheses, so parsing
// anchors on the LAST ')' — everything after it is whitespace-separated
// fields starting at field 3 (state).
func parseProcStat(stat string, hostUptimeSeconds float64) (ProcStats, error) {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return ProcStats{}, fmt.Errorf("malformed stat line")
	}
	rest := strings.Fields(stat[closeParen+1:])
	// rest[0] is field 3 (state); utime=field 14, stime=15, starttime=22.
	if len(rest) < 20 {
		return ProcStats{}, fmt.Errorf("malformed stat line: %d fields after comm", len(rest))
	}
	utime, _ := strconv.ParseInt(rest[11], 10, 64)
	stime, _ := strconv.ParseInt(rest[12], 10, 64)
	starttime, _ := strconv.ParseInt(rest[19], 10, 64)

	uptime := int64(hostUptimeSeconds) - starttime/clockTicks
	if uptime < 0 {
		uptime = 0
	}
	return ProcStats{
		CPUSeconds:    float64(utime+stime) / clockTicks,
		UptimeSeconds: uptime,
	}, nil
}
