package orch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"microhosted/orchestrator/engine"
	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeEngine is an in-memory engine: enough of the API to check what the
// orchestrator asks for and what it leaves behind.
type fakeEngine struct {
	mu       sync.Mutex
	vms      map[string]*types.VMResponse
	nets     map[string]*types.NetworkResponse
	next     int
	creates  int
	destroys []string
	// exec answers a command; default: exit 0, no output.
	exec func(vm *types.VMResponse, cmd string) (*types.ExecResponse, error)
	// createErr, when set, fails every create.
	createErr error
	// lastCreate is the last create request.
	lastCreate types.CreateVMRequest
	// image, when set, is what GetImage answers (Digest is filled in).
	image *types.ImageResponse
	// execs records every command run in a VM.
	execs []string
}

func newFake() *fakeEngine {
	return &fakeEngine{vms: map[string]*types.VMResponse{}, nets: map[string]*types.NetworkResponse{}}
}

func match(l, sel map[string]string) bool {
	for k, v := range sel {
		if l[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeEngine) ListVMs(_ context.Context, sel map[string]string) ([]types.VMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.VMResponse
	for _, v := range f.vms {
		if match(v.Labels, sel) {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (f *fakeEngine) CreateVM(_ context.Context, req types.CreateVMRequest) (*types.VMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreate = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	for _, v := range f.vms {
		if v.Name == req.Name {
			return nil, &engine.Error{Status: 409, Msg: "name taken"}
		}
	}
	if req.Network != "" && f.nets[req.Network] == nil {
		return nil, &engine.Error{Status: 404, Msg: "no network " + req.Network}
	}
	f.next++
	f.creates++
	v := &types.VMResponse{ID: fmt.Sprintf("%08x", f.next), Name: req.Name, Labels: req.Labels, Image: req.Image,
		State: types.VMStateRunning, Network: req.Network, GuestIP: req.GuestIP, MemMB: req.MemMB,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	f.vms[v.ID] = v
	cp := *v
	return &cp, nil
}

func (f *fakeEngine) DestroyVM(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, id)
	f.destroys = append(f.destroys, id)
	return nil
}

func (f *fakeEngine) WaitReady(context.Context, string, time.Duration) error { return nil }

func (f *fakeEngine) Exec(_ context.Context, id, cmd string, _ time.Duration) (*types.ExecResponse, error) {
	f.mu.Lock()
	v, ok := f.vms[id]
	h := f.exec
	f.execs = append(f.execs, cmd)
	f.mu.Unlock()
	if !ok {
		return nil, &engine.Error{Status: 404, Msg: "no vm"}
	}
	if h != nil {
		return h(v, cmd)
	}
	return &types.ExecResponse{}, nil
}

func (f *fakeEngine) PatchVMLabels(_ context.Context, id string, patch map[string]*string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range patch {
		if v == nil {
			delete(f.vms[id].Labels, k)
		} else {
			f.vms[id].Labels[k] = *v
		}
	}
	return nil
}

func (f *fakeEngine) PatchNetworkLabels(_ context.Context, name string, patch map[string]*string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nets[name]
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	for k, v := range patch {
		if v == nil {
			delete(n.Labels, k)
		} else {
			n.Labels[k] = *v
		}
	}
	return nil
}

func (f *fakeEngine) ListNetworks(_ context.Context, sel map[string]string) ([]types.NetworkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.NetworkResponse
	for _, n := range f.nets {
		if match(n.Labels, sel) {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (f *fakeEngine) CreateNetwork(_ context.Context, r types.CreateNetworkRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nets[r.Name] = &types.NetworkResponse{Name: r.Name, Labels: r.Labels, Subnet: r.Subnet, Egress: r.Egress,
		EgressIface: r.EgressIface, AllowedEgress: r.AllowedEgress, AllowedIngress: r.AllowedIngress, Intra: r.Intra}
	return nil
}

func (f *fakeEngine) DeleteNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.vms {
		if v.Network == name {
			return &engine.Error{Status: 409, Msg: "in use"}
		}
	}
	delete(f.nets, name)
	return nil
}

func (f *fakeEngine) SetEgress(_ context.Context, name string, r types.UpdateNetworkEgressRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nets[name].Egress, f.nets[name].EgressIface, f.nets[name].AllowedEgress = r.Egress, r.EgressIface, r.AllowedEgress
	return nil
}

func (f *fakeEngine) SetIngress(_ context.Context, name string, rules []types.IngressRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(rules) == 0 {
		rules = nil
	}
	f.nets[name].AllowedIngress = rules
	return nil
}

func (f *fakeEngine) SetIntra(_ context.Context, name string, intra bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nets[name].Intra = intra
	return nil
}

func (f *fakeEngine) GetImage(_ context.Context, ref string) (*types.ImageResponse, error) {
	if !strings.HasSuffix(ref, digest) {
		return nil, &engine.Error{Status: 404, Msg: "no such image"}
	}
	if f.image != nil {
		img := *f.image
		img.Digest = digest
		return &img, nil
	}
	return &types.ImageResponse{Digest: digest, MemMB: 128}, nil
}

// Console answers only while the VM exists, like the engine: a record that
// has it was captured before the destroy.
func (f *fakeEngine) Console(_ context.Context, id string, _ int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vms[id] == nil {
		return nil, &engine.Error{Status: 404, Msg: "no vm"}
	}
	return []byte("[    0.1] console of " + f.vms[id].Name + "\x1b[2J\r\n"), nil
}

func (f *fakeEngine) count(sel map[string]string) int {
	vms, _ := f.ListVMs(context.Background(), sel)
	return len(vms)
}

const testSpec = `
version: 1
budget: { max_vms: 5, max_mem_mb: 1024, workers: 2 }
networks:
  demo: { subnet: 172.30.10.0/24, intra: true }
functions:
  server:
    image: alpine:1.0@` + digest + `
    network: demo
    ip: 172.30.10.2
    command: serve
    health: { command: check }
    lifecycle: { mode: persistent }
  whoami:
    image: alpine:1.0@` + digest + `
    network: none
    command: whoami
    lifecycle: { mode: transaction, every: 30s, timeout: 5s }
`

func mustSpec(t *testing.T, y string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newOrch(t *testing.T, eng Engine, y string) (*Orchestrator, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	st, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(eng, mustSpec(t, y), log.New(io.Discard, "", 0), &out, st), &out
}

func fastTimers(t *testing.T) {
	t.Helper()
	old := []time.Duration{settle, healthWait, healthRetry, pollEvery, superviseEvery, backoffMin}
	settle, healthWait, healthRetry, pollEvery, superviseEvery, backoffMin = 10*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		settle, healthWait, healthRetry, pollEvery, superviseEvery, backoffMin = old[0], old[1], old[2], old[3], old[4], old[5]
	})
}

func applyOnce(t *testing.T, o *Orchestrator) *Plan {
	t.Helper()
	p, err := o.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An empty engine gets the network and the persistent function, labelled as
// the orchestrator's; a second apply has nothing to do.
func TestApplyCreatesThenConverges(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	p := applyOnce(t, o)
	if p.PeakVMs != 2 || p.PeakMemMB != 256 {
		t.Errorf("worst case %d VMs %d MB, want 2 and 256 (1 persistent + 1 worker: one cycle function needs no second)", p.PeakVMs, p.PeakMemMB)
	}
	if n := f.nets["demo"]; n == nil || n.Labels[LabelManagedBy] != Owner || !n.Intra {
		t.Fatalf("network demo: %+v", n)
	}
	vms, _ := f.ListVMs(context.Background(), owned())
	if len(vms) != 1 {
		t.Fatalf("%d VMs, want the one persistent function's", len(vms))
	}
	v := vms[0]
	if v.Name != "default-server-1" || v.GuestIP != "172.30.10.2" || v.Labels[LabelFunction] != "server" || v.Labels[LabelSpec] == "" || v.Image != "alpine:1.0@"+digest {
		t.Errorf("vm %+v", v)
	}
	if p2, err := o.Plan(context.Background()); err != nil || len(p2.Actions) != 0 {
		t.Fatalf("second plan: %v %v, want no changes", p2.Actions, err)
	}
}

// What is not the orchestrator's is never touched, and a name collision with
// it is refused before any change.
func TestForeignObjectsUntouched(t *testing.T) {
	f := newFake()
	f.vms["user0001"] = &types.VMResponse{ID: "user0001", Name: "dev", State: types.VMStateRunning, Labels: map[string]string{"function": "server"}}
	f.nets["web"] = &types.NetworkResponse{Name: "web", Subnet: "172.16.2.0/24"}
	o, _ := newOrch(t, f, testSpec)
	applyOnce(t, o)
	if f.vms["user0001"] == nil || f.nets["web"] == nil {
		t.Fatal("a VM or network without the orchestrator's mark was removed")
	}

	f2 := newFake()
	f2.nets["demo"] = &types.NetworkResponse{Name: "demo", Subnet: "172.30.10.0/24"}
	o2, _ := newOrch(t, f2, testSpec)
	if _, err := o2.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "not the orchestrator's") {
		t.Fatalf("plan over a foreign network of the same name: %v", err)
	}
	f3 := newFake()
	f3.nets["other"] = &types.NetworkResponse{Name: "other", Subnet: "172.30.0.0/16"}
	o3, _ := newOrch(t, f3, testSpec)
	if _, err := o3.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("plan over an overlapping subnet: %v", err)
	}
}

// Removing a function or a network from the spec removes what it owned;
// quarantined VMs are kept.
func TestApplyPrunes(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	applyOnce(t, o)
	f.vms["quar0001"] = &types.VMResponse{ID: "quar0001", Name: "server-0", Quarantine: true, State: types.VMStateRunning,
		Labels: map[string]string{LabelManagedBy: Owner, LabelProject: "default", LabelFunction: "server"}}

	smaller := `
version: 1
budget: { max_vms: 5, max_mem_mb: 1024 }
functions:
  whoami:
    image: alpine:1.0@` + digest + `
    network: none
    command: whoami
    lifecycle: { mode: transaction, every: 30s, timeout: 5s }
`
	o2, _ := newOrch(t, f, smaller)
	p := applyOnce(t, o2)
	if len(p.Kept) != 1 {
		t.Errorf("kept %v, want the quarantined VM", p.Kept)
	}
	if f.count(owned()) != 1 || f.vms["quar0001"] == nil {
		t.Errorf("after prune: %d owned VMs, want only the quarantined one", f.count(owned()))
	}
	if f.nets["demo"] != nil {
		t.Error("network demo left behind")
	}
}

// A spec whose worst case does not fit its budget, or whose image is not the
// stored digest, is refused before any change.
func TestPlanRefuses(t *testing.T) {
	f := newFake()
	o, _ := newOrch(t, f, strings.Replace(testSpec, "max_mem_mb: 1024", "max_mem_mb: 200", 1))
	if _, err := o.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "max_mem_mb is 200") {
		t.Errorf("over budget: %v", err)
	}
	other := "sha256:" + strings.Repeat("f", 64)
	o, _ = newOrch(t, f, strings.Replace(testSpec, digest, other, 1))
	if _, err := o.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("unknown image: %v", err)
	}
	if len(f.nets) != 0 || len(f.vms) != 0 {
		t.Error("a refused plan changed the engine")
	}
}

// A persistent function whose health never passes is not left half-started.
func TestStartPersistentFailureLeavesNothing(t *testing.T) {
	fastTimers(t)
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if cmd == "check" {
			return &types.ExecResponse{ExitCode: 1, Output: "connection refused"}, nil
		}
		return &types.ExecResponse{}, nil
	}
	o, _ := newOrch(t, f, testSpec)
	p, err := o.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = o.Apply(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("apply: %v, want the health check's output", err)
	}
	if n := f.count(owned()); n != 0 {
		t.Errorf("%d VMs left after a failed start", n)
	}
}

// A transaction cycle runs its command on a fresh VM and destroys it, whatever
// the outcome; a full host is a skip, not a failure.
func TestTransactionCycle(t *testing.T) {
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		return &types.ExecResponse{Output: "root\n"}, nil
	}
	o, out := newOrch(t, f, testSpec)
	fn := o.spec.Functions["whoami"]
	r := o.cycle(context.Background(), "whoami", fn)
	if !r.OK || r.Output != "root\n" || *r.Exit != 0 || r.VM != "default-whoami-1" {
		t.Errorf("result %+v", r)
	}
	if f.count(owned()) != 0 {
		t.Error("cycle VM not destroyed")
	}

	f.exec = func(*types.VMResponse, string) (*types.ExecResponse, error) {
		return &types.ExecResponse{ExitCode: 3, Output: "boom"}, nil
	}
	r = o.cycle(context.Background(), "whoami", fn)
	if r.OK || r.Cause != "exit 3" || f.count(owned()) != 0 {
		t.Errorf("failing cycle %+v, %d VMs left", r, f.count(owned()))
	}

	f.exec = func(*types.VMResponse, string) (*types.ExecResponse, error) {
		return nil, &engine.Error{Status: http.StatusGatewayTimeout, Msg: "timeout"}
	}
	if r = o.cycle(context.Background(), "whoami", fn); r.OK || !strings.HasPrefix(r.Cause, "timeout") {
		t.Errorf("timed-out cycle %+v", r)
	}

	f.createErr = &engine.Error{Status: http.StatusServiceUnavailable, Msg: "host full"}
	if r = o.cycle(context.Background(), "whoami", fn); !r.Skipped {
		t.Errorf("503 on create: %+v, want skipped", r)
	}

	o.report(r)
	var line Result
	if err := json.Unmarshal(out.Bytes(), &line); err != nil || line.Function != "whoami" {
		t.Errorf("result line %q: %v", out.String(), err)
	}
}

// A window cycle succeeds when its command outlives the window, and fails when
// it ends before.
func TestWindowCycle(t *testing.T) {
	fastTimers(t)
	y := strings.Replace(testSpec, "lifecycle: { mode: transaction, every: 30s, timeout: 5s }", "lifecycle: { mode: window, every: 30s, duration: 1s }", 1)
	f := newFake()
	o, _ := newOrch(t, f, y)
	fn := o.spec.Functions["whoami"]
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if strings.HasPrefix(cmd, "tail") {
			return &types.ExecResponse{Output: "tick\n"}, nil
		}
		return &types.ExecResponse{}, nil
	}
	fn.Lifecycle.Duration = spec.Duration(100 * time.Millisecond)
	if r := o.cycle(context.Background(), "whoami", fn); !r.OK || r.Output != "tick\n" {
		t.Errorf("window %+v", r)
	}
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if cmd == aliveCmd {
			return &types.ExecResponse{ExitCode: 1}, nil
		}
		return &types.ExecResponse{}, nil
	}
	if r := o.cycle(context.Background(), "whoami", fn); r.OK || !strings.Contains(r.Cause, "command ended") {
		t.Errorf("window whose command ends early: %+v", r)
	}
	if f.count(owned()) != 0 {
		t.Error("window VM not destroyed")
	}
}

