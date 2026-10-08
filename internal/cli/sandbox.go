package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"cmp"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"microhosted/pkg/types"
)

// mh sandbox: run code you do not trust in a fresh VM built from sandbox/
// (docs/sandbox.md) and report what it did — the decoys it read, how it
// looked for a VM, what it changed, what it tried to reach. Everything here is
// a client of the API (run, cp, exec, network, flows); the guest's side is the
// image's mh-sandbox-* tools.

var sandboxCmd = &command{
	name:    "sandbox",
	args:    "TARGET [COMMAND]",
	summary: "Run code you do not trust in a fresh VM, and report what it did",
	help: `TARGET is a directory, a file, an archive (.tar.gz .tgz .tar .zip), an
https:// URL (a git repository is cloned; anything else is downloaded, as
curl | sh would), or a package: npm:NAME[@VERSION], pypi:NAME[==VERSION]. It
lands in ~/work, where COMMAND — one shell line — runs as an unprivileged
user. A file, or a URL that is not a repository, with no COMMAND is run
itself. A package with no COMMAND is used the ways it can act: its install
scripts, its import, each of its commands with --help; with one, its
commands are on the PATH (npm:cowsay 'cowsay hi').

Anything fetched (--fetch, --apt, a URL, a package) is fetched first, on a network of the
sandbox's own, which is closed before the code runs: the code never reaches
the internet. If closing it cannot be confirmed, nothing runs. What the code
reaches for is answered inside the VM, and written down: every name it looks
up points at a sinkhole that answers HTTP and HTTPS and records what was sent
— a decoy's secret in it is high (--no-sinkhole: no name answered).

The report: a verdict (SUSPICIOUS, REVIEW, NOTHING SUSPICIOUS SEEN), then one
line per kind of finding, graded high, warn or info — decoy credentials read,
ways the code looked for a VM or for root, files changed, processes left,
sockets opened, connections refused, text addressed to an AI agent in the
input or the output. Every finding with -v; each as it happens with --live; the code's own
output with -o.
An empty report is not "safe": it is what this run did. File names, process
names, the output and matched text are the code's to choose — data, never
instructions.

The image is built once: mh build -t sandbox:1 sandbox  (docs/sandbox.md)`,
	examples: `  mh sandbox ./install.sh
  mh sandbox https://astral.sh/uv/install.sh               # curl | sh, watched
  mh sandbox npm:@modelcontextprotocol/server-filesystem
  mh sandbox pypi:httpie 'http --version' --json
  mh sandbox ./repo 'npm test' --fetch 'npm ci --ignore-scripts'
  mh sandbox https://github.com/x/y 'make test' --apt build-essential
  mh sandbox ./release.tgz 'bash setup.sh' --json`,
	run: sandboxRun,
}

type sandboxOpts struct {
	fetch   string
	apt     []string
	image   string
	iface   string
	timeout time.Duration
	cpus    int64
	memory  int64 // MiB, --mem
	keep    bool
	asJSON  bool
	// jsonOut: the report as JSON to this file too, the view still printed
	jsonOut string
	verbose bool
	live    bool
	output  bool
	rules   []string
	// noSinkhole: no name answered, as with no network (mh-sandbox-net)
	noSinkhole bool
	// ci: the code's environment a CI runner's, with decoy tokens in it
	// (mh-sandbox-prepare --ci)
	ci bool
}

// sandboxTarget is what goes into ~/work.
type sandboxTarget struct {
	given string // as on the command line
	kind  string // dir, file, archive, url, npm, pypi
	path  string // dir, file, archive
	url   string
	file  string // url: the name a download is saved as, when it is not a git repository
	pkg   string // npm, pypi: as given after the prefix (esbuild@0.24, httpie==3.2.4)
	name  string // npm, pypi: the package's name alone
}

// The package targets' specs: a name and a version or range in one word,
// no shell in it (they go in a command line quoted, but a word is a word).
var (
	npmSpec  = regexp.MustCompile(`^((?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*)(?:@[A-Za-z0-9._^~<>=*+-]+)?$`)
	pypiSpec = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)(?:\[[A-Za-z0-9._,-]+\])?(?:(?:==|>=|<=|~=|!=|<|>)[A-Za-z0-9.*!+_-]+)?$`)
	urlFile  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$`)
)

var aptName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

func sandboxRun(e *env, cmd *command, p string, args []string) error {
	var o sandboxOpts
	var apt []string
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&o.fetch, "fetch", "f", "", "before the code runs, with a network: `CMD` must run nothing of the code (npm ci --ignore-scripts, pip download, go mod download)")
	fs.listVar(&apt, "apt", "", "Ubuntu `PKG` to install, by root, before the code runs (repeatable, or comma-separated)")
	fs.stringVar(&o.image, "image", "i", "", "the sandbox `IMAGE` (default: the newest sandbox:N)")
	fs.stringVar(&o.iface, "iface", "", "", "the fetch network's way out, a host `INTERFACE` (default: the default route's)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "for the fetch and the run each, a `DURATION` of at most 10m")
	fs.alias("timeout", "t")
	fs.defined = append(fs.defined, "timeout")
	o.memory = 2048
	fs.Int64Var(&o.cpus, "cpus", 2, "the VM's `N` vCPUs")
	fs.alias("cpus", "c")
	fs.defined = append(fs.defined, "cpus")
	fs.Var((*mbValue)(&o.memory), "mem", "the VM's memory `SIZE` in MiB, or 4G (a build — go, cargo, webpack — needs 2G or more)")
	fs.alias("mem", "m")
	fs.defined = append(fs.defined, "mem")
	fs.boolVar(&o.keep, "keep", "k", "keep the VM and its network afterwards, to look inside")
	fs.boolVar(&o.asJSON, "json", "", "the report as JSON on stdout (docs/sandbox.md)")
	fs.stringVar(&o.jsonOut, "json-out", "", "", "the report as JSON to `FILE` too, the view still printed: for a program that keeps it while a person reads it")
	fs.boolVar(&o.verbose, "verbose", "v", "the report in full: every finding, every probe by every program, every command it ran")
	fs.boolVar(&o.live, "live", "", "print each finding as it happens (every 2 s), while the code runs; the report follows")
	fs.boolVar(&o.output, "output", "o", "print the code's own output too (its last 8 KiB): by default only what it did is reported")
	fs.boolVar(&o.noSinkhole, "no-sinkhole", "", "answer no name the code looks up, as with no network: by default the sandbox's own network answers HTTP and HTTPS and writes down what was sent")
	fs.boolVar(&o.ci, "ci", "", "run it as in CI: GitHub Actions' variables, and decoy tokens in its environment (GITHUB_TOKEN, NPM_TOKEN, AWS keys…), as a CI job has them")
	fs.listVar(&o.rules, "rules", "", "a `FILE` of rules of your own: detections, findings accepted (repeatable; sandbox/rules/, docs/sandbox.md)")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef(p, "expected TARGET and at most one COMMAND (quote it: 'npm ci && npm test')")
	}
	if o.timeout < time.Second || o.timeout > 10*time.Minute {
		return usagef(p, "--timeout must be between 1s and 10m")
	}
	if o.cpus < 1 || o.cpus > 32 || o.memory < 256 || o.memory > 32768 {
		return usagef(p, "--cpus must be 1 to 32, --mem 256M to 32G")
	}
	for _, a := range apt {
		for _, n := range strings.Split(a, ",") {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			if !aptName.MatchString(n) {
				return usagef(p, "--apt %q: not a package name", n)
			}
			o.apt = append(o.apt, n)
		}
	}
	t, err := sandboxClassify(pos[0])
	if err != nil {
		return usagef(p, "%v", err)
	}
	command := ""
	if len(pos) == 2 {
		command = pos[1]
	}
	given := command != ""
	switch {
	case t.kind == "npm" || t.kind == "pypi":
		command = packageCommand(t, command)
	case command != "", t.kind == "url":
		// a URL's: once fetched, a repository or a file to run (fetchPhase)
	case t.kind == "file":
		command = "./" + shellQuote(filepath.Base(t.path)) // packed executable: a chmod here would show as a change
	default:
		return usagef(p, "a %s needs a COMMAND to run in it", t.kind)
	}
	// The guest's agent reads one line.
	if strings.ContainsAny(command+o.fetch, "\n\r") {
		return usagef(p, "COMMAND and --fetch must be one line each")
	}
	// The code under test does not grade itself: no rules from inside it.
	if t.kind == "dir" || t.kind == "file" {
		for _, f := range o.rules {
			if within(f, t.path) {
				return usagef(p, "--rules %s: inside %s, the code under test: its rules could accept what it does", f, t.given)
			}
		}
	}
	// before anything is created: a bad rule costs no VM
	rules, err := loadSandboxRules(o.rules)
	if err != nil {
		return usagef(p, "rules: %v", err)
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	s := &sandbox{e: e, c: c, o: o, t: t, command: command, given: given, rules: rules}
	return s.run()
}

