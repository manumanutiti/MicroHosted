package build

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// agent is the guest's vsock listener: the same script
// scripts/build-rootfs-alpine.sh installs (a test keeps the two equal).
//
//go:embed guest/microhosted-exec
var agent []byte

// AgentPort is the vsock port the engine's exec, cp and health checks reach.
const AgentPort = 52

// Runner runs the build's commands as root.
type Runner interface {
	// Run runs name with args; stdin may be nil.
	Run(ctx context.Context, stdin io.Reader, name string, args ...string) error
	// Output runs name with args and returns its standard output.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Options is one build.
type Options struct {
	Spec *Spec
	// Context is the directory Spec's file sources are relative to.
	Context string
	// Arch is the image's architecture: the engine host's (x86_64, aarch64).
	Arch string
	// Store is the engine's instances directory: the build writes its
	// rootfs under Store/build, where the engine imports from.
	Store string
	// Kernels is the engine's kernel directory; a missing kernel is
	// installed there.
	Kernels string
	// CacheDir keeps downloads between builds (the user's: the pinned
	// ones are hashed again in root's copy).
	CacheDir string
	// Log receives one line per step.
	Log    io.Writer
	Runner Runner
	// Fetch downloads url to dst; nil uses HTTP.
	Fetch func(ctx context.Context, url, dst string) error
	// NoCache runs every step, taking no layer from the store (the new
	// layers replace the old ones for the next build).
	NoCache bool
	// MaxAge is how long a layer is reused; 0 takes DefaultLayerMaxAge.
	MaxAge time.Duration
}

// Result is a finished build: files under the engine's store, ready to
// import. Cleanup removes them once imported (the store keeps its own copy).
type Result struct {
	Kernel  string
	Rootfs  string
	cleanup func(context.Context) error
}

// NewResult is a result whose Cleanup calls cleanup (for callers' tests).
func NewResult(kernel, rootfs string, cleanup func(context.Context) error) *Result {
	return &Result{Kernel: kernel, Rootfs: rootfs, cleanup: cleanup}
}

// Cleanup removes the build's working directory.
func (r *Result) Cleanup(ctx context.Context) error {
	return r.cleanup(ctx)
}

// staging is where a COPY's source is put, in the build's scaffold, before a
// copy run in the chroot puts it in place: a path in the image is resolved
// by the chroot, never by the host, so a symlink in the image (Alpine's
// /var/run → /run) cannot send a write to the host's own directories.
const staging = "/.mh-build"

// Build builds o.Spec, one layer per step (layers.go), taking every step
// the layer store already has for the same inputs. Every write into the
// image after the base is laid down happens inside the chroot; run steps
// execute as root on the build host, which a chroot does not confine — build
// only specs you trust, as with any Dockerfile.
func Build(ctx context.Context, o Options) (res *Result, err error) {
	s := o.Spec
	if o.Fetch == nil {
		o.Fetch = httpFetch
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultLayerMaxAge
	}
	if !safePathRE.MatchString(o.Store) || strings.Contains(o.Store, "..") {
		return nil, fmt.Errorf("store directory %q: the build's overlays need a path of letters, digits and . _ - /", o.Store)
	}
	sources, err := resolveSources(o.Context, s)
	if err != nil {
		return nil, err
	}
	base, err := resolveBase(s.Base)
	if err != nil {
		return nil, err
	}
	d := distros[base.Distro]
	if err := d.check(base, o); err != nil {
		return nil, err
	}
	kernelSum, ok := Kernels[s.Kernel][o.Arch]
	if !ok {
		return nil, fmt.Errorf("kernel %s has no pinned build for %s", s.Kernel, o.Arch)
	}
	r := o.Runner
	if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
		return nil, err
	}

	buildDir := filepath.Join(o.Store, "build")
	b := &builder{ctx: ctx, r: r, o: o, base: base,
		layers: layersDir(o.Store), pkgCache: pkgCacheDir(o.Store, base, o.Arch), maxAge: o.MaxAge}
	for _, dir := range []string{buildDir, b.layers, filepath.Join(buildDir, "cache")} {
		if err := r.Run(ctx, nil, "mkdir", "-p", "-m", "0700", dir); err != nil {
			return nil, err
		}
	}
	// The package cache is reached through build/ (0700); inside the
	// chroot, the distro's download user (apt's _apt) needs to enter it.
	if err := r.Run(ctx, nil, "mkdir", "-p", "-m", "0755", b.pkgCache); err != nil {
		return nil, err
	}
	out, err := r.Output(ctx, "mktemp", "-d", filepath.Join(buildDir, "mh-build-XXXXXX"))
	if err != nil {
		return nil, err
	}
	work := strings.TrimSpace(string(out))
	if !strings.HasPrefix(work, buildDir+"/mh-build-") || !safePathRE.MatchString(work) {
		return nil, fmt.Errorf("mktemp returned %q", work)
	}
	b.work = work
	defer func() {
		// The steps' mounts live in namespaces that end with them; anything
		// still mounted under the work directory would make it impossible
		// to remove, so it goes first, on every path.
		if uerr := unmountUnder(context.WithoutCancel(ctx), r, work); uerr != nil && err == nil {
			err = uerr
		}
		if err != nil {
			if cerr := removeWork(context.WithoutCancel(ctx), r, work); cerr != nil {
				fmt.Fprintf(o.Log, "cleaning up %s: %v\n", work, cerr)
			}
		}
	}()
	// The layers are moved in from the work directory and stacked with
	// it: one filesystem, or overlayfs's inode numbers — which mkfs uses to
	// find hard links — could collide between layers.
	if out, err := r.Output(ctx, "stat", "-L", "-c", "%d", b.layers, work); err != nil {
		return nil, err
	} else if f := strings.Fields(string(out)); len(f) != 2 || f[0] != f[1] {
		return nil, fmt.Errorf("%s and %s are on different filesystems: the build's layers need one", b.layers, work)
	}
	if err := b.makeScaffold(len(sources) > 0); err != nil {
		return nil, err
	}

	steps, err := b.steps(d, sources)
	if err != nil {
		return nil, err
	}
	if len(steps) > maxLayers {
		return nil, fmt.Errorf("%d copies and commands: a build takes at most %d", len(steps)-4, maxLayers-4)
	}
	total := len(steps) + 3
	n := 0
	stepLog := func(format string, a ...any) {
		n++
		fmt.Fprintf(o.Log, "Step %d/%d : %s\n", n, total, fmt.Sprintf(format, a...))
	}

	stepLog("KERNEL %s", s.Kernel)
	kernel := filepath.Join(o.Kernels, "vmlinux-"+s.Kernel)
	if err := b.installKernel(kernel, KernelURL(s.Kernel, o.Arch), kernelSum); err != nil {
		return nil, err
	}

	var chain []layer
	parent := ""
	for i, st := range steps {
		stepLog("%s", st.desc)
		key := st.key(o.Arch, parent)
		if !o.NoCache {
			l, ok, err := b.lookup(key)
			if err != nil {
				return nil, err
			}
			if ok {
				fmt.Fprintf(o.Log, " ---> Using cache (%s, built %s)\n", l.id[:12], l.built.Format("2006-01-02 15:04"))
				chain, parent = append(chain, l), l.id
				continue
			}
		}
		dir := filepath.Join(work, "steps", strconv.Itoa(i))
		if st.from != nil {
			upper := filepath.Join(dir, "upper")
			if err := r.Run(ctx, nil, "mkdir", "-p", "-m", "0755", upper); err != nil {
				return nil, err
			}
			if err := st.from(upper); err != nil {
				return nil, err
			}
		} else {
			if st.stage != nil {
				if err := st.stage(); err != nil {
					return nil, fmt.Errorf("%s: %w", st.desc, err)
				}
			}
			if err := b.runStep(chain, dir, st.actions); err != nil {
				return nil, fmt.Errorf("%s: %w", st.desc, err)
			}
		}
		l, err := b.commit(key, filepath.Join(dir, "upper"))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(o.Log, " ---> %s\n", l.id[:12])
		chain, parent = append(chain, l), l.id
	}

	// FINISH is never kept: it undoes what the steps needed (the package
	// cache's configuration, the indexes) and gives the image the DNS the
	// engine hands out — the kernel's ip= writes the nameservers to
	// /proc/net/pnp.
	stepLog("FINISH")
	finish := filepath.Join(work, "finish")
	if err := b.runStep(chain, finish, []action{{
		Script: d.cleanup + ` && rm -f /etc/resolv.conf && ln -s /proc/net/pnp /etc/resolv.conf`,
	}}); err != nil {
		return nil, fmt.Errorf("finishing the tree: %w", err)
	}
	top, merged := filepath.Join(finish, "upper"), filepath.Join(finish, "merged")

	sizeMB, inodes, err := b.measure(top, merged, chain, s.SizeMB)
	if err != nil {
		return nil, err
	}
	stepLog("ROOTFS ext4, %d MB", sizeMB)
	rootfs := filepath.Join(work, "rootfs.ext4")
	// mkfs says it creates a missing file even with -q: the file exists.
	if err := r.Run(ctx, nil, "truncate", "-s", strconv.FormatInt(sizeMB, 10)+"M", rootfs); err != nil {
		return nil, err
	}
	if _, err := b.onFinal(ctx, top, merged, chain, false, "mkfs.ext4", "-q", "-F", "-L", "microhosted", "-N", strconv.FormatInt(inodes, 10),
		"-E", "root_owner=0:0", "-d", "$MERGED", rootfs, strconv.FormatInt(sizeMB, 10)+"M"); err != nil {
		return nil, fmt.Errorf("writing the ext4 (size_mb %d too small?): %w", sizeMB, err)
	}
	if err := r.Run(ctx, nil, "chmod", "0644", rootfs); err != nil {
		return nil, err
	}
	for _, dir := range []string{filepath.Join(work, "steps"), finish, filepath.Join(work, "scaffold")} {
		if err := removeTree(ctx, r, dir); err != nil {
			return nil, err
		}
	}
	return NewResult(kernel, rootfs, func(ctx context.Context) error { return removeWork(ctx, r, work) }), nil
}

