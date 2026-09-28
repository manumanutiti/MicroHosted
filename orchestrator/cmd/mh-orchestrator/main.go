// mh-orchestrator keeps a MicroHosted host in the state a plant spec declares.
// See docs/orchestrator.md and orchestrator/examples/.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"microhosted/orchestrator/engine"
	"microhosted/orchestrator/orch"
	"microhosted/orchestrator/spec"
)

const usage = `Usage: mh-orchestrator COMMAND [-f PLANT.yml] [FLAGS] [FUNCTION]

Commands:
  up         apply, then keep it: run cycles, replace what dies or fails
             (run in the foreground; up -d leaves it in the background)
  validate   check the spec on its own (no engine needed)
  plan       show what apply would change; changes nothing
  apply      converge once: networks, persistent functions, pruning. With a
             run going on this file, hand the new spec to it instead: it
             rolls the changes out one function at a time
  run        apply, then keep it: run cycles, replace what dies or fails;
             reloads the file on SIGHUP (what apply sends it)
  status     every function, its VMs, their health and last failure
  failures   why a function's VMs failed (all functions, or FUNCTION): cause,
             output and console tail, captured before each VM was destroyed
  down       stop the project's run, if any, and remove everything the
             project owns (quarantined VMs stay)

Flags:
  -f FILE    the plant spec (default ./microse.yml, or ./microse.yaml)
  -H SOCKET  the engine's API socket (default $MICROHOSTED_HOST, $MICROHOSTED_SOCKET or /run/microhosted.sock)
  -y         apply / down without asking (required when stdin is not a terminal)
  -d         up: in the background (log in the project's state directory)
  -state DIR failure records and flags (default $XDG_STATE_HOME/mh-orchestrator
             or ~/.local/state/mh-orchestrator)

Projects: each plant spec is a project — its name:, else its directory's
name, as docker compose names them. What the orchestrator creates carries the
project; a spec sees and changes only its own project's objects, so several
run side by side. VMs are named PROJECT-FUNCTION-N; network names are shared
by the whole host.

run writes one JSON line per finished cycle to stdout; logs go to stderr.

${NAME} in the spec's values is taken from the environment, else from the
.env file next to the spec (docker compose's rules; ${NAME:-default},
${NAME:?message}; a bare $NAME is left for the guest's shell). It carries
what mh build prints:
  echo "SITE=$(mh build -q -t site ./site)" >> .env    # image: ${SITE}
A running orchestrator reloads the file with its own environment: keep such
variables in .env.

A function may say build: DIR (or a build.yml) instead of image:. plan,
apply and run run mh build on it and use the image it makes; mh build tags
by the fingerprint of the spec and its files, so an unchanged build: builds
nothing. A change to the build context is a new image, rolled out like any
other change. A running orchestrator never builds on reload: apply does it
before handing the spec over.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		if len(os.Args) < 2 {
			os.Exit(2)
		}
		return
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	file := fs.String("f", "", "plant spec")
	host := fs.String("H", "", "engine socket")
	yes := fs.Bool("y", false, "do not ask")
	stateDir := fs.String("state", orch.DefaultStateDir(), "state directory")
	detach := fs.Bool("d", false, "up: apply, then keep it running in the background")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if *file == "" {
		*file = spec.FindIn(".", spec.PlantFiles)
	}
	if fs.NArg() > 1 || (fs.NArg() == 1 && cmd != "failures") {
		fs.Usage()
		os.Exit(2)
	}
	if *detach && cmd != "up" {
		fs.Usage()
		os.Exit(2)
	}
	if cmd == "up" && !*detach {
		cmd = "run" // up in the foreground is run
	}
	if err := run(cmd, *file, *host, *stateDir, fs.Arg(0), *yes); err != nil {
		fmt.Fprintf(os.Stderr, "mh-orchestrator %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func run(cmd, file, host, stateDir, function string, yes bool) error {
	s, err := spec.Load(file)
	if err != nil {
		return err
	}
	if cmd == "validate" {
		fmt.Printf("%s: valid — %d networks, %d functions\n", file, len(s.Networks), len(s.Functions))
		return nil
	}
	// build: functions get their image: plan, apply and run build what is
	// missing (mh build skips what the store already has); the others only
	// look, and leave a function not built without an image.
	switch cmd {
	case "plan", "apply", "run", "up":
		if _, err := resolveBuilds(s, host, buildMissing); err != nil {
			return err
		}
	default:
		if _, err := resolveBuilds(s, host, lookOnly); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	// Each project keeps its failure records apart, and has its own lock:
	// projects run side by side.
	projectDir := filepath.Join(stateDir, s.Project)
	state, err := orch.OpenState(projectDir)
	if err != nil {
		return fmt.Errorf("state directory %s: %w", projectDir, err)
	}
	o := orch.New(engine.New(engine.ResolveSocket(host)), s, logger, os.Stdout, state)

	switch cmd {
	case "plan":
		p, err := o.Plan(ctx)
		if err != nil {
			return err
		}
		if h := lockHolder(s.Project); h != nil && h.path == absPath(file) {
			printRollout(os.Stdout, p, h)
			return nil
		}
		printPlan(os.Stdout, p)
		return nil

	case "apply", "up":
		// up -d: apply without asking, then leave a run supervising it.
		if cmd == "up" {
			yes = true
		}
		unlock, h, err := lock(s.Project, file)
		if h != nil {
			return handOver(ctx, o, s, file, h, yes)
		}
		if err != nil {
			return err
		}
		defer unlock()
		p, err := o.Plan(ctx)
		if err != nil {
			return err
		}
		printPlan(os.Stdout, p)
		if len(p.Actions) > 0 {
			if ok, err := confirm("Apply these changes?", yes); !ok {
				return err
			}
		}
		if len(p.Actions) > 0 {
			if err := o.Apply(ctx, p); err != nil {
				return err
			}
			fmt.Println("Applied.")
		}
		if cmd == "up" {
			unlock()
			return startDetached(file, host, stateDir, projectDir, s.Project)
		}
		return nil

	case "run":
		unlock, _, err := lock(s.Project, file)
		if err != nil {
			return err
		}
		defer unlock()
		logger.Printf("project %s: running %s (stop with Ctrl-C: cycles in flight are cleaned up, persistent functions keep running; SIGHUP or mh-orchestrator apply reloads it)", s.Project, file)
		return o.Run(ctx, reloads(ctx, file, host, logger))

	case "status":
		fns, orphans, err := o.Status(ctx)
		if err != nil {
			return err
		}
		printStatus(os.Stdout, fns, orphans, file)
		return nil

	case "failures":
		names := s.FunctionOrder
		if function != "" {
			if _, ok := s.Functions[function]; !ok {
				return fmt.Errorf("no function %q in %s", function, file)
			}
			names = []string{function}
		}
		printFailures(os.Stdout, state, names)
		return nil

	case "down":
		if ok, err := confirm(fmt.Sprintf("Destroy every VM and network of project %s?", s.Project), yes); !ok {
			return err
		}
		// A run keeping this project up stops first, or it would bring back
		// what down removes.
		if err := stopRunner(s.Project); err != nil {
			return err
		}
		unlock, _, err := lock(s.Project, file)
		if err != nil {
			return err
		}
		defer unlock()
		kept, err := o.Down(ctx)
		for _, k := range kept {
			fmt.Println("kept:", k)
		}
		return err
	}
	return fmt.Errorf("unknown command %q (see mh-orchestrator --help)", cmd)
}

// lockPath is the project's lock: one run, apply or down at a time per
// project; different projects run side by side.
func lockPath(project string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "mh-orchestrator-"+project+".lock")
}

// holder is the process that holds the lock, as it recorded itself.
type holder struct {
	pid  int
	path string // its spec, absolute
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// lock makes this the only orchestrator changing the engine: a run and an
// apply on the same host would both think they own the functions. The holder
// writes its pid and spec into the lock file, so an apply can hand a new spec
// to a running orchestrator instead of fighting it. When the lock is taken,
// the holder is returned along with the error.
func lock(project, spec string) (func(), *holder, error) {
	path := lockPath(project)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, lockHolder(project), fmt.Errorf("another mh-orchestrator (run, apply or down) of project %s holds %s", project, path)
		}
		return nil, nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), absPath(spec))
	return func() { _ = f.Truncate(0); f.Close() }, nil, nil
}

// lockHolder reads who holds the lock; nil when nobody does.
func lockHolder(project string) *holder {
	f, err := os.Open(lockPath(project))
	if err != nil {
		return nil
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		return nil // nobody holds it
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 2 {
		return nil
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 0 {
		return nil
	}
	return &holder{pid: pid, path: lines[1]}
}

// handOver gives a new spec to the orchestrator running on this file: it is
// validated and planned here, shown, and — confirmed — sent as SIGHUP; the
// running orchestrator plans it again and rolls it out.
func handOver(ctx context.Context, o *orch.Orchestrator, s *spec.Spec, file string, h *holder, yes bool) error {
	if h.path != absPath(file) {
		return fmt.Errorf("an mh-orchestrator (pid %d) is running %s: change that file and apply it, or stop it first", h.pid, h.path)
	}
	// The running orchestrator reloads the file itself, with its own
	// environment: a value only this shell has would not reach it.
	envFile, err := spec.ReadEnvFile(filepath.Dir(file))
	if err != nil {
		return err
	}
	var shell []string
	for name, src := range s.Vars {
		if v, inFile := envFile[name]; src == spec.FromEnvironment && (!inFile || v != os.Getenv(name)) {
			shell = append(shell, name)
		}
	}
	if len(shell) > 0 {
		sort.Strings(shell)
		return fmt.Errorf("the spec takes %s from this shell's environment, which the running orchestrator (pid %d) does not see when it reloads the file: put them in %s next to the spec (e.g. mh build -q … | sed 's/^/NAME=/' > %s), then apply",
			strings.Join(shell, ", "), h.pid, filepath.Join(filepath.Dir(file), spec.EnvFile), spec.EnvFile)
	}
	p, err := o.Plan(ctx)
	if err != nil {
		return err
	}
	printRollout(os.Stdout, p, h)
	if ok, err := confirm("Hand this spec to the running orchestrator?", yes); !ok {
		return err
	}
	if err := syscall.Kill(h.pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signalling pid %d: %w", h.pid, err)
	}
	fmt.Printf("Handed over to pid %d. Follow its log, or: mh-orchestrator status -f %s\n", h.pid, file)
	return nil
}

// reloads turns each SIGHUP into a freshly loaded spec for the running
// orchestrator; a file that does not load is logged and the running spec
// kept. Only the latest spec waits if a roll-out is still going on.
func reloads(ctx context.Context, file, host string, logger *log.Logger) <-chan *spec.Spec {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	out := make(chan *spec.Spec, 1)
	go func() {
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
			}
			s, err := spec.Load(file)
			if err == nil {
				// A reload never builds (no terminal for sudo): apply builds
				// before it hands the spec over, so the images are there.
				var missing []string
				missing, err = resolveBuilds(s, host, lookOnly)
				if err == nil && len(missing) > 0 {
					err = fmt.Errorf("functions %s: image not built; mh-orchestrator apply builds it", strings.Join(missing, ", "))
				}
			}
			if err != nil {
				logger.Printf("reload refused, the previous spec keeps running: %v", err)
				continue
			}
			select {
			case <-out: // a newer spec replaces one not picked up yet
			default:
			}
			out <- s
		}
	}()
	return out
}

func printRollout(w io.Writer, p *orch.Plan, h *holder) {
	fmt.Fprintf(w, "An orchestrator (pid %d) is running this spec. Applied, it would roll out:\n", h.pid)
	fmt.Fprintf(w, "Worst case: %d VMs, %d MB\n", p.PeakVMs, p.PeakMemMB)
	for _, a := range p.Rollout() {
		fmt.Fprintln(w, a)
	}
	fmt.Fprintln(w, "plus every changed transaction or window function. Changed functions are updated one at a")
	fmt.Fprintln(w, "time, each verified before the next; one that fails goes back to its previous version and")
	fmt.Fprintln(w, "stops the roll-out.")
}

func confirm(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("stdin is not a terminal: pass -y to confirm")
	}
	fmt.Printf("%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return true, nil
	}
	return false, errors.New("cancelled")
}

func printPlan(w io.Writer, p *orch.Plan) {
	fmt.Fprintf(w, "Worst case: %d VMs, %d MB\n", p.PeakVMs, p.PeakMemMB)
	for _, k := range p.Kept {
		fmt.Fprintf(w, "kept     %s\n", k)
	}
	if len(p.Actions) == 0 {
		fmt.Fprintln(w, "No changes: the engine matches the spec.")
		return
	}
	for _, a := range p.Actions {
		fmt.Fprintln(w, a)
	}
}

func printStatus(w io.Writer, fns []orch.FunctionStatus, orphans []string, file string) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "FUNCTION\tMODE\tVM\tID\tSTATE\tIP\tAGE\tHEALTH")
	for _, f := range fns {
		if len(f.Instances) == 0 {
			idle := "-"
			if f.Mode == spec.ModePersistent {
				idle = "NOT RUNNING"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\t\t\n", f.Name, f.Mode, idle)
			continue
		}
		for _, in := range f.Instances {
			state, ip := in.State, in.IP
			if in.Quarantined {
				// Off the network: the address is what the guest believes
				// it has, not a live one (as mh ps shows it).
				state += " (quarantined)"
				if ip != "" {
					ip = "(" + ip + ")"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", f.Name, f.Mode, in.Name, in.ID, state, ip, in.Age, oneLine(in.Health, 80))
		}
	}
	tw.Flush()
	var failing []orch.FunctionStatus
	for _, f := range fns {
		if f.Degraded || f.Held != "" || f.LastFailure != nil {
			failing = append(failing, f)
		}
	}
	if len(failing) > 0 {
		fmt.Fprintln(w, "\nLast failures (details: mh-orchestrator failures -f "+file+" FUNCTION)")
		for _, f := range failing {
			flag := ""
			if f.Degraded {
				flag = "  DEGRADED: no more attempts until the orchestrator restarts"
			}
			if f.Held != "" {
				flag += "  HELD on its previous version (update failed; apply again to retry)"
			}
			if l := f.LastFailure; l != nil {
				fmt.Fprintf(w, "  %-12s %s ago, %s: %s (%d kept)%s\n", f.Name, since(l.At), l.VM, oneLine(l.Cause, 100), f.Failures, flag)
			} else {
				fmt.Fprintf(w, "  %-12s%s\n", f.Name, flag)
			}
		}
	}
	if len(orphans) == 0 {
		fmt.Fprintln(w, "\nOrphans: none")
		return
	}
	fmt.Fprintf(w, "\nOrphans (%d): owned, not in the spec — the next apply removes them\n", len(orphans))
	for _, o := range orphans {
		fmt.Fprintln(w, "  "+o)
	}
}

// printFailures shows the kept failure records, newest first. Output and
// console are the guest's: made safe for the terminal before printing.
func printFailures(w io.Writer, state *orch.State, names []string) {
	any := false
	for _, name := range names {
		st := state.Get(name)
		if len(st.Failures) == 0 {
			continue
		}
		any = true
		for i := len(st.Failures) - 1; i >= 0; i-- {
			f := st.Failures[i]
			fmt.Fprintf(w, "=== %s  %s  (%s ago)\n", f.Function, f.At, since(f.At))
			vm := f.VM
			if vm == "" {
				vm = "(never created)"
			} else {
				vm += " (" + f.VMID + ")"
			}
			fmt.Fprintf(w, "vm:      %s, %s, %s after it was created\n", vm, f.Mode, time.Duration(f.AfterMS)*time.Millisecond)
			fmt.Fprintf(w, "image:   %s\n", f.Image)
			fmt.Fprintf(w, "cause:   %s\n", orch.Printable(f.Cause))
			if f.Output != "" {
				fmt.Fprintf(w, "output:\n%s\n", indent(orch.Printable(f.Output)))
			}
			if f.Console != "" {
				fmt.Fprintf(w, "console (last %d KiB):\n%s\n", orch.ConsoleKept>>10, indent(orch.Printable(f.Console)))
			}
			fmt.Fprintln(w)
		}
	}
	if !any {
		fmt.Fprintln(w, "No failures recorded.")
	}
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  | " + strings.ReplaceAll(s, "\n", "\n  | ")
}

// oneLine fits guest text into a table cell.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(orch.Printable(s)), " ")
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

func since(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return "?"
	}
	return time.Since(t).Round(time.Second).String()
}
