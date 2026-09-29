package build

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
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
	// CacheDir keeps downloads between builds.
	CacheDir string
	// Log receives one line per step.
	Log    io.Writer
	Runner Runner
	// Fetch downloads url to dst; nil uses HTTP.
	Fetch func(ctx context.Context, url, dst string) error
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

// staging is where file sources are copied inside the tree before a copy
// run in the chroot puts them in place: a path in the image is resolved by
// the chroot, never by the host, so a symlink in the image (Alpine's
// /var/run → /run) cannot send a write to the host's own directories.
const staging = "/.mh-build"

// Build builds o.Spec. Every write into the image after the base is laid
// down happens inside the chroot; run steps execute as root on the build
// host, which a chroot does not confine — build only specs you trust, as with
// any Dockerfile.
func Build(ctx context.Context, o Options) (res *Result, err error) {
	s := o.Spec
	if o.Fetch == nil {
		o.Fetch = httpFetch
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

	steps := 6 + len(s.FileOrder) + len(s.Run)
	n := 0
	step := func(format string, a ...any) {
		n++
		fmt.Fprintf(o.Log, "Step %d/%d : %s\n", n, steps, fmt.Sprintf(format, a...))
	}
	r := o.Runner
	if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
		return nil, err
	}

	buildDir := filepath.Join(o.Store, "build")
	if err := r.Run(ctx, nil, "mkdir", "-p", "-m", "0700", buildDir); err != nil {
		return nil, err
	}
	out, err := r.Output(ctx, "mktemp", "-d", filepath.Join(buildDir, "mh-build-XXXXXX"))
	if err != nil {
		return nil, err
	}
	work := strings.TrimSpace(string(out))
	if !strings.HasPrefix(work, buildDir+"/mh-build-") {
		return nil, fmt.Errorf("mktemp returned %q", work)
	}
	tree := filepath.Join(work, "tree")
	defer func() {
		// Whatever the build mounted in the tree goes first, on every path:
		// a mount left behind would make the tree impossible to remove.
		if uerr := unmountUnder(context.WithoutCancel(ctx), r, tree); uerr != nil && err == nil {
			err = uerr
		}
		if err != nil {
			if cerr := removeWork(context.WithoutCancel(ctx), r, work); cerr != nil {
				fmt.Fprintf(o.Log, "cleaning up %s: %v\n", work, cerr)
			}
		}
	}()
	if err := r.Run(ctx, nil, "mkdir", tree); err != nil {
		return nil, err
	}
	b := &builder{ctx: ctx, r: r, tree: tree, work: work, o: o, base: base}

	step("KERNEL %s", s.Kernel)
	kernel := filepath.Join(o.Kernels, "vmlinux-"+s.Kernel)
	if err := b.installKernel(kernel, KernelURL(s.Kernel, o.Arch), kernelSum); err != nil {
		return nil, err
	}

	step("FROM %s (%s)", s.Base, o.Arch)
	if err := d.bootstrap(b); err != nil {
		return nil, err
	}
	// Packages and run steps need DNS, a /dev/null and a /proc; all three go
	// at the end. --remove-destination: never write through a link the base
	// might have.
	if err := r.Run(ctx, nil, "cp", "--remove-destination", "/etc/resolv.conf", filepath.Join(tree, "etc/resolv.conf")); err != nil {
		return nil, err
	}
	if err := b.chroot(nil, `mkdir -p /dev /proc && for n in "null 1 3" "zero 1 5" "random 1 8" "urandom 1 9"; do set -- $n; [ -e /dev/$1 ] || mknod -m 666 /dev/$1 c $2 $3; done && mount -t proc proc /proc`); err != nil {
		return nil, fmt.Errorf("preparing /dev and /proc: %w", err)
	}

	pkgs := append(append([]string{}, d.basePackages...), s.Packages...)
	step("PACKAGES %s", strings.Join(pkgs, " "))
	if err := d.install(b, pkgs); err != nil {
		return nil, fmt.Errorf("installing packages: %w", err)
	}

	step("AGENT vsock:%d, %s", AgentPort, d.init)
	if err := b.chroot(bytes.NewReader(agent), `mkdir -p /usr/local/bin && cat > /usr/local/bin/microhosted-exec && chmod 0755 /usr/local/bin/microhosted-exec`); err != nil {
		return nil, fmt.Errorf("installing the agent: %w", err)
	}
	if err := d.configure(b); err != nil {
		return nil, fmt.Errorf("configuring %s: %w", d.init, err)
	}

	if len(s.FileOrder) > 0 {
		if err := r.Run(ctx, nil, "mkdir", "-m", "0700", filepath.Join(tree, staging)); err != nil {
			return nil, err
		}
	}
	for i, guest := range s.FileOrder {
		src := sources[guest]
		step("COPY %s %s", s.Files[guest], guest)
		staged := path.Join(staging, strconv.Itoa(i))
		if err := r.Run(ctx, nil, "cp", "-a", "--no-preserve=ownership", src.path, filepath.Join(tree, staged)); err != nil {
			return nil, fmt.Errorf("files[%s]: %w", guest, err)
		}
		var script string
		switch {
		case src.dir:
			script = `mkdir -p "$2" && cp -a "$1"/. "$2"/`
		case strings.HasSuffix(guest, "/"):
			script = `mkdir -p "$2" && cp -a "$1" "$2/$3"`
		default:
			script = `mkdir -p "$(dirname "$2")" && cp -a "$1" "$2"`
		}
		if err := b.chroot(nil, script, staged, guest, filepath.Base(src.path)); err != nil {
			return nil, fmt.Errorf("files[%s]: %w", guest, err)
		}
	}

	for _, c := range s.Run {
		step("RUN %s", c)
		if err := b.chroot(nil, c); err != nil {
			return nil, fmt.Errorf("run %q: %w", c, err)
		}
	}

	// DNS as the engine hands it out: the kernel's ip= writes the
	// nameservers to /proc/net/pnp.
	step("FINISH")
	if err := unmountUnder(ctx, r, tree); err != nil {
		return nil, err
	}
	if err := b.chroot(nil, d.cleanup+` && rm -rf `+staging+` && rm -f /etc/resolv.conf && ln -s /proc/net/pnp /etc/resolv.conf`); err != nil {
		return nil, fmt.Errorf("finishing the tree: %w", err)
	}
	if !d.keepDev {
		if err := b.chroot(nil, `rm -f /dev/null /dev/zero /dev/random /dev/urandom`); err != nil {
			return nil, fmt.Errorf("finishing the tree: %w", err)
		}
	}

	sizeMB, inodes, err := measure(ctx, r, tree, s.SizeMB)
	if err != nil {
		return nil, err
	}
	step("ROOTFS ext4, %d MB", sizeMB)
	rootfs := filepath.Join(work, "rootfs.ext4")
	if err := r.Run(ctx, nil, "mkfs.ext4", "-q", "-F", "-L", "microhosted", "-N", strconv.FormatInt(inodes, 10),
		"-E", "root_owner=0:0", "-d", tree, rootfs, strconv.FormatInt(sizeMB, 10)+"M"); err != nil {
		return nil, fmt.Errorf("writing the ext4 (size_mb %d too small?): %w", sizeMB, err)
	}
	if err := r.Run(ctx, nil, "chmod", "0644", rootfs); err != nil {
		return nil, err
	}
	if err := removeTree(ctx, r, tree); err != nil {
		return nil, err
	}
	return NewResult(kernel, rootfs, func(ctx context.Context) error { return removeWork(ctx, r, work) }), nil
}

