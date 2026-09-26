package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"microhosted/internal/api"
	"microhosted/internal/events"
	"microhosted/internal/images"
	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

func main() {
	def := jailer.DefaultDefaults()

	// A Unix socket by default, not a port. The daemon runs as root and its API
	// is the platform's trust anchor: whoever reaches it can create a VM, exec
	// inside it and define a network's egress policy. On a socket that
	// authorization is the file's mode and owner — something the host already
	// audits and revokes. Putting it on a port instead is an explicit operator
	// decision, not what happens when you say nothing.
	socketPath := flag.String("socket", "/run/microhosted.sock", "Unix socket the HTTP API listens on; its file permissions are the API's authorization")
	socketGroup := flag.String("socket-group", "", "group allowed to use the API socket (mode 0660); empty keeps it root-only")
	// Interfaces this daemon takes responsibility for. Declaring one hands it
	// that interface's whole policy — denied in both directions, with each
	// network's interface-scoped egress rules as the only holes — so that the
	// policy an operator declares through the API is the policy actually in
	// force. A second ruleset covering the same interface silently wins any
	// drop, which is the failure this removes.
	managedIfaceList := flag.String("managed-iface", "", "comma-separated host interfaces whose whole nftables policy this daemon owns (e.g. \"wlan0\"): denied both ways except each network's interface-scoped egress rules and its ingress rules")
	// Nothing is assumed about what the host offers that segment: the default
	// covers the usual case (the host runs its DHCP) and an empty value denies
	// everything, including DHCP.
	hostAllowList := flag.String("managed-host-allow", "udp/67", "host services reachable from a managed interface, e.g. \"udp/67,udp/123\"; empty denies every host service")
	addr := flag.String("addr", "", "serve on this TCP address INSTEAD of the socket — unauthenticated, exposes root-equivalent control of the host")
	catalogPath := flag.String("catalog", storage.DefaultCatalogPath, "path to the template catalog (JSON); must be root-owned and writable by root alone")
	instancesDir := flag.String("instances-dir", "/var/lib/microhosted/store", "disk store (btrfs CoW): clones, goldens, kernels, and the Jailer chroot. Outside the repo on purpose: it's root-owned runtime data, not sources")
	dbPath := flag.String("db", storage.DefaultDBPath, "path to the SQLite state database; must be root-owned and writable by root alone")
	chrootBase := flag.String("chroot-base", "", "Jailer chroot base directory (default <instances-dir>/jailer)")
	jailerBinary := flag.String("jailer-binary", def.JailerBinary, "path to the jailer binary")
	execFile := flag.String("exec-file", def.ExecFile, "path to the firecracker binary")
	// Every VM runs as an identity of its own from this range (uid = gid),
	// never shared with another VM or with any account on the host.
	idBase := flag.Int("jailer-id-base", def.IDBase, "first uid/gid of the range VMs and volumes get their own identity from; must not overlap any user, group or /etc/sub[ug]id range")
	idCount := flag.Int("jailer-id-count", def.IDCount, "size of the identity range: the most VMs plus volumes the daemon can hold")
	cgroupVersion := flag.String("cgroup-version", def.CgroupVersion, "cgroup version Jailer uses (auto-detected; only force it if needed)")
	// Admission control (docs/roadmap.md Phase 0b): judged against what the
	// host has now, since guest memory is allocated lazily.
	memReserve := flag.Int64("mem-reserve-mb", vm.DefaultMemReserveMB, "refuse a VM launch that would leave less than this much host memory available (MB)")
	var quotaFlags []string
	flag.Func("quota", "cap what one consumer (VMs labelled managed-by=CONSUMER) may run at once: `CONSUMER=vms:N,mem:MB` (repeatable; either part optional). Over it a launch is refused with 429", func(s string) error {
		quotaFlags = append(quotaFlags, s)
		return nil
	})
	defaultQuota := flag.String("quota-default", "", "quota for every labelled consumer without its own --quota: `vms:N,mem:MB` (default: none)")
	diskReserve := flag.Int64("disk-reserve-mb", vm.DefaultDiskReserveMB, "store space kept free (MB): VM launches, snapshots, volumes and image imports that would leave less are refused, and host.disk_low is raised while the store is under it")
	maxVMs := flag.Int("max-vms", 0, "cap on running VMs plus launches in progress (0 = no cap)")
	maxBoots := flag.Int("max-parallel-boots", vm.DefaultMaxParallelBoots, "launches (create/fork/start) allowed to run at once; the rest wait")
	diskMiBs := flag.Int64("vm-disk-mib-s", vm.DefaultDiskMiBs, "ceiling on each VM drive's throughput (MiB/s); a VM may ask for less, never more; 0 lifts it")
	diskIOPS := flag.Int64("vm-disk-iops", vm.DefaultDiskIOPS, "ceiling on each VM drive's operations per second; 0 lifts it")
	netMbit := flag.Int64("vm-net-mbit", vm.DefaultNetMbit, "ceiling on each VM's network throughput, per direction (Mbit/s); 0 lifts it")
	flag.Parse()

	// The Jailer chroot MUST live on the same filesystem as the rootfs clones:
	// Jailer hardlinks the rootfs (and the kernel) into the chroot, and a
	// hardlink doesn't cross devices (EXDEV). With the CoW store the clones are
	// on a separate btrfs, so the chroot derives by default from --instances-dir
	// (ends up as <instances-dir>/jailer, same FS) instead of the historical
	// /srv/jailer, which would be on another filesystem and break the boot. The
	// catalog's kernel must be on that same FS for the same reason (see
	// scripts/setup-host.sh, which places it in <instances-dir>/kernels).
	// We resolve to absolute paths so the hardlinks don't depend on the cwd.
	absInstances, err := filepath.Abs(*instancesDir)
	if err != nil {
		log.Fatalf("resolving instances-dir %s: %v", *instancesDir, err)
	}
	*instancesDir = absInstances
	if *chrootBase == "" {
		*chrootBase = filepath.Join(absInstances, "jailer")
	}
	// 0711: Jailer (root) builds each chroot under it and Firecracker only
	// ever sees the inside of its own; nobody needs to list it.
	if err := os.MkdirAll(*chrootBase, storage.StoreDirMode); err != nil {
		log.Fatalf("creating chroot base %s: %v", *chrootBase, err)
	}
	if err := os.Chmod(*chrootBase, storage.StoreDirMode); err != nil {
		log.Fatalf("restricting chroot base %s: %v", *chrootBase, err)
	}

	// The database and the catalog name the paths the daemon truncates, clones,
	// deletes and boots as root: if anyone else can write them, or a directory
	// above them, they can aim those operations at any file on the host. Refuse
	// to start rather than trust them (see storage.CheckRootOnly). Only as root:
	// an unprivileged run cannot hurt anything the files point at.
	if os.Geteuid() == 0 {
		for _, p := range []struct{ flag, path string }{{"--db", *dbPath}, {"--catalog", *catalogPath}} {
			if err := storage.CheckRootOnly(p.path); err != nil {
				log.Fatalf("%s %s is not safe to trust: %v — keep it under %s (scripts/install-service.sh migrates it)", p.flag, p.path, err, storage.StateDir)
			}
		}
	}

	catalog, err := storage.LoadCatalog(*catalogPath)
	if err != nil {
		log.Fatalf("loading catalog: %v", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening state database: %v", err)
	}
	defer st.Close()
	// Records hold network policy, labels and paths: nobody but the daemon
	// reads them.
	if err := storage.RestrictDB(*dbPath); err != nil {
		log.Fatalf("restricting state database %s: %v", *dbPath, err)
	}

	jcfg := jailer.Defaults{
		IDBase:        *idBase,
		IDCount:       *idCount,
		ChrootBaseDir: *chrootBase,
		JailerBinary:  *jailerBinary,
		ExecFile:      *execFile,
		CgroupVersion: *cgroupVersion,
	}
	// Refuse to start rather than hand a VM an id some account, group or
	// rootless-container range already uses: that account would own the VM's
	// disk.
	if err := jcfg.ValidateIDRange(); err != nil {
		log.Fatalf("--jailer-id-base: %v", err)
	}

	// Networks come up before VMs: recreate bridges wiped by a host reboot,
	// ensure the default network exists, and install the nftables ruleset —
	// so VMs adopted just below can re-reserve their IPs on live networks.
	managed, err := parseManagedIfaces(*managedIfaceList, *hostAllowList)
	if err != nil {
		log.Fatalf("--managed-iface: %v", err)
	}
	for _, mi := range managed {
		log.Printf("managing the nftables policy of %s (host services allowed: %s)", mi.Name, describeHostAllow(mi.HostAllow))
	}
	// The event bus comes first so that what startup finds (a ruleset that
	// fails, VMs that died while the daemon was down) is published too.
	bus := events.NewBus(events.DefaultCapacity)
	netmgr := network.NewManager(st, managed)
	netmgr.SetEvents(bus)
	if err := netmgr.Reconcile(); err != nil {
		log.Fatalf("reconciling networks: %v", err)
	}

	// Warn loudly, once, if the instances store can't do copy-on-write clones.
	// On such a filesystem (plain ext4) every VM is a FULL copy of its rootfs,
	// so a handful of 1GB VMs fills the disk fast — provision a CoW store with
	// scripts/setup-host.sh. Just a warning, not fatal: full-copy clones still
	// work, they just don't scale.
	if err := os.MkdirAll(*instancesDir, storage.StoreDirMode); err != nil {
		log.Fatalf("creating instances directory %s: %v", *instancesDir, err)
	}
	// Stores created before the lockdown held world-readable disks and logs:
	// bring every existing file to the modes new ones get. A file that can't
	// be fixed is reported, not fatal — the daemon still owns the VMs that
	// keep running, and refusing to start would not make that file private.
	if err := storage.HardenStore(*instancesDir); err != nil {
		log.Printf("WARNING: some store files could not be made private: %v", err)
	}
	if !storage.SupportsReflink(*instancesDir) {
		log.Printf("WARNING: the instances store %s does NOT support copy-on-write (reflink): "+
			"every VM will be a FULL COPY of its rootfs and the disk will fill up fast. "+
			"Provision a CoW store with scripts/setup-host.sh.", *instancesDir)
	}

	mgr := vm.NewManager(catalog, jcfg, *instancesDir, st, netmgr)
	mgr.SetEvents(bus)
	if *diskMiBs < 0 || *diskIOPS < 0 || *netMbit < 0 {
		log.Fatalf("--vm-disk-mib-s, --vm-disk-iops and --vm-net-mbit must not be negative (0 lifts the limit)")
	}
	io := types.IOLimits{DiskMiBs: *diskMiBs, DiskIOPS: *diskIOPS, NetMbit: *netMbit}
	if io.DiskMiBs == 0 || io.DiskIOPS == 0 || io.NetMbit == 0 {
		log.Printf("WARNING: a VM throughput limit is lifted (disk %d MiB/s, %d IOPS, net %d Mbit/s; 0 = none): "+
			"a compromised VM can saturate the storage or its network and degrade every other VM", io.DiskMiBs, io.DiskIOPS, io.NetMbit)
	}
	quotas := make(map[string]vm.Quota, len(quotaFlags))
	for _, s := range quotaFlags {
		consumer, q, err := vm.ParseConsumerQuota(s)
		if err != nil {
			log.Fatalf("--quota: %v", err)
		}
		if _, dup := quotas[consumer]; dup {
			log.Fatalf("--quota: %s given twice", consumer)
		}
		quotas[consumer] = q
	}
	var defQuota *vm.Quota
	if *defaultQuota != "" {
		q, err := vm.ParseQuota(*defaultQuota)
		if err != nil {
			log.Fatalf("--quota-default: %v", err)
		}
		defQuota = &q
	}
	mgr.SetLimits(vm.Limits{MemReserveMB: *memReserve, MaxVMs: *maxVMs, MaxParallelBoots: *maxBoots, DiskReserveMB: *diskReserve,
		Quotas: quotas, DefaultQuota: defQuota, IO: io})

	// The content-addressed image store lives in the instances dir: clones
	// reflink from it and the kernel is hard-linked from it into each jail.
	// A store that cannot open disables creates by image, not the daemon:
	// VMs already created from an image keep their kernel and disk paths.
	if imgs, err := images.Open(*instancesDir, st); err != nil {
		log.Printf("WARNING: image store unavailable, creates by image are refused: %v", err)
	} else {
		imgs.SetReserve(mgr.ReserveCopy)
		mgr.SetImages(imgs)
	}

	// Volumes load before Reconcile: sweeping a dead VM releases its volumes, so
	// the volume index must already be populated when Reconcile runs.
	vols, err := st.ListVolumes()
	if err != nil {
		log.Fatalf("loading persisted volumes: %v", err)
	}
	mgr.LoadVolumes(vols)

	// Recover state from a previous run before serving: adopt VMs still
	// running, keep those that died while we were down (a host reboot) as
	// stopped. This also tells us which tap devices are live so the orphan
	// sweep below doesn't tear down a healthy adopted VM's networking, and
	// which dead VMs asked to be booted again.
	records, err := st.ListVMs()
	if err != nil {
		log.Fatalf("loading persisted state: %v", err)
	}
	keepTaps, autostart := mgr.Reconcile(records)
	// Every lease is known now: pin each VM's TAP to its addresses. Not fatal —
	// VMs already running keep the filter the previous run installed — but no
	// TAP joins a bridge until it is in force (network.Manager.RequirePorts).
	if err := netmgr.ApplyPorts(); err != nil {
		log.Printf("WARNING: port filter (anti-spoofing) not installed; no VM can join a network until it is: %v", err)
	}

	// Snapshots are inert (files + record, no liveness): just re-index them,
	// dropping any whose files were removed out-of-band.
	snaps, err := st.ListSnapshots()
	if err != nil {
		log.Fatalf("loading persisted snapshots: %v", err)
	}
	mgr.LoadSnapshots(snaps)

	// Remove tap devices left by an uncleanly-terminated previous run, except
	// those belonging to VMs we just adopted — see network.SweepOrphans.
	if err := network.SweepOrphans(keepTaps); err != nil {
		log.Fatalf("cleaning up orphaned tap devices: %v", err)
	}
	// And the rest of what a previous run can leave when it dies mid-operation:
	// Firecracker processes nobody adopted, jail dirs and cgroups of VMs that
	// are not running, claims on volumes whose VM is gone.
	mgr.SweepResidue()
	// Pre-grown copies of goldens whose template was removed from the catalog,
	// or whose golden was rebuilt or deleted since the last run.
	mgr.PruneSizedGoldens()

	// From here on, a VM that dies on its own is noticed within seconds and
	// marked stopped, instead of claiming to run until the next restart.
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	go mgr.Monitor(monitorCtx, 2*time.Second)

	// Boot the autostart VMs only now: the orphan sweep above would delete the
	// TAPs Start creates. In the background so the API serves meanwhile — a
	// fleet boots one VM at a time, and each shows as stopped until its turn.
	go mgr.AutostartVMs(autostart)

	// The observability report shows these paths to an operator working
	// anywhere on the host — absolute, so they don't depend on the daemon's
	// cwd. Best-effort: on failure the flag value is shown as given.
	absOr := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	srv := api.NewServer(mgr, netmgr, bus, api.SystemConfig{
		DBPath:      absOr(*dbPath),
		CatalogPath: absOr(*catalogPath),
		StartedAt:   time.Now(),
	})

	ln, listeningOn, err := apiListener(*addr, *socketPath, *socketGroup)
	if err != nil {
		log.Fatalf("opening the API listener: %v", err)
	}

	go func() {
		log.Printf("microhosted listening on %s", listeningOn)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("error shutting down the server: %v", err)
	}
}

// apiListener opens the socket the API serves on: the Unix socket by default,
// or a TCP address when the operator asked for one. The TCP path warns every
// time rather than once in the docs — there is no authentication in front of
// this API, so the only thing standing between a reachable port and root on
// this host is whoever is reading the journal.
func apiListener(addr, socketPath, socketGroup string) (net.Listener, string, error) {
	if addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", err
		}
		log.Printf("WARNING: serving the API on tcp %s with no authentication — anyone who can reach it has root-equivalent control of this host", addr)
		return ln, "tcp " + addr, nil
	}
	ln, err := api.ListenUnix(socketPath, socketGroup)
	if err != nil {
		return nil, "", err
	}
	return ln, "unix " + socketPath, nil
}