// Run keeps a persistent function served: a VM that dies is destroyed and a
// fresh one takes its place. On stop, no cycle VM is left behind.
func TestRunReplacesDeadVM(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- o.Run(ctx, nil) }()

	server := map[string]string{LabelFunction: "server"}
	waitFor(t, func() bool { return f.count(server) == 1 })
	vms, _ := f.ListVMs(ctx, server)
	f.mu.Lock()
	f.vms[vms[0].ID].State = types.VMStateStopped
	f.vms[vms[0].ID].LastExit = &types.VMExitResponse{Reason: "killed by the OOM killer"}
	f.mu.Unlock()
	waitFor(t, func() bool {
		vms, _ := f.ListVMs(ctx, server)
		return len(vms) == 1 && vms[0].Name == "default-server-2" && vms[0].State == types.VMStateRunning
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.count(map[string]string{LabelFunction: "whoami"}); n != 0 {
		t.Errorf("%d cycle VMs left after stop", n)
	}
}

// The launched command's exit code is recorded when it ends — also when it
// execs, as a runner started through setpriv does — and only then.
func TestLaunchRecordsExitCode(t *testing.T) {
	dir := t.TempDir()
	for cmd, want := range map[string]string{"exit 3": "3", "exec sh -c 'exit 0'": "0", "exec false": "1"} {
		script := launchCmd(cmd)
		for _, p := range []string{guestLog, guestPID, guestExit} {
			script = strings.ReplaceAll(script, p, filepath.Join(dir, filepath.Base(p)))
		}
		if err := exec.Command("sh", "-c", script).Run(); err != nil {
			t.Fatal(err)
		}
		var got []byte
		waitFor(t, func() bool {
			got, _ = os.ReadFile(filepath.Join(dir, filepath.Base(guestExit)))
			return len(got) > 0
		})
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%q: exit file %q, want %s", cmd, got, want)
		}
	}
}