// builder is what a distro's steps work with.
type builder struct {
	ctx  context.Context
	r    Runner
	tree string
	work string // root's working directory; tree is inside it
	o    Options
	base Base
}

// chroot runs script with /bin/sh inside the image, with a clean
// environment; args are its $1, $2…
func (b *builder) chroot(stdin io.Reader, script string, args ...string) error {
	return b.chrootEnv(stdin, nil, script, args...)
}

func (b *builder) chrootEnv(stdin io.Reader, env []string, script string, args ...string) error {
	argv := append([]string{b.tree, "/usr/bin/env", "-i", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}, env...)
	argv = append(append(argv, "/bin/sh", "-c", script, "sh"), args...)
	return b.r.Run(b.ctx, stdin, "chroot", argv...)
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

// resolveSources finds every file source in the build context. A source
// that resolves outside it — through a symlink too — is refused, as COPY
// refuses it.
func resolveSources(dir string, s *Spec) (map[string]source, error) {
	out := map[string]source{}
	if len(s.Files) == 0 {
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
	for _, guest := range s.FileOrder {
		src := s.Files[guest]
		p, err := filepath.EvalSymlinks(filepath.Join(root, src))
		if err != nil {
			errs = append(errs, fmt.Errorf("files[%s]: %w", guest, err))
			continue
		}
		if rel, err := filepath.Rel(root, p); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			errs = append(errs, fmt.Errorf("files[%s]: %s is outside the build context %s", guest, src, root))
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("files[%s]: %w", guest, err))
			continue
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("files[%s]: %s is neither a file nor a directory", guest, src))
			continue
		}
		if fi.IsDir() && !strings.HasSuffix(guest, "/") {
			// COPY dir /path: the contents land in /path either way; say it
			// with the slash so the spec reads as what it does.
			errs = append(errs, fmt.Errorf("files[%s]: %s is a directory: write the guest path as %s/ (its contents are copied into it)", guest, src, guest))
			continue
		}
		out[guest] = source{path: p, dir: fi.IsDir()}
	}
	return out, errors.Join(errs...)
}

// measure sizes the ext4 for tree: its content plus a quarter and 32 MB of
// headroom (at least 64 MB), and inodes for every file with room to spare.
func measure(ctx context.Context, r Runner, tree string, want int64) (sizeMB, inodes int64, err error) {
	out, err := r.Output(ctx, "du", "-s", "-x", "--block-size=1M", tree)
	if err != nil {
		return 0, 0, err
	}
	usedMB, err := strconv.ParseInt(strings.Fields(string(out) + " x")[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("du %s: %q", tree, out)
	}
	out, err = r.Output(ctx, "find", tree, "-xdev", "-printf", ".")
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
	if os.Geteuid() == 0 {
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, "sudo", append([]string{"--", name}, args...)...)
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