// sandboxClassify tells what TARGET is.
func sandboxClassify(given string) (sandboxTarget, error) {
	t := sandboxTarget{given: given}
	switch {
	case strings.HasPrefix(given, "npm:"), strings.HasPrefix(given, "pypi:"):
		kind, spec, _ := strings.Cut(given, ":")
		re := npmSpec
		if kind == "pypi" {
			re = pypiSpec
		}
		m := re.FindStringSubmatch(spec)
		if m == nil {
			return t, fmt.Errorf("%s: not a %s package (NAME, NAME%s)", given, kind, map[string]string{"npm": "@VERSION", "pypi": "==VERSION"}[kind])
		}
		t.kind, t.pkg, t.name = kind, spec, m[1]
		return t, nil
	case strings.HasPrefix(given, "https://"):
		u, err := url.Parse(given)
		if err != nil || u.Host == "" || strings.ContainsAny(given, " \t'\"\\") {
			return t, fmt.Errorf("%s: not a URL to fetch", given)
		}
		t.kind, t.url = "url", given
		// saved under its own name when it is a file (install.sh), or one of ours
		t.file = "download"
		if b := path.Base(u.Path); urlFile.MatchString(b) {
			t.file = b
		}
		return t, nil
	case strings.Contains(given, "://") || strings.HasPrefix(given, "git@"):
		return t, fmt.Errorf("%s: only https:// URLs are fetched", given)
	}
	fi, err := os.Stat(given)
	if err != nil {
		return t, err
	}
	t.path = given
	switch {
	case fi.IsDir():
		t.kind = "dir"
	case !fi.Mode().IsRegular():
		return t, fmt.Errorf("%s: not a directory, a file or an archive", given)
	case archiveKind(given) != "":
		t.kind = "archive"
	default:
		t.kind = "file"
	}
	return t, nil
}

// packageCommand is what runs for a package target: the package used the
// ways it can act (mh-sandbox-try) with no COMMAND; with one, COMMAND, its
// commands on the PATH — after npm's install scripts, which the fetch left
// out — and an MCP server it starts spoken to, not left waiting on stdin.
func packageCommand(t sandboxTarget, command string) string {
	c := "mh-sandbox-try " + t.kind + " " + shellQuote(t.name)
	if command != "" {
		c += " " + shellQuote(command)
	}
	return c
}

// packageFetch brings a package and its dependencies into ~/work running
// nothing of them: npm without its scripts, pip wheels only (installing a
// wheel unpacks it; building an sdist runs its setup.py).
func packageFetch(t sandboxTarget) string {
	if t.kind == "npm" {
		return "npm init -y >/dev/null && npm install --ignore-scripts --no-audit --no-fund --loglevel=error " + shellQuote(t.pkg)
	}
	// No wheel: the sdist, downloaded unbuilt, with the wheels its build
	// and its dependencies need (mh-sandbox-sdist); mh-sandbox-try builds
	// it in the run.
	return "python3 -m venv .v && { .v/bin/pip install -q --disable-pip-version-check --only-binary=:all: " + shellQuote(t.pkg) +
		" || { command -v mh-sandbox-sdist >/dev/null || { echo 'no wheel, and this image predates sdists: rebuild it from sandbox/'; exit 1; }; " +
		"echo 'no wheel: the sdist, not built'; mh-sandbox-sdist " + shellQuote(t.pkg) + "; }; }"
}

func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	}
	return ""
}

// sandbox is one run: the VM, its network, what to clean up.
type sandbox struct {
	e       *env
	c       *Client
	o       sandboxOpts
	t       sandboxTarget
	command string
	given   bool // the caller gave the COMMAND

	fetchOut strings.Builder // what the steps before the run printed

	rules *sandboxRules

	image   string
	vm      string
	network string // the run's own, when something is fetched
	once    sync.Once
	// before is what the host had refused before the code ran: the fetch's
	// last packets, cut mid-close. The report is what came after. The guest
	// lets the fetch's connections close before the cut (mh-sandbox-run
	// --fetch); a server that still resends its FIN later gets an ACK that
	// shows here, under the fetch's own destination.
	before map[string]uint64
	// began: when the command was started, by this host's clock
	began time.Time
}

func (s *sandbox) say(format string, a ...any) {
	fmt.Fprintf(s.e.stderr, "== "+format+"\n", a...)
}

func (s *sandbox) needsNetwork() bool {
	switch s.t.kind {
	case "url", "npm", "pypi":
		return true
	}
	return s.o.fetch != "" || len(s.o.apt) > 0
}

// packed: the target goes in as an archive from here, not fetched in the VM
func (s *sandbox) packed() bool {
	switch s.t.kind {
	case "dir", "file", "archive":
		return true
	}
	return false
}

