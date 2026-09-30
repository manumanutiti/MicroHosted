package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"microhosted/internal/build"
	"microhosted/pkg/types"
)

const imageBuildHelp = `Builds an image from an image spec (build.yml: base, packages, files, run,
and the defaults of its VMs — command, health, vcpus, mem_mb), imports it
into the engine's store and prints its pinned reference, NAME:VERSION@sha256:…,
on stdout: the only thing it prints there.

Without -t, the image is named after the spec's name: (else the context
directory) and versioned with the fingerprint of everything the build takes
in — the spec and every file it copies. When the store already holds that
tag and this user's last build under it imported that same image (mh build
keeps a record per tag in $XDG_STATE_HOME/microhosted/builds), nothing is
built: the existing reference is printed, without sudo. A tag this user did
not build, or one that now names other bytes than it built, is refused
rather than trusted: anyone who can import can bind a removed tag again.
That is what lets a plant spec say build: instead of image: (mh-orchestrator
builds on plan and apply, and only what changed).

Each step (FROM, PACKAGES, every file, every run command) is a layer kept
for the next build, as Docker's build cache: a change reruns its step and
the ones after it, never those before — a new line in a file copies that
file again, it does not lay the base down again. A step takes the network's
state as it was when it ran (the base's upgrade, unpinned packages), so a
layer is reused for 7 days at most; --no-cache reruns every step now. The
layers are root's, under the daemon's store; mh builder prune removes the
unused ones.

The build runs on the engine's host, as root through sudo: it lays down the
base, installs packages and runs the run steps in a chroot — which does not
confine root, so build only specs you trust, as with a Dockerfile. File
sources are relative to CONTEXT (default: the spec's directory) and cannot
leave it. Progress goes to stderr (-q hides it, except a failing step's).
Tags never move: -t NAME:VERSION of an existing tag is refused.
Format of build.yml: docs/cli.md § Building an image.`

const imageBuildExamples = `  mh build                                     # ./build.yml, context .
  mh build web/                                # web/build.yml, context web/
  mh build -q -f web/build.yml                 # just the reference
  mh build -t site:1.0 web/                    # an explicit tag
  mh build --no-build web/ || echo "not built" # look only, exit 3 if absent
  mh build --adopt web/                        # trust the image the store has for these inputs`

// runBuild and newBuildRunner are variables so tests can run mh build
// without root.
var (
	runBuild       = build.Build
	runPrune       = build.Prune
	newBuildRunner = func(out io.Writer, quiet bool) buildRunner { return build.Sudo{Out: out, Quiet: quiet} }
)

type buildRunner interface {
	build.Runner
	Authenticate(ctx context.Context) error
}

// buildArch is the image's architecture: this host's, which is the engine's
// (the build writes into its store).
var buildArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]

// BuildFile is the image spec mh build looks for in the build context, as
// docker build looks for a Dockerfile; build.yaml is accepted too.
const BuildFile = "build.yml"

func findSpecFile(dir string) string {
	for _, n := range []string{BuildFile, "build.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return filepath.Join(dir, n)
		}
	}
	return filepath.Join(dir, BuildFile)
}