// on_exit: replace — a command that ends with 0 is work done: the VM is
// replaced at once and nothing is recorded; any other code is a failure.
func TestOnExitReplace(t *testing.T) {
	fastTimers(t)
	y := strings.Replace(testSpec, "    health: { command: check }\n    lifecycle: { mode: persistent }", "    lifecycle: { mode: persistent, on_exit: replace }", 1)
	f := newFake()
	o, _ := newOrch(t, f, y)
	var mu sync.Mutex
	exited := map[string]string{} // vm id → exit code, once its command ended
	checks := map[string]int{}    // vm id → liveness checks answered
	f.exec = func(v *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		code, ended := exited[v.ID]
		switch cmd {
		case aliveCmd:
			checks[v.ID]++
			if ended {
				return &types.ExecResponse{ExitCode: 1}, nil
			}
		case exitCmd:
			if ended {
				return &types.ExecResponse{Output: code + "\n"}, nil
			}
			return &types.ExecResponse{ExitCode: 1}, nil
		}
		return &types.ExecResponse{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- o.Run(ctx, nil) }()
	defer func() { cancel(); <-done }()

	server := map[string]string{LabelFunction: "server"}
	serving := func(name string) func() bool {
		return func() bool {
			vms, _ := f.ListVMs(ctx, server)
			return len(vms) == 1 && vms[0].Name == name && vms[0].State == types.VMStateRunning
		}
	}
	// The command ends once its VM is supervised — past the start's own
	// liveness check, where an exit is a failed start, not work done.
	end := func(code string) {
		vms, _ := f.ListVMs(ctx, server)
		id := vms[0].ID
		waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return checks[id] >= 2 })
		mu.Lock()
		exited[id] = code
		mu.Unlock()
	}
	waitFor(t, serving("default-server-1"))
	end("0")
	waitFor(t, serving("default-server-2"))
	if n := len(o.state.Get("server").Failures); n != 0 {
		t.Errorf("a finished VM was recorded as %d failures", n)
	}
	end("2")
	waitFor(t, serving("default-server-3"))
	fails := o.state.Get("server").Failures
	if len(fails) != 1 || !strings.Contains(fails[0].Cause, "command exited 2") {
		t.Errorf("failures after exit 2: %+v", fails)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestShellQuote(t *testing.T) {
	for _, s := range []string{`echo "a b"`, `it's`, `$(whoami) '; rm -rf / '`} {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("shellQuote(%q) round-trips to %q (%v)", s, out, err)
		}
	}
}

