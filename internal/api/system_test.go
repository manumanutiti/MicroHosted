package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"microhosted/internal/jailer"
	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// newTestServer wires a real manager (temp store, temp catalog, no root, no
// KVM use) behind the real mux — enough to exercise the observability
// endpoints end-to-end without hardware.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()

	catalogPath := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(`[{"name":"t1","kernel_path":"/k","rootfs_path":"/r","vcpus":1,"mem_mb":128}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := storage.LoadCatalog(catalogPath)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	jcfg := jailer.DefaultDefaults()
	jcfg.ChrootBaseDir = filepath.Join(dir, "jailer")
	jcfg.ExecFile = filepath.Join(dir, "missing-firecracker") // version probe fails: that's fine, the report must still work

	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := vm.NewManager(catalog, jcfg, storeDir, st, network.NewManager(st, nil))

	srv := NewServer(mgr, network.NewManager(st, nil), SystemConfig{
		DBPath:      filepath.Join(dir, "state.db"),
		CatalogPath: catalogPath,
		StartedAt:   time.Now().Add(-3 * time.Second),
	})
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestSystemEndpoint(t *testing.T) {
	ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/v1/system")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// /v1/system always answers 200 — health lives in the body; the 503
	// behavior belongs to /v1/health.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/system = %d, want 200", res.StatusCode)
	}

	var sys types.SystemResponse
	if err := json.NewDecoder(res.Body).Decode(&sys); err != nil {
		t.Fatalf("decoding system response: %v", err)
	}

	// The firecracker binary doesn't exist in this environment, so the
	// platform must self-report as degraded — not lie, not 500.
	if sys.Status != "degraded" {
		t.Errorf("status = %q, want degraded (missing firecracker binary)", sys.Status)
	}
	if len(sys.Checks) == 0 {
		t.Error("no health checks in report")
	}
	if sys.Daemon.PID != os.Getpid() {
		t.Errorf("daemon.pid = %d, want %d", sys.Daemon.PID, os.Getpid())
	}
	if sys.Daemon.UptimeSeconds < 3 {
		t.Errorf("daemon.uptime_seconds = %d, want >= 3", sys.Daemon.UptimeSeconds)
	}
	if sys.Daemon.Paths.Store == "" || sys.Daemon.Paths.Database == "" || sys.Daemon.Paths.Catalog == "" {
		t.Errorf("daemon.paths incomplete: %+v", sys.Daemon.Paths)
	}
	if sys.Host.CPUs <= 0 {
		t.Errorf("host.cpus = %d, want > 0", sys.Host.CPUs)
	}
	if sys.Host.Memory.TotalMB <= 0 {
		t.Errorf("host.memory.total_mb = %d, want > 0", sys.Host.Memory.TotalMB)
	}
	if sys.Storage.TotalMB <= 0 || sys.Storage.FSType == "" {
		t.Errorf("storage totals missing: %+v", sys.Storage)
	}
	if len(sys.Storage.Breakdown) == 0 {
		t.Error("storage.breakdown empty")
	}
	if sys.Fleet.Templates != 1 {
		t.Errorf("fleet.templates = %d, want 1", sys.Fleet.Templates)
	}
	if sys.Fleet.VMs.Total != 0 {
		t.Errorf("fleet.vms.total = %d, want 0", sys.Fleet.VMs.Total)
	}
}

func TestHealthEndpointDegradedIs503(t *testing.T) {
	ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Missing firecracker binary → degraded → 503, so a monitor needs only
	// the status code.
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/health = %d, want 503", res.StatusCode)
	}

	var h types.HealthResponse
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "degraded" {
		t.Errorf("status = %q, want degraded", h.Status)
	}
	seen := map[string]bool{}
	for _, c := range h.Checks {
		seen[c.Name] = true
	}
	for _, want := range []string{"kvm", "database", "store_writable", "disk_space", "store_cow", "firecracker"} {
		if !seen[want] {
			t.Errorf("check %q missing from %v", want, h.Checks)
		}
	}
}
