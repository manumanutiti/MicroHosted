package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"microhosted/pkg/types"
)

// fakeAPI serves a scripted subset of the daemon's API on a Unix socket and
// records every request, so each test can assert both what mh sent and what
// it printed — the whole client path, socket dialing included.
type fakeAPI struct {
	t      *testing.T
	socket string
	mu     sync.Mutex
	reqs   []recorded
}

type recorded struct {
	method, path, query string
	body                []byte
}

func newFakeAPI(t *testing.T, mux *http.ServeMux) *fakeAPI {
	t.Helper()
	// Short dir: a Unix socket path is capped at ~108 bytes and t.TempDir()
	// can exceed that.
	dir, err := os.MkdirTemp("", "mh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeAPI{t: t, socket: filepath.Join(dir, "api.sock")}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, body})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

// run executes mh against the fake API and returns exit code, stdout, stderr.
func (f *fakeAPI) run(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append([]string{"-H", f.socket}, args...), strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// last returns the last recorded request with the given method.
func (f *fakeAPI) last(method string) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method == method {
			return f.reqs[i]
		}
	}
	f.t.Fatalf("no %s request was made; got %+v", method, f.reqs)
	return recorded{}
}

func reply(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
}

func replyStatus(code int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
}

var twoVMs = []types.VMResponse{
	{ID: "a1b2c3d4", Template: "base-alpine", State: types.VMStateRunning, Network: "lab", GuestIP: "10.0.0.2", VCPUs: 1, MemMB: 128, CreatedAt: "2026-09-18T10:00:00Z"},
	{ID: "a1ffffff", Template: "base-alpine", State: types.VMStateStopped, Network: "default", GuestIP: "172.16.0.2", VCPUs: 1, MemMB: 128, CreatedAt: "2026-09-18T09:00:00Z"},
}

func TestCreateVMBuildsRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms", replyStatus(http.StatusCreated, types.VMResponse{ID: "deadbeef", Template: "base-alpine", Network: "lab", GuestIP: "10.0.0.9"}))
	f := newFakeAPI(t, mux)

	// Verb-first spelling, flags after the positional, short and long flags,
	// size suffixes: all the ways a person actually types it.
	code, out, errOut := f.run("", "create", "vm", "base-alpine", "--net", "lab", "-c", "2", "-m", "1G", "--disk", "2g",
		"-v", "sample:/mnt/s:ro", "-v", "output", "--autostart")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != "deadbeef\n" {
		t.Errorf("stdout = %q, want the bare ID so $(mh run ...) captures it", out)
	}
	if !strings.Contains(errOut, "10.0.0.9") {
		t.Errorf("stderr %q should mention the new VM's IP", errOut)
	}
	var got types.CreateVMRequest
	if err := json.Unmarshal(f.last("POST").body, &got); err != nil {
		t.Fatal(err)
	}
	want := types.CreateVMRequest{
		Template: "base-alpine", Network: "lab", VCPUs: 2, MemMB: 1024, DiskMB: 2048,
		Volumes:   []types.VolumeAttachRequest{{Name: "sample", GuestPath: "/mnt/s", ReadOnly: true}, {Name: "output"}},
		Autostart: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request = %+v\nwant      %+v", got, want)
	}
}

func TestPsHidesStoppedUnlessAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(twoVMs))
	f := newFakeAPI(t, mux)

	_, out, _ := f.run("", "ps", "-q")
	if out != "a1b2c3d4\n" {
		t.Errorf("mh ps -q = %q, want only the running VM", out)
	}
	_, out, _ = f.run("", "list", "vm", "-a", "-q")
	if out != "a1b2c3d4\na1ffffff\n" {
		t.Errorf("mh list vm -a -q = %q, want both, newest first", out)
	}
	_, out, _ = f.run("", "vm", "ls", "-a")
	if !strings.Contains(out, "VM ID") || !strings.Contains(out, "172.16.0.2") {
		t.Errorf("table output missing header or rows:\n%s", out)
	}
}

