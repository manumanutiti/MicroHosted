package cli

import (
	"context"
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
tag, nothing is built: the existing reference is printed, without sudo.
That is what lets a plant spec say build: instead of image: (mh-orchestrator
builds on plan and apply, and only what changed). Packages are whatever the
repositories serve at build time; --no-cache rebuilds to pick up newer ones.

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
  mh build --no-build web/ || echo "not built" # look only, exit 3 if absent`

// runBuild and newBuildRunner are variables so tests can run mh build
// without root.
var (
	runBuild       = build.Build
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
	var noCache, noBuild bool
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&file, "file", "f", "", "image spec `FILE` (default CONTEXT/"+BuildFile+")")
	fs.stringVar(&tag, "tag", "t", "", "`NAME[:VERSION]` to tag the image with (default: the spec's name:, else the context directory's name; VERSION: the inputs' fingerprint)")
	fs.boolVar(&quiet, "quiet", "q", "print only the pinned reference; build output only if a step fails")
	fs.boolVar(&noCache, "no-cache", "", "build even if an image of the same inputs exists (tagged with the build time)")
	fs.boolVar(&noBuild, "no-build", "", "only look: print the reference of an image of the same inputs, or exit 3 if there is none")
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
		// Same inputs, already built: the image is the answer.
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
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
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
		CacheDir: filepath.Join(cache, "microhosted"), Log: log, Runner: runner,
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
	fmt.Fprintf(log, "Successfully built %s\n", tag)
	fmt.Fprintln(e.stdout, tag+"@"+img.Digest)
	return nil
}
