// Package orch reconciles the engine toward a plant spec: it creates the
// spec's networks and functions, keeps them in their declared state and
// removes whatever it owns that the spec no longer declares.
//
// Ownership (docs/orchestrator.md §6): everything the orchestrator creates
// carries managed-by=mh-orchestrator, and it never touches an object without
// that mark — not the operator's own VMs, not another consumer's.
package orch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

// Owner is this orchestrator's managed-by value.
const Owner = "mh-orchestrator"

// Labels the orchestrator sets on what it creates.
const (
	LabelManagedBy  = "managed-by"
	LabelProject    = "project"
	LabelFunction   = "function"
	LabelGeneration = "generation"
	LabelSpec       = "spec"
)

// Engine is the part of the engine API the orchestrator uses.
type Engine interface {
	ListVMs(ctx context.Context, labels map[string]string) ([]types.VMResponse, error)
	CreateVM(ctx context.Context, req types.CreateVMRequest) (*types.VMResponse, error)
	DestroyVM(ctx context.Context, id string) error
	WaitReady(ctx context.Context, id string, timeout time.Duration) error
	Exec(ctx context.Context, id, cmd string, timeout time.Duration) (*types.ExecResponse, error)
	PatchVMLabels(ctx context.Context, id string, labels map[string]*string) error
	PatchNetworkLabels(ctx context.Context, name string, labels map[string]*string) error
	ListNetworks(ctx context.Context, labels map[string]string) ([]types.NetworkResponse, error)
	CreateNetwork(ctx context.Context, req types.CreateNetworkRequest) error
	DeleteNetwork(ctx context.Context, name string) error
	SetEgress(ctx context.Context, name string, req types.UpdateNetworkEgressRequest) error
	SetIngress(ctx context.Context, name string, rules []types.IngressRule) error
	SetIntra(ctx context.Context, name string, intra bool) error
	GetImage(ctx context.Context, ref string) (*types.ImageResponse, error)
	Console(ctx context.Context, id string, tail int) ([]byte, error)
}

// Orchestrator holds one plant spec and the engine it is applied to.
type Orchestrator struct {
	eng  Engine
	spec *spec.Spec
	// project is the spec's project: the orchestrator sees and changes only
	// objects labelled with it (and the unlabelled ones it adopts, see mine).
	project string
	log     *log.Logger
	// out receives one line per finished cycle (the v1 result sink).
	out io.Writer
	// state keeps failure records and degraded flags; nil keeps nothing.
	state *State

	mu  sync.Mutex
	gen map[string]int // function → last generation handed out
	// effective is the version each function runs: the spec's, or — for a
	// function held after a failed update — the previous one (rollout.go).
	effective map[string]*spec.Function
	// images caches resolved image references (digest-pinned: they never
	// change under the same reference).
	images map[string]*types.ImageResponse
}

// New returns an orchestrator for s. Cycle results go to out; failure
// records to state (nil: none kept).
func New(eng Engine, s *spec.Spec, logger *log.Logger, out io.Writer, state *State) *Orchestrator {
	eff := make(map[string]*spec.Function, len(s.Functions))
	for name, f := range s.Functions {
		eff[name] = f
	}
	project := s.Project
	if project == "" {
		project = "default"
	}
	return &Orchestrator{eng: eng, spec: s, project: project, log: logger, out: out, state: state, gen: map[string]int{}, effective: eff, images: map[string]*types.ImageResponse{}}
}

// fn returns the version a function runs now; nil once it is removed.
func (o *Orchestrator) fn(name string) *spec.Function {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.effective[name]
}

// desired returns the spec the orchestrator holds now (a reload replaces it).
func (o *Orchestrator) desired() *spec.Spec {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.spec
}

func (o *Orchestrator) setFn(name string, f *spec.Function) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f == nil {
		delete(o.effective, name)
		return
	}
	o.effective[name] = f
}

// recordFailure keeps why a function's VM failed. It is called before the VM
// is destroyed: the console tail is read from the engine while the VM (and
// its log) still exists. vm is nil when the VM was never created.
func (o *Orchestrator) recordFailure(name string, f *spec.Function, vm *types.VMResponse, born time.Time, cause string, exit *int, output string) {
	rec := Failure{
		At:       time.Now().UTC().Format(time.RFC3339),
		Function: name,
		Mode:     f.Lifecycle.Mode,
		Image:    f.Image,
		Cause:    cause,
		Exit:     exit,
		AfterMS:  time.Since(born).Milliseconds(),
		Output:   tail(output),
	}
	if vm != nil {
		rec.VM, rec.VMID = vm.Name, vm.ID
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if data, err := o.eng.Console(ctx, vm.ID, ConsoleKept); err != nil {
			rec.Console = fmt.Sprintf("(console unavailable: %v)", err)
		} else {
			rec.Console = string(data)
		}
	}
	if o.state == nil {
		return
	}
	if err := o.state.RecordFailure(rec); err != nil {
		o.log.Printf("function %s: recording failure: %v", name, err)
	}
}

