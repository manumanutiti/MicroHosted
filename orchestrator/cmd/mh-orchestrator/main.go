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
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"microhosted/orchestrator/engine"
	"microhosted/orchestrator/orch"
	"microhosted/orchestrator/spec"
)

const usage = `Usage: mh-orchestrator COMMAND -f PLANT.yaml [FLAGS] [FUNCTION]

Commands:
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
  down       remove everything the orchestrator owns (quarantined VMs stay)

Flags:
  -f FILE    the plant spec (required)
  -H SOCKET  the engine's API socket (default $MICROHOSTED_HOST, $MICROHOSTED_SOCKET or /run/microhosted.sock)
  -y         apply / down without asking (required when stdin is not a terminal)
  -state DIR failure records and flags (default $XDG_STATE_HOME/mh-orchestrator
             or ~/.local/state/mh-orchestrator)

run writes one JSON line per finished cycle to stdout; logs go to stderr.
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
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if *file == "" || fs.NArg() > 1 || (fs.NArg() == 1 && cmd != "failures") {
		fs.Usage()
		os.Exit(2)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	state, err := orch.OpenState(stateDir)
	if err != nil {
		return fmt.Errorf("state directory %s: %w", stateDir, err)
	}
	o := orch.New(engine.New(engine.ResolveSocket(host)), s, logger, os.Stdout, state)

	switch cmd {
	case "plan":
		p, err := o.Plan(ctx)
		if err != nil {
			return err
		}
		if h := lockHolder(); h != nil && h.path == absPath(file) {
			printRollout(os.Stdout, p, h)
			return nil
		}
		printPlan(os.Stdout, p)
		return nil

	case "apply":
		unlock, h, err := lock(file)
		if h != nil {
			return handOver(ctx, o, file, h, yes)
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
		if len(p.Actions) == 0 {
			return nil
		}
		if ok, err := confirm("Apply these changes?", yes); !ok {
			return err
		}
		if err := o.Apply(ctx, p); err != nil {
			return err
		}
		fmt.Println("Applied.")
		return nil

	case "run":
		unlock, _, err := lock(file)
		if err != nil {
			return err
		}
		defer unlock()
		logger.Printf("running %s (stop with Ctrl-C: cycles in flight are cleaned up, persistent functions keep running; SIGHUP or mh-orchestrator apply reloads it)", file)
		return o.Run(ctx, reloads(ctx, file, logger))

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
		unlock, _, err := lock(file)
		if err != nil {
			return err
		}
		defer unlock()
		if ok, err := confirm("Destroy every VM and network the orchestrator owns?", yes); !ok {
			return err
		}
		kept, err := o.Down(ctx)
		for _, k := range kept {
			fmt.Println("kept:", k)
		}
		return err
	}
	return fmt.Errorf("unknown command %q (see mh-orchestrator --help)", cmd)
}

func lockPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "mh-orchestrator.lock")
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
func lock(spec string) (func(), *holder, error) {
	path := lockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, lockHolder(), fmt.Errorf("another mh-orchestrator (run, apply or down) holds %s", path)
		}
		return nil, nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), absPath(spec))
	return func() { _ = f.Truncate(0); f.Close() }, nil, nil
}

// lockHolder reads who holds the lock; nil when nobody does.
func lockHolder() *holder {
	f, err := os.Open(lockPath())
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
func handOver(ctx context.Context, o *orch.Orchestrator, file string, h *holder, yes bool) error {
	if h.path != absPath(file) {
		return fmt.Errorf("an mh-orchestrator (pid %d) is running %s: change that file and apply it, or stop it first", h.pid, h.path)
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
func reloads(ctx context.Context, file string, logger *log.Logger) <-chan *spec.Spec {
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
