package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"microhosted/internal/api"
	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/internal/vm"
)

func main() {
	def := jailer.DefaultDefaults()

	addr := flag.String("addr", ":8080", "address the HTTP API listens on")
	catalogPath := flag.String("catalog", "images/catalog.json", "path to the template catalog (JSON)")
	instancesDir := flag.String("instances-dir", "/var/lib/microhosted/store", "disk store (btrfs CoW): clones, goldens, kernels, and the Jailer chroot. Outside the repo on purpose: it's root-owned runtime data, not sources")
	dbPath := flag.String("db", "images/microhosted.db", "path to the SQLite state database")
	chrootBase := flag.String("chroot-base", "", "Jailer chroot base directory (default <instances-dir>/jailer)")
	jailerBinary := flag.String("jailer-binary", def.JailerBinary, "path to the jailer binary")
	execFile := flag.String("exec-file", def.ExecFile, "path to the firecracker binary")
	uid := flag.Int("jailer-uid", def.UID, "uid Jailer runs firecracker as")
	gid := flag.Int("jailer-gid", def.GID, "gid Jailer runs firecracker as")
	cgroupVersion := flag.String("cgroup-version", def.CgroupVersion, "cgroup version Jailer uses (auto-detected; only force it if needed)")
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
	if err := os.MkdirAll(*chrootBase, 0o755); err != nil {
		log.Fatalf("creating chroot base %s: %v", *chrootBase, err)
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

	jcfg := jailer.Defaults{
		UID:           *uid,
		GID:           *gid,
		ChrootBaseDir: *chrootBase,
		JailerBinary:  *jailerBinary,
		ExecFile:      *execFile,
		CgroupVersion: *cgroupVersion,
	}

	// Networks come up before VMs: recreate bridges wiped by a host reboot,
	// ensure the default network exists, and install the nftables ruleset —
	// so VMs adopted just below can re-reserve their IPs on live networks.
	netmgr := network.NewManager(st)
	if err := netmgr.Reconcile(); err != nil {
		log.Fatalf("reconciling networks: %v", err)
	}

	// Warn loudly, once, if the instances store can't do copy-on-write clones.
	// On such a filesystem (plain ext4) every VM is a FULL copy of its rootfs,
	// so a handful of 1GB VMs fills the disk fast — provision a CoW store with
	// scripts/setup-host.sh. Just a warning, not fatal: full-copy clones still
	// work, they just don't scale.
	if err := os.MkdirAll(*instancesDir, 0o755); err != nil {
		log.Fatalf("creating instances directory %s: %v", *instancesDir, err)
	}
	if !storage.SupportsReflink(*instancesDir) {
		log.Printf("WARNING: the instances store %s does NOT support copy-on-write (reflink): "+
			"every VM will be a FULL COPY of its rootfs and the disk will fill up fast. "+
			"Provision a CoW store with scripts/setup-host.sh.", *instancesDir)
	}

	mgr := vm.NewManager(catalog, jcfg, *instancesDir, st, netmgr)

	// Volumes load before Reconcile: sweeping a dead VM releases its volumes, so
	// the volume index must already be populated when Reconcile runs.
	vols, err := st.ListVolumes()
	if err != nil {
		log.Fatalf("loading persisted volumes: %v", err)
	}
	mgr.LoadVolumes(vols)

	// Recover state from a previous run before serving: adopt VMs still
	// running, sweep those that died while we were down. This also tells us
	// which tap devices are live so the orphan sweep below doesn't tear down a
	// healthy adopted VM's networking.
	records, err := st.ListVMs()
	if err != nil {
		log.Fatalf("loading persisted state: %v", err)
	}
	keepTaps := mgr.Reconcile(records)

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

	// The observability report shows these paths to an operator working
	// anywhere on the host — absolute, so they don't depend on the daemon's
	// cwd. Best-effort: on failure the flag value is shown as given.
	absOr := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	srv := api.NewServer(mgr, netmgr, *addr, api.SystemConfig{
		DBPath:      absOr(*dbPath),
		CatalogPath: absOr(*catalogPath),
		StartedAt:   time.Now(),
	})

	go func() {
		log.Printf("microhosted listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