// The orchestrator is a client of the engine API: it must build without any
// of the engine's internal packages (docs/orchestrator.md §3).
func TestNoEngineInternals(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "microhosted/orchestrator/...").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "microhosted/internal/") {
			t.Errorf("the orchestrator depends on %s", dep)
		}
	}
}

// A cycle cut short because the orchestrator stops is a skip, not a failure
// of the function.
func TestInterruptedCycleIsSkipped(t *testing.T) {
	fastTimers(t)
	y := strings.Replace(testSpec, "lifecycle: { mode: transaction, every: 30s, timeout: 5s }", "lifecycle: { mode: window, every: 30s, duration: 10s }", 1)
	f := newFake()
	o, _ := newOrch(t, f, y)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	r := o.cycle(ctx, "whoami", o.spec.Functions["whoami"])
	if r.OK || !r.Skipped || !strings.HasPrefix(r.Cause, "interrupted") {
		t.Errorf("interrupted window: %+v, want skipped", r)
	}
	if f.count(owned()) != 0 {
		t.Error("interrupted cycle's VM not destroyed")
	}
}

// A function that never starts gets exactly degradedAfter attempts, the
// initial apply's included, then no more.
func TestDegradedAfterFailedStarts(t *testing.T) {
	fastTimers(t)
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if cmd == "check" {
			return &types.ExecResponse{ExitCode: 1}, nil
		}
		return &types.ExecResponse{}, nil
	}
	o, _ := newOrch(t, f, testSpec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- o.Run(ctx, nil) }()
	server := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, v := range f.vms {
			if v.Labels[LabelFunction] == "server" {
				n++
			}
		}
		return n
	}
	waitFor(t, func() bool { return o.state.Get("server").Degraded })
	time.Sleep(300 * time.Millisecond) // well past any further back-off step
	cancel()
	<-done
	o.mu.Lock()
	attempts := o.gen["server"]
	o.mu.Unlock()
	if attempts != degradedAfter {
		t.Errorf("%d start attempts, want %d", attempts, degradedAfter)
	}
	if st := o.state.Get("server"); !st.Degraded || len(st.Failures) != FailuresKept {
		t.Errorf("state after degrading: degraded %v, %d failures kept; want true and %d", st.Degraded, len(st.Failures), FailuresKept)
	}
	if n := server(); n != 0 {
		t.Errorf("%d server VMs left after failed starts", n)
	}
}

