// Package cli implements mh, the command-line client for the microhosted
// daemon: docker-style commands over the HTTP API on its Unix socket, so the
// everyday operations (create a VM, list networks, open an egress hole) are
// one short command instead of a hand-written curl with a JSON body.
//
// Shape of the command line, deliberately the same as docker's:
//
//	mh <object> <verb> [ARGS] [FLAGS]      mh network create lab --intra
//	mh <shortcut> [ARGS] [FLAGS]           mh ps, mh run, mh exec  (VMs)
//
// The verb may also come first ("mh create vm", "mh list network"): it is
// rewritten to the object-first form before dispatch, so both spellings reach
// exactly the same code.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// env is what every command runs against: the process's streams and a lazily
// built API client (help and argument errors must not need a daemon).
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	host   string
	client *Client
}

func (e *env) api() (*Client, error) {
	if e.client == nil {
		c, err := NewClient(e.host)
		if err != nil {
			return nil, err
		}
		e.client = c
	}
	return e.client, nil
}

// command is one leaf of the tree ("create" under "vm").
type command struct {
	name    string
	aliases []string
	args    string // positional-argument synopsis for the usage line
	summary string
	// help, when set, is printed under the summary in --help: the concepts a
	// flag list alone doesn't explain. examples closes the help. Both are
	// optional; flags of a command with help are listed in definition order,
	// so they can be grouped by concept instead of alphabetically.
	help     string
	examples string
	run      func(e *env, cmd *command, path string, args []string) error
}

// group is a management object ("vm", "network"...) holding its verbs.
type group struct {
	name    string
	aliases []string
	summary string
	cmds    []*command
}

// shortcut is a top-level command that is really a group's verb — docker's
// "docker ps" for "docker container ls".
type shortcut struct {
	name, group, cmd string
}

var groups = []*group{vmGroup, networkGroup, volumeGroup, snapshotGroup, templateGroup, systemGroup}

var shortcuts = []shortcut{
	{"ps", "vm", "ls"},
	{"run", "vm", "create"},
	{"exec", "vm", "exec"},
	{"stop", "vm", "stop"},
	{"start", "vm", "start"},
	{"rm", "vm", "rm"},
	{"cp", "vm", "cp"},
	{"logs", "vm", "logs"},
	{"fork", "vm", "fork"},
	{"restore", "vm", "restore"},
	{"inspect", "vm", "inspect"},
	{"images", "template", "ls"},
	{"health", "system", "health"},
	{"info", "system", "info"},
}

// exitError ends the process with code without printing anything more: the
// command already said what it had to (exec's guest exit code, a degraded
// health report).
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// usageError is a malformed command line; it gets the command's usage hint.
type usageError struct {
	path string
	msg  string
}

func (e usageError) Error() string { return e.msg }

func usagef(path, format string, a ...any) error {
	return usageError{path: path, msg: fmt.Sprintf(format, a...)}
}

// Main runs mh with args (without the program name) and returns the process
// exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}

	fs := newFlags("mh")
	var host string
	fs.stringVar(&host, "host", "H", "", "daemon endpoint `HOST`: socket path, unix://PATH or tcp://HOST:PORT (env MICROHOSTED_HOST; default "+DefaultSocket+")")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(stdout, fs)
			return 0
		}
		fmt.Fprintf(stderr, "mh: %v\nRun 'mh --help' for usage.\n", err)
		return 2
	}
	e.host = ResolveHost(host)
	args = fs.Args()
	if len(args) == 0 {
		printHelp(stdout, fs)
		return 0
	}

	err := dispatch(e, args, fs)
	var ex exitError
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ex):
		return ex.code
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "mh: %s\nRun '%s --help' for usage.\n", ue.msg, ue.path)
		return 2
	default:
		fmt.Fprintf(stderr, "mh: %v\n", err)
		return 1
	}
}

