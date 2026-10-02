package build

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	s, err := Parse([]byte(`
base: alpine:3.22
packages: [nginx, "python3=3.12.11-r0"]
files:
  /srv/www/: out/
  /etc/nginx/http.d/default.conf: nginx.conf
run: ["mkdir -p /run/nginx"]
command: nginx -g 'daemon off;'
health: { command: "wget -qO- http://127.0.0.1/", every: 10s, timeout: 2s }
mem_mb: 64
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Kernel != DefaultKernel || s.VCPUs != 1 || s.MemMB != 64 {
		t.Errorf("defaults: kernel %q vcpus %d mem %d", s.Kernel, s.VCPUs, s.MemMB)
	}
	// files: in guest path order, then run:.
	want := []Op{
		{Guest: "/etc/nginx/http.d/default.conf", Source: "nginx.conf", Where: "files[/etc/nginx/http.d/default.conf]"},
		{Guest: "/srv/www/", Source: "out/", Where: "files[/srv/www/]"},
		{Run: "mkdir -p /run/nginx", Where: "run[0]"},
	}
	if !slices.Equal(s.Ops, want) {
		t.Errorf("Ops = %+v, want %+v", s.Ops, want)
	}

	// steps: as written.
	s, err = Parse([]byte(`
base: alpine:3.22
steps:
  - copy: {/opt/app/requirements.txt: app/requirements.txt}
  - run: pip install -r /opt/app/requirements.txt
  - copy: {/opt/app/: app/, /etc/app.conf: app.conf}
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, op := range s.Ops {
		got = append(got, op.Where)
	}
	if want := []string{"steps[0].copy[/opt/app/requirements.txt]", "steps[1].run", "steps[2].copy[/etc/app.conf]", "steps[2].copy[/opt/app/]"}; !slices.Equal(got, want) {
		t.Errorf("steps: Ops = %v, want %v", got, want)
	}

	for name, spec := range map[string]string{
		"unknown field":   "base: alpine:3.22\nimage: x\n",
		"no base":         "packages: [nginx]\n",
		"unknown base":    "base: debian:12\n",
		"unknown kernel":  "base: alpine:3.22\nkernel: 5.10\n",
		"bad package":     "base: alpine:3.22\npackages: [\"nginx; rm -rf /\"]\n",
		"relative guest":  "base: alpine:3.22\nfiles: {srv/x: x}\n",
		"guest ..":        "base: alpine:3.22\nfiles: {/srv/../etc/x: x}\n",
		"guest root":      "base: alpine:3.22\nfiles: {/: out/}\n",
		"absolute source": "base: alpine:3.22\nfiles: {/x: /etc/shadow}\n",
		"source ..":       "base: alpine:3.22\nfiles: {/x: ../secret}\n",
		"two lines":       "base: alpine:3.22\ncommand: \"a\\nb\"\n",
		"health no cmd":   "base: alpine:3.22\nhealth: {every: 10s}\n",
		"health timeout":  "base: alpine:3.22\nhealth: {command: x, every: 2s, timeout: 2s}\n",
		"small mem":       "base: alpine:3.22\nmem_mb: 16\n",
		"two documents":   "base: alpine:3.22\n---\nbase: alpine:3.22\n",
		"empty run":       "base: alpine:3.22\nrun: [\"  \"]\n",
		"steps and files": "base: alpine:3.22\nfiles: {/x: x}\nsteps: [{run: true}]\n",
		"steps and run":   "base: alpine:3.22\nrun: [true]\nsteps: [{run: true}]\n",
		"copy and run":    "base: alpine:3.22\nsteps: [{run: true, copy: {/x: x}}]\n",
		"empty step":      "base: alpine:3.22\nsteps: [{}]\n",
		"empty copy":      "base: alpine:3.22\nsteps: [{copy: {}}]\n",
		"step unknown":    "base: alpine:3.22\nsteps: [{cmd: true}]\n",
		"step bad guest":  "base: alpine:3.22\nsteps: [{copy: {x: x}}]\n",
		"step source ..":  "base: alpine:3.22\nsteps: [{copy: {/x: ../x}}]\n",
	} {
		if _, err := Parse([]byte(spec)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The pins here and in scripts/checksums.sha256 are one list kept twice.
func TestPinsMatchChecksums(t *testing.T) {
	f, err := os.Open("../../scripts/checksums.sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	file := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && !strings.HasPrefix(fields[0], "#") {
			file[fields[1]] = fields[0]
		}
	}
	for _, b := range Bases {
		for arch, sum := range b.SHA256 { // Alpine's; Ubuntu is verified by signature
			key := "alpine/" + arch + "/" + b.File(arch)
			if file[key] != sum {
				t.Errorf("%s: pinned %s here, %q in checksums.sha256", key, sum, file[key])
			}
		}
	}
	for v, byArch := range Kernels {
		for arch, sum := range byArch {
			key := "kernel/" + arch + "/vmlinux-" + v
			if file[key] != sum {
				t.Errorf("%s: pinned %s here, %q in checksums.sha256", key, sum, file[key])
			}
		}
	}
}

// The agent is the guest's side of the engine's exec protocol: the one the
// Alpine script installs.
func TestAgentMatchesScript(t *testing.T) {
	data, err := os.ReadFile("../../scripts/build-rootfs-alpine.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "<<'AGENT'\n")
	script, _, ok2 := strings.Cut(rest, "\nAGENT\n")
	if !ok || !ok2 {
		t.Fatal("no AGENT heredoc in build-rootfs-alpine.sh")
	}
	if script+"\n" != string(agent) {
		t.Error("guest/microhosted-exec differs from the agent in scripts/build-rootfs-alpine.sh")
	}
}

type call struct {
	name  string
	args  []string
	stdin string
}

func (c call) String() string { return c.name + " " + strings.Join(c.args, " ") }

// script is the shell script a sh -c call runs ("" if none).
func (c call) script() string {
	for i, a := range c.args {
		if a == "-c" && i+1 < len(c.args) {
			return c.args[i+1]
		}
	}
	return ""
}

// inImage is what a step call runs chrooted in the image: the script after
// env, and its arguments.
func (c call) inImage() (script string, args []string, ok bool) {
	if c.name != "unshare" || !slices.Contains(c.args, stepScript) {
		return "", nil, false
	}
	i := slices.Index(c.args, "/usr/bin/env")
	for j := i; j < len(c.args)-1; j++ {
		if c.args[j] == "-c" {
			return c.args[j+1], c.args[j+3:], true
		}
	}
	return "", nil, false
}

// fakeRunner stands in for root. Its layer store (keys) outlives a build, as
// the real one does: a second build with the same runner finds the layers
// the first one kept.
type fakeRunner struct {
	calls []call
	fail  string // a command line containing this fails
	work  string
	// installed maps what install wrote to its source, so sha256sum of the
	// copy hashes the real file; tamper makes every copy differ from it.
	installed map[string]string
	tamper    bool
	keys      map[string]string // layer key → "<id> <built>"
	devices   string            // stat -c %d's answer
}

func (f *fakeRunner) Run(_ context.Context, stdin io.Reader, name string, args ...string) error {
	c := call{name: name, args: args}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin = string(b)
	}
	f.calls = append(f.calls, c)
	if f.fail != "" && strings.Contains(c.String(), f.fail) {
		return fmt.Errorf("%s failed", name)
	}
	if name == "install" && len(args) >= 2 {
		if f.installed == nil {
			f.installed = map[string]string{}
		}
		f.installed[args[len(args)-1]] = args[len(args)-2]
	}
	if c.script() == commitScript {
		if f.keys == nil {
			f.keys = map[string]string{}
		}
		// layers id key dir built
		f.keys[args[5]] = args[4] + " " + args[7]
	}
	return nil
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	c := call{name: name, args: args}
	f.calls = append(f.calls, c)
	if f.fail != "" && strings.Contains(c.String(), f.fail) {
		return nil, fmt.Errorf("%s failed", name)
	}
	if c.script() == lookupScript {
		if v, ok := f.keys[args[len(args)-1]]; ok {
			return []byte(v + "\n"), nil
		}
		return nil, nil
	}
	if name == "unshare" && slices.Contains(args, finalScript) {
		switch {
		case slices.Contains(args, "du"):
			return []byte("40\t/merged\n"), nil
		case slices.Contains(args, "find"):
			return []byte(strings.Repeat(".", 1000)), nil
		}
	}
	switch name {
	case "mktemp":
		return []byte(f.work + "\n"), nil
	case "stat":
		if f.devices != "" {
			return []byte(f.devices), nil
		}
		return []byte("2049\n2049\n"), nil
	case "sha256sum":
		sum, err := hashFile(f.installed[args[0]])
		if err != nil {
			return nil, err
		}
		if f.tamper {
			sum = strings.Repeat("0", 64)
		}
		return []byte(sum + "  " + args[0] + "\n"), nil
	}
	return nil, nil // sh: the kernel is not installed yet
}

func (f *fakeRunner) find(sub string) []call {
	var out []call
	for _, c := range f.calls {
		if strings.Contains(c.String(), sub) {
			out = append(out, c)
		}
	}
	return out
}

// exact lists the calls whose command line is line.
func (f *fakeRunner) exact(line string) []call {
	var out []call
	for _, c := range f.calls {
		if c.String() == line {
			out = append(out, c)
		}
	}
	return out
}

// ran lists the scripts the steps ran in the image, in order.
func (f *fakeRunner) ran() []string {
	var out []string
	for _, c := range f.calls {
		if s, _, ok := c.inImage(); ok {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeRunner) reset() { f.calls = nil }

// pinnedFetch serves content whose hash it pins for the test's duration.
func pinnedFetch(t *testing.T) func(context.Context, string, string) error {
	t.Helper()
	content := []byte("pinned file")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	oldBase, oldKernels := Bases["alpine:3.22.0"], Kernels[DefaultKernel]
	b := oldBase
	b.SHA256 = map[string]string{"x86_64": hexSum}
	Bases["alpine:3.22.0"] = b
	Kernels[DefaultKernel] = map[string]string{"x86_64": hexSum}
	t.Cleanup(func() { Bases["alpine:3.22.0"], Kernels[DefaultKernel] = oldBase, oldKernels })
	return func(_ context.Context, _, dst string) error { return os.WriteFile(dst, content, 0o644) }
}

func testOptions(t *testing.T, spec string) (Options, *fakeRunner) {
	t.Helper()
	ctxDir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ctxDir, "out/css"), 0o755))
	must(t, os.WriteFile(filepath.Join(ctxDir, "out/index.html"), []byte("<h1>hi</h1>"), 0o644))
	must(t, os.WriteFile(filepath.Join(ctxDir, "nginx.conf"), []byte("server {}"), 0o644))
	r := &fakeRunner{work: "/store/build/mh-build-abc123"}
	return Options{Spec: parse(t, spec), Context: ctxDir, Arch: "x86_64", Store: "/store", Kernels: "/store/kernels",
		CacheDir: t.TempDir(), Log: io.Discard, Runner: r, Fetch: pinnedFetch(t)}, r
}

func parse(t *testing.T, spec string) *Spec {
	t.Helper()
	s, err := Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const nginxSpec = `
base: alpine:3.22
packages: [nginx]
files:
  /srv/www/: out/
  /etc/nginx/http.d/default.conf: nginx.conf
run: ["mkdir -p /run/nginx"]
`

func TestBuildSteps(t *testing.T) {
	o, r := testOptions(t, nginxSpec)
	res, err := Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	work := "/store/build/mh-build-abc123"
	if res.Rootfs != work+"/rootfs.ext4" || res.Kernel != "/store/kernels/vmlinux-6.1.102" {
		t.Errorf("result = %+v", res)
	}
	if len(r.find("install -D -o root -g root -m 0644")) != 1 {
		t.Error("the missing kernel was not installed")
	}
	scaffold := work + "/scaffold"
	for _, c := range r.calls {
		switch c.name {
		case "cp":
			// The host side writes only into the build's own scaffold;
			// everything else goes into the image through the chroot.
			if dst := c.args[len(c.args)-1]; !strings.HasPrefix(dst, scaffold+staging+"/") {
				t.Errorf("host-side copy into the image: %s", c)
			}
		case "tar":
			if c.args[len(c.args)-1] != work+"/steps/0/upper" {
				t.Errorf("the base was unpacked into %s", c.args[len(c.args)-1])
			}
		case "unshare":
			// Every step in its own namespaces, the scaffold on top.
			if slices.Contains(c.args, stepScript) {
				if !slices.Equal(c.args[:4], []string{"--mount", "--uts", "--ipc", "--pid"}) || !slices.Contains(c.args, superviseScript) {
					t.Errorf("a step not isolated: %s", c)
				}
				if lower := c.args[slices.Index(c.args, stepScript)+3]; !strings.HasPrefix(lower, scaffold+":") {
					t.Errorf("lowerdir = %s, want the scaffold on top", lower)
				}
			}
		}
	}
	want := []string{
		alpine.prepare, alpine.install,
		`mkdir -p /usr/local/bin && cat > /usr/local/bin/microhosted-exec && chmod 0755 /usr/local/bin/microhosted-exec`, alpine.configure,
		`mkdir -p "$(dirname "$2")" && cp -a "$1" "$2"`, `mkdir -p "$2" && cp -a "$1"/. "$2"/`, // files in path order
		"mkdir -p /run/nginx",
		alpine.cleanup + ` && rm -f /etc/resolv.conf && ln -s /proc/net/pnp /etc/resolv.conf`,
	}
	if got := r.ran(); !slices.Equal(got, want) {
		t.Errorf("steps ran\n%q\nwant\n%q", got, want)
	}
	for _, c := range r.calls {
		if s, args, ok := c.inImage(); ok && s == alpine.install && strings.Join(args, " ") != "socat nginx" {
			t.Errorf("apk add %v", args)
		}
	}
	if a := r.find("/usr/local/bin/microhosted-exec"); len(a) != 1 || a[0].stdin != string(agent) {
		t.Error("the agent was not installed")
	}
	if len(r.keys) != 7 {
		t.Errorf("%d layers kept, want 7 (FROM, PREPARE, PACKAGES, AGENT, 2 COPY, RUN)", len(r.keys))
	}
	mkfs := r.find("mkfs.ext4")
	if len(mkfs) != 1 || mkfs[0].name != "unshare" || !slices.Contains(mkfs[0].args, finalScript) ||
		!strings.Contains(mkfs[0].String(), "-d "+work+"/finish/merged") || !strings.HasSuffix(mkfs[0].String(), " 82M") {
		t.Errorf("mkfs = %v (want 40 MB used → 82M, on the finished overlay)", mkfs)
	}
	// The image is the layers and FINISH's changes, without the scaffold.
	if lower := mkfs[0].args[slices.Index(mkfs[0].args, finalScript)+3]; !strings.HasPrefix(lower, work+"/finish/upper:") || strings.Contains(lower, "scaffold") {
		t.Errorf("the image's lowerdir = %s", lower)
	}
	for _, dir := range []string{"steps", "finish", "scaffold"} {
		if len(r.exact("rm -rf --one-file-system "+work+"/"+dir)) != 1 {
			t.Errorf("%s was not removed", dir)
		}
	}
	must(t, res.Cleanup(context.Background()))
	if n := len(r.exact("rm -rf --one-file-system " + work)); n != 1 {
		t.Error("Cleanup did not remove the working directory")
	}
}

// A second build takes every layer it can: a change reruns its step and the
// ones after it, nothing before.
func TestBuildCache(t *testing.T) {
	o, r := testOptions(t, nginxSpec)
	_, err := Build(context.Background(), o)
	must(t, err)

	r.reset()
	_, err = Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 1 || !strings.Contains(got[0], "/proc/net/pnp") {
		t.Errorf("unchanged, the build ran %q: want only FINISH", got)
	}
	if len(r.find("tar -xzf")) != 0 {
		t.Error("the base was unpacked again")
	}

	// A new run step: only it.
	r.reset()
	o.Spec = parse(t, strings.Replace(nginxSpec, `run: ["mkdir -p /run/nginx"]`, `run: ["mkdir -p /run/nginx", "nginx -t"]`, 1))
	_, err = Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 2 || got[0] != "nginx -t" {
		t.Errorf("with a new run step, the build ran %q", got)
	}

	// A changed file: its COPY and what follows. The config, copied
	// before it, is not copied again.
	r.reset()
	must(t, os.WriteFile(filepath.Join(o.Context, "out/index.html"), []byte("<h1>v2</h1>"), 0o644))
	_, err = Build(context.Background(), o)
	must(t, err)
	want := []string{`mkdir -p "$2" && cp -a "$1"/. "$2"/`, "mkdir -p /run/nginx", "nginx -t"}
	if got := r.ran(); len(got) != 4 || !slices.Equal(got[:3], want) {
		t.Errorf("with a changed file, the build ran %q", got)
	}
	if cp := r.find("cp -a --no-preserve=ownership"); len(cp) != 1 || !strings.Contains(cp[0].String(), "/out ") {
		t.Errorf("staged %v: want only the changed source", cp)
	}

	// VM defaults are not in the tree: every layer is taken.
	r.reset()
	o.Spec.MemMB, o.Spec.Command = 256, "nginx"
	_, err = Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 1 {
		t.Errorf("with new VM defaults, the build ran %q", got)
	}
}

// steps: run in the order written, so a dependency list and its install,
// before the code, survive a change to the code.
func TestBuildOrderedSteps(t *testing.T) {
	o, r := testOptions(t, `
base: alpine:3.22
steps:
  - copy: {/opt/app/deps.txt: app/deps.txt}
  - run: install-deps /opt/app/deps.txt
  - copy: {/opt/app/: app/}
  - run: check /opt/app
`)
	must(t, os.MkdirAll(filepath.Join(o.Context, "app"), 0o755))
	must(t, os.WriteFile(filepath.Join(o.Context, "app/deps.txt"), []byte("pg\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(o.Context, "app/main.js"), []byte("v1"), 0o644))
	_, err := Build(context.Background(), o)
	must(t, err)
	fileCopy, dirCopy := `mkdir -p "$(dirname "$2")" && cp -a "$1" "$2"`, `mkdir -p "$2" && cp -a "$1"/. "$2"/`
	if got := r.ran(); len(got) != 9 || !slices.Equal(got[4:8], []string{fileCopy, "install-deps /opt/app/deps.txt", dirCopy, "check /opt/app"}) {
		t.Errorf("ran %q", got)
	}

	// The code changes: the install is taken from the cache.
	r.reset()
	must(t, os.WriteFile(filepath.Join(o.Context, "app/main.js"), []byte("v2"), 0o644))
	_, err = Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 3 || got[0] != dirCopy || got[1] != "check /opt/app" {
		t.Errorf("after a code change, ran %q: want the last COPY, its check and FINISH", got)
	}

	// The dependencies change: the install runs again, and what follows.
	r.reset()
	must(t, os.WriteFile(filepath.Join(o.Context, "app/deps.txt"), []byte("pg\nexpress\n"), 0o644))
	_, err = Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 5 || got[1] != "install-deps /opt/app/deps.txt" {
		t.Errorf("after a dependency change, ran %q", got)
	}
}

// The same source copied twice is staged afresh each time, never into the
// copy staged before.
func TestBuildSameCopyTwice(t *testing.T) {
	o, r := testOptions(t, "base: alpine:3.22\nsteps:\n  - copy: {/srv/www/: out/}\n  - run: rm -rf /srv/www\n  - copy: {/srv/www/: out/}\n")
	_, err := Build(context.Background(), o)
	must(t, err)
	cps := r.find("cp -a --no-preserve=ownership")
	if len(cps) != 2 || cps[0].String() != cps[1].String() {
		t.Fatalf("staged %v", cps)
	}
	dst := cps[0].args[len(cps[0].args)-1]
	if len(r.exact("rm -rf --one-file-system "+dst)) != 2 {
		t.Error("the second copy was staged over the first")
	}
}

// A layer is reused for DefaultLayerMaxAge at most; after that, and with
// NoCache, the build starts from the base again — and the layers on the new
// base are new too (keys chain on the parent's id).
func TestBuildLayerAge(t *testing.T) {
	o, r := testOptions(t, nginxSpec)
	_, err := Build(context.Background(), o)
	must(t, err)
	first := maps.Clone(r.keys)

	defer func(old func() time.Time) { now = old }(now)
	now = func() time.Time { return time.Now().Add(DefaultLayerMaxAge + time.Hour) }
	r.reset()
	_, err = Build(context.Background(), o)
	must(t, err)
	if len(r.find("tar -xzf")) != 1 || len(r.ran()) != 8 {
		t.Errorf("expired layers were reused: ran %q", r.ran())
	}
	if n := newKeys(first, r.keys); n != 6 {
		t.Errorf("%d new keys on the rebuilt base, want 6: the steps above it", n)
	}

	now = time.Now
	second := maps.Clone(r.keys)
	r.reset()
	o.NoCache = true
	_, err = Build(context.Background(), o)
	must(t, err)
	if len(r.find(lookupScript)) != 0 || len(r.ran()) != 8 {
		t.Errorf("NoCache looked up layers or skipped steps: ran %q", r.ran())
	}
	if n := newKeys(second, r.keys); n != 6 {
		t.Errorf("%d new keys after NoCache, want 6", n)
	}
}

func newKeys(before, after map[string]string) int {
	n := 0
	for k := range after {
		if _, ok := before[k]; !ok {
			n++
		}
	}
	return n
}

// The store's answer is checked: a malformed id is a miss, never a path.
func TestBuildRefusesMalformedLayer(t *testing.T) {
	o, r := testOptions(t, "base: alpine:3.22\n")
	_, err := Build(context.Background(), o)
	must(t, err)
	for k := range r.keys {
		r.keys[k] = "../../etc 1700000000"
	}
	r.reset()
	_, err = Build(context.Background(), o)
	must(t, err)
	if len(r.find("tar -xzf")) != 1 {
		t.Error("a malformed layer entry was taken")
	}
	for _, c := range r.calls {
		if strings.Contains(c.String(), "../../etc") {
			t.Errorf("the malformed id reached a command: %s", c)
		}
	}
}

// A failed step keeps the layers before it: the next build starts there.
func TestBuildFailureCleansUp(t *testing.T) {
	o, r := testOptions(t, "base: alpine:3.22\nrun: [\"false\"]\n")
	r.fail = `-c false`
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), `RUN false`) {
		t.Fatalf("Build = %v, want the run step's failure", err)
	}
	if len(r.find("rm -rf --one-file-system /store/build/mh-build-abc123")) != 1 {
		t.Error("a failed build left its working directory")
	}
	if len(r.keys) != 4 {
		t.Errorf("%d layers kept, want the 4 before the failed step", len(r.keys))
	}
	r.reset()
	r.fail = ""
	_, err := Build(context.Background(), o)
	must(t, err)
	if got := r.ran(); len(got) != 2 || got[0] != "false" {
		t.Errorf("after a failure, the build ran %q: want the failed step and FINISH", got)
	}
}

// What the overlays need of the store: a path overlayfs's options can
// carry, and the layers on the working directory's filesystem.
func TestBuildChecksStore(t *testing.T) {
	for _, store := range []string{"/srv/a:b", "/srv/a,upperdir=/", "srv", "/srv/../etc"} {
		o, _ := testOptions(t, "base: alpine:3.22\n")
		o.Store = store
		if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "store directory") {
			t.Errorf("store %q: %v", store, err)
		}
	}
	o, r := testOptions(t, "base: alpine:3.22\n")
	r.devices = "2049\n2050\n"
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "different filesystems") {
		t.Errorf("layers on another filesystem: %v", err)
	}
}

func TestBuildTooManySteps(t *testing.T) {
	run := strings.Repeat(`"true", `, maxLayers)
	o, _ := testOptions(t, "base: alpine:3.22\nrun: ["+run+"\"true\"]\n")
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("Build = %v", err)
	}
}

func TestBuildRefusesSourcesOutsideContext(t *testing.T) {
	o, _ := testOptions(t, "base: alpine:3.22\nfiles: {/x: link}\n")
	must(t, os.Symlink("/etc/hostname", filepath.Join(o.Context, "link")))
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "outside the build context") {
		t.Errorf("Build = %v, want outside the build context", err)
	}
	o, _ = testOptions(t, "base: alpine:3.22\nfiles: {/srv/www: out}\n")
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "/srv/www/") {
		t.Errorf("Build = %v, want the hint to write the directory's guest path with a slash", err)
	}
}