// owned is the selector for everything any project of the orchestrator
// created; mine narrows it to this one.
func owned() map[string]string { return map[string]string{LabelManagedBy: Owner} }

// mine reports whether an object the orchestrator created belongs to this
// project: its project label says so, or — made before projects existed — it
// has none and s declares it (a network by name, a VM by its function). Such
// an object is adopted: the next apply labels it. Everything else is another
// project's, never touched.
func (o *Orchestrator) mine(labels map[string]string, declared bool) bool {
	if labels[LabelManagedBy] != Owner {
		return false
	}
	if p, ok := labels[LabelProject]; ok {
		return p == o.project
	}
	return declared
}

// myVMs lists this project's VMs (s says which unlabelled ones it adopts).
func (o *Orchestrator) myVMs(ctx context.Context, s *spec.Spec) ([]types.VMResponse, error) {
	all, err := o.eng.ListVMs(ctx, owned())
	if err != nil {
		return nil, err
	}
	var out []types.VMResponse
	for _, v := range all {
		if o.mine(v.Labels, s.Functions[v.Labels[LabelFunction]] != nil) {
			out = append(out, v)
		}
	}
	return out, nil
}

// myNetworks lists this project's networks.
func (o *Orchestrator) myNetworks(ctx context.Context, s *spec.Spec) ([]types.NetworkResponse, error) {
	all, err := o.eng.ListNetworks(ctx, owned())
	if err != nil {
		return nil, err
	}
	var out []types.NetworkResponse
	for _, n := range all {
		if o.mine(n.Labels, s.Networks[n.Name] != nil) {
			out = append(out, n)
		}
	}
	return out, nil
}

// VMName is what a function's VM is called: <project>-<function>-<generation>,
// so two projects' functions of the same name never collide.
func (o *Orchestrator) VMName(function string, gen int) string {
	return fmt.Sprintf("%s-%s-%d", o.project, function, gen)
}

// SpecHash is the short digest of what a function's VM is born with: a change
// in any of it needs a new VM. Labels and health are changed without one.
func SpecHash(f *spec.Function) string {
	// Files by the digest of their content: a changed file (or secret) needs
	// a VM born with it, and the hash never carries the content itself.
	files := map[string]string{}
	for _, fs := range fileSpecs(f) {
		sum := sha256.Sum256(fs.Content)
		files[fs.Path] = fmt.Sprintf("%s %d %d %v %x", fs.Mode, fs.UID, fs.GID, fs.Secret, sum)
	}
	b, _ := json.Marshal(struct {
		Image, Network, IP, Command, Mode string
		Resources                         spec.Resources
		Files                             map[string]string `json:",omitempty"`
	}{f.Image, f.Network, f.IP, f.Command, f.Lifecycle.Mode, f.Resources, files})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// fileSpecs is what the engine writes into a function's disk before boot.
func fileSpecs(f *spec.Function) []types.FileSpec {
	var out []types.FileSpec
	for _, set := range []struct {
		files  map[string]*spec.File
		secret bool
	}{{f.Files, false}, {f.Secrets, true}} {
		paths := make([]string, 0, len(set.files))
		for p := range set.files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			src := set.files[p]
			out = append(out, types.FileSpec{Path: p, Content: src.Content, Mode: src.Mode, UID: src.UID, GID: src.GID, Secret: set.secret})
		}
	}
	return out
}

// functionLabels is the full label set of a function's VM.
func functionLabels(project, name string, f *spec.Function, gen int) map[string]string {
	l := make(map[string]string, len(f.Labels)+4)
	for k, v := range f.Labels {
		l[k] = v
	}
	l[LabelManagedBy] = Owner
	l[LabelProject] = project
	l[LabelFunction] = name
	l[LabelGeneration] = strconv.Itoa(gen)
	l[LabelSpec] = SpecHash(f)
	return l
}