func TestVMUpdateAutostart(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(twoVMs))
	mux.HandleFunc("PUT /v1/vms/{id}/autostart", reply(twoVMs[0]))
	f := newFakeAPI(t, mux)

	for _, tc := range []struct {
		flag string
		want bool
	}{{"--autostart", true}, {"--no-autostart", false}} {
		code, out, errOut := f.run("", "vm", "update", "a1b", tc.flag)
		if code != 0 || out != "a1b2c3d4\n" {
			t.Fatalf("%s: exit %d, out %q, stderr %q", tc.flag, code, out, errOut)
		}
		req := f.last("PUT")
		var got types.UpdateVMAutostartRequest
		if err := json.Unmarshal(req.body, &got); err != nil {
			t.Fatal(err)
		}
		if req.path != "/v1/vms/a1b2c3d4/autostart" || got.Autostart != tc.want {
			t.Errorf("%s: PUT %s %+v, want autostart=%v", tc.flag, req.path, got, tc.want)
		}
	}

	if code, _, _ := f.run("", "vm", "update", "a1b"); code == 0 {
		t.Error("vm update with no flag must refuse")
	}
	if code, _, _ := f.run("", "vm", "update", "a1b", "--autostart", "--no-autostart"); code == 0 {
		t.Error("--autostart with --no-autostart must refuse")
	}
}

func TestVMReferenceResolution(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(twoVMs))
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	f := newFakeAPI(t, mux)

	code, out, _ := f.run("", "rm", "a1b")
	if code != 0 || out != "a1b2c3d4\n" || f.last("DELETE").path != "/v1/vms/a1b2c3d4" {
		t.Errorf("unique prefix: exit %d, out %q, last DELETE %s", code, out, f.last("DELETE").path)
	}
	code, _, errOut := f.run("", "rm", "a1")
	if code == 0 || !strings.Contains(errOut, "matches several") {
		t.Errorf("ambiguous prefix must refuse: exit %d, stderr %q", code, errOut)
	}
}

func TestNetworkUpdateMergesRules(t *testing.T) {
	cur := types.NetworkResponse{Name: "lab", AllowedEgress: []types.EgressRule{
		{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		{IP: "203.0.113.7", Protocol: "icmp"},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/networks/lab", reply(cur))
	mux.HandleFunc("PUT /v1/networks/lab/egress", reply(cur))
	mux.HandleFunc("PUT /v1/networks/lab/intra", reply(cur))
	f := newFakeAPI(t, mux)

	code, _, errOut := f.run("", "change", "network", "lab",
		"--out", "tcp:192.168.50.52:502@wlan0", "--rm-out", "icmp:203.0.113.7", "--intra")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var egress types.UpdateNetworkEgressRequest
	f.mu.Lock()
	for _, r := range f.reqs {
		if r.method == "PUT" && r.path == "/v1/networks/lab/egress" {
			json.Unmarshal(r.body, &egress)
		}
	}
	f.mu.Unlock()
	want := []types.EgressRule{
		{IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		{Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
	}
	if !reflect.DeepEqual(egress.AllowedEgress, want) {
		t.Errorf("merged rules = %+v\nwant           %+v", egress.AllowedEgress, want)
	}
	if last := f.last("PUT"); last.path != "/v1/networks/lab/intra" || !strings.Contains(string(last.body), `"intra":true`) {
		t.Errorf("intra not sent: %s %s", last.path, last.body)
	}

	// Removing a rule that is not there must fail rather than report success.
	code, _, errOut = f.run("", "network", "update", "lab", "--rm-out", "udp:1.1.1.1:53")
	if code == 0 || !strings.Contains(errOut, "not in the network's policy") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(twoVMs))
	mux.HandleFunc("POST /v1/vms/{id}/exec", reply(types.ExecResponse{Output: "boom\n", ExitCode: 3}))
	f := newFakeAPI(t, mux)

	// Flags after the VM belong to the guest command, not to mh.
	code, out, _ := f.run("", "exec", "a1b2", "ls", "-la", "my file")
	if code != 3 || out != "boom\n" {
		t.Errorf("exit %d out %q, want 3 and the guest output", code, out)
	}
	var req types.ExecRequest
	json.Unmarshal(f.last("POST").body, &req)
	if req.Cmd != "ls -la 'my file'" {
		t.Errorf("cmd = %q", req.Cmd)
	}

	f.run("", "exec", "a1b2", "--", "sh", "-c", "ps | wc -l")
	json.Unmarshal(f.last("POST").body, &req)
	if req.Cmd != "sh -c 'ps | wc -l'" {
		t.Errorf("with --: cmd = %q", req.Cmd)
	}

	// mh's own flags go before the VM; the deadline travels as timeout_ms.
	req = types.ExecRequest{}
	f.run("", "exec", "-t", "1.5s", "a1b2", "sleep", "9")
	json.Unmarshal(f.last("POST").body, &req)
	if req.Cmd != "sleep 9" || req.TimeoutMS != 1500 {
		t.Errorf("with -t: %+v, want cmd \"sleep 9\" and timeout_ms 1500", req)
	}
}

func TestHealthDegradedIsAnAnswer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", replyStatus(http.StatusServiceUnavailable, types.HealthResponse{
		Status: "degraded", Checks: []types.HealthCheck{{Name: "kvm", OK: false, Detail: "/dev/kvm missing"}},
	}))
	f := newFakeAPI(t, mux)

	code, out, _ := f.run("", "health")
	if code != 1 || !strings.Contains(out, "FAIL") || !strings.Contains(out, "/dev/kvm missing") {
		t.Errorf("exit %d, out:\n%s", code, out)
	}
}