func TestBuildRefusesWrongDownload(t *testing.T) {
	o, _ := testOptions(t, "base: alpine:3.22\n")
	o.Fetch = func(_ context.Context, _, dst string) error { return os.WriteFile(dst, []byte("tampered"), 0o644) }
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not the pinned file") {
		t.Errorf("Build = %v, want the pin refused", err)
	}
}

// A download that changes after its check in the user's cache is caught by
// root's hash of its own copy, and nothing reads the cache's file as root.
func TestBuildChecksRootCopy(t *testing.T) {
	o, r := testOptions(t, "base: alpine:3.22\n")
	r.tamper = true
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "changed after it was checked") {
		t.Fatalf("Build = %v, want the copy refused", err)
	}
	o, r = testOptions(t, "base: alpine:3.22\n")
	if _, err := Build(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if (c.name == "tar" || c.name == "install" && strings.Contains(c.String(), "-o root")) && strings.Contains(c.String(), o.CacheDir) {
			t.Errorf("root reads the user's cache directly: %s", c)
		}
	}
}

// Keys: the same step on another parent, or with other input, is another
// key; the log's description is not the only thing that counts.
func TestStepKey(t *testing.T) {
	s := step{desc: "RUN x", actions: []action{{Script: "x"}}}
	k := s.key("x86_64", "a")
	if s.key("x86_64", "b") == k || s.key("aarch64", "a") == k {
		t.Error("the parent or the arch does not change the key")
	}
	s2 := s
	s2.actions = []action{{Script: "x", Stdin: "other"}}
	if s2.key("x86_64", "a") == k {
		t.Error("stdin does not change the key")
	}
	s3 := s
	s3.inputs = "tree"
	if s3.key("x86_64", "a") == k || !layerKeyRE.MatchString(k) {
		t.Error("inputs do not change the key")
	}
}