// steps are the spec's layers: FROM, PREPARE (the base brought up to date),
// PACKAGES, AGENT, then one per file and one per run command.
func (b *builder) steps(d distro, sources map[int]source) ([]step, error) {
	s := b.o.Spec
	from, err := d.fromInputs(b)
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", s.Base, err)
	}
	pins, err := json.Marshal(b.base)
	if err != nil {
		return nil, err
	}
	prep := action{Script: d.prepare, Env: d.env}
	if d.prepareStdin != nil {
		prep.Stdin = d.prepareStdin(b)
	}
	pkgs := append(append([]string{}, d.basePackages...), s.Packages...)
	steps := []step{
		{desc: fmt.Sprintf("FROM %s (%s)", s.Base, b.o.Arch), inputs: string(pins) + "\x00" + from,
			from: func(dir string) error { return d.bootstrap(b, dir) }},
		{desc: "PREPARE " + d.update, actions: []action{prep}},
		{desc: "PACKAGES " + strings.Join(pkgs, " "), actions: []action{{Script: d.install, Args: pkgs, Env: d.env}}},
		{desc: fmt.Sprintf("AGENT vsock:%d, %s", AgentPort, d.init), actions: []action{
			{Script: `mkdir -p /usr/local/bin && cat > /usr/local/bin/microhosted-exec && chmod 0755 /usr/local/bin/microhosted-exec`, Stdin: string(agent)},
			{Script: d.configure, Stdin: d.configureStdin},
		}},
	}
	for i, op := range s.Ops {
		if !op.IsCopy() {
			steps = append(steps, step{desc: "RUN " + op.Run, actions: []action{{Script: op.Run}}})
			continue
		}
		src := sources[i]
		// The staged name is the copy's own: the same copy keeps its key
		// when others are added before it.
		staged := path.Join(staging, sha256Hex([]byte(op.Guest + "\x00" + op.Source))[:16])
		h := sha256.New()
		if err := hashTree(h, src.path); err != nil {
			return nil, fmt.Errorf("%s: %w", op.Where, err)
		}
		var script string
		switch {
		case src.dir:
			script = `mkdir -p "$2" && cp -a "$1"/. "$2"/`
		case strings.HasSuffix(op.Guest, "/"):
			script = `mkdir -p "$2" && cp -a "$1" "$2/$3"`
		default:
			script = `mkdir -p "$(dirname "$2")" && cp -a "$1" "$2"`
		}
		dst := filepath.Join(b.scaffold, staged)
		steps = append(steps, step{
			desc:    fmt.Sprintf("COPY %s %s", op.Source, op.Guest),
			inputs:  hex.EncodeToString(h.Sum(nil)),
			actions: []action{{Script: script, Args: []string{staged, op.Guest, filepath.Base(src.path)}}},
			stage: func() error {
				// The same copy twice in steps: stage it afresh.
				if err := b.r.Run(b.ctx, nil, "rm", "-rf", "--one-file-system", dst); err != nil {
					return err
				}
				return b.r.Run(b.ctx, nil, "cp", "-a", "--no-preserve=ownership", src.path, dst)
			},
		})
	}
	return steps, nil
}

