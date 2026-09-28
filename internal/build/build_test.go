package build

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if want := []string{"/etc/nginx/http.d/default.conf", "/srv/www/"}; strings.Join(s.FileOrder, ",") != strings.Join(want, ",") {
		t.Errorf("FileOrder = %v, want %v", s.FileOrder, want)
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

type fakeRunner struct {
	calls []call
	fail  string // a command line containing this fails
	work  string
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
	return nil
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: args})
	switch name {
	case "mktemp":
		return []byte(f.work + "\n"), nil
	case "du":
		return []byte("40\t" + args[len(args)-1] + "\n"), nil
	case "find":
		return []byte(strings.Repeat(".", 1000)), nil
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
	s, err := Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{work: "/store/build/mh-build-abc123"}
	return Options{Spec: s, Context: ctxDir, Arch: "x86_64", Store: "/store", Kernels: "/store/kernels",
		CacheDir: t.TempDir(), Log: io.Discard, Runner: r, Fetch: pinnedFetch(t)}, r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildSteps(t *testing.T) {
	o, r := testOptions(t, `
base: alpine:3.22
packages: [nginx]
files:
  /srv/www/: out/
  /etc/nginx/http.d/default.conf: nginx.conf
run: ["mkdir -p /run/nginx"]
`)
	res, err := Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rootfs != "/store/build/mh-build-abc123/rootfs.ext4" || res.Kernel != "/store/kernels/vmlinux-6.1.102" {
		t.Errorf("result = %+v", res)
	}
	if len(r.find("install -D -o root -g root -m 0644")) != 1 {
		t.Error("the missing kernel was not installed")
	}
	// Every write into the image after unpacking goes through the chroot;
	// the host side only writes into paths the build created itself.
	tree := "/store/build/mh-build-abc123/tree"
	for _, c := range r.calls {
		switch c.name {
		case "chroot":
			if c.args[0] != tree {
				t.Errorf("chroot into %s", c.args[0])
			}
		case "cp":
			dst := c.args[len(c.args)-1]
			if dst != tree+"/etc/resolv.conf" && !strings.HasPrefix(dst, tree+staging+"/") {
				t.Errorf("host-side copy into the image: %s", c)
			}
		}
	}
	if apk := r.find("apk add"); len(apk) != 1 || strings.Join(apk[0].args[len(apk[0].args)-2:], " ") != "socat nginx" {
		t.Errorf("apk = %v", apk)
	}
	if a := r.find("/usr/local/bin/microhosted-exec"); len(a) != 1 || a[0].stdin != string(agent) {
		t.Error("the agent was not installed")
	}
	// Files in path order: the config (a file) first, then the directory's
	// contents.
	copies := r.find(`cp -a "$1"`)
	if len(copies) != 2 || !strings.Contains(copies[0].String(), "/etc/nginx/http.d/default.conf") ||
		!strings.Contains(copies[1].String(), `"$1"/. "$2"/`) {
		t.Errorf("copies = %v", copies)
	}
	if len(r.find("mkdir -p /run/nginx")) != 1 {
		t.Error("the run step did not run")
	}
	mkfs := r.find("mkfs.ext4")
	if len(mkfs) != 1 || !strings.Contains(mkfs[0].String(), "-d "+tree) || !strings.HasSuffix(mkfs[0].String(), " 82M") {
		t.Errorf("mkfs = %v (want 40 MB used → 82M)", mkfs)
	}
	if len(r.find("rm -rf --one-file-system "+tree)) != 1 {
		t.Error("the tree was not removed")
	}
	must(t, res.Cleanup(context.Background()))
	if len(r.find("rm -rf --one-file-system /store/build/mh-build-abc123")) != 2 {
		t.Error("Cleanup did not remove the working directory")
	}
}

func TestBuildFailureCleansUp(t *testing.T) {
	o, r := testOptions(t, "base: alpine:3.22\nrun: [\"false\"]\n")
	r.fail = "false"
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), `run "false"`) {
		t.Fatalf("Build = %v, want the run step's failure", err)
	}
	if len(r.find("rm -rf --one-file-system /store/build/mh-build-abc123")) != 1 {
		t.Error("a failed build left its working directory")
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

// The image specs the orchestrator's examples build stay valid.
func TestExampleImageSpecs(t *testing.T) {
	files, _ := filepath.Glob("../../orchestrator/examples/*/build.yml")
	more, _ := filepath.Glob("../../orchestrator/examples/images/*/build.yml")
	files = append(files, more...)
	if len(files) < 4 {
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

// An Ubuntu base: debootstrap verified by the keyring, apt with services kept
// from starting, systemd starting the agent, /proc unmounted before the
// tree is written out.
func TestBuildUbuntu(t *testing.T) {
	keyring := filepath.Join(t.TempDir(), "ubuntu-archive-keyring.gpg")
	must(t, os.WriteFile(keyring, []byte("keys"), 0o644))
	oldKeyring, oldHave, oldMounts := ubuntuKeyring, haveDebootstrap, mountsUnder
	t.Cleanup(func() { ubuntuKeyring, haveDebootstrap, mountsUnder = oldKeyring, oldHave, oldMounts })
	ubuntuKeyring, haveDebootstrap = keyring, func() bool { return true }

	o, r := testOptions(t, "base: ubuntu:noble\npackages: [nginx-light]\nrun: [\"true\"]\n")
	tree := r.work + "/tree"
	// /proc stays mounted in the tree until the build unmounts it.
	mountsUnder = func(dir string) []string {
		if len(r.find("mount -t proc")) > 0 && len(r.find("umount -l "+tree+"/proc")) == 0 && strings.HasPrefix(tree, dir) {
			return []string{tree + "/proc"}
		}
		return nil
	}
	if _, err := Build(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if d := r.find("debootstrap --keyring=" + keyring); len(d) != 1 || !strings.Contains(d[0].String(), "noble "+tree+" http://archive.ubuntu.com/ubuntu") {
		t.Errorf("debootstrap = %v", d)
	}
	apt := r.find("apt-get install")
	if len(apt) != 1 || !strings.Contains(apt[0].String(), "policy-rc.d") || !strings.HasSuffix(apt[0].String(), "socat nginx-light") {
		t.Errorf("apt = %v", apt)
	}
	if len(r.find("systemctl enable microhosted-exec.service")) != 1 || len(r.find("/etc/inittab")) != 0 {
		t.Error("the agent must be a systemd unit on Ubuntu")
	}
	umount, mkfs := -1, -1
	for i, c := range r.calls {
		switch {
		case c.name == "umount":
			umount = i
		case c.name == "mkfs.ext4":
			mkfs = i
		}
	}
	if umount < 0 || umount > mkfs {
		t.Errorf("/proc unmounted at call %d, mkfs at %d", umount, mkfs)
	}
	if len(r.find("rm -f /dev/null")) != 0 {
		t.Error("Ubuntu's own /dev nodes were removed")
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
	if v := CacheVersion(base); !strings.HasPrefix(v, "sha-") || len(v) != 16 {
		t.Errorf("CacheVersion = %q", v)
	}
	for in, want := range map[string]string{"/x/web-alpine": "web-alpine", "/x/My Site_2": "my-site-2", "/x/---": "image"} {
		if got := DefaultName(in); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", in, got, want)
		}
	}
}