func TestPrune(t *testing.T) {
	old, stale, gone, super := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	fresh := strings.Repeat("e", 32)
	k := func(c string) string { return strings.Repeat(c, 64) }
	t0 := time.Now()
	list := fmt.Sprintf("layer %s %d\nlayer %s %d\nlayer %s %d\nlayer %s %d\nlayer ../x 0\nkey %s %s\nkey %s %s\nkey %s %s\nkey %s %s\n",
		old, t0.Add(-8*24*time.Hour).Unix(), stale, t0.Add(-2*time.Hour).Unix(), fresh, t0.Unix(), super, t0.Add(-2*time.Hour).Unix(),
		k("1"), old, k("2"), fresh, k("3"), gone, k("4"), stale)
	r := &listRunner{out: list}
	res, err := Prune(context.Background(), r, "/store", PruneOptions{})
	must(t, err)
	// old: unused for 8 days; super: no key points at it and unused for 2 h;
	// stale: 2 h but current; fresh: kept.
	var rm []string
	for _, c := range r.calls {
		rm = append(rm, c.String())
	}
	want := []string{
		"rm -rf --one-file-system /store/build/layers/" + old,
		"rm -rf --one-file-system /store/build/layers/" + super,
	}
	if res.Layers != 2 || !slices.Equal(rm[:2], want) {
		t.Errorf("removed %v", rm)
	}
	slices.Sort(rm)
	if !slices.Contains(rm, "rm -f /store/build/layers/keys/"+k("1")) || !slices.Contains(rm, "rm -f /store/build/layers/keys/"+k("3")) || len(rm) != 4 {
		t.Errorf("keys removed: %v: want the old layer's and the missing one's", rm)
	}

	r = &listRunner{out: list}
	res, err = Prune(context.Background(), r, "/store", PruneOptions{All: true})
	must(t, err)
	if res.Layers != 4 || !res.Caches || len(r.find("rm -rf --one-file-system /store/build/cache")) != 1 {
		t.Errorf("All: %+v, %v", res, r.calls)
	}
}

