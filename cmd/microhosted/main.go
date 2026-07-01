package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	addr := flag.String("addr", ":8080", "dirección donde escucha la API HTTP")
	catalogPath := flag.String("catalog", "images/catalog.json", "ruta al catálogo de plantillas (JSON)")
	instancesDir := flag.String("instances-dir", "images/instances", "directorio donde se clonan los rootfs por VM")
	dbPath := flag.String("db", "images/microhosted.db", "ruta a la base de datos SQLite de estado")
	chrootBase := flag.String("chroot-base", def.ChrootBaseDir, "directorio base de chroot de Jailer")
	jailerBinary := flag.String("jailer-binary", def.JailerBinary, "ruta al binario jailer")
	execFile := flag.String("exec-file", def.ExecFile, "ruta al binario firecracker")
	uid := flag.Int("jailer-uid", def.UID, "uid con el que Jailer ejecuta firecracker")
	gid := flag.Int("jailer-gid", def.GID, "gid con el que Jailer ejecuta firecracker")
	cgroupVersion := flag.String("cgroup-version", def.CgroupVersion, "versión de cgroup que usa Jailer (autodetectada; solo forzarla si hace falta)")
	flag.Parse()

	catalog, err := storage.LoadCatalog(*catalogPath)
	if err != nil {
		log.Fatalf("cargando catálogo: %v", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("abriendo base de datos de estado: %v", err)
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
		log.Fatalf("reconciliando redes: %v", err)
	}

	mgr := vm.NewManager(catalog, jcfg, *instancesDir, st, netmgr)

	// Recover state from a previous run before serving: adopt VMs still
	// running, sweep those that died while we were down. This also tells us
	// which tap devices are live so the orphan sweep below doesn't tear down a
	// healthy adopted VM's networking.
	records, err := st.ListVMs()
	if err != nil {
		log.Fatalf("cargando estado persistido: %v", err)
	}
	keepTaps := mgr.Reconcile(records)

	// Remove tap devices left by an uncleanly-terminated previous run, except
	// those belonging to VMs we just adopted — see network.SweepOrphans.
	if err := network.SweepOrphans(keepTaps); err != nil {
		log.Fatalf("limpiando tap devices huérfanos: %v", err)
	}

	srv := api.NewServer(mgr, netmgr, *addr)

	go func() {
		log.Printf("microhosted escuchando en %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("servidor HTTP: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("apagando...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("error al apagar el servidor: %v", err)
	}
}