func TestAPIErrorIsShownVerbatim(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/networks/lab", replyStatus(http.StatusConflict, map[string]string{"error": "network lab still has 2 VMs"}))
	f := newFakeAPI(t, mux)

	code, _, errOut := f.run("", "network", "rm", "lab")
	if code != 1 || !strings.Contains(errOut, "network lab still has 2 VMs") || !strings.Contains(errOut, "-f") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestCopyUploadAndDownload(t *testing.T) {
	var uploaded []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(twoVMs))
	mux.HandleFunc("PUT /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "from the guest")
	})
	f := newFakeAPI(t, mux)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.bin")
	os.WriteFile(src, []byte("payload"), 0o644)

	if code, _, errOut := f.run("", "cp", src, "a1b2c3d4:/root/"); code != 0 {
		t.Fatalf("upload: exit %d %s", code, errOut)
	}
	if up := f.last("PUT"); up.query != "path=%2Froot%2Fin.bin" || string(uploaded) != "payload" {
		t.Errorf("upload query %q body %q", up.query, uploaded)
	}

	if code, _, errOut := f.run("", "cp", "a1b2c3d4:/var/log/out.txt", dir); code != 0 {
		t.Fatalf("download: exit %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(b) != "from the guest" {
		t.Errorf("downloaded %q", b)
	}

	code, out, _ := f.run("", "cp", "a1b2c3d4:/x", "-")
	if code != 0 || out != "from the guest" {
		t.Errorf("download to stdout: exit %d out %q", code, out)
	}
}