// parseManagedIfaces builds the managed-interface list from the two flags. A
// bad name fails the start instead of being skipped: an operator who declared
// an interface and got silence would believe it is governed while nothing is
// rendered for it, which is the exact failure this feature exists to remove.
func parseManagedIfaces(ifaces, hostAllow string) ([]network.ManagedIface, error) {
	if strings.TrimSpace(ifaces) == "" {
		return nil, nil
	}
	services, err := parseHostServices(hostAllow)
	if err != nil {
		return nil, err
	}
	var out []network.ManagedIface
	for _, raw := range strings.Split(ifaces, ",") {
		name := strings.TrimSpace(raw)
		if err := network.ValidateIfaceName(name); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(out, func(m network.ManagedIface) bool { return m.Name == name }) {
			return nil, fmt.Errorf("interface %q listed twice", name)
		}
		out = append(out, network.ManagedIface{Name: name, HostAllow: services})
	}
	return out, nil
}

// parseHostServices reads "udp/67,tcp/8883". Empty means no host service at all
// is reachable from a managed interface — a legitimate choice when something
// else addresses that segment.
func parseHostServices(list string) ([]network.HostService, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var out []network.HostService
	for _, raw := range strings.Split(list, ",") {
		spec := strings.TrimSpace(raw)
		proto, portStr, ok := strings.Cut(spec, "/")
		if !ok {
			return nil, fmt.Errorf("host service %q must look like udp/67 or tcp/8883", spec)
		}
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("host service %q: protocol must be tcp or udp", spec)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("host service %q: port must be 1-65535", spec)
		}
		out = append(out, network.HostService{Protocol: proto, Port: port})
	}
	return out, nil
}

// describeHostAllow renders the allowed host services for the startup log, so
// "nothing" is stated rather than shown as an empty space an operator has to
// interpret.
func describeHostAllow(services []network.HostService) string {
	if len(services) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(services))
	for _, s := range services {
		parts = append(parts, fmt.Sprintf("%s/%d", s.Protocol, s.Port))
	}
	return strings.Join(parts, ", ")
}