func (s *sandbox) run() (err error) {
	if s.image, err = sandboxImage(s.c, s.o.image); err != nil {
		return err
	}
	// Packed before anything is created: a bad archive costs no VM.
	var tgz string
	if s.packed() {
		if tgz, err = packTarget(s.t); err != nil {
			return err
		}
		defer os.Remove(tgz)
	}

	// mh sandbox … | head: a closed pipe makes a write fail, not kill mh
	// before it removes the VM.
	signal.Ignore(syscall.SIGPIPE)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		if _, ok := <-stop; ok {
			fmt.Fprintln(s.e.stderr, "\ninterrupted")
			s.o.keep = false
			s.cleanup()
			os.Exit(130)
		}
	}()
	defer s.cleanup()

	id := randomID()
	netName := "default"
	labels := map[string]string{"managed-by": "mh-sandbox"}
	if s.needsNetwork() {
		iface := s.o.iface
		if iface == "" {
			if iface, err = defaultRouteIface("/proc/net/route"); err != nil {
				return err
			}
		}
		// Its own network: closed without touching anyone else's, the VM
		// staying on its bridge — recorded — once it is.
		req := types.CreateNetworkRequest{
			Name: "sbx-" + id, Labels: labels, Egress: true, EgressIface: iface,
			EgressPorts: []types.PortRule{{Protocol: "tcp", Port: 80}, {Protocol: "tcp", Port: 443}, {Protocol: "udp", Port: 53}},
		}
		var n types.NetworkResponse
		if err := s.c.Do("POST", "/v1/networks", req, &n); err != nil {
			return fmt.Errorf("creating the fetch network: %w", err)
		}
		s.network, netName = n.Name, n.Name
		s.say("VM (%s on %s: internet@%s, ports tcp:80,tcp:443,udp:53, to fetch)", s.image, netName, iface)
	} else {
		s.say("VM (%s on %s: no way out)", s.image, netName)
	}
	var vm types.VMResponse
	if err := s.c.Do("POST", "/v1/vms", types.CreateVMRequest{Image: s.image, Network: netName, Name: "sbx-" + id, Labels: labels, VCPUs: s.o.cpus, MemMB: s.o.memory}, &vm); err != nil {
		return err
	}
	s.vm = vm.ID
	fmt.Fprintln(s.e.stderr, s.vm)
	if err := s.c.Do("GET", fmt.Sprintf("/v1/vms/%s/ready?timeout_ms=%d", s.vm, 60000), nil, nil); err != nil {
		return fmt.Errorf("the VM's agent did not answer: %w", err)
	}
	if tgz != "" {
		if err := s.upload(tgz, "/root/code.tgz"); err != nil {
			return err
		}
	}
	if s.o.ci {
		// before the fetch: an image without it costs no download
		if r, _, err := s.exec("grep -q -e --ci /usr/sbin/mh-sandbox-prepare", time.Minute); err != nil {
			return err
		} else if r.ExitCode != 0 {
			return fmt.Errorf("image %s predates --ci: rebuild it from sandbox/ (docs/sandbox.md)", s.image)
		}
	}

	prepare := "mh-sandbox-prepare"
	if s.o.noSinkhole {
		prepare += " --no-sinkhole"
	}
	if s.o.ci {
		prepare += " --ci"
	}
	prepare += " /root/code.tgz"
	if s.needsNetwork() {
		if err := s.fetchPhase(tgz != ""); err != nil {
			var nr *sandboxNotRun
			if (s.o.asJSON || s.o.jsonOut != "") && errors.As(err, &nr) {
				rep := &sandboxReport{Target: s.t.given, Image: s.image, VM: s.vm, CI: s.o.ci, Verdict: "did_not_run",
					Warnings: []string{nr.why}, Output: untrustedTail(nr.output, 8<<10)}
				if perr := s.writeJSON(rep); perr != nil {
					return perr
				}
				if s.o.asJSON {
					if perr := printJSON(s.e.stdout, rep); perr != nil {
						return perr
					}
				}
			}
			return err
		}
		prepare = strings.TrimSuffix(prepare, " /root/code.tgz")
	}
	if err := s.uploadRules(); err != nil {
		return err
	}
	if err := s.root(prepare, time.Minute); err != nil {
		return err
	}
	if err := s.root("mh-sandbox-scan", 2*time.Minute); err != nil {
		return err
	}
	// what the scan found, kept here before the code runs: a run that
	// brings the VM down takes the report with it, not this
	scanned := s.scanned()

	if s.before, err = s.flowCounts(); err != nil {
		return err
	}
	s.say("run: %s", s.command)
	stopWatch := func() {}
	if s.o.live {
		stopWatch = s.watch()
	}
	s.began = time.Now()
	res, timedOut, err := s.exec("mh-sandbox-run "+shellQuote(s.command), s.o.timeout)
	took := time.Since(s.began)
	stopWatch()
	if err != nil {
		return err
	}
	code := res.ExitCode
	if timedOut {
		code = 124
	}

	rep := s.report()
	if !rep.Complete {
		rep.AddressesAnAgent = append(rep.AddressesAnAgent, scanned...)
	}
	rep.applyRules(s.rules)
	rep.ExitCode, rep.TimedOut, rep.DurationMS = code, timedOut, took.Milliseconds()
	rep.Output = untrustedTail(res.Output, 8<<10)
	patterns, perr := s.patterns()
	if perr != nil {
		rep.Warnings = append(rep.Warnings, "no agent patterns in the image ("+perr.Error()+"): the output was not scanned")
	}
	rep.AddressesAnAgent = append(rep.AddressesAnAgent, scanText("output", res.Output, patterns)...)
	for _, name := range rep.Changed.names() {
		for _, a := range scanText("created", name, patterns) {
			a.File, a.Line = name, 0
			rep.AddressesAnAgent = append(rep.AddressesAnAgent, a)
		}
	}
	if w := rep.failed(); w != "" {
		rep.Warnings = append(rep.Warnings, w)
	}
	rep.summarize()

	if err := s.writeJSON(rep); err != nil {
		return err
	}
	if s.o.asJSON {
		if err := printJSON(s.e.stdout, rep); err != nil {
			return err
		}
	} else {
		rep.render(s.e.stdout, styleFor(s.e.stdout), s.o.verbose, s.o.output)
	}
	if code != 0 {
		return exitError{code: code}
	}
	return nil
}

// writeJSON writes the report to --json-out's file, if any.
func (s *sandbox) writeJSON(rep *sandboxReport) error {
	if s.o.jsonOut == "" {
		return nil
	}
	f, err := os.Create(s.o.jsonOut)
	if err != nil {
		return fmt.Errorf("--json-out: %w", err)
	}
	if err := printJSON(f, rep); err != nil {
		f.Close()
		return fmt.Errorf("--json-out: %w", err)
	}
	return f.Close()
}

// watchEvery is how often --live looks at what the code has done while it runs.
var watchEvery = 2 * time.Second