func dispatch(e *env, args []string, global *flagSet) error {
	name := args[0]
	if name == "help" {
		if len(args) > 1 {
			if g := findGroup(args[1]); g != nil {
				printGroupHelp(e.stdout, g)
				return nil
			}
			if s := findShortcut(args[1]); s != nil {
				return runCommand(e, findGroup(s.group), s.cmd, []string{"--help"}, "mh "+s.name)
			}
		}
		printHelp(e.stdout, global)
		return nil
	}
	// Verb-first spelling: "mh create vm X" → "mh vm create X". Checked before
	// shortcuts so "mh rm network lab" reaches the network, not the VM rm.
	if len(args) > 1 {
		if g := findGroup(args[1]); g != nil && g.find(name) != nil {
			return runCommand(e, g, name, args[2:], "mh "+g.name+" "+g.find(name).name)
		}
	}
	if g := findGroup(name); g != nil {
		if len(args) == 1 || args[1] == "-h" || args[1] == "--help" {
			printGroupHelp(e.stdout, g)
			return nil
		}
		c := g.find(args[1])
		if c == nil {
			return usagef("mh "+g.name, "unknown command %q for %q", args[1], "mh "+g.name)
		}
		return runCommand(e, g, args[1], args[2:], "mh "+g.name+" "+c.name)
	}
	if s := findShortcut(name); s != nil {
		return runCommand(e, findGroup(s.group), s.cmd, args[1:], "mh "+s.name)
	}
	return usagef("mh", "unknown command %q", name)
}

func runCommand(e *env, g *group, verb string, args []string, path string) error {
	c := g.find(verb)
	return c.run(e, c, path, args)
}

func findGroup(name string) *group {
	for _, g := range groups {
		if g.name == name || contains(g.aliases, name) {
			return g
		}
	}
	return nil
}

func findShortcut(name string) *shortcut {
	for i := range shortcuts {
		if shortcuts[i].name == name {
			return &shortcuts[i]
		}
	}
	return nil
}