type listRunner struct {
	fakeRunner
	out string
}

func (l *listRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return []byte(l.out), nil
}

// The image specs the orchestrator's examples build stay valid.
func TestExampleImageSpecs(t *testing.T) {
	files, _ := filepath.Glob("../../orchestrator/examples/*/build.yml")
	more, _ := filepath.Glob("../../orchestrator/examples/*/*/build.yml") // stack/db, classroom/dns…
	files = append(files, more...)
	if len(files) < 7 {
		t.Fatalf("example image specs found: %v", files)
	}
	for _, f := range files {
		s, err := Load(f)
		if err != nil {
			t.Error(err)
			continue
		}
		// Every file source is there, inside the context (the spec's
		// directory, mh build's default).
		if _, err := resolveSources(filepath.Dir(f), s); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// An Ubuntu base: debootstrap (minbase) verified by the keyring in its own
// namespaces with root's cache, apt with services kept from starting and
// its downloads in the package cache, systemd starting the agent.
func TestBuildUbuntu(t *testing.T) {
	keyring := filepath.Join(t.TempDir(), "ubuntu-archive-keyring.gpg")
	must(t, os.WriteFile(keyring, []byte("keys"), 0o644))
	oldKeyring, oldHave := ubuntuKeyring, haveDebootstrap
	t.Cleanup(func() { ubuntuKeyring, haveDebootstrap = oldKeyring, oldHave })
	ubuntuKeyring, haveDebootstrap = keyring, func() bool { return true }

	o, r := testOptions(t, "base: ubuntu:noble\npackages: [nginx-light]\nrun: [\"true\"]\n")
	if _, err := Build(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	d := r.find("debootstrap --keyring=" + keyring)
	if len(d) != 1 || d[0].name != "unshare" || !slices.Contains(d[0].args, superviseScript) ||
		!strings.Contains(d[0].String(), "--variant=minbase --include=systemd-sysv,udev") ||
		!strings.Contains(d[0].String(), "--cache-dir=/store/build/cache/debootstrap-noble-amd64 noble "+r.work+"/steps/0/upper http://archive.ubuntu.com/ubuntu") {
		t.Errorf("debootstrap = %v", d)
	}
	ran := r.ran()
	if len(ran) < 3 || ran[0] != ubuntu.prepare || ran[1] != ubuntu.install {
		t.Fatalf("ran %q", ran)
	}
	for _, c := range r.calls {
		s, args, ok := c.inImage()
		switch {
		case !ok:
		case s == ubuntu.prepare:
			if !strings.Contains(c.stdin, "noble-security main universe") || !slices.Contains(c.args, "DEBIAN_FRONTEND=noninteractive") {
				t.Errorf("prepare: %s", c)
			}
		case s == ubuntu.install:
			if strings.Join(args, " ") != "socat nginx-light" {
				t.Errorf("apt-get install %v", args)
			}
		}
		if c.name == "unshare" && slices.Contains(c.args, stepScript) && c.args[slices.Index(c.args, stepScript)+7] != "/store/build/cache/ubuntu-24.04-x86_64" {
			t.Errorf("package cache: %s", c)
		}
	}
	for _, want := range []string{"policy-rc.d", "force-unsafe-io", `Dir::Cache::archives "/.mh-cache/apt/"`} {
		if !strings.Contains(ubuntu.prepare, want) {
			t.Errorf("prepare does not set %s", want)
		}
	}
	for _, f := range []string{"policy-rc.d", "00mh-build"} {
		if !strings.Contains(ubuntu.cleanup, f) {
			t.Errorf("FINISH leaves %s in the image", f)
		}
	}
	if len(r.find("127.0.1.1")) != 1 {
		t.Error("the hostname does not resolve locally: every getfqdn() waits for DNS")
	}
	if len(r.find("systemctl enable microhosted-exec.service")) != 1 || len(r.find("/etc/inittab")) != 0 {
		t.Error("the agent must be a systemd unit on Ubuntu")
	}

	// A new keyring is a new base.
	first := maps.Clone(r.keys)
	must(t, os.WriteFile(keyring, []byte("new keys"), 0o644))
	r.reset()
	_, err := Build(context.Background(), o)
	must(t, err)
	if n := newKeys(first, r.keys); len(r.find("debootstrap --keyring")) != 1 || n != 5 {
		t.Errorf("a new keyring reused the base built with the old one (%d new keys)", n)
	}

	haveDebootstrap = func() bool { return false }
	o, _ = testOptions(t, "base: ubuntu:24.04\n")
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "apt-get install debootstrap") {
		t.Errorf("without debootstrap: %v", err)
	}
}

// The fingerprint follows what the build takes in — the parsed spec and the
// copied files — and nothing else: not comments, not the image's name.
func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "site/css"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "site/index.html"), []byte("v1"), 0o644))
	fp := func(spec string) string {
		t.Helper()
		s, err := Parse([]byte(spec))
		if err != nil {
			t.Fatal(err)
		}
		f, err := Fingerprint(s, dir, "x86_64")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := fp("base: alpine:3.22\nfiles: {/srv/www/: site/}\n")
	if got := fp("# a comment\nname: other\nbase:   alpine:3.22\nfiles:\n  /srv/www/: site/\n"); got != base {
		t.Error("a comment, spacing or the name changed the fingerprint")
	}
	if fp("base: alpine:3.22\nfiles: {/srv/www/: site/}\npackages: [nginx]\n") == base {
		t.Error("a package did not change the fingerprint")
	}
	must(t, os.WriteFile(filepath.Join(dir, "site/css/a.css"), []byte("x"), 0o644))
	if fp("base: alpine:3.22\nfiles: {/srv/www/: site/}\n") == base {
		t.Error("a new file in a copied directory did not change the fingerprint")
	}
	// A new pin behind the same base name is a new image.
	old := Bases["alpine:3.22.0"]
	b := old
	b.SHA256 = map[string]string{"x86_64": strings.Repeat("ab", 32)}
	Bases["alpine:3.22.0"] = b
	repinned := fp("base: alpine:3.22\nfiles: {/srv/www/: site/}\n")
	Bases["alpine:3.22.0"] = old
	if repinned == fp("base: alpine:3.22\nfiles: {/srv/www/: site/}\n") {
		t.Error("a new base pin did not change the fingerprint")
	}
	// steps: the order counts; files: and the steps it means are one spec.
	ordered := fp("base: alpine:3.22\nsteps:\n  - copy: {/srv/www/: site/}\n  - run: a\n")
	if fp("base: alpine:3.22\nsteps:\n  - run: a\n  - copy: {/srv/www/: site/}\n") == ordered {
		t.Error("the order of steps does not change the fingerprint")
	}
	if v := CacheVersion(base); !strings.HasPrefix(v, "sha-") || len(v) != 16 {
		t.Errorf("CacheVersion = %q", v)
	}
	for in, want := range map[string]string{"/x/web-alpine": "web-alpine", "/x/My Site_2": "my-site-2", "/x/---": "image"} {
		if got := DefaultName(in); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", in, got, want)
		}
	}
}