func imageBuild(e *env, cmd *command, p string, args []string) error {
	var file, tag string
	var quiet bool
	var noCache, noBuild, adopt bool
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&file, "file", "f", "", "image spec `FILE` (default CONTEXT/"+BuildFile+")")
	fs.stringVar(&tag, "tag", "t", "", "`NAME[:VERSION]` to tag the image with (default: the spec's name:, else the context directory's name; VERSION: the inputs' fingerprint)")
	fs.boolVar(&quiet, "quiet", "q", "print only the pinned reference; build output only if a step fails")
	fs.boolVar(&noCache, "no-cache", "", "run every step, reusing no layer, even if an image of the same inputs exists (tagged with the build time)")
	fs.boolVar(&noBuild, "no-build", "", "only look: print the reference of an image of the same inputs, or exit 3 if there is none")
	fs.boolVar(&adopt, "adopt", "", "take the image the store holds for these inputs as this spec's build although this user has no record of building it (check it first: mh image inspect)")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef(p, "expected at most one CONTEXT directory")
	}
	if noCache && noBuild {
		return usagef(p, "--no-cache and --no-build are mutually exclusive")
	}
	if adopt && (noCache || tag != "" && strings.Contains(tag, ":")) {
		return usagef(p, "--adopt takes the image of the inputs' fingerprint: not with --no-cache or -t NAME:VERSION")
	}
	contextDir := ""
	if len(pos) == 1 {
		contextDir = pos[0]
	}
	switch {
	case file == "" && contextDir == "":
		contextDir = "."
		file = findSpecFile(".")
	case file == "":
		file = findSpecFile(contextDir)
	case contextDir == "":
		contextDir = filepath.Dir(file)
	}
	spec, err := build.Load(file)
	if err != nil {
		return err
	}
	if buildArch == "" {
		return fmt.Errorf("mh build: unsupported architecture %s", runtime.GOARCH)
	}

	// The tag: -t, else the spec's name, else the context directory's; the
	// version, unless -t gives one, is the fingerprint of everything the
	// build takes in — the cache key.
	name, version, explicit := strings.Cut(tag, ":")
	if name == "" {
		name = spec.Name
	}
	if name == "" {
		name = build.DefaultName(contextDir)
	}
	cached := false
	switch {
	case explicit:
	case noCache:
		version = time.Now().Format("20060102-150405")
	default:
		fp, err := build.Fingerprint(spec, contextDir, buildArch)
		if err != nil {
			return err
		}
		version, cached = build.CacheVersion(fp), true
	}
	tag = name + ":" + version

	c, err := e.api()
	if err != nil {
		return err
	}
	var have types.ImageResponse
	switch err := c.Do("GET", "/v1/images/"+url.PathEscape(tag), nil, &have); {
	case err == nil && cached:
		// Same inputs, already built: the image is the answer — if it is
		// the one this user built. The tag alone proves nothing: a removed
		// tag can be imported again with other bytes by anyone with the API.
		if err := checkBuildRecord(tag, have.Digest, adopt); err != nil {
			return err
		}
		if !quiet {
			fmt.Fprintf(e.stderr, "%s: already built from the same spec and files (--no-cache rebuilds)\n", tag)
		}
		fmt.Fprintln(e.stdout, tag+"@"+have.Digest)
		return nil
	case err == nil:
		// A taken tag fails now, not after minutes of building.
		return fmt.Errorf("%s is already %s: tags never move; build with a new version", tag, have.Digest)
	case !IsNotFound(err):
		return err
	}
	if noBuild {
		fmt.Fprintf(e.stderr, "mh: %s is not built\n", tag)
		return exitError{code: 3}
	}
	var sys types.SystemResponse
	if err := c.Do("GET", "/v1/system", nil, &sys); err != nil {
		return err
	}
	paths := sys.Daemon.Paths
	if paths.Store == "" || paths.Kernels == "" {
		return fmt.Errorf("the daemon does not report its store directories; mh build needs a newer daemon")
	}
	// Never a shared directory such as /tmp: downloads are checked there
	// before root copies them.
	cache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("no cache directory for the build's downloads: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := e.stderr
	if quiet {
		log = io.Discard
	}
	runner := newBuildRunner(e.stderr, quiet)
	if err := runner.Authenticate(ctx); err != nil {
		return err
	}
	res, err := runBuild(ctx, build.Options{
		Spec: spec, Context: contextDir, Arch: buildArch,
		Store: paths.Store, Kernels: paths.Kernels,
		CacheDir: filepath.Join(cache, "microhosted"), Log: log, Runner: runner, NoCache: noCache,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := res.Cleanup(context.WithoutCancel(ctx)); err != nil {
			fmt.Fprintf(e.stderr, "mh: removing the build's files: %v\n", err)
		}
	}()

	req := types.ImportImageRequest{
		Name: tag, KernelPath: res.Kernel, RootfsPath: res.Rootfs,
		VCPUs: spec.VCPUs, MemMB: spec.MemMB, DiskMB: spec.DiskMB, Command: spec.Command,
	}
	if h := spec.Health; h != nil {
		req.Health = &types.ImageHealth{Command: h.Command, Every: h.Every, Timeout: h.Timeout, Failures: h.Failures}
	}
	fmt.Fprintf(log, "Importing %s…\n", tag)
	var img types.ImageResponse
	if err := c.Do("POST", "/v1/images", req, &img); err != nil {
		return err
	}
	if cached {
		if err := writeBuildRecord(tag, img.Digest); err != nil {
			fmt.Fprintf(e.stderr, "mh: %v: the next build of the same inputs will ask for --adopt\n", err)
		}
	}
	fmt.Fprintf(log, "Successfully built %s\n", tag)
	fmt.Fprintln(e.stdout, tag+"@"+img.Digest)
	return nil
}

// buildRecordPath is where mh build records the digest it imported under a
// fingerprint tag: the user's own state directory, which nobody else writes.
func buildRecordPath(tag string) (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no state directory for mh build's records: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	name, version, _ := strings.Cut(tag, ":")
	return filepath.Join(dir, "microhosted", "builds", name, version), nil
}

// checkBuildRecord accepts digest as tag's cached build only if this user's
// record says it imported exactly that; adopt records it instead.
func checkBuildRecord(tag, digest string, adopt bool) error {
	p, err := buildRecordPath(tag)
	if err != nil {
		return err
	}
	if adopt {
		return writeBuildRecord(tag, digest)
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is in the store as %s, but this user has no record of building it (%s): it is not taken on trust as this spec's image. "+
			"Check it (mh image inspect %s) and take it with mh build --adopt, or remove the tag (mh image rm %s) and build again", tag, digest, p, tag, tag)
	}
	if err != nil {
		return err
	}
	if want := strings.TrimSpace(string(data)); want != digest {
		return fmt.Errorf("%s now names %s, but this user's build imported %s under it: the tag was removed and imported again with other bytes. "+
			"Not using it: find out who imported it (mh image inspect %s), remove the tag, and build again", tag, digest, want, tag)
	}
	return nil
}