// watch prints, while the code runs, each finding as it appears: a decoy
// read, a probe for a VM or for a way to root (the image's mh-sandbox-watch),
// a connection the host refused. The returned stop looks one last time and
// waits: what a short run did shows too. The report afterwards is the whole
// account; this is it as it happens.
func (s *sandbox) watch() (stop func()) {
	start := time.Now()
	seen := map[string]bool{}
	tools := map[string][]string{} // a decoy's own tools
	read := map[string]bool{}      // a decoy whose reader was named
	pos := ""
	guest := true
	st := styleFor(s.e.stderr)
	look := func() {
		// once per thing found: the first program to find it is named
		show := func(key string, sev severity, what, detail string) {
			if !seen[key] {
				seen[key] = true
				liveLine(s.e.stderr, st, time.Since(start), sev, what, detail)
			}
		}
		if guest {
			r, timedOut, err := s.exec("mh-sandbox-watch "+pos, 30*time.Second)
			switch {
			case err != nil || timedOut:
			case r.ExitCode == 127:
				guest = false
				fmt.Fprintf(s.e.stderr, "  (image %s has no mh-sandbox-watch: only connections show live; rebuild it from sandbox/)\n", s.image)
			case r.ExitCode == 0:
				// a decoy READ by its times shows after the look's records:
				// a reader named (decoyby) says more, and may be its own tool
				var decoys [][]string
				for _, line := range strings.Split(r.Output, "\n") {
					f := strings.Split(line, "\t")
					at := func(i int) string {
						if i < len(f) {
							return untrusted(f[i], 160)
						}
						return ""
					}
					switch f[0] {
					case "at":
						pos = shellQuote(at(1)) + " " + shellQuote(at(2))
					case "decoy":
						tools[at(2)] = strings.Fields(at(3))
						decoys = append(decoys, []string{at(1), at(2)})
					case "decoyby":
						read[at(3)] = true
						sev := sevHigh
						if slices.Contains(tools[at(3)], at(4)) {
							sev = sevInfo
						}
						show("decoyby "+at(3)+"\t"+at(4), sev, "decoy", "opened "+at(3)+"  (by "+at(4)+")")
					case "probe":
						show("probe "+at(1)+at(3), vmProbeSeverity(at(3)), "VM probe", at(3)+" "+at(1)+"  (by "+at(4)+")")
					case "privesc":
						show("privesc "+at(1)+at(3), privescSeverity(at(3)), "privesc", at(3)+" "+at(1)+"  (by "+at(4)+")")
					case "alert":
						k := s.rules.kindOf(at(1))
						show("alert "+at(1)+at(3), k.Severity, "alert", k.Title+": "+at(3)+"  (by "+at(4)+")")
					}
				}
				for _, d := range decoys {
					if d[0] != "READ" || !read[d[1]] {
						show("decoy "+d[0]+d[1], sevHigh, "decoy", d[0]+"  "+d[1])
					}
				}
			}
		}
		var flows types.FlowList
		if err := s.c.Do("GET", "/v1/vms/"+s.vm+"/flows", nil, &flows); err == nil {
			for _, f := range flows.Flows {
				if f.Count > s.before[flowKey(f)] {
					dst := connDst(sandboxConn{Dst: f.Dst, DstPort: f.DstPort})
					show("flow "+flowKey(f), sevWarn, "connection", f.Protocol+" "+dst+" refused  ("+f.Reason+")")
					for _, d := range s.rules.detects() {
						if d.Connect != "" && d.conn.match(f.Dst, f.DstPort) {
							show("rule "+d.Name+flowKey(f), d.sev, "alert", "your rule "+d.Name+": "+dst)
						}
					}
				}
			}
		}
	}
	s.say("live: what it does, as it happens")
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(watchEvery)
		defer t.Stop()
		for {
			look()
			select {
			case <-done:
				look()
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// fetchPhase brings in what the code needs while there is a network — no
// decoy yet, nothing reported — then closes the network's way out and
// confirms it on the daemon's account. If it cannot, nothing runs.
func (s *sandbox) fetchPhase(unpack bool) error {
	if unpack {
		if err := s.root("mh-sandbox-unpack /root/code.tgz", time.Minute); err != nil {
			return err
		}
	}
	failed := ""
	if s.t.kind == "url" {
		// A git repository is cloned; anything else (sh.rustup.rs, an
		// install.sh) is downloaded, as curl | sh would, and run.
		u, f := shellQuote(s.t.url), shellQuote(s.t.file)
		s.say("fetch: %s", s.t.url)
		cmd := "export GIT_TERMINAL_PROMPT=0; if git ls-remote -q -- " + u + " >/dev/null 2>&1; then echo 'a git repository: cloned' && git clone -q --depth 1 -- " + u + " .; " +
			"else curl -fsSL --proto =https --proto-redir =https --max-filesize 1G -o " + f + " -- " + u + " && chmod +x " + f + " && echo 'a file: saved as '" + f + "; fi"
		if r, _, err := s.exec("mh-sandbox-run --fetch "+shellQuote(cmd), s.o.timeout); err != nil {
			return err
		} else if s.fetched(r.Output); r.ExitCode != 0 {
			failed = fmt.Sprintf("the fetch of %s failed (exit %d)", s.t.url, r.ExitCode)
		} else {
			failed = s.urlCommand()
		}
	}
	if failed == "" && len(s.o.apt) > 0 {
		s.say("apt: %s", strings.Join(s.o.apt, " "))
		cmd := "export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq --no-install-recommends " + strings.Join(s.o.apt, " ")
		if r, _, err := s.exec(cmd, s.o.timeout); err != nil {
			return err
		} else if r.ExitCode != 0 {
			s.fetched(r.Output)
			failed = fmt.Sprintf("apt failed (exit %d)", r.ExitCode)
		}
	}
	if failed == "" && (s.t.kind == "npm" || s.t.kind == "pypi") {
		if r, _, err := s.exec("test -x /usr/local/bin/mh-sandbox-try && test -x /usr/local/bin/node", time.Minute); err != nil {
			return err
		} else if r.ExitCode != 0 {
			failed = fmt.Sprintf("image %s predates npm: and pypi: (no node or mh-sandbox-try): rebuild it from sandbox/ (docs/sandbox.md)", s.image)
		} else if s.given {
			if r, _, err := s.exec("mh-sandbox-try --can command", time.Minute); err != nil {
				return err
			} else if r.ExitCode != 0 {
				failed = fmt.Sprintf("image %s predates a COMMAND for a package: rebuild it from sandbox/ (docs/sandbox.md)", s.image)
			}
		}
	}
	if failed == "" && (s.t.kind == "npm" || s.t.kind == "pypi") {
		cmd := packageFetch(s.t)
		s.say("fetch: %s", cmd)
		r, timedOut, err := s.exec("mh-sandbox-run --fetch "+shellQuote(cmd), s.o.timeout)
		if err != nil {
			return err
		}
		s.fetched(r.Output)
		switch {
		case timedOut:
			failed = "the fetch timed out"
		case r.ExitCode != 0:
			failed = fmt.Sprintf("the fetch failed (exit %d)", r.ExitCode)
		}
	}
	if failed == "" && s.o.fetch != "" {
		s.say("fetch: %s", s.o.fetch)
		r, timedOut, err := s.exec("mh-sandbox-run --fetch "+shellQuote(s.o.fetch), s.o.timeout)
		if err != nil {
			return err
		}
		s.fetched(r.Output)
		if timedOut {
			failed = "the fetch timed out"
		} else if r.ExitCode != 0 {
			failed = fmt.Sprintf("the fetch failed (exit %d)", r.ExitCode)
		}
	}

	s.say("cutting the network")
	path := "/v1/networks/" + url.PathEscape(s.network)
	var n types.NetworkResponse
	if err := s.c.Do("PUT", path+"/egress", types.UpdateNetworkEgressRequest{}, &n); err != nil {
		s.o.keep = false
		return fmt.Errorf("could not close %s's way out, nothing runs: %w", s.network, err)
	}
	if err := s.c.Do("GET", path, nil, &n); err != nil || n.Egress || len(n.AllowedEgress) > 0 {
		s.o.keep = false
		return fmt.Errorf("%s's way out is not confirmed closed: nothing runs", s.network)
	}
	fmt.Fprintf(s.e.stderr, "%s: no way out\n", s.network)
	if failed != "" {
		return &sandboxNotRun{why: failed + ": not running the code", output: s.fetchOut.String()}
	}
	return nil
}

// fetched shows the output of a step before the run, and keeps it for a
// report of why the code did not run.
func (s *sandbox) fetched(out string) {
	fmt.Fprint(s.e.stderr, out)
	s.fetchOut.WriteString(out)
}

// sandboxNotRun: a step before the run failed (the fetch, apt, an image
// too old for the target): nothing of the code ran. With --json it is still
// a report, did_not_run, so a program reading it has a verdict.
type sandboxNotRun struct{ why, output string }

func (e *sandboxNotRun) Error() string { return e.why }

// urlCommand, once a URL is fetched: with no COMMAND, a file is run — by
// itself when it says how (#!, a program), with sh otherwise, as curl | sh
// would. A repository needs a COMMAND. Returns why it cannot run, or "".
func (s *sandbox) urlCommand() string {
	if s.command != "" {
		return ""
	}
	f := shellQuote(s.t.file)
	r, _, err := s.exec("mh-sandbox-run --fetch "+shellQuote(`if [ -d .git ]; then echo git; else case "$(head -c 4 `+f+` | tr -d '\0')" in '#!'*|?ELF) echo exec;; *) echo sh;; esac; fi`), time.Minute)
	if err != nil {
		return err.Error()
	}
	switch strings.TrimSpace(r.Output) {
	case "exec":
		s.command = "./" + f
	case "sh":
		s.command = "sh " + f
	case "git":
		return s.t.url + " is a git repository: it needs a COMMAND to run in it"
	default:
		return "could not tell what " + s.t.url + " is"
	}
	return ""
}

func (s *sandbox) cleanup() {
	s.once.Do(func() {
		if s.o.keep && s.vm != "" {
			where := ""
			if s.network != "" {
				where = " on " + s.network
			}
			fmt.Fprintf(s.e.stderr, "kept: %s%s (mh rm %s", s.vm, where, s.vm)
			if s.network != "" {
				fmt.Fprintf(s.e.stderr, "; mh network rm %s", s.network)
			}
			fmt.Fprintln(s.e.stderr, " when done)")
			return
		}
		if s.vm != "" {
			if err := s.c.Do("DELETE", "/v1/vms/"+s.vm, nil, nil); err != nil {
				fmt.Fprintf(s.e.stderr, "mh: could not remove VM %s: %v\n", s.vm, err)
			} else {
				fmt.Fprintf(s.e.stderr, "removed %s\n", s.vm)
			}
		}
		if s.network != "" {
			if err := s.c.Do("DELETE", "/v1/networks/"+url.PathEscape(s.network), nil, nil); err != nil {
				fmt.Fprintf(s.e.stderr, "mh: could not remove network %s: %v\n", s.network, err)
			}
		}
	})
}

// exec runs cmd as root in the VM. timedOut: the daemon stopped waiting —
// the command may still be running.
func (s *sandbox) exec(cmd string, timeout time.Duration) (types.ExecResponse, bool, error) {
	var res types.ExecResponse
	err := s.c.Do("POST", "/v1/vms/"+s.vm+"/exec", types.ExecRequest{Cmd: cmd, TimeoutMS: timeout.Milliseconds()}, &res)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusGatewayTimeout {
		return res, true, nil
	}
	return res, false, err
}

// root runs one of the image's tools; its failure stops the sandbox.
func (s *sandbox) root(cmd string, timeout time.Duration) error {
	r, timedOut, err := s.exec(cmd, timeout)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", cmd, err)
	case timedOut:
		return fmt.Errorf("%s: timed out", cmd)
	case r.ExitCode == 127:
		return fmt.Errorf("%s: not in image %s — rebuild it from sandbox/ (docs/sandbox.md)", strings.Fields(cmd)[0], s.image)
	case r.ExitCode != 0:
		return fmt.Errorf("%s (exit %d): %s", cmd, r.ExitCode, strings.TrimSpace(r.Output))
	}
	fmt.Fprint(s.e.stderr, r.Output)
	return nil
}

// uploadRules gives the guest the user's path and command rules, root's
// alone, before the sandbox is prepared (and after the fetch, which runs as
// the sandbox's user).
func (s *sandbox) uploadRules() error {
	g := s.rules.guest()
	if g == "" {
		return nil
	}
	f, err := os.CreateTemp("", "mh-sandbox-rules-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(g)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := s.root("install -d -m 700 /var/lib/mh-sandbox", time.Minute); err != nil {
		return err
	}
	if err := s.upload(f.Name(), "/var/lib/mh-sandbox/rules"); err != nil {
		return err
	}
	return s.root("chmod 600 /var/lib/mh-sandbox/rules", time.Minute)
}

func (s *sandbox) upload(local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return s.c.Stream("PUT", "/v1/vms/"+s.vm+"/files?path="+url.QueryEscape(remote), f, fi.Size())
}

// report is what the VM and the host say the code did. When the VM's side
// cannot be had, the rest — the output, the exit code, the host's
// connections — still is, marked incomplete: unknown is not none.
func (s *sandbox) report() *sandboxReport {
	var rep *sandboxReport
	r, timedOut, err := s.exec("mh-sandbox-report --tsv", 5*time.Minute)
	failed := ""
	switch {
	case err != nil:
		failed = err.Error()
	case timedOut:
		failed = "timed out"
	case r.ExitCode != 0:
		failed = fmt.Sprintf("exit %d: %s", r.ExitCode, untrusted(strings.TrimSpace(r.Output), 512))
	}
	if failed != "" {
		rep = parseSandboxReport("")
		rep.Warnings = append(rep.Warnings, "the report from inside the VM failed ("+failed+"): decoys, probes, commands, files and processes are unknown, not none")
	} else {
		rep = parseSandboxReport(r.Output)
		rep.Complete = true
		if rep.commands < 0 {
			rep.Warnings = append(rep.Warnings, "image "+s.image+" does not record the commands the code ran: rebuild it from sandbox/ (docs/sandbox.md)")
		}
		if !rep.netSeen {
			rep.Warnings = append(rep.Warnings, "image "+s.image+" does not record the names the code looked up: rebuild it from sandbox/ (docs/sandbox.md)")
		}
		if s.rules.guest() != "" && rep.rulesApplied == 0 {
			rep.Warnings = append(rep.Warnings, "image "+s.image+" does not apply your path and command rules (only connect and accept ones were): rebuild it from sandbox/ (docs/sandbox.md)")
		}
	}
	rep.Target, rep.Image, rep.VM, rep.Kept = s.t.given, s.image, s.vm, s.o.keep
	rep.CI = s.o.ci
	var flows types.FlowList
	if err := s.c.Do("GET", "/v1/vms/"+s.vm+"/flows", nil, &flows); err != nil {
		rep.Warnings = append(rep.Warnings, "the host's flows could not be read: "+err.Error())
	} else {
		if !flows.Recording {
			rep.Warnings = append(rep.Warnings, "this host is not recording refused connections: an empty list proves nothing")
		}
		if flows.Overruns > 0 {
			rep.Warnings = append(rep.Warnings, "the daemon missed flow records: counts may be low")
		}
		for _, f := range flows.Flows {
			if n := f.Count - s.before[flowKey(f)]; n > 0 {
				c := sandboxConn{Protocol: f.Protocol, Dst: f.Dst, DstPort: f.DstPort, Count: n, Reason: f.Reason}
				// the host's clock: refused before this run too, its
				// first time is not this run's
				if !s.began.IsZero() {
					last := f.Last.Sub(s.began).Milliseconds()
					c.LastMS = &last
					if s.before[flowKey(f)] == 0 {
						first := f.First.Sub(s.began).Milliseconds()
						c.FirstMS = &first
					}
				}
				rep.Connections = append(rep.Connections, c)
			}
		}
	}
	return rep
}

func flowKey(f types.Flow) string {
	return fmt.Sprintf("%s %s %d %s", f.Protocol, f.Dst, f.DstPort, f.Reason)
}

// flowCounts is what the host has refused from the VM so far.
func (s *sandbox) flowCounts() (map[string]uint64, error) {
	var flows types.FlowList
	if err := s.c.Do("GET", "/v1/vms/"+s.vm+"/flows", nil, &flows); err != nil {
		return nil, fmt.Errorf("reading the host's flows: %w", err)
	}
	m := map[string]uint64{}
	for _, f := range flows.Flows {
		m[flowKey(f)] = f.Count
	}
	return m, nil
}

// scanned is what mh-sandbox-scan found in the input, as the report would
// list it (agent input FILE LINE TEXT). Nothing when it cannot be read: the
// report lists it too, when it can be had.
func (s *sandbox) scanned() []sandboxAgentText {
	r, _, err := s.exec("sed 's/^/agent\tinput\t/' /var/lib/mh-sandbox/scan 2>/dev/null", 30*time.Second)
	if err != nil {
		return nil
	}
	return parseSandboxReport(r.Output).AddressesAnAgent
}

// patterns are the image's: one list for the input (grep, in the guest) and
// the output (here).
func (s *sandbox) patterns() ([]*regexp.Regexp, error) {
	r, _, err := s.exec("cat /usr/share/mh-sandbox/agent-patterns", 10*time.Second)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, errors.New(strings.TrimSpace(r.Output))
	}
	return compilePatterns(r.Output), nil
}

func compilePatterns(text string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, l := range strings.Split(text, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if re, err := regexp.Compile("(?i)" + l); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// scanText finds text addressed to an agent in one of the code's outputs:
// a pattern, or a character that hides text from a person.
func scanText(where, text string, patterns []*regexp.Regexp) []sandboxAgentText {
	var out []sandboxAgentText
	for i, line := range strings.Split(text, "\n") {
		for _, re := range patterns {
			if m := re.FindString(line); m != "" {
				out = append(out, sandboxAgentText{Where: where, Line: i + 1, Text: untrusted(m, 160)})
			}
		}
		if h := hiddenText(line, where == "created"); h != "" {
			out = append(out, sandboxAgentText{Where: where, Line: i + 1, Text: h})
		}
		if len(out) >= 200 {
			break
		}
	}
	return out
}

// hiddenText says how line hides text from a person and not from a model,
// as mh-sandbox-scan does for the input: Unicode tag characters (a run of
// them that is not a flag, U+1F3F4 then 2 to 6 of a-z 0-9 then cancel:
// Scotland's), a run of zero-width ones (one alone is in emoji and in Persian
// or Indic names), and — where asked, in a file's name — bidirectional
// controls. "" if it does not.
func hiddenText(line string, bidi bool) string {
	rs := []rune(line)
	run := 0
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case isTag(r):
			j := i
			for j < len(rs) && isTag(rs[j]) {
				j++
			}
			if !(i > 0 && rs[i-1] == 0x1F3F4 && flagTags(rs[i:j])) && j-i >= 2 {
				return "(Unicode tag characters: text a model reads and a person does not see)"
			}
			i, run = j-1, 0
			continue
		case r >= 0x200B && r <= 0x200D || r >= 0x2060 && r <= 0x2064 || r == 0xFEFF:
			if run++; run >= 3 {
				return "(a run of zero-width characters: hidden text)"
			}
			continue
		case bidi && (r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069):
			return "(bidirectional controls: it reads otherwise than it is)"
		}
		run = 0
	}
	return ""
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// flagTags: the tags of a subdivision's flag, 2 to 6 of a-z 0-9 then cancel.
func flagTags(t []rune) bool {
	if len(t) < 3 || len(t) > 7 || t[len(t)-1] != 0xE007F {
		return false
	}
	for _, r := range t[:len(t)-1] {
		if !(r >= 0xE0030 && r <= 0xE0039 || r >= 0xE0061 && r <= 0xE007A) {
			return false
		}
	}
	return true
}

func invisible(r rune) bool {
	return r >= 0x200B && r <= 0x200F || r >= 0x202A && r <= 0x202E || r >= 0x2060 && r <= 0x2064 ||
		r >= 0x2066 && r <= 0x2069 || r == 0xFEFF || r >= 0xE0000 && r <= 0xE007F
}

// untrusted makes a string the code chose safe to print: no control
// character (a terminal escape, a carriage return hiding a line), at most n
// bytes.
func untrusted(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || invisible(r) || r == unicode.ReplacementChar {
			return '?'
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// untrustedTail is the end of the command's output, where the failure is.
func untrustedTail(s string, n int) string {
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return untrusted(s, n+len("…"))
}

// sandboxImage is the image to run: the one asked for, which must exist, or
// the newest sandbox:N.
func sandboxImage(c *Client, ref string) (string, error) {
	if ref != "" {
		if _, err := getImage(c, ref); err != nil {
			return "", fmt.Errorf("image %s: %w", ref, err)
		}
		return ref, nil
	}
	var imgs []types.ImageResponse
	if err := c.Do("GET", "/v1/images", nil, &imgs); err != nil {
		return "", err
	}
	best, bestN := "", -1
	for _, img := range imgs {
		for _, tag := range img.Tags {
			v, ok := strings.CutPrefix(tag, "sandbox:")
			if n, err := strconv.Atoi(v); ok && err == nil && n > bestN {
				best, bestN = tag, n
			}
		}
	}
	if best == "" {
		return "", errors.New("no sandbox image: build one once, from the repository's root: mh build -t sandbox:1 sandbox")
	}
	return best, nil
}

// defaultRouteIface reads the host's routing table for the interface of its
// default route: the fetch network's way out.
func defaultRouteIface(routes string) (string, error) {
	f, err := os.Open(routes)
	if err != nil {
		return "", fmt.Errorf("finding the host's way out: %w (give --iface)", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) > 2 && fl[1] == "00000000" && fl[0] != "Iface" {
			return fl[0], nil
		}
	}
	return "", errors.New("the host has no default route: give --iface")
}

func randomID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// The target, packed into a .tar.gz the guest unpacks as the sandbox's user.
// Archives are repacked rather than passed through: one format for the guest,
// and every name checked here — none absolute, none with "..".
// ---------------------------------------------------------------------------

func packTarget(t sandboxTarget) (string, error) {
	out, err := os.CreateTemp("", "mh-sandbox-*.tgz")
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	switch t.kind {
	case "dir":
		err = packDir(tw, t.path)
	case "file":
		err = packFile(tw, t.path, filepath.Base(t.path), 0o755)
	case "archive":
		err = repack(tw, t.path)
	}
	for _, c := range []io.Closer{tw, gz, out} {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(out.Name())
		return "", fmt.Errorf("%s: %w", t.given, err)
	}
	return out.Name(), nil
}

func packDir(tw *tar.Writer, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode().IsRegular():
			return packFile(tw, p, rel, 0)
		case fi.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()})
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: target, Mode: 0o777, ModTime: fi.ModTime()})
		}
		return nil // devices, FIFOs, sockets: not code
	})
}