func TestParseRule(t *testing.T) {
	good := map[string]types.EgressRule{
		"tcp:203.0.113.7:8883":        {IP: "203.0.113.7", Protocol: "tcp", Port: 8883},
		"UDP:10.0.0.0/24:53":          {IP: "10.0.0.0/24", Protocol: "udp", Port: 53},
		"icmp:10.0.0.1":               {IP: "10.0.0.1", Protocol: "icmp"},
		"tcp:192.168.50.52:502@wlan0": {Iface: "wlan0", IP: "192.168.50.52", Protocol: "tcp", Port: 502},
	}
	for in, want := range good {
		got, err := parseRule(in)
		if err != nil || got != want {
			t.Errorf("parseRule(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if back, _ := parseRule(formatRule(got)); back != got {
			t.Errorf("formatRule(%+v) = %q does not round-trip", got, formatRule(got))
		}
	}
	for _, bad := range []string{"", "tcp", "tcp:1.2.3.4", "icmp:1.2.3.4:80", "sctp:1.2.3.4:9", "tcp:1.2.3.4:http", "tcp:1.2.3.4:80@"} {
		if _, err := parseRule(bad); err == nil {
			t.Errorf("parseRule(%q) accepted", bad)
		}
	}
}

func TestParseIngressRule(t *testing.T) {
	good := map[string]types.IngressRule{
		"tcp:192.168.50.60:1883@wlan0=172.16.9.2":  {Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"},
		"UDP:192.168.60.0/24:5683@eth1=172.16.9.3": {Iface: "eth1", SrcIP: "192.168.60.0/24", Protocol: "udp", Port: 5683, ToIP: "172.16.9.3"},
	}
	for in, want := range good {
		got, err := parseIngressRule(in)
		if err != nil || got != want {
			t.Errorf("parseIngressRule(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if back, _ := parseIngressRule(formatIngressRule(got)); back != got {
			t.Errorf("formatIngressRule(%+v) = %q does not round-trip", got, formatIngressRule(got))
		}
	}
	for _, bad := range []string{
		"", "tcp:192.168.50.60:1883@wlan0", "tcp:192.168.50.60:1883=172.16.9.2",
		"tcp:192.168.50.60:1883@=172.16.9.2", "tcp:192.168.50.60:1883@wlan0=",
		"icmp:192.168.50.60:0@wlan0=172.16.9.2", "tcp:192.168.50.60@wlan0=172.16.9.2",
		"tcp:192.168.50.60:mqtt@wlan0=172.16.9.2", "tcp::1883@wlan0=172.16.9.2",
	} {
		if _, err := parseIngressRule(bad); err == nil {
			t.Errorf("parseIngressRule(%q) accepted", bad)
		}
	}
}

func TestNetworkUpdateMergesIngress(t *testing.T) {
	a := types.IngressRule{Iface: "wlan0", SrcIP: "192.168.50.60", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.2"}
	b := types.IngressRule{Iface: "wlan0", SrcIP: "192.168.50.61", Protocol: "tcp", Port: 1883, ToIP: "172.16.9.3"}
	cur := types.NetworkResponse{Name: "mqtt", AllowedIngress: []types.IngressRule{a}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/networks/mqtt", reply(cur))
	mux.HandleFunc("PUT /v1/networks/mqtt/ingress", reply(cur))
	f := newFakeAPI(t, mux)

	code, _, errOut := f.run("", "network", "update", "mqtt", "--in", formatIngressRule(b))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var req types.UpdateNetworkIngressRequest
	json.Unmarshal(f.last("PUT").body, &req)
	if want := []types.IngressRule{a, b}; !reflect.DeepEqual(req.AllowedIngress, want) {
		t.Errorf("merged ingress = %+v\nwant            %+v", req.AllowedIngress, want)
	}

	// --no-in must send an explicit empty list, not omit the field.
	if code, _, errOut = f.run("", "network", "update", "mqtt", "--no-in"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if body := string(f.last("PUT").body); !strings.Contains(body, `"allowed_ingress":null`) && !strings.Contains(body, `"allowed_ingress":[]`) {
		t.Errorf("--no-in sent %s", body)
	}

	if code, _, errOut = f.run("", "network", "update", "mqtt", "--no-in", "--in", formatIngressRule(b)); code == 0 {
		t.Errorf("--no-in with --in was accepted")
	}
}

// The flags were renamed from egress/ingress to OUT/IN. The old spellings must
// keep working (scripts, muscle memory) but not show in help, where two names
// for one thing is exactly the confusion the rename removes.
func TestNetworkLegacyFlagsStillWork(t *testing.T) {
	cur := types.NetworkResponse{Name: "lab", AllowedEgress: []types.EgressRule{{IP: "203.0.113.7", Protocol: "icmp"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/networks/lab", reply(cur))
	mux.HandleFunc("PUT /v1/networks/lab/egress", reply(cur))
	mux.HandleFunc("PUT /v1/networks/lab/ingress", reply(cur))
	f := newFakeAPI(t, mux)

	for _, args := range [][]string{
		{"--add-allow", "tcp:1.2.3.4:80"},
		{"--rm-allow", "icmp:203.0.113.7"},
		{"--allow", "tcp:1.2.3.4:80"},
		{"--no-egress"},
		{"--egress", "eth0"},
		{"--add-ingress", "tcp:192.168.50.60:1883@wlan0=172.16.9.2"},
		{"--ingress", "tcp:192.168.50.60:1883@wlan0=172.16.9.2"},
		{"--no-ingress"},
	} {
		if code, _, errOut := f.run("", append([]string{"network", "update", "lab"}, args...)...); code != 0 {
			t.Errorf("legacy %v: exit %d: %s", args, code, errOut)
		}
	}

	_, out, _ := f.run("", "network", "update", "--help")
	for _, gone := range []string{"--add-allow", "--rm-allow", "--allow", "--egress", "--ingress", "--add-ingress"} {
		if strings.Contains(out, gone+" ") {
			t.Errorf("help still lists legacy %s:\n%s", gone, out)
		}
	}
	// Grouped by direction, in definition order — not alphabetical.
	if strings.Index(out, "--out ") > strings.Index(out, "--in ") || !strings.Contains(out, "Examples:") {
		t.Errorf("help lost its OUT-then-IN grouping or its examples:\n%s", out)
	}
}

func TestParseVolumeSpec(t *testing.T) {
	good := map[string]types.VolumeAttachRequest{
		"data":           {Name: "data"},
		"data:ro":        {Name: "data", ReadOnly: true},
		"data:/mnt/d":    {Name: "data", GuestPath: "/mnt/d"},
		"data:/mnt/d:ro": {Name: "data", GuestPath: "/mnt/d", ReadOnly: true},
		"data:/mnt/d:rw": {Name: "data", GuestPath: "/mnt/d"},
	}
	for in, want := range good {
		if got, err := parseVolumeSpec(in); err != nil || got != want {
			t.Errorf("parseVolumeSpec(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ":ro", "data:mnt", "data:/a:/b"} {
		if _, err := parseVolumeSpec(bad); err == nil {
			t.Errorf("parseVolumeSpec(%q) accepted", bad)
		}
	}
}

func TestParseMB(t *testing.T) {
	for in, want := range map[string]int64{"512": 512, "512M": 512, "512mb": 512, "2G": 2048, "1GiB": 1024} {
		if got, err := parseMB(in); err != nil || got != want {
			t.Errorf("parseMB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "1.5G", "lots"} {
		if _, err := parseMB(bad); err == nil {
			t.Errorf("parseMB(%q) accepted", bad)
		}
	}
}

func TestSplitRemote(t *testing.T) {
	cases := []struct {
		in       string
		ref, p   string
		isRemote bool
	}{
		{"a1b2:/root/x", "a1b2", "/root/x", true},
		{"data:/sample.bin", "data", "/sample.bin", true},
		{"./local:file", "", "", false},
		{"/abs/path", "", "", false},
		{"-", "", "", false},
	}
	for _, c := range cases {
		ref, p, ok := splitRemote(c.in)
		if ref != c.ref || p != c.p || ok != c.isRemote {
			t.Errorf("splitRemote(%q) = %q %q %v", c.in, ref, p, ok)
		}
	}
}

func TestResolveHost(t *testing.T) {
	t.Setenv("MICROHOSTED_HOST", "")
	t.Setenv("MICROHOSTED_SOCKET", "/tmp/legacy.sock")
	if got := ResolveHost(""); got != "/tmp/legacy.sock" {
		t.Errorf("legacy env: %q", got)
	}
	t.Setenv("MICROHOSTED_HOST", "tcp://127.0.0.1:8080")
	if got := ResolveHost(""); got != "tcp://127.0.0.1:8080" {
		t.Errorf("MICROHOSTED_HOST should win over MICROHOSTED_SOCKET: %q", got)
	}
	if got := ResolveHost("/run/x.sock"); got != "/run/x.sock" {
		t.Errorf("-H should win: %q", got)
	}
	if _, err := NewClient("nonsense"); err == nil {
		t.Error("NewClient accepted a host with no scheme, path or port")
	}
}

func TestVMNamesAndLabels(t *testing.T) {
	named := []types.VMResponse{
		{ID: "a1b2c3d4", Name: "ts01-a", Labels: map[string]string{"sensor": "ts-01"}, Template: "alpine-py", State: types.VMStateRunning, CreatedAt: "2026-09-23T10:00:00Z"},
		{ID: "b1b2c3d4", Template: "alpine-py", State: types.VMStateRunning, CreatedAt: "2026-09-23T09:00:00Z"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply(named))
	mux.HandleFunc("POST /v1/vms", replyStatus(http.StatusCreated, named[0]))
	mux.HandleFunc("PATCH /v1/vms/{id}/labels", reply(named[0]))
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	f := newFakeAPI(t, mux)

	code, _, errOut := f.run("", "run", "alpine-py", "--name", "ts01-a", "-l", "sensor=ts-01", "--label", "managed-by=ot")
	if code != 0 {
		t.Fatalf("run: exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(errOut, "a1b2c3d4 (ts01-a)") {
		t.Errorf("stderr %q should name the new VM", errOut)
	}
	var req types.CreateVMRequest
	if err := json.Unmarshal(f.last("POST").body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "ts01-a" || !reflect.DeepEqual(req.Labels, map[string]string{"sensor": "ts-01", "managed-by": "ot"}) {
		t.Errorf("request name %q labels %v", req.Name, req.Labels)
	}
	if code, _, _ := f.run("", "run", "alpine-py", "-l", "novalue"); code == 0 {
		t.Error("a label without = must be refused before calling the daemon")
	}

	// The selector travels as ?label=, one per term; the daemon filters.
	f.run("", "ps", "-l", "sensor=ts-01", "-l", "managed-by=ot")
	if q := f.last("GET").query; q != "label=sensor%3Dts-01&label=managed-by%3Dot" {
		t.Errorf("ps -l query = %q", q)
	}
	_, out, _ := f.run("", "ps")
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "ts01-a") {
		t.Errorf("ps table should show the NAME column:\n%s", out)
	}
	if strings.Contains(out, "LABELS") {
		t.Errorf("labels are opt-in (--show-labels):\n%s", out)
	}
	_, out, _ = f.run("", "ps", "-L")
	if !strings.Contains(out, "LABELS") || !strings.Contains(out, "sensor=ts-01") {
		t.Errorf("ps -L should show the LABELS column:\n%s", out)
	}

	// A name is a reference like an ID.
	if code, out, _ := f.run("", "rm", "ts01-a"); code != 0 || out != "a1b2c3d4\n" || f.last("DELETE").path != "/v1/vms/a1b2c3d4" {
		t.Errorf("rm by name: exit %d, out %q, DELETE %s", code, out, f.last("DELETE").path)
	}

	// label: KEY=VALUE sets, KEY- removes, as a merge patch.
	if code, _, errOut := f.run("", "label", "ts01-a", "sensor=ts-02", "role-"); code != 0 {
		t.Fatalf("label: exit %d, stderr %q", code, errOut)
	}
	patch := f.last("PATCH")
	if patch.path != "/v1/vms/a1b2c3d4/labels" {
		t.Errorf("PATCH %s", patch.path)
	}
	if strings.TrimSpace(string(patch.body)) != `{"labels":{"role":null,"sensor":"ts-02"}}` {
		t.Errorf("PATCH body = %s", patch.body)
	}
	if code, _, _ := f.run("", "label", "ts01-a", "oops"); code == 0 {
		t.Error("label with neither = nor a trailing - must refuse")
	}
}

func TestNetworkLabels(t *testing.T) {
	lab := types.NetworkResponse{Name: "ts-01", Subnet: "172.16.5.0/24", Labels: map[string]string{"managed-by": "mh-orchestrator"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/networks", reply([]types.NetworkResponse{lab}))
	mux.HandleFunc("POST /v1/networks", replyStatus(http.StatusCreated, lab))
	mux.HandleFunc("PATCH /v1/networks/{name}/labels", reply(lab))
	f := newFakeAPI(t, mux)

	if code, out, errOut := f.run("", "network", "create", "ts-01", "-l", "managed-by=mh-orchestrator"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, errOut)
	} else if !strings.Contains(out, "LABELS") || !strings.Contains(out, "managed-by=mh-orchestrator") {
		t.Errorf("a create with labels shows them:\n%s", out)
	}
	var req types.CreateNetworkRequest
	if err := json.Unmarshal(f.last("POST").body, &req); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Labels, map[string]string{"managed-by": "mh-orchestrator"}) {
		t.Errorf("request labels %v", req.Labels)
	}

	f.run("", "network", "ls", "-l", "managed-by=mh-orchestrator")
	if q := f.last("GET").query; q != "label=managed-by%3Dmh-orchestrator" {
		t.Errorf("selector query %q", q)
	}
	if _, out, _ := f.run("", "network", "ls"); strings.Contains(out, "LABELS") {
		t.Errorf("labels are opt-in on ls (-L):\n%s", out)
	}
	if _, out, _ := f.run("", "network", "ls", "-L"); !strings.Contains(out, "managed-by=mh-orchestrator") {
		t.Errorf("ls -L shows labels:\n%s", out)
	}

	if code, _, errOut := f.run("", "network", "label", "ts-01", "zone=north", "managed-by-"); code != 0 {
		t.Fatalf("label: exit %d, stderr %q", code, errOut)
	}
	patch := f.last("PATCH")
	if patch.path != "/v1/networks/ts-01/labels" || strings.TrimSpace(string(patch.body)) != `{"labels":{"managed-by":null,"zone":"north"}}` {
		t.Errorf("PATCH %s %s", patch.path, patch.body)
	}
	if code, _, _ := f.run("", "network", "label", "ts-01"); code == 0 {
		t.Error("label without edits must refuse")
	}
}

func TestImageCommands(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	img := types.ImageResponse{Digest: digest, Tags: []string{"parser:1.0"}, VCPUs: 1, MemMB: 64, SizeMB: 60, ImportedAt: "2026-09-25T10:00:00Z"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images", replyStatus(http.StatusCreated, img))
	mux.HandleFunc("GET /v1/images", reply([]types.ImageResponse{img}))
	mux.HandleFunc("POST /v1/images/{ref}/verify", reply(types.ImageVerifyResponse{Digest: digest, OK: false, Error: "mismatch"}))
	mux.HandleFunc("POST /v1/vms", replyStatus(http.StatusCreated, types.VMResponse{ID: "a1b2c3d4"}))
	f := newFakeAPI(t, mux)

	code, out, errOut := f.run("", "image", "import", "parser:1.0", "--kernel", "/store/vmlinux", "-r", "rootfs.ext4", "--mem", "64")
	if code != 0 {
		t.Fatalf("import: exit %d, stderr %q", code, errOut)
	}
	if out != "parser:1.0@"+digest+"\n" {
		t.Errorf("import prints the pinned reference, got %q", out)
	}
	var req types.ImportImageRequest
	if err := json.Unmarshal(f.last("POST").body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "parser:1.0" || req.KernelPath != "/store/vmlinux" || !filepath.IsAbs(req.RootfsPath) || req.MemMB != 64 || req.VCPUs != 1 {
		t.Errorf("import request %+v", req)
	}
	if code, _, _ := f.run("", "image", "import", "parser:1.0", "--kernel", "/k"); code == 0 {
		t.Error("import without --rootfs must refuse")
	}

	if _, out, _ := f.run("", "image", "ls"); !strings.Contains(out, "parser:1.0") || !strings.Contains(out, "abababababab") {
		t.Errorf("ls:\n%s", out)
	}
	if _, out, _ := f.run("", "image", "ls", "-q"); out != "parser:1.0@"+digest+"\n" {
		t.Errorf("ls -q = %q", out)
	}
	if code, out, _ := f.run("", "image", "verify", "parser:1.0"); code == 0 || !strings.Contains(out, "FAILED") {
		t.Errorf("a failed verify: exit %d, out %q", code, out)
	}

	// run: a reference with ':' is an image, anything else a template.
	f.run("", "run", "parser:1.0@"+digest)
	var create types.CreateVMRequest
	if err := json.Unmarshal(f.last("POST").body, &create); err != nil {
		t.Fatal(err)
	}
	if create.Image != "parser:1.0@"+digest || create.Template != "" {
		t.Errorf("run IMAGE sent %+v", create)
	}
	f.run("", "run", "alpine-py")
	create = types.CreateVMRequest{}
	if err := json.Unmarshal(f.last("POST").body, &create); err != nil {
		t.Fatal(err)
	}
	if create.Template != "alpine-py" || create.Image != "" {
		t.Errorf("run TEMPLATE sent %+v", create)
	}
}

// --file and --secret read the local file on the client side and carry its
// content; replace without files sends null (the daemon decides), --no-files
// an explicit empty list.
func TestFileFlags(t *testing.T) {
	vm := types.VMResponse{ID: "a1b2c3d4", Template: "alpine-py", State: types.VMStateRunning, CreatedAt: "2026-09-23T10:00:00Z"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply([]types.VMResponse{vm}))
	mux.HandleFunc("POST /v1/vms", replyStatus(http.StatusCreated, vm))
	mux.HandleFunc("POST /v1/vms/{id}/replace", reply(types.ReplaceVMResponse{Replacement: vm}))
	f := newFakeAPI(t, mux)

	dir := t.TempDir()
	conf, key := filepath.Join(dir, "p.conf"), filepath.Join(dir, "k")
	os.WriteFile(conf, []byte("port=502\n"), 0o600)
	os.WriteFile(key, []byte("s3cret"), 0o600)

	code, _, errOut := f.run("", "run", "alpine-py", "-f", "/etc/p.conf="+conf+",mode=0640,uid=10", "--secret", "/etc/k="+key)
	if code != 0 {
		t.Fatalf("run: exit %d, %s", code, errOut)
	}
	var req types.CreateVMRequest
	json.Unmarshal(f.last("POST").body, &req)
	want := []types.FileSpec{
		{Path: "/etc/p.conf", Content: []byte("port=502\n"), Mode: "0640", UID: 10},
		{Path: "/etc/k", Content: []byte("s3cret"), Secret: true},
	}
	if !reflect.DeepEqual(req.Files, want) {
		t.Errorf("files = %+v", req.Files)
	}
	for _, bad := range []string{"/etc/x", "/etc/x=" + conf + ",owner=1", "/etc/x=" + filepath.Join(dir, "missing")} {
		if code, _, _ := f.run("", "run", "alpine-py", "-f", bad); code == 0 {
			t.Errorf("-f %q accepted", bad)
		}
	}

	f.run("", "replace", "a1b2c3d4")
	if body := string(f.last("POST").body); !strings.Contains(body, `"files":null`) {
		t.Errorf("replace without files: %s", body)
	}
	f.run("", "replace", "a1b2c3d4", "--no-files")
	if body := string(f.last("POST").body); !strings.Contains(body, `"files":[]`) {
		t.Errorf("replace --no-files: %s", body)
	}
}

// run --wait asks the daemon for readiness after the create, with the timeout
// given; the ID is already on stdout, and a VM that never gets ready fails
// the command (the daemon's error shown) without hiding which VM it was.
func TestRunWait(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms", replyStatus(http.StatusCreated, types.VMResponse{ID: "deadbeef", Template: "base-alpine"}))
	mux.HandleFunc("GET /v1/vms/deadbeef/ready", reply(types.VMResponse{ID: "deadbeef"}))
	mux.HandleFunc("GET /v1/vms/badbad00/ready", replyStatus(http.StatusBadGateway, map[string]string{"error": "guest agent not answering"}))
	mux.HandleFunc("GET /v1/vms", reply([]types.VMResponse{{ID: "badbad00", State: types.VMStateStopped}}))
	mux.HandleFunc("POST /v1/vms/badbad00/start", reply(nil))
	f := newFakeAPI(t, mux)

	code, out, errOut := f.run("", "run", "base-alpine", "--wait", "--wait-timeout", "5s")
	if code != 0 || out != "deadbeef\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if got := f.last("GET"); got.path != "/v1/vms/deadbeef/ready" || got.query != "timeout_ms=5000" {
		t.Errorf("wait request %s?%s, want /v1/vms/deadbeef/ready?timeout_ms=5000", got.path, got.query)
	}

	// Without --wait nothing waits.
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	if code, _, _ := f.run("", "run", "base-alpine"); code != 0 {
		t.Fatal("run without --wait failed")
	}
	for _, r := range f.reqs {
		if r.method == "GET" && strings.HasSuffix(r.path, "/ready") {
			t.Errorf("run without --wait made %s %s", r.method, r.path)
		}
	}

	code, out, errOut = f.run("", "start", "badbad00", "--wait")
	if code == 0 || out != "badbad00\n" || !strings.Contains(errOut, "guest agent not answering") {
		t.Errorf("start --wait on an unready VM: exit %d stdout %q stderr %q; want a failure naming the VM on stdout and the reason", code, out, errOut)
	}

	if code, _, _ := f.run("", "run", "base-alpine", "--wait", "--wait-timeout", "11m"); code == 0 {
		t.Error("--wait-timeout over 10m accepted")
	}
}

// logs without -f reads the console through the API (no root, no need to be
// on the daemon's host) and keeps the last --tail lines.
func TestLogsThroughAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", reply([]types.VMResponse{{ID: "deadbeef", Name: "web"}}))
	mux.HandleFunc("GET /v1/vms/deadbeef/console", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("boot\nline 2\nline 3\n"))
	})
	f := newFakeAPI(t, mux)
	if code, out, errOut := f.run("", "logs", "web", "-n", "2"); code != 0 || out != "line 2\nline 3\n" {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}