// makeScaffold writes the build's own top layer (see stepScript).
func (b *builder) makeScaffold(files bool) error {
	b.scaffold = filepath.Join(b.work, "scaffold")
	dirs := []string{b.scaffold, filepath.Join(b.scaffold, "etc"), filepath.Join(b.scaffold, "dev"), filepath.Join(b.scaffold, ".mh-cache")}
	if err := b.r.Run(b.ctx, nil, "mkdir", append([]string{"-m", "0755"}, dirs...)...); err != nil {
		return err
	}
	if err := b.r.Run(b.ctx, nil, "mkdir", "-m", "0555", filepath.Join(b.scaffold, "proc")); err != nil {
		return err
	}
	if files {
		if err := b.r.Run(b.ctx, nil, "mkdir", "-m", "0700", filepath.Join(b.scaffold, staging)); err != nil {
			return err
		}
	}
	return b.r.Run(b.ctx, nil, "install", "-m", "0644", "/etc/resolv.conf", filepath.Join(b.scaffold, "etc/resolv.conf"))
}

// builder is what a build's steps work with.
type builder struct {
	ctx      context.Context
	r        Runner
	work     string // root's working directory
	scaffold string // the steps' top layer, in work
	layers   string // the layer store
	pkgCache string // mounted at /.mh-cache
	maxAge   time.Duration
	o        Options
	base     Base
}