// A failed cycle keeps why — cause, exit, output and the console, read while
// the VM still existed; a successful one keeps nothing.
func TestFailureRecords(t *testing.T) {
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	fn := o.spec.Functions["whoami"]
	if r := o.cycle(context.Background(), "whoami", fn); !r.OK {
		t.Fatalf("cycle %+v", r)
	}
	if n := len(o.state.Get("whoami").Failures); n != 0 {
		t.Fatalf("%d failures recorded for a successful cycle", n)
	}

	f.exec = func(*types.VMResponse, string) (*types.ExecResponse, error) {
		return &types.ExecResponse{ExitCode: 3, Output: "about to fail\n"}, nil
	}
	o.cycle(context.Background(), "whoami", fn)
	fs := o.state.Get("whoami").Failures
	if len(fs) != 1 {
		t.Fatalf("%d failures, want 1", len(fs))
	}
	r := fs[0]
	if r.Cause != "exit 3" || *r.Exit != 3 || r.Output != "about to fail\n" || r.VM != "default-whoami-2" || r.Image != fn.Image {
		t.Errorf("record %+v", r)
	}
	if !strings.Contains(r.Console, "console of default-whoami-2") {
		t.Errorf("console %q: not captured before the destroy", r.Console)
	}

	// Bounded: only the newest are kept.
	for range FailuresKept + 2 {
		o.cycle(context.Background(), "whoami", fn)
	}
	if fs := o.state.Get("whoami").Failures; len(fs) != FailuresKept || fs[len(fs)-1].VM != "default-whoami-9" {
		t.Errorf("kept %d failures, newest %s; want %d, default-whoami-9", len(fs), fs[len(fs)-1].VM, FailuresKept)
	}

	// A create the engine refuses is recorded without a VM; a full host
	// is a skip, not a failure.
	f.createErr = &engine.Error{Status: 400, Msg: "bad request"}
	o.cycle(context.Background(), "whoami", fn)
	if fs := o.state.Get("whoami").Failures; fs[len(fs)-1].VM != "" || !strings.Contains(fs[len(fs)-1].Cause, "bad request") {
		t.Errorf("create failure record %+v", fs[len(fs)-1])
	}
	f.createErr = &engine.Error{Status: 503, Msg: "full"}
	before := o.state.Get("whoami").Failures
	o.cycle(context.Background(), "whoami", fn)
	if after := o.state.Get("whoami").Failures; after[len(after)-1].At != before[len(before)-1].At || after[len(after)-1].Cause != before[len(before)-1].Cause {
		t.Error("a skipped cycle was recorded as a failure")
	}
}

// A persistent VM that dies is recorded with its engine reason and console
// before the supervisor destroys it.
func TestDeathRecorded(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- o.Run(ctx, nil) }()
	server := map[string]string{LabelFunction: "server"}
	waitFor(t, func() bool { return f.count(server) == 1 })
	vms, _ := f.ListVMs(ctx, server)
	f.mu.Lock()
	f.vms[vms[0].ID].State = types.VMStateStopped
	f.vms[vms[0].ID].LastExit = &types.VMExitResponse{Reason: "killed by the OOM killer"}
	f.mu.Unlock()
	waitFor(t, func() bool { return len(o.state.Get("server").Failures) == 1 })
	cancel()
	<-done
	r := o.state.Get("server").Failures[0]
	if r.Cause != "died: killed by the OOM killer" || r.VM != "default-server-1" || !strings.Contains(r.Console, "console of default-server-1") {
		t.Errorf("record %+v", r)
	}
}

// A new run forgets removed functions and clears degraded flags.
func TestStatePrune(t *testing.T) {
	st, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.RecordFailure(Failure{Function: "gone", Cause: "x"})
	st.RecordFailure(Failure{Function: "kept", Cause: "y"})
	st.SetDegraded("kept", true)
	if err := st.Prune(map[string]bool{"kept": true}); err != nil {
		t.Fatal(err)
	}
	if len(st.Get("gone").Failures) != 0 {
		t.Error("state of a removed function kept")
	}
	if k := st.Get("kept"); k.Degraded || len(k.Failures) != 1 {
		t.Errorf("kept: %+v, want its failure and no degraded flag", k)
	}
}

// Guest text cannot drive the operator's terminal.
func TestPrintable(t *testing.T) {
	got := Printable("ok\r\nline\x1b[2J\x07\ttab\n")
	if got != "ok\nline\\x1b[2J\\x07\ttab\n" {
		t.Errorf("Printable = %q", got)
	}
}