// observeGenerations raises the generation counters past what the engine
// already holds, so a new instance never reuses a live VM's name.
func (o *Orchestrator) observeGenerations(vms []types.VMResponse) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, v := range vms {
		fn := v.Labels[LabelFunction]
		if g, err := strconv.Atoi(v.Labels[LabelGeneration]); err == nil && g > o.gen[fn] {
			o.gen[fn] = g
		}
	}
}

func (o *Orchestrator) nextGeneration(fn string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gen[fn]++
	return o.gen[fn]
}

// image resolves a function's image in the engine's store and checks that the
// store holds exactly the digest the spec pins.
func (o *Orchestrator) image(ctx context.Context, ref string) (*types.ImageResponse, error) {
	o.mu.Lock()
	img, ok := o.images[ref]
	o.mu.Unlock()
	if ok {
		return img, nil
	}
	at := strings.LastIndex(ref, "@sha256:")
	if at < 0 || len(ref)-at != len("@sha256:")+64 {
		// A function with build: whose image was not built (status, down).
		return nil, fmt.Errorf("image %q: not a pinned reference (a build: not built yet? plan or apply builds it)", ref)
	}
	img, err := o.eng.GetImage(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if want := ref[len(ref)-len("sha256:")-64:]; img.Digest != want {
		return nil, fmt.Errorf("image %s: the store has digest %s", ref, img.Digest)
	}
	o.mu.Lock()
	o.images[ref] = img
	o.mu.Unlock()
	return img, nil
}

// withImageDefaults fills what f leaves to its image (docs/orchestrator.md
// §4): an empty command takes the image's default command, and a persistent
// function without a health check the image's check. Idempotent: it fills
// f once, and a later call finds f complete. A cycle function must end up
// with a command, from the spec or from the image.
func withImageDefaults(f *spec.Function, img *types.ImageResponse) error {
	if f.Command == "" && img.Command != "" {
		f.Command = img.Command
		f.FromImage = append(f.FromImage, "command")
	}
	if f.Health == nil && img.Health != nil && f.Lifecycle.Mode == spec.ModePersistent {
		h := &spec.Health{Command: img.Health.Command, Failures: img.Health.Failures}
		for _, d := range []struct {
			v   string
			dst *spec.Duration
		}{{img.Health.Every, &h.Every}, {img.Health.Timeout, &h.Timeout}} {
			if d.v == "" {
				continue
			}
			v, err := time.ParseDuration(d.v)
			if err != nil || v <= 0 {
				return fmt.Errorf("image %s: health duration %q", f.Image, d.v)
			}
			*d.dst = spec.Duration(v)
		}
		h.SetDefaults()
		if h.Timeout >= h.Every {
			return fmt.Errorf("image %s: its health check's timeout %s is not shorter than every %s", f.Image, h.Timeout.D(), h.Every.D())
		}
		f.Health = h
		f.FromImage = append(f.FromImage, "health")
	}
	if f.Command == "" && f.Lifecycle.Mode != spec.ModePersistent {
		return fmt.Errorf("command: required for %s, and image %s declares none", f.Lifecycle.Mode, f.Image)
	}
	return nil
}

// resolveDefaults applies every function's image defaults to s, as far as
// the images resolve; the errors are those of the functions that could not
// be completed.
func (o *Orchestrator) resolveDefaults(ctx context.Context, s *spec.Spec) []error {
	var errs []error
	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		img, err := o.image(ctx, f.Image)
		if err == nil {
			err = withImageDefaults(f, img)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("functions.%s: %w", name, err))
		}
	}
	return errs
}

// memOf is what one VM of f is promised: the budget counts it, not what the
// guest happens to use.
func (o *Orchestrator) memOf(ctx context.Context, f *spec.Function) (int64, error) {
	if f.Resources.MemMB > 0 {
		return f.Resources.MemMB, nil
	}
	img, err := o.image(ctx, f.Image)
	if err != nil {
		return 0, err
	}
	return img.MemMB, nil
}

// byFunction groups owned VMs by their function label; quarantined VMs are
// left out — they are evidence, never serving and never reused.
func byFunction(vms []types.VMResponse) (serving map[string][]types.VMResponse, quarantined []types.VMResponse) {
	serving = map[string][]types.VMResponse{}
	for _, v := range vms {
		if v.Quarantine {
			quarantined = append(quarantined, v)
			continue
		}
		fn := v.Labels[LabelFunction]
		serving[fn] = append(serving[fn], v)
	}
	return serving, quarantined
}