// unmountUnder unmounts everything mounted at or below dir, deepest first.
func unmountUnder(ctx context.Context, r Runner, dir string) error {
	mounts := mountsUnder(dir)
	for i := len(mounts) - 1; i >= 0; i-- {
		if err := r.Run(ctx, nil, "umount", "-l", mounts[i]); err != nil {
			return fmt.Errorf("unmounting %s: %w", mounts[i], err)
		}
	}
	return nil
}

type source struct {
	path string
	dir  bool
}

// resolveSources finds every copy's source in the build context, by the
// copy's index in s.Ops. A source that resolves outside it — through a
// symlink too — is refused, as COPY refuses it.
func resolveSources(dir string, s *Spec) (map[int]source, error) {
	out := map[int]source{}
	if !slices.ContainsFunc(s.Ops, Op.IsCopy) {
		return out, nil
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var errs []error
	for i, op := range s.Ops {
		if !op.IsCopy() {
			continue
		}
		guest, src, w := op.Guest, op.Source, op.Where
		p, err := filepath.EvalSymlinks(filepath.Join(root, src))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w, err))
			continue
		}
		if rel, err := filepath.Rel(root, p); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			errs = append(errs, fmt.Errorf("%s: %s is outside the build context %s", w, src, root))
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w, err))
			continue
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: %s is neither a file nor a directory", w, src))
			continue
		}
		if fi.IsDir() && !strings.HasSuffix(guest, "/") {
			// COPY dir /path: the contents land in /path either way; say it
			// with the slash so the spec reads as what it does.
			errs = append(errs, fmt.Errorf("%s: %s is a directory: write the guest path as %s/ (its contents are copied into it)", w, src, guest))
			continue
		}
		out[i] = source{path: p, dir: fi.IsDir()}
	}
	return out, errors.Join(errs...)
}

// measure sizes the ext4 for the finished image: its content plus a
// quarter and 32 MB of headroom (at least 64 MB), and inodes for every file
// with room to spare.
func (b *builder) measure(top, merged string, chain []layer, want int64) (sizeMB, inodes int64, err error) {
	out, err := b.onFinal(b.ctx, top, merged, chain, true, "du", "-s", "-x", "--block-size=1M", "$MERGED")
	if err != nil {
		return 0, 0, err
	}
	usedMB, err := strconv.ParseInt(strings.Fields(string(out) + " x")[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("du of the image: %q", out)
	}
	out, err = b.onFinal(b.ctx, top, merged, chain, true, "find", "$MERGED", "-xdev", "-printf", ".")
	if err != nil {
		return 0, 0, err
	}
	files := int64(len(out))
	inodes = files + files/4 + 4096
	sizeMB = want
	if sizeMB == 0 {
		sizeMB = max(64, usedMB+usedMB/4+32)
	}
	if sizeMB < usedMB {
		return 0, 0, fmt.Errorf("size_mb %d: the image's files take %d MB", sizeMB, usedMB)
	}
	return sizeMB, inodes, nil
}

// removeTree deletes a build tree, refusing while anything is mounted under
// it: rm -rf through a mount would delete what is mounted.
func removeTree(ctx context.Context, r Runner, dir string) error {
	if m := mountsUnder(dir); len(m) > 0 {
		return fmt.Errorf("%s is mounted under the build directory; unmount it and remove %s by hand", m[0], dir)
	}
	return r.Run(ctx, nil, "rm", "-rf", "--one-file-system", dir)
}

// mountsUnder lists the mount points at or below dir, in mount order.
// Mount points are listed with octal escapes (\040 for a space); the build's
// own paths have none, so a plain comparison is exact for them.
var mountsUnder = func(dir string) []string {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 1 && (f[1] == dir || strings.HasPrefix(f[1], dir+"/")) {
			out = append(out, f[1])
		}
	}
	return out
}