func (g *group) find(name string) *command {
	for _, c := range g.cmds {
		if c.name == name || contains(c.aliases, name) {
			return c
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func printHelp(w io.Writer, global *flagSet) {
	fmt.Fprint(w, `mh — command-line client for the microhosted daemon

Usage:  mh [-H HOST] COMMAND [ARGS...] [FLAGS]

Common commands (VMs):
`)
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, s := range shortcuts {
		g := findGroup(s.group)
		fmt.Fprintf(tw, "  %s\t%s\n", s.name, g.find(s.cmd).summary)
	}
	tw.Flush()
	fmt.Fprint(w, "\nManagement commands:\n")
	for _, g := range groups {
		fmt.Fprintf(tw, "  %s\t%s\n", g.name, g.summary)
	}
	tw.Flush()
	fmt.Fprint(w, "\nGlobal flags:\n")
	global.printDefaults(w)
	fmt.Fprint(w, `
The verb may also come first: "mh create vm base-alpine", "mh list network".
Run 'mh COMMAND --help' for more on a command.
`)
}

func printGroupHelp(w io.Writer, g *group) {
	fmt.Fprintf(w, "Usage:  mh %s COMMAND\n\n%s\n\nCommands:\n", g.name, g.summary)
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, c := range g.cmds {
		name := c.name
		if len(c.aliases) > 0 {
			name += " (" + strings.Join(c.aliases, ", ") + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", name, c.summary)
	}
	tw.Flush()
	if len(g.aliases) > 0 {
		fmt.Fprintf(w, "\nAliases: %s\n", strings.Join(g.aliases, ", "))
	}
	fmt.Fprintf(w, "\nRun 'mh %s COMMAND --help' for more on a command.\n", g.name)
}

// ---------------------------------------------------------------------------
// Flags: stdlib flag, plus docker's conventions — "-q, --quiet" pairs, help
// that prints "--long", and flags accepted after positional arguments
// ("mh run base-alpine --net lab").
// ---------------------------------------------------------------------------

type flagSet struct {
	*flag.FlagSet
	path string
	// shortOf maps a long flag name to its one-letter alias, for help output.
	shortOf map[string]string
	// defined lists the visible flags in definition order; inOrder makes help
	// use it instead of sorting.
	defined []string
	inOrder bool
}

func newFlags(path string) *flagSet {
	f := &flagSet{FlagSet: flag.NewFlagSet(path, flag.ContinueOnError), path: path, shortOf: map[string]string{}}
	f.SetOutput(io.Discard)
	return f
}

// newCmdFlags builds the flag set of command c invoked as path.
func newCmdFlags(e *env, path string, c *command) *flagSet {
	f := newFlags(path)
	f.Usage = func() {
		fmt.Fprintf(e.stdout, "Usage:  %s %s\n\n%s\n", path, strings.TrimSpace(c.args+" [FLAGS]"), c.summary)
		if c.help != "" {
			fmt.Fprintf(e.stdout, "\n%s\n", strings.Trim(c.help, "\n"))
		}
		hasFlags := false
		f.VisitAll(func(fl *flag.Flag) { hasFlags = hasFlags || fl.Usage != "" })
		if hasFlags {
			fmt.Fprint(e.stdout, "\nFlags:\n")
			f.printDefaults(e.stdout)
		}
		if c.examples != "" {
			fmt.Fprintf(e.stdout, "\nExamples:\n%s\n", strings.Trim(c.examples, "\n"))
		}
	}
	f.inOrder = c.help != ""
	return f
}

// alias registers the one-letter spelling of an already-defined long flag,
// hidden from help (which shows the pair on one line instead).
func (f *flagSet) alias(long, short string) {
	if short == "" {
		return
	}
	fl := f.Lookup(long)
	f.Var(fl.Value, short, "")
	f.shortOf[long] = short
}

// hidden registers an old spelling of an already-defined flag that keeps
// working but no longer shows in help — renames must not break scripts.
func (f *flagSet) hidden(long string, old ...string) {
	fl := f.Lookup(long)
	for _, o := range old {
		f.Var(fl.Value, o, "")
	}
}

func (f *flagSet) boolVar(p *bool, long, short, usage string) {
	f.BoolVar(p, long, false, usage)
	f.alias(long, short)
	f.defined = append(f.defined, long)
}

func (f *flagSet) stringVar(p *string, long, short, def, usage string) {
	f.StringVar(p, long, def, usage)
	f.alias(long, short)
	f.defined = append(f.defined, long)
}

func (f *flagSet) int64Var(p *int64, long, short string, usage string) {
	f.Int64Var(p, long, 0, usage)
	f.alias(long, short)
	f.defined = append(f.defined, long)
}

// listVar is a repeatable string flag: --out A --out B.
func (f *flagSet) listVar(p *[]string, long, short, usage string) {
	f.Var((*listValue)(p), long, usage)
	f.alias(long, short)
	f.defined = append(f.defined, long)
}

type listValue []string

func (l *listValue) String() string     { return strings.Join(*l, ",") }
func (l *listValue) Set(s string) error { *l = append(*l, s); return nil }

// isSet reports whether the flag (by long name) appeared on the command line,
// under either spelling.
func (f *flagSet) isSet(long string) bool {
	set := false
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == long || (f.shortOf[long] != "" && fl.Name == f.shortOf[long]) {
			set = true
		}
	})
	return set
}

func (f *flagSet) printDefaults(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	var names []string
	if f.inOrder {
		names = f.defined
	} else {
		f.VisitAll(func(fl *flag.Flag) {
			if fl.Usage != "" {
				names = append(names, fl.Name)
			}
		})
		sort.Strings(names)
	}
	for _, n := range names {
		fl := f.Lookup(n)
		arg, usage := flag.UnquoteUsage(fl)
		spell := "--" + n
		if s := f.shortOf[n]; s != "" {
			spell = "-" + s + ", " + spell
		} else {
			spell = "    " + spell
		}
		if arg != "" {
			spell += " " + arg
		}
		if fl.DefValue != "" && fl.DefValue != "false" && fl.DefValue != "0" {
			usage += fmt.Sprintf(" (default %s)", fl.DefValue)
		}
		fmt.Fprintf(tw, "  %s\t%s\n", spell, usage)
	}
	tw.Flush()
}

// parse reads flags anywhere on the line and returns the positionals, docker
// style. "--" ends flag parsing; everything after it is positional.
func (f *flagSet) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err // flag already printed f.Usage
			}
			return nil, usageError{path: f.path, msg: err.Error()}
		}
		rest := f.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseLeading reads flags only up to the first positional: for exec, whose
// trailing arguments are the guest command and may look like flags themselves.
func (f *flagSet) parseLeading(args []string) ([]string, error) {
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usageError{path: f.path, msg: err.Error()}
	}
	return f.Args(), nil
}

// ---------------------------------------------------------------------------
// Output.
// ---------------------------------------------------------------------------

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printInspect prints one object bare (so "| jq .guest_ip" works) and several
// as an array.
func printInspect[T any](w io.Writer, items []T) error {
	if len(items) == 1 {
		return printJSON(w, items[0])
	}
	return printJSON(w, items)
}

func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// eachArg runs fn on every argument and keeps going past failures, like
// "docker rm a b c": one bad ID must not leave the rest undone. Failures are
// reported as they happen; the returned error only sets the exit code.
func eachArg(e *env, args []string, fn func(string) error) error {
	failed := 0
	for _, a := range args {
		if err := fn(a); err != nil {
			fmt.Fprintf(e.stderr, "mh: %s: %v\n", a, err)
			failed++
		}
	}
	if failed > 0 {
		return exitError{code: 1}
	}
	return nil
}