// A function's files reach the engine with its create, and a changed file
// changes the spec hash — the VM is recreated to be born with it.
func TestFilesReachEngine(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	fn := o.spec.Functions["server"]
	fn.Files = map[string]*spec.File{"/opt/app.py": {From: "app.py", Mode: "0755", Content: []byte("print(1)")}}
	fn.Secrets = map[string]*spec.File{"/etc/token": {From: "token", Content: []byte("s3cret")}}
	h1 := SpecHash(fn)
	applyOnce(t, o)
	got := f.lastCreate.Files
	if len(got) != 2 || got[0].Path != "/opt/app.py" || got[0].Mode != "0755" || got[0].Secret || got[1].Path != "/etc/token" || !got[1].Secret || string(got[1].Content) != "s3cret" {
		t.Errorf("files sent: %+v", got)
	}
	if strings.Contains(h1, "s3cret") {
		t.Error("the spec hash carries a secret")
	}
	fn.Secrets["/etc/token"].Content = []byte("rotated")
	if SpecHash(fn) == h1 {
		t.Error("a changed secret does not change the spec hash")
	}
	p, err := o.Plan(context.Background())
	if err != nil || len(p.Actions) != 2 || p.Actions[0].Why != "spec changed" {
		t.Errorf("plan after a secret rotation: %v %v, want destroy + create", p.Actions, err)
	}
}

// A secret from a command is minted on this host for each VM, with the VM's
// name in its environment; the spec hash follows the command, not its
// output, so a fresh credential is not a spec change.
func TestSecretFromCommand(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	fn := o.spec.Functions["server"]
	dir := t.TempDir()
	fn.Secrets = map[string]*spec.File{"/etc/jit": {Command: `printf '%s' "jit-for-$MH_VM_NAME"; echo noise >&2`, Dir: dir}}
	h1 := SpecHash(fn)
	applyOnce(t, o)
	got := f.lastCreate.Files
	if len(got) != 1 || !got[0].Secret || string(got[0].Content) != "jit-for-"+f.lastCreate.Name {
		t.Fatalf("files sent: %+v (vm %s)", got, f.lastCreate.Name)
	}
	if SpecHash(fn) != h1 {
		t.Error("the spec hash changed with nothing changed")
	}
	if p, err := o.Plan(context.Background()); err != nil || len(p.Actions) != 0 {
		t.Errorf("plan after a minted secret: %v %v, want nothing to do", p.Actions, err)
	}
	fn.Secrets["/etc/jit"].Command = "echo other"
	if SpecHash(fn) == h1 {
		t.Error("a changed command does not change the spec hash")
	}

	// A failing command fails the create, with its last line of stderr; a
	// command that prints nothing too.
	for cmd, want := range map[string]string{"echo 'token expired' >&2; exit 3": "token expired", "true": "printed nothing"} {
		src := &spec.File{Command: cmd, Dir: dir}
		if _, err := runSecretCommand(context.Background(), src, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", cmd, err, want)
		}
	}
}

// startRun runs the orchestrator with a reload channel; stop ends it.
func startRun(t *testing.T, o *Orchestrator) (reload chan *spec.Spec, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reload = make(chan *spec.Spec, 1)
	done := make(chan error)
	go func() { done <- o.Run(ctx, reload) }()
	return reload, func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func servingVM(f *fakeEngine, fn string) *types.VMResponse {
	vms, _ := f.ListVMs(context.Background(), map[string]string{LabelFunction: fn})
	for _, v := range vms {
		if v.State == types.VMStateRunning && !v.Quarantine {
			return &v
		}
	}
	return nil
}

// A changed persistent function is replaced by a VM of the new version,
// verified by its health check.
func TestRolloutUpdatesPersistent(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	reload, stop := startRun(t, o)
	defer stop()
	waitFor(t, func() bool { return servingVM(f, "server") != nil })

	ns := mustSpec(t, strings.Replace(testSpec, "command: serve", "command: serve --v2", 1))
	reload <- ns
	want := SpecHash(ns.Functions["server"])
	waitFor(t, func() bool { v := servingVM(f, "server"); return v != nil && v.Labels[LabelSpec] == want })
	if n := f.count(map[string]string{LabelFunction: "server"}); n != 1 {
		t.Errorf("%d server VMs after the update, want 1", n)
	}
	if o.state.Get("server").Held {
		t.Error("held after a successful update")
	}
}

// A new version that fails its checks goes back to the previous version on a
// fresh VM, is held, and stops the roll-out: a later changed function keeps
// its previous version too.
func TestRolloutRevertsAndHalts(t *testing.T) {
	fastTimers(t)
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if cmd == "check-v2" {
			return &types.ExecResponse{ExitCode: 1, Output: "v2 is broken"}, nil
		}
		return &types.ExecResponse{Output: "root\n"}, nil
	}
	o, _ := newOrch(t, f, testSpec)
	reload, stop := startRun(t, o)
	defer stop()
	waitFor(t, func() bool { return servingVM(f, "server") != nil })
	oldHash := SpecHash(o.spec.Functions["server"])
	oldWhoami := o.fn("whoami")

	y := strings.Replace(testSpec, "command: serve\n    health: { command: check }", "command: serve --v2\n    health: { command: check-v2 }", 1)
	y = strings.Replace(y, "command: whoami", "command: whoami --v2", 1)
	reload <- mustSpec(t, y)
	waitFor(t, func() bool { return o.state.Get("server").Held })
	waitFor(t, func() bool { v := servingVM(f, "server"); return v != nil && v.Labels[LabelSpec] == oldHash })
	if v := servingVM(f, "server"); v.Name == "default-server-1" {
		t.Error("the previous version is the old VM, not a fresh one")
	}
	if o.fn("whoami") != oldWhoami {
		t.Error("a function after the failed one was rolled out")
	}
	if fs := o.state.Get("server").Failures; len(fs) == 0 || !strings.Contains(fs[len(fs)-1].Cause, "v2 is broken") {
		t.Errorf("no failure record for the failed version: %+v", fs)
	}
}