func removeWork(ctx context.Context, r Runner, work string) error {
	if work == "" {
		return nil
	}
	return removeTree(ctx, r, work)
}

// cached makes dst the pinned file at url: kept from an earlier build when
// its hash still matches, downloaded otherwise.
func cached(ctx context.Context, fetch func(context.Context, string, string) error, url, dst, sum string) error {
	if got, err := hashFile(dst); err == nil && got == sum {
		return nil
	}
	tmp := dst + ".tmp"
	defer os.Remove(tmp)
	if err := fetch(ctx, url, tmp); err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	got, err := hashFile(tmp)
	if err != nil {
		return err
	}
	if got != sum {
		return fmt.Errorf("%s is not the pinned file: sha256 %s, want %s", url, got, sum)
	}
	return os.Rename(tmp, dst)
}

// pinned makes the pinned file at url available to root: downloaded into
// the user's cache (or kept from there), then copied into the build's
// working directory, which only root can write, and hashed again there. What
// root then reads is that copy: the cache can change after its check, the
// copy cannot.
func (b *builder) pinned(url, name, sum string) (string, error) {
	local := filepath.Join(b.o.CacheDir, name)
	if err := cached(b.ctx, b.o.Fetch, url, local, sum); err != nil {
		return "", err
	}
	dst := filepath.Join(b.work, "downloads", name)
	if err := b.r.Run(b.ctx, nil, "install", "-D", "-m", "0600", local, dst); err != nil {
		return "", err
	}
	out, err := b.r.Output(b.ctx, "sha256sum", dst)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(string(out)); len(f) == 0 || f[0] != sum {
		return "", fmt.Errorf("%s changed after it was checked: the copy is not the pinned file (want sha256 %s)", local, sum)
	}
	return dst, nil
}

// installKernel makes sure the pinned kernel is at dst, in the engine's
// kernel directory (root's: checked and written through the runner).
func (b *builder) installKernel(dst, url, sum string) error {
	if out, err := b.r.Output(b.ctx, "sh", "-c", `[ ! -e "$1" ] || sha256sum "$1"`, "sh", dst); err != nil {
		return err
	} else if f := strings.Fields(string(out)); len(f) > 0 {
		if f[0] != sum {
			return fmt.Errorf("%s is not the pinned kernel: sha256 %s, want %s", dst, f[0], sum)
		}
		return nil
	}
	src, err := b.pinned(url, filepath.Base(dst), sum)
	if err != nil {
		return err
	}
	return b.r.Run(b.ctx, nil, "install", "-D", "-o", "root", "-g", "root", "-m", "0644", src, dst)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchClient bounds a download: the largest, a guest kernel, is tens of MB.
var fetchClient = &http.Client{Timeout: 10 * time.Minute}

func httpFetch(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Sudo runs commands as root: directly when this process is root, through
// sudo otherwise. Their output goes to Out; with Quiet it is kept and only
// shown when a command fails.
type Sudo struct {
	Out   io.Writer
	Quiet bool
}

func (s Sudo) command(ctx context.Context, name string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.CommandContext(ctx, name, args...)
	} else {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"--", name}, args...)...)
	}
	// Cancelled (Ctrl-C), the command is asked to stop — sudo passes
	// SIGTERM on, and a step's supervisor (layers.go) ends everything in its
	// namespaces — and killed if it has not within 10 s. A SIGKILL to sudo
	// alone would leave its command running.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// Run implements Runner.
func (s Sudo) Run(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	cmd := s.command(ctx, name, args)
	cmd.Stdin = stdin
	var buf bytes.Buffer
	if s.Quiet {
		cmd.Stdout, cmd.Stderr = &buf, &buf
	} else {
		cmd.Stdout, cmd.Stderr = s.Out, s.Out
	}
	if err := cmd.Run(); err != nil {
		if s.Quiet {
			s.Out.Write(buf.Bytes())
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Output implements Runner.
func (s Sudo) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := s.command(ctx, name, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Authenticate asks for sudo's password once, up front, instead of in the
// middle of a step's output. A no-op as root.
func (s Sudo) Authenticate(ctx context.Context) error {
	if os.Geteuid() == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "sudo", "-v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the build runs as root and sudo refused: %w", err)
	}
	return nil
}