// packFile adds the file at p as name; a non-zero mode replaces its own.
func packFile(tw *tar.Writer, p, name string, mode os.FileMode) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if mode == 0 {
		mode = fi.Mode().Perm()
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(mode), Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// safeName is an archive member's name if it stays inside ~/work.
func safeName(name string) (string, error) {
	n := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	if n == "" || strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("member %q: an absolute name", name)
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("member %q: it climbs out with ..", name)
		}
	}
	return n, nil
}

func repack(tw *tar.Writer, p string) error {
	if archiveKind(p) == "zip" {
		return repackZip(tw, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if archiveKind(p) == "tgz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := safeName(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			return fmt.Errorf("member %q: hard links are not taken", h.Name)
		default:
			continue // devices, FIFOs: not code
		}
		out := &tar.Header{Typeflag: h.Typeflag, Name: name, Linkname: h.Linkname, Mode: h.Mode & 0o777, Size: h.Size, ModTime: h.ModTime}
		if h.Typeflag != tar.TypeReg {
			out.Size = 0
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

func repackZip(tw *tar.Writer, p string) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		name, err := safeName(zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		h := &tar.Header{Name: name, Mode: int64(mode.Perm()), ModTime: zf.Modified}
		switch {
		case mode.IsDir():
			h.Typeflag = tar.TypeDir
			if !strings.HasSuffix(h.Name, "/") {
				h.Name += "/"
			}
			if h.Mode == 0 {
				h.Mode = 0o755
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, string(target)
		case mode.IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, int64(zf.UncompressedSize64)
			if h.Mode == 0 {
				h.Mode = 0o644
			}
		default:
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The report.
// ---------------------------------------------------------------------------

// sandboxReport is what --json prints (docs/sandbox.md). Fields marked
// untrusted hold strings the code chose.
type sandboxReport struct {
	Target string `json:"target"`
	Image  string `json:"image"`
	VM     string `json:"vm"`
	Kept   bool   `json:"kept"`
	// CI: run as in CI (--ci): a runner's variables and decoy tokens in
	// the code's environment
	CI       bool `json:"ci"`
	ExitCode int  `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
	// DurationMS: from the command's start to its end (or the timeout).
	DurationMS int64 `json:"duration_ms"`
	// Complete: the VM's side of the report was read. When false, every
	// list but connections is unknown, not empty (warnings say why).
	Complete bool           `json:"complete"`
	Summary  sandboxSummary `json:"summary"`

	Decoys      []sandboxDecoy   `json:"decoys"`
	VMProbes    []sandboxProbe   `json:"vm_probes"` // untrusted: path, by
	Privesc     []sandboxProbe   `json:"privesc"`   // untrusted: path, by
	Commands    []sandboxCommand `json:"commands"`  // untrusted: args
	Alerts      []sandboxAlert   `json:"alerts"`    // untrusted: what, by
	Changed     sandboxChanged   `json:"changed"`   // untrusted
	Processes   []sandboxProc    `json:"processes"` // untrusted: args
	Listening   []string         `json:"listening"`
	Connections []sandboxConn    `json:"connections"`
	// Net: the code's network, the sandbox's own inside the VM
	// (mh-sandbox-net): sinkhole (names answered with addresses the VM
	// holds, HTTP and HTTPS answered and written down), servfail (no name
	// answered: --no-sinkhole), off (an image without it: not recorded)
	Net string `json:"net"`
	// DNS: the names the code looked up
	DNS []sandboxDNS `json:"dns"` // untrusted: name
	// Requests: what it sent the sinkhole; TLSRefused: HTTPS clients that
	// refused its certificate (their own list of authorities): the name only
	Requests         []sandboxRequest   `json:"requests"`           // untrusted: method, url
	TLSRefused       []sandboxTLS       `json:"tls_refused"`        // untrusted: name
	AddressesAnAgent []sandboxAgentText `json:"addresses_an_agent"` // untrusted: file, text
	Output           string             `json:"output"`             // untrusted
	Warnings         []string           `json:"warnings"`
	// Rules: the files of the user's rules this run read (sandbox_rules.go);
	// Accepted: what their accept rules matched, out of the lists above.
	Rules    []string          `json:"rules"`
	Accepted []sandboxAccepted `json:"accepted"`

	// Verdict, for a program: suspicious (a high finding), review (warn),
	// clean (nothing high or warn: not proof it is safe), incomplete (the
	// VM's side could not be read), did_not_run (exit 126 or 127: the shell
	// could not find or run the command; or a step before the run failed,
	// in warnings — fix the call, it proves nothing).
	Verdict string `json:"verdict"`

	user     string
	token    string            // this run's, in every decoy: seen in a name, a secret went out
	netSeen  bool              // the image reports its network (or says it cannot: off)
	sinkAddr map[string]string // the sinkhole's address → the name it was given to
	commands int               // distinct, as the image counted them; -1: not recorded
	decoyBy  map[string]map[string]int
	// decoyOpens: decoyBy with when, by decoy
	decoyOpens map[string][]sandboxOpen
	readers    bool // the image names who opened a decoy (decoy has TOOLS, decoyby)
	// start: when the command started, in ms since the epoch by the VM's
	// clock (the image's start record); started: the image said
	start   int64
	started bool
	rules   *sandboxRules
	// rulesApplied: the user's path and command rules the image read
	rulesApplied int
}

type sandboxSummary struct {
	// High, Warn, Info: the findings, one per line of the report without -v.
	High               int  `json:"high"`
	Warn               int  `json:"warn"`
	Info               int  `json:"info"`
	DecoysRead         int  `json:"decoys_read"` // touched, by any program
	VMProbes           int  `json:"vm_probes"`
	EvasionSuspected   bool `json:"evasion_suspected"`
	Privesc            int  `json:"privesc"`
	Commands           int  `json:"commands"` // distinct; commands lists the first 2000
	Alerts             int  `json:"alerts"`
	ChangedOutsideWork int  `json:"changed_outside_work"`
	ProcessesLeft      int  `json:"processes_left"`
	Listening          int  `json:"listening"`
	ConnectionsRefused int  `json:"connections_refused"`
	DNSNames           int  `json:"dns_names"` // distinct names looked up
	Requests           int  `json:"requests"`  // to the sinkhole, and HTTPS refused
	// SecretsSent: requests and names that carried this run's decoy token
	SecretsSent      int `json:"secrets_sent"`
	AddressesAnAgent int `json:"addresses_an_agent"`
}

type sandboxDecoy struct {
	Path         string `json:"path"`
	State        string `json:"state"`
	Legitimately string `json:"legitimately"`
	// By: the programs that opened it (audit), "name ×N"; untrusted. Empty
	// for a decoy READ: who read it is unknown, not no one.
	By []string `json:"by"`
	// Opens: the same, one entry per program with when it opened it,
	// first opened first
	Opens []sandboxOpen `json:"opens"`
	// ByItsTool: only READ, and only by the program that reads it
	// legitimately (npm, ~/.npmrc): graded info, not high.
	ByItsTool bool   `json:"by_its_tool"`
	Severity  string `json:"severity"`
	// Accepted: the why of the user's accept rule that matched it
	Accepted string `json:"accepted,omitempty"`

	tools  []string
	counts map[string]int
}

// sandboxOpen is a program that opened a decoy (audit): its name, untrusted.
type sandboxOpen struct {
	By    string `json:"by"`
	Count int    `json:"count"`
	sandboxWhen
}

// sandboxWhen is when a finding was first and last seen, in milliseconds
// from the command's start (by the VM's clock; connections by the host's).
// Absent where it is not known: an older image, a finding made after the
// run (files left, a .pth hook, lines added to a shell's startup file), or
// a connection that had been refused before this run too (first_ms).
type sandboxWhen struct {
	FirstMS *int64 `json:"first_ms,omitempty"`
	LastMS  *int64 `json:"last_ms,omitempty"`
}

type sandboxProbe struct {
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Count int    `json:"count"`
	By    string `json:"by"`
	sandboxWhen
}

type sandboxWorkEntry struct {
	Entry string `json:"entry"`
	Count int    `json:"count"`
}

type sandboxChanged struct {
	Files     []string           `json:"files"`
	Dirs      []string           `json:"dirs"`
	WorkFiles []sandboxWorkEntry `json:"work_files"`
	WorkDirs  []sandboxWorkEntry `json:"work_dirs"`
}

// sandboxAlert is one "alert KIND COUNT WHAT BY" record: KIND one of
// alertKinds (sandbox_kinds.go), WHAT and BY the code's choice.
type sandboxAlert struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
	What     string `json:"what"`
	By       string `json:"by"`
	sandboxWhen
}

// sandboxCommand is a command line the code ran, every run of it counted;
// PID and PPID are its first run's (0: not known).
type sandboxCommand struct {
	Count int    `json:"count"`
	Args  string `json:"args"`
	PID   int    `json:"pid,omitempty"`
	PPID  int    `json:"ppid,omitempty"`
	sandboxWhen
}

type sandboxProc struct {
	PID  int    `json:"pid"`
	Args string `json:"args"`
}

type sandboxConn struct {
	Protocol string `json:"protocol"`
	Dst      string `json:"dst"`
	DstPort  int    `json:"dst_port,omitempty"`
	Count    uint64 `json:"count"`
	Reason   string `json:"reason"`
	sandboxWhen
}

type sandboxDNS struct {
	Count int    `json:"count"`
	Type  string `json:"type"` // A, AAAA, TXT…
	Name  string `json:"name"`
	sandboxWhen
}

type sandboxRequest struct {
	Count  int    `json:"count"`
	Scheme string `json:"scheme"` // http, https (the sinkhole's certificate accepted)
	Method string `json:"method"`
	URL    string `json:"url"`
	Port   int    `json:"port"`
	Bytes  int    `json:"bytes"` // the bodies', summed
	// CarriesToken: this run's decoy token was in it — as is, URL- or
	// base64-encoded, gzipped: a secret sent out
	CarriesToken bool `json:"carries_token"`
	sandboxWhen
}

type sandboxTLS struct {
	Count int    `json:"count"`
	Name  string `json:"name"`
	sandboxWhen
}

type sandboxAgentText struct {
	Where string `json:"where"` // input, output
	File  string `json:"file,omitempty"`
	Line  int    `json:"line"`
	Text  string `json:"text"`
	// Severity: high for text hidden from a person (hiddenLabels), warn for
	// a phrase — agent tooling (skills, prompts, their tests) is full of them
	Severity string `json:"severity"`
}

// tagLabel is what hiddenText and the image's mh-sandbox-scan say in place
// of text spelled in Unicode tag characters: a model reads it as ASCII, a
// person sees nothing, and nothing but a flag uses them — high. The rest is
// warn: a phrase (skills, prompts and injection tests have them), a run of
// zero-width characters or bidirectional controls (they deceive a person
// reading code, and the tests of terminals, editors and i18n have them).
const tagLabel = "(Unicode tag characters: text a model reads and a person does not see)"

func (a sandboxAgentText) severity() severity {
	if a.Text == tagLabel {
		return sevHigh
	}
	return sevWarn
}

// parseSandboxReport reads mh-sandbox-report --tsv. Lines it does not know
// are skipped: a newer image may say more.
func parseSandboxReport(tsv string) *sandboxReport {
	r := &sandboxReport{
		Decoys: []sandboxDecoy{}, VMProbes: []sandboxProbe{}, Privesc: []sandboxProbe{}, Commands: []sandboxCommand{}, Alerts: []sandboxAlert{}, Processes: []sandboxProc{}, Listening: []string{},
		Connections: []sandboxConn{}, DNS: []sandboxDNS{}, Requests: []sandboxRequest{}, TLSRefused: []sandboxTLS{}, AddressesAnAgent: []sandboxAgentText{}, Warnings: []string{}, Rules: []string{}, Accepted: []sandboxAccepted{},
		Changed:  sandboxChanged{Files: []string{}, Dirs: []string{}, WorkFiles: []sandboxWorkEntry{}, WorkDirs: []sandboxWorkEntry{}},
		commands: -1,
	}
	for _, line := range strings.Split(tsv, "\n") {
		f := strings.Split(line, "\t")
		at := func(i int) string {
			if i < len(f) {
				return untrusted(f[i], 512)
			}
			return ""
		}
		num := func(i int) int { n, _ := strconv.Atoi(at(i)); return n }
		// fields i and i+1: when first and last seen, against start
		when := func(i int) sandboxWhen { return r.when(at(i), at(i+1)) }
		switch f[0] {
		case "start":
			r.start, r.started = epochMS(at(1))
		case "user":
			r.user = at(1)
		case "token":
			r.token = at(1)
		case "section":
			r.netSeen = r.netSeen || at(1) == "net" || at(1) == "dns"
		case "net":
			r.Net = at(1)
			if r.Net == "off" {
				r.Warnings = append(r.Warnings, "no network of the sandbox in this image: the names the code looked up are not recorded")
			}
		case "dns":
			if at(1) == "off" { // an image before the sinkhole
				r.Net = "off"
				r.Warnings = append(r.Warnings, "no resolver of the sandbox in this image: the names the code looked up are not recorded")
			} else {
				r.DNS = append(r.DNS, sandboxDNS{Count: num(1), Type: at(2), Name: at(3), sandboxWhen: when(4)})
			}
		case "addr":
			if r.sinkAddr == nil {
				r.sinkAddr = map[string]string{}
			}
			r.sinkAddr[at(1)] = at(2)
		case "http":
			r.Requests = append(r.Requests, sandboxRequest{Count: num(1), Scheme: at(2), Method: at(3), URL: at(4), Bytes: num(5), CarriesToken: at(6) == "1", Port: num(7), sandboxWhen: when(8)})
		case "tls":
			r.TLSRefused = append(r.TLSRefused, sandboxTLS{Count: num(1), Name: at(2), sandboxWhen: when(3)})
		case "rules":
			r.rulesApplied = num(1)
		case "decoy":
			r.readers = r.readers || len(f) > 4
			r.Decoys = append(r.Decoys, sandboxDecoy{State: at(1), Path: at(2), Legitimately: at(3), By: []string{}, Opens: []sandboxOpen{}, tools: strings.Fields(at(4))})
		case "decoyby":
			if r.decoyBy == nil {
				r.decoyBy = map[string]map[string]int{}
			}
			if r.decoyBy[at(3)] == nil {
				r.decoyBy[at(3)] = map[string]int{}
			}
			r.decoyBy[at(3)][at(4)] += num(2)
			if r.decoyOpens == nil {
				r.decoyOpens = map[string][]sandboxOpen{}
			}
			r.decoyOpens[at(3)] = append(r.decoyOpens[at(3)], sandboxOpen{By: at(4), Count: num(2), sandboxWhen: when(5)})
		case "audit":
			if at(1) == "off" {
				r.Warnings = append(r.Warnings, "no audit in this image: how the code looked for a VM is not recorded")
			} else if n := num(2); n > 0 {
				r.Warnings = append(r.Warnings, fmt.Sprintf("audit lost %d events: vm_probes may be incomplete", n))
			}
		case "probe":
			r.VMProbes = append(r.VMProbes, sandboxProbe{Found: at(1) == "found", Count: num(2), Path: at(3), By: at(4), sandboxWhen: when(5)})
		case "privesc":
			r.Privesc = append(r.Privesc, sandboxProbe{Found: at(1) == "found", Count: num(2), Path: at(3), By: at(4), sandboxWhen: when(5)})
		case "commands":
			r.commands = num(1)
		case "alert":
			r.Alerts = append(r.Alerts, sandboxAlert{Kind: at(1), Severity: kindOf(at(1)).Severity.String(), Count: num(2), What: at(3), By: at(4), sandboxWhen: when(5)})
		case "exec":
			r.Commands = append(r.Commands, sandboxCommand{Count: num(1), Args: at(2), PID: num(5), PPID: num(6), sandboxWhen: when(3)})
		case "file":
			r.Changed.Files = append(r.Changed.Files, at(1))
		case "dir":
			r.Changed.Dirs = append(r.Changed.Dirs, at(1))
		case "work":
			w := sandboxWorkEntry{Entry: at(2), Count: num(3)}
			if at(1) == "dir" {
				r.Changed.WorkDirs = append(r.Changed.WorkDirs, w)
			} else {
				r.Changed.WorkFiles = append(r.Changed.WorkFiles, w)
			}
		case "proc":
			r.Processes = append(r.Processes, sandboxProc{PID: num(1), Args: at(2)})
		case "listen":
			r.Listening = append(r.Listening, at(1)+" "+at(2))
		case "agent":
			r.AddressesAnAgent = append(r.AddressesAnAgent, sandboxAgentText{Where: at(1), File: at(2), Line: num(3), Text: at(4)})
		}
	}
	return r
}

// epochMS reads a time as the VM's tools write it, seconds since the epoch
// with a fraction (date +%s.%N, audit's, $EPOCHREALTIME), in milliseconds.
func epochMS(s string) (int64, bool) {
	sec, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	frac = (frac + "000")[:3]
	ms, err := strconv.Atoi(frac)
	if err != nil {
		return 0, false
	}
	return n*1000 + int64(ms), true
}

// when is a finding's first and last time (epoch seconds, as the VM wrote
// them) in milliseconds from the command's start; unknown ones, none.
func (r *sandboxReport) when(first, last string) sandboxWhen {
	var w sandboxWhen
	if !r.started {
		return w
	}
	if t, ok := epochMS(first); ok {
		d := t - r.start
		w.FirstMS = &d
	}
	if t, ok := epochMS(last); ok {
		d := t - r.start
		w.LastMS = &d
	}
	return w
}

// cmpWhen orders two first times, the unknown last.
func cmpWhen(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*a, *b)
}

// tokenIn says whether s carries this run's decoy token.
func (r *sandboxReport) tokenIn(s string) bool {
	return r.token != "" && strings.Contains(strings.ToLower(s), strings.ToLower(r.token))
}

// commonProbes are read by ordinary programs too — Node's os.cpus(), cgroup
// limits read by Node, Go and the JVM — so they alone do not suggest evasion.
var commonProbes = map[string]bool{"/proc/cpuinfo": true, "/proc/self/cgroup": true}

func (r *sandboxReport) summarize() {
	s := &r.Summary
	for i := range r.Decoys {
		d := &r.Decoys[i]
		d.grade(r.decoyBy[d.Path])
		d.Opens = append(d.Opens, r.decoyOpens[d.Path]...)
		slices.SortStableFunc(d.Opens, func(a, b sandboxOpen) int { return cmpWhen(a.FirstMS, b.FirstMS) })
		if d.State != "untouched" {
			s.DecoysRead++
		}
	}
	s.VMProbes = len(r.VMProbes)
	for _, p := range r.VMProbes {
		if !commonProbes[p.Path] {
			s.EvasionSuspected = true
		}
	}
	s.Privesc = len(r.Privesc)
	s.Alerts = len(r.Alerts)
	s.Commands = max(r.commands, len(r.Commands))
	s.ChangedOutsideWork = len(r.Changed.Files) + len(r.Changed.Dirs)
	s.ProcessesLeft = len(r.Processes)
	s.Listening = len(r.Listening)
	for _, c := range r.Connections {
		s.ConnectionsRefused += int(c.Count)
	}
	s.AddressesAnAgent = len(r.AddressesAnAgent)
	names := map[string]bool{}
	for _, d := range r.DNS {
		names[d.Name] = true
	}
	s.DNSNames = len(names)
	s.Requests = len(r.Requests) + len(r.TLSRefused)
	for _, q := range r.Requests {
		if q.CarriesToken {
			s.SecretsSent++
		}
	}
	for _, d := range r.DNS {
		if r.tokenIn(d.Name) {
			s.SecretsSent++
		}
	}
	for i := range r.AddressesAnAgent {
		r.AddressesAnAgent[i].Severity = r.AddressesAnAgent[i].severity().String()
	}

	counts := map[severity]int{}
	for _, c := range r.categories(viewStyle{}, false) {
		for _, f := range c.items {
			counts[f.sev]++
		}
	}
	s.High, s.Warn, s.Info = counts[sevHigh], counts[sevWarn], counts[sevInfo]
	switch {
	case !r.Complete:
		r.Verdict = "incomplete"
	case s.High > 0:
		r.Verdict = "suspicious"
	case r.notRun():
		// the shell's 127 (not found), 126 (not executable): what the code
		// does was not seen, and nothing here may read as clean
		r.Verdict = "did_not_run"
	case s.Warn > 0:
		r.Verdict = "review"
	default:
		r.Verdict = "clean"
	}
}

// notRun: the command never started — the shell could not find it (127)
// or run it (126). A missing tool (npm, go) or a path that is not there.
func (r *sandboxReport) notRun() bool {
	return !r.TimedOut && (r.ExitCode == 126 || r.ExitCode == 127)
}

// failed says why a run that started ended early, for the warnings: what
// the code would have done past the failure is not in the report.
func (r *sandboxReport) failed() string {
	if r.TimedOut || r.ExitCode == 0 || r.notRun() {
		return ""
	}
	return fmt.Sprintf("the command failed (exit %d): what it would have done past the failure is not in this report (-o: its output)", r.ExitCode)
}

// grade names who opened the decoy and how serious that is: info when it
// was only read, and only by the tool that reads it (npm, ~/.npmrc); high
// otherwise — tampered, deleted, read by anything else, or by no program
// audit saw (unknown is not innocent).
func (d *sandboxDecoy) grade(by map[string]int) {
	d.counts = by
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	d.By = d.By[:0]
	own := len(names) > 0
	for _, n := range names {
		d.By = append(d.By, n+" ×"+strconv.Itoa(by[n]))
		if !slices.Contains(d.tools, n) {
			own = false
		}
	}
	d.ByItsTool = d.State == "READ" && own
	switch {
	case d.State == "untouched":
		d.Severity = ""
	case d.ByItsTool, d.Accepted != "":
		d.Severity = sevInfo.String()
	default:
		d.Severity = sevHigh.String()
	}
}

// names are the paths the code created or changed, ~/work's entries
// included: a file's name can speak to an agent as well as its content.
func (c sandboxChanged) names() []string {
	n := append(append([]string(nil), c.Files...), c.Dirs...)
	for _, e := range c.WorkFiles {
		n = append(n, "~/work/"+e.Entry)
	}
	return n
}