// A cycle function's new version is verified by one cycle at once; a failed
// one goes back.
func TestRolloutCycleVerification(t *testing.T) {
	fastTimers(t)
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		if cmd == "whoami --broken" {
			return &types.ExecResponse{ExitCode: 2, Output: "nope"}, nil
		}
		return &types.ExecResponse{}, nil
	}
	o, _ := newOrch(t, f, testSpec)
	reload, stop := startRun(t, o)
	defer stop()
	waitFor(t, func() bool { return servingVM(f, "server") != nil })

	good := mustSpec(t, strings.Replace(testSpec, "command: whoami", "command: whoami --v2", 1))
	reload <- good
	waitFor(t, func() bool { return o.fn("whoami").Command == "whoami --v2" && o.desired() == good })

	reload <- mustSpec(t, strings.Replace(testSpec, "command: whoami", "command: whoami --broken", 1))
	waitFor(t, func() bool { return o.state.Get("whoami").Held })
	if c := o.fn("whoami").Command; c != "whoami --v2" {
		t.Errorf("whoami runs %q after a failed update, want the previous whoami --v2", c)
	}
}

// A spec that does not plan is refused whole; functions added and removed by
// a reload are started and pruned.
func TestRolloutRefusesAndReshapes(t *testing.T) {
	fastTimers(t)
	f := newFake()
	o, _ := newOrch(t, f, testSpec)
	reload, stop := startRun(t, o)
	defer stop()
	waitFor(t, func() bool { return servingVM(f, "server") != nil })

	before := o.desired()
	reload <- mustSpec(t, strings.Replace(testSpec, "max_mem_mb: 1024", "max_mem_mb: 100", 1))
	time.Sleep(100 * time.Millisecond)
	if o.desired() != before {
		t.Fatal("an over-budget spec was adopted")
	}

	reshaped := `
version: 1
budget: { max_vms: 5, max_mem_mb: 1024 }
networks:
  demo: { subnet: 172.30.10.0/24, intra: true }
functions:
  api:
    image: alpine:1.0@` + digest + `
    network: demo
    command: serve-api
    lifecycle: { mode: persistent }
`
	reload <- mustSpec(t, reshaped)
	waitFor(t, func() bool { return servingVM(f, "api") != nil && servingVM(f, "server") == nil })
	if o.fn("whoami") != nil || o.fn("server") != nil {
		t.Error("removed functions still run")
	}
}

// A command that dies at once fails the start at once, not after the whole
// health wait.
func TestStartFailsFastWhenCommandExits(t *testing.T) {
	fastTimers(t)
	healthWait = 10 * time.Second
	f := newFake()
	f.exec = func(_ *types.VMResponse, cmd string) (*types.ExecResponse, error) {
		switch cmd {
		case "check", aliveCmd:
			return &types.ExecResponse{ExitCode: 1}, nil
		case tailCmd:
			return &types.ExecResponse{Output: "SyntaxError: invalid syntax\n"}, nil
		}
		return &types.ExecResponse{}, nil
	}
	f.nets["demo"] = &types.NetworkResponse{Name: "demo", Subnet: "172.30.10.0/24"}
	o, _ := newOrch(t, f, testSpec)
	start := time.Now()
	_, err := o.startPersistent(context.Background(), "server", o.spec.Functions["server"])
	if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("start: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s: waited out the health check", d)
	}
}

// A function that leaves its command and health check out runs the image's;
// what the spec sets wins. A cycle function needs a command from one of
// the two.
func TestImageDefaults(t *testing.T) {
	fastTimers(t)
	f := newFake()
	f.image = &types.ImageResponse{MemMB: 64, Command: "nginx -g 'daemon off;'",
		Health: &types.ImageHealth{Command: "wget -qO- http://127.0.0.1/", Every: "30s"}}
	o, _ := newOrch(t, f, `
version: 1
budget: { max_vms: 5, max_mem_mb: 1024 }
functions:
  site:
    image: site:1.0@`+digest+`
    network: none
    lifecycle: { mode: persistent }
  other:
    image: site:1.0@`+digest+`
    network: none
    command: httpd -f
    lifecycle: { mode: persistent }
`)
	p := applyOnce(t, o)
	site, other := o.spec.Functions["site"], o.spec.Functions["other"]
	if site.Command != "nginx -g 'daemon off;'" || site.Health == nil || site.Health.Every.D() != 30*time.Second ||
		site.Health.Timeout.D() != spec.DefaultHealthTimeout || site.Health.Failures != spec.DefaultHealthFailures {
		t.Errorf("site = command %q, health %+v", site.Command, site.Health)
	}
	if other.Command != "httpd -f" || other.Health == nil || other.Health.Command != "wget -qO- http://127.0.0.1/" {
		t.Errorf("other = command %q, health %+v: the spec's command wins, the image's health still applies", other.Command, other.Health)
	}
	if !strings.Contains(p.Actions[0].Why, "command and health from the image") {
		t.Errorf("plan says %q", p.Actions[0].Why)
	}
	launched := false
	for _, c := range f.execs {
		launched = launched || strings.Contains(c, "nginx -g")
	}
	if !launched {
		t.Errorf("the image's command was not launched: %q", f.execs)
	}
	if p2, err := o.Plan(context.Background()); err != nil || len(p2.Actions) != 0 {
		t.Errorf("second plan: %v %v, want no changes", p2, err)
	}

	f2 := newFake()
	f2.image = &types.ImageResponse{MemMB: 64}
	o2, _ := newOrch(t, f2, `
version: 1
budget: { max_vms: 5, max_mem_mb: 1024 }
functions:
  job:
    image: site:1.0@`+digest+`
    network: none
    lifecycle: { mode: transaction, every: 30s, timeout: 5s }
`)
	if _, err := o2.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "functions.job: command: required for transaction") {
		t.Errorf("a cycle without a command anywhere: %v", err)
	}
}