func writeBuildRecord(tag, digest string) error {
	p, err := buildRecordPath(tag)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("recording the build of %s: %w", tag, err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(digest+"\n"), 0o600); err != nil {
		return fmt.Errorf("recording the build of %s: %w", tag, err)
	}
	return os.Rename(tmp, p)
}

var builderGroup = &group{
	name:    "builder",
	summary: "Manage mh build's layer cache",
	cmds: []*command{
		{name: "prune", summary: "Remove build layers no build has used lately (--all: every layer and the package caches)", help: builderPruneHelp, run: builderPrune},
	},
}

const builderPruneHelp = `Removes the layers mh build keeps under the daemon's store (as root, through
sudo): those no build has taken for --older-than (default 7 days, after
which a build would not take them anyway), and those a newer layer replaced
for the same step, after an hour. --all removes every layer and the package
caches: the next build starts from scratch, and a build running now may
fail. Images already built are not touched.`

func builderPrune(e *env, cmd *command, p string, args []string) error {
	var all bool
	var older string
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&all, "all", "a", "remove every layer and the package caches")
	fs.stringVar(&older, "older-than", "", "", "remove the layers unused for this `DURATION` (default 168h)")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(p, "unexpected argument %q", pos[0])
	}
	var o build.PruneOptions
	o.All = all
	if older != "" {
		d, err := time.ParseDuration(older)
		if err != nil || d <= 0 {
			return usagef(p, "--older-than %q: a positive duration such as 72h", older)
		}
		o.OlderThan = d
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var sys types.SystemResponse
	if err := c.Do("GET", "/v1/system", nil, &sys); err != nil {
		return err
	}
	if sys.Daemon.Paths.Store == "" {
		return fmt.Errorf("the daemon does not report its store directory; mh builder prune needs a newer daemon")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runner := newBuildRunner(e.stderr, true)
	if err := runner.Authenticate(ctx); err != nil {
		return err
	}
	res, err := runPrune(ctx, runner, sys.Daemon.Paths.Store, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "removed %d layers", res.Layers)
	if res.Caches {
		fmt.Fprint(e.stdout, " and the package caches")
	}
	fmt.Fprintln(e.stdout)
	return nil
}