// Projects: each spec sees and changes only its own objects, so two run side
// by side; what was made before projects is adopted by the spec declaring it.
func TestProjects(t *testing.T) {
	fastTimers(t)
	f := newFake()
	// Made before projects: no project label.
	f.nets["legacy"] = &types.NetworkResponse{Name: "legacy", Subnet: "172.30.60.0/24", Labels: map[string]string{LabelManagedBy: Owner}}
	f.vms["old00001"] = &types.VMResponse{ID: "old00001", Name: "site-2", State: types.VMStateRunning, Image: "alpine:1.0@" + digest, Network: "legacy",
		Labels: map[string]string{LabelManagedBy: Owner, LabelFunction: "site", LabelGeneration: "2"}}

	a, _ := newOrch(t, f, "version: 1\nname: alpha\nbudget: { max_vms: 4, max_mem_mb: 1024 }\nnetworks:\n  a-net: { subnet: 172.30.61.0/24 }\nfunctions:\n  web:\n    image: alpine:1.0@"+digest+"\n    network: a-net\n    lifecycle: { mode: persistent }\n")
	b, _ := newOrch(t, f, "version: 1\nname: beta\nbudget: { max_vms: 4, max_mem_mb: 1024 }\nnetworks:\n  b-net: { subnet: 172.30.62.0/24 }\nfunctions:\n  web:\n    image: alpine:1.0@"+digest+"\n    network: b-net\n    lifecycle: { mode: persistent }\n")
	applyOnce(t, a)
	pb := applyOnce(t, b)
	for _, act := range pb.Actions {
		if act.Verb == "destroy" || act.Verb == "delete" {
			t.Errorf("project beta planned %s on alpha's or legacy objects", act)
		}
	}
	if f.count(map[string]string{LabelProject: "alpha"}) != 1 || f.count(map[string]string{LabelProject: "beta"}) != 1 {
		t.Fatal("each project should have its own VM")
	}
	if servingVM(f, "web") == nil || f.nets["legacy"] == nil || f.vms["old00001"] == nil {
		t.Fatal("an apply removed another project's or a legacy object")
	}
	names := map[string]bool{}
	for _, v := range f.vms {
		names[v.Name] = true
	}
	if !names["alpha-web-1"] || !names["beta-web-1"] {
		t.Errorf("VM names %v, want alpha-web-1 and beta-web-1", names)
	}
	if kept, err := b.Down(context.Background()); err != nil || len(kept) != 0 || f.count(map[string]string{LabelProject: "alpha"}) != 1 || f.nets["a-net"] == nil || f.nets["b-net"] != nil {
		t.Errorf("down of beta: kept %v, %v; alpha must be untouched and b-net gone", kept, err)
	}

	// The spec that declares the legacy objects adopts them: labels, no
	// recreation.
	c, _ := newOrch(t, f, "version: 1\nname: gamma\nbudget: { max_vms: 4, max_mem_mb: 1024 }\nnetworks:\n  legacy: { subnet: 172.30.60.0/24 }\nfunctions:\n  site:\n    image: alpine:1.0@"+digest+"\n    network: legacy\n    lifecycle: { mode: persistent }\n")
	// It was made from this very function: same spec hash.
	f.vms["old00001"].Labels[LabelSpec] = SpecHash(c.spec.Functions["site"])
	p := applyOnce(t, c)
	for _, act := range p.Actions {
		if act.Verb != "update" {
			t.Errorf("adoption planned %s", act)
		}
	}
	if f.nets["legacy"].Labels[LabelProject] != "gamma" || f.vms["old00001"].Labels[LabelProject] != "gamma" {
		t.Errorf("legacy objects not adopted: net %v, vm %v", f.nets["legacy"].Labels, f.vms["old00001"].Labels)
	}
	// Once labelled, a network of the same name is refused to another project.
	d, _ := newOrch(t, f, "version: 1\nname: delta\nbudget: { max_vms: 4, max_mem_mb: 1024 }\nnetworks:\n  legacy: { subnet: 172.30.60.0/24 }\nfunctions:\n  x:\n    image: alpine:1.0@"+digest+"\n    network: legacy\n    lifecycle: { mode: persistent }\n")
	if _, err := d.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "belongs to project gamma") {
		t.Errorf("another project's network: %v", err)
	}
}
