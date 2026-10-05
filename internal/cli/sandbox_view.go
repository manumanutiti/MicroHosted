package cli

import (
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The report for a person (mh sandbox without --json): a verdict first, then
// one block per kind of finding, graded high, warn or info, its lines what
// the findings amount to (sandbox_compact.go) — then one line naming what was
// checked and found clean, so that an empty section never reads as an
// unchecked one. -v prints every finding instead, and what is only detail:
// every probe by every program, every command, what changed in ~/work. --output adds the
// program's own output, every line of it prefixed so it cannot pass for the
// report.
//
// Severities (the alerts' come from their kind, sandbox_kinds.go):
//
//	high  a decoy TAMPERED or DELETED, or READ by anything but its own tool;
//	      text hidden from a person (Unicode tags, zero-width runs, bidi
//	      controls); privesc: find -perm for setuid/setgid files,
//	      /etc/shadow, /etc/gshadow, /etc/sudoers*, a container runtime's
//	      socket, /proc/PID/mem
//	warn  a phrase addressed to an AI agent; every other privesc probe (cron,
//	      root's home, the kernel's switches, find -perm for writable files);
//	      a VM probe; a connection refused; a file changed outside ~/work
//	      that is not a cache or a temporary file; a process left; a
//	      listening socket
//	info  a decoy read only by its own tool (npm, ~/.npmrc); a lookup of sudo,
//	      su… (installers check for sudo); a VM probe ordinary programs make
//	      too (commonProbes); a cache, a temporary file, a directory whose
//	      entries changed; with -v, the commands run and what changed in
//	      ~/work
//
// Everything the code chose — paths, names, arguments, its output — arrives
// here through untrusted(): no escape of its own reaches the terminal, and no
// line of its output can pass for one of ours. Colors are ours alone.

// viewStyle is how to draw: color and Unicode symbols on a terminal, plain
// ASCII otherwise (pipes, files, NO_COLOR, TERM=dumb).
type viewStyle struct{ color bool }

func styleFor(w io.Writer) viewStyle {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return viewStyle{}
	}
	fi, err := f.Stat()
	return viewStyle{color: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (st viewStyle) paint(sgr, s string) string {
	if !st.color || s == "" {
		return s
	}
	return "\x1b[" + sgr + "m" + s + "\x1b[0m"
}

func (st viewStyle) dim(s string) string  { return st.paint("2", s) }
func (st viewStyle) bold(s string) string { return st.paint("1", s) }

func (st viewStyle) pick(fancy, plain string) string {
	if st.color {
		return fancy
	}
	return plain
}

func (st viewStyle) times() string { return st.pick("×", "x") }
func (st viewStyle) ell() string   { return st.pick("…", "...") }
func (st viewStyle) sep() string   { return st.pick(" · ", ", ") }

var sevColor = map[severity]string{sevHigh: "1;31", sevWarn: "33", sevInfo: "36"}
var sevLabel = map[severity]string{sevHigh: "HIGH", sevWarn: "WARN", sevInfo: "INFO"}

// badge is a category's grade, six columns wide either way.
func (st viewStyle) badge(s severity) string {
	if st.color {
		return st.paint(sevColor[s]+";7", " "+sevLabel[s]+" ")
	}
	return "[" + sevLabel[s] + "]"
}

// mark starts each finding's line.
func (st viewStyle) mark(s severity) string {
	if st.color {
		return st.paint(sevColor[s], "●")
	}
	return map[severity]string{sevHigh: "!", sevWarn: "*", sevInfo: "-"}[s]
}

// viewWidth is the width lines are kept to, where they can be.
const viewWidth = 100

// finding is one line under a category: columns aligned across the
// category, then a tail. short is it in a few words, for the summary.
type finding struct {
	sev     severity
	cols    []string
	tail    string
	tailDim bool
	short   string
}

type category struct {
	title  string // ours
	note   string // ours: what it means, in a few words
	phrase string // ours: for the verdict, "read the decoy credentials"
	detail bool   // -v's detail, not a finding: not counted in the verdict
	footer bool   // about the report, not the code (audit_health): a warning below
	items  []finding
	// compact: the default view's lines, what the items amount to (by
	// theme, by command); nil: the items themselves.
	compact []finding
}

func (c category) sev() severity {
	s := sevInfo
	for _, f := range c.items {
		s = max(s, f.sev)
	}
	return s
}

// count is the findings of severity at least min.
func (c category) count(min severity) int {
	n := 0
	for _, f := range c.items {
		if f.sev >= min {
			n++
		}
	}
	return n
}

// privescSeverity grades one privesc probe: high for what an escalation
// goes for, warn for the rest of the look around.
func privescSeverity(p string) severity {
	if v, ok := strings.CutPrefix(p, "find -perm "); ok {
		for _, s := range []string{"4000", "2000", "6000", "u=s", "u+s", "g=s", "g+s"} {
			if strings.Contains(v, s) {
				return sevHigh
			}
		}
		return sevWarn
	}
	switch {
	case p == "/etc/shadow", p == "/etc/gshadow", strings.HasPrefix(p, "/etc/sudoers"),
		strings.HasPrefix(p, "/proc/PID/mem"):
		return sevHigh
	case !strings.Contains(p, "/"):
		// a tool looked up by name (sudo, su, pkexec…): installers check
		// for sudo; using it shows as what it reads
		return sevInfo
	case strings.HasSuffix(p, ".sock") || strings.HasSuffix(p, ".socket"):
		for _, rt := range []string{"docker", "containerd", "podman", "lxd", "snapd"} {
			if strings.Contains(p, rt) {
				return sevHigh
			}
		}
	}
	return sevWarn
}

func vmProbeSeverity(p string) severity {
	if commonProbes[p] {
		return sevInfo
	}
	return sevWarn
}

// mergeProbes is one line per path — found if any program found it, the
// counts added, the programs named (three, then …) — in the order first seen.
func mergeProbes(ps []sandboxProbe) []sandboxProbe {
	var out []sandboxProbe
	at := map[string]int{}
	by := map[string][]string{}
	for _, p := range ps {
		i, ok := at[p.Path]
		if !ok {
			i = len(out)
			at[p.Path] = i
			out = append(out, sandboxProbe{Path: p.Path})
		}
		out[i].Count += p.Count
		out[i].Found = out[i].Found || p.Found
		if !slices.Contains(by[p.Path], p.By) {
			by[p.Path] = append(by[p.Path], p.By)
		}
	}
	for i := range out {
		names := by[out[i].Path]
		if len(names) > 3 {
			names = append(names[:3:3], "…")
		}
		out[i].By = strings.Join(names, ", ")
	}
	return out
}

// categories are the report's findings, most serious first. verbose: every
// probe as recorded, every file changed one by one, and what is only detail
// (commands, ~/work).
func (r *sandboxReport) categories(st viewStyle, verbose bool) []category {
	x := st.times()
	home := ""
	if r.user != "" {
		home = "/home/" + r.user
	}
	tilde := func(p string) string {
		if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
			return "~" + p[len(home):]
		}
		return p
	}
	probes := func(ps []sandboxProbe, grade func(string) severity) []finding {
		if !verbose {
			ps = mergeProbes(ps)
		}
		var out []finding
		for _, p := range ps {
			state := "absent"
			if p.Found {
				state = "found"
			}
			out = append(out, finding{sev: grade(p.Path), cols: []string{x + strconv.Itoa(p.Count), tilde(p.Path), state}, tail: "by " + p.By, tailDim: true, short: tilde(p.Path)})
		}
		if !verbose {
			sort.SliceStable(out, func(i, j int) bool { return out[i].sev > out[j].sev })
		}
		return out
	}

	var cs []category
	if r.Complete {
		c := category{title: "decoy credentials read", note: "fake secrets planted for this run", phrase: "read the decoy credentials"}
		own := category{title: "decoys read by their own tool", note: "npm reading ~/.npmrc: expected"}
		for _, d := range r.Decoys {
			if d.State == "untouched" || d.Accepted != "" {
				continue
			}
			if d.State != "READ" {
				c.phrase = "tampered with the decoy credentials"
			}
			by := ""
			if len(d.By) > 0 {
				by = "by " + strings.Join(d.By, ", ")
			} else if r.readers {
				by = "by a program audit did not see"
			}
			f := finding{sev: sevHigh, cols: []string{d.State, tilde(d.Path)}, tail: by, tailDim: true, short: tilde(d.Path)}
			if d.ByItsTool {
				f.sev = sevInfo
				own.items = append(own.items, f)
				continue
			}
			if d.Legitimately != "" {
				f.tail = strings.TrimPrefix(f.tail+"; legitimately, "+d.Legitimately, "; ")
			}
			c.items = append(c.items, f)
		}
		if !verbose {
			var ps []sandboxProbe
			state := map[string]string{}
			grade := map[string]severity{}
			for _, d := range r.Decoys {
				if d.State == "untouched" || d.ByItsTool || d.Accepted != "" {
					continue
				}
				state[d.Path], grade[d.Path] = d.State, sevHigh
				if len(d.counts) == 0 {
					ps = append(ps, sandboxProbe{Path: d.Path, Found: true, Count: 1, By: "?"})
				}
				for n, k := range d.counts {
					ps = append(ps, sandboxProbe{Path: d.Path, Found: true, Count: k, By: n})
				}
			}
			c.compact = themed(ps, decoyThemes, func(p string) severity { return grade[p] }, tilde, func(p string) string { return state[p] })
			if !r.readers {
				for i := range c.compact {
					c.compact[i].tail = ""
				}
			}
		}
		cs = append(cs, c, own)
	}
	c := category{title: "text addressed to an AI agent", note: "a signal, not proof", phrase: "has text addressed to an AI agent"}
	for _, a := range r.AddressesAnAgent {
		where := a.Where
		if a.File != "" {
			where += " " + tilde(a.File)
		}
		if a.Line > 0 {
			where += ":" + strconv.Itoa(a.Line)
		}
		c.items = append(c.items, finding{sev: a.severity(), cols: []string{where}, tail: strconv.Quote(a.Text), short: strconv.Quote(a.Text)})
	}
	if !verbose {
		c.compact = groupAgentText(r.AddressesAnAgent, tilde, x)
	}
	if c.sev() < sevHigh {
		c.note = "phrases only: skills, prompts and their tests have them too; read the lines"
	}
	cs = append(cs, c)
	if r.Complete {
		phrase := "looked for a way to become root"
		for _, p := range r.Privesc {
			if strings.HasPrefix(p.Path, "find -perm ") && privescSeverity(p.Path) == sevHigh {
				phrase = "searched for setuid binaries"
			}
		}
		c := category{title: "looked for a way to become root", note: "absent ones count: asking is the tell", phrase: phrase,
			items: probes(r.Privesc, privescSeverity)}
		if !verbose {
			c.compact = themed(r.Privesc, privescThemes, privescSeverity, tilde, nil)
		}
		cs = append(cs, c)

		var kinds []string
		byKind := map[string][]sandboxAlert{}
		for _, a := range r.Alerts {
			if _, ok := byKind[a.Kind]; !ok {
				kinds = append(kinds, a.Kind)
			}
			byKind[a.Kind] = append(byKind[a.Kind], a)
		}
		for _, k := range kinds {
			if k == "connect" {
				continue // with the connections, below: who tried each
			}
			kind := r.rules.kindOf(k)
			c := category{title: kind.Title, note: kind.Means, phrase: strings.ToLower(kind.Title), footer: k == "audit_health"}
			if strings.HasPrefix(k, "rule:") {
				c.phrase = "matched " + kind.Title
			}
			for _, a := range byKind[k] {
				c.items = append(c.items, finding{sev: kind.Severity, cols: []string{x + strconv.Itoa(a.Count), tilde(a.What)}, tail: "by " + a.By, tailDim: true, short: tilde(a.What)})
			}
			if !verbose && commandKinds[k] {
				c.compact = groupCommands(byKind[k], x)
				for i := range c.compact {
					c.compact[i].sev = kind.Severity
				}
			}
			cs = append(cs, c)
		}

		c = category{title: "looked for a VM", note: "absent: not in this VM; asking is the tell", phrase: "looked for a VM",
			items: probes(r.VMProbes, vmProbeSeverity)}
		if !verbose {
			c.compact = themed(r.VMProbes, vmThemes, vmProbeSeverity, tilde, nil)
		}
		cs = append(cs, c)
	}
	cs = append(cs, r.network(x))
	if r.Complete {
		cs = append(cs, category{title: "changed outside ~/work", note: "caches and temporary files are info", phrase: "changed files outside ~/work",
			items: r.changedOutside(home, tilde, verbose)})

		c = category{title: "processes left running", note: "as " + r.user, phrase: "left processes running"}
		var args []string
		pids := map[string][]string{}
		for _, p := range r.Processes {
			if _, ok := pids[p.Args]; !ok {
				args = append(args, p.Args)
			}
			pids[p.Args] = append(pids[p.Args], strconv.Itoa(p.PID))
		}
		for _, a := range args {
			col := pids[a][0]
			if n := len(pids[a]); n > 1 && !verbose {
				col = x + strconv.Itoa(n)
			} else if n > 1 {
				col = strings.Join(pids[a], ",")
			}
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{col}, tail: a, short: a})
		}
		cs = append(cs, c)
		c = category{title: "listening sockets", phrase: "opened listening sockets"}
		for _, l := range r.Listening {
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{l}, short: l})
		}
		cs = append(cs, c)

		if verbose {
			c = category{detail: true, title: "changed in ~/work", note: "one line per entry, what is inside counted"}
			for _, e := range r.Changed.WorkFiles {
				if strings.HasSuffix(e.Entry, "/") {
					c.items = append(c.items, finding{sev: sevInfo, cols: []string{"~/work/" + e.Entry}, tail: strconv.Itoa(e.Count) + " inside", tailDim: true})
				} else {
					c.items = append(c.items, finding{sev: sevInfo, cols: []string{"~/work/" + path.Clean(e.Entry)}})
				}
			}
			cs = append(cs, c)
			c = category{detail: true, title: "commands it ran", note: "in order; the first: mh-sandbox-run starting it"}
			if r.Summary.Commands > len(r.Commands) {
				c.note = fmt.Sprintf("in order; %d distinct, the first %d listed", r.Summary.Commands, len(r.Commands))
			}
			for _, cmd := range r.Commands {
				c.items = append(c.items, finding{sev: sevInfo, cols: []string{x + strconv.Itoa(cmd.Count)}, tail: cmd.Args})
			}
			cs = append(cs, c)
		}
	}

	c = category{title: "accepted by your rules", note: "checked before, by whoever wrote the rule: its why", phrase: "matched your accept rules"}
	for _, a := range r.Accepted {
		tail := "why: " + a.Why
		if a.By != "" {
			tail = "by " + a.By + "; " + tail
		}
		c.items = append(c.items, finding{sev: sevInfo, cols: []string{a.Kind, x + strconv.Itoa(a.Count), a.What}, tail: tail, tailDim: true, short: a.What})
	}
	cs = append(cs, c)

	var found []category
	for _, c := range cs {
		if len(c.items) > 0 {
			found = append(found, c)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].detail != found[j].detail {
			return found[j].detail
		}
		return found[i].sev() > found[j].sev()
	})
	return found
}

// homeCaches are where tools keep what they download or build, relative to
// the home: written by every build, worth knowing only.
var homeCaches = []string{".cache", ".npm", "go", ".cargo/registry", ".cargo/git", ".cargo/.package-cache", ".cargo/.package-cache-mutate", ".cargo/.global-cache", ".rustup", ".m2", ".gradle", ".nuget",
	".yarn", ".pnpm-store", ".local/share", ".local/state", ".gitconfig", ".python_history", ".node_repl_history", ".lesshst", ".wget-hsts"}

// changedSeverity grades a path changed outside ~/work: info for a cache, a
// tool's own settings (~/.config, but where code makes itself start again),
// a temporary file; warn for the rest — the shell's files, ~/.ssh,
// ~/.local/bin, the system.
func changedSeverity(p, home string) severity {
	for _, t := range []string{"/tmp/", "/var/tmp/", "/dev/shm/"} {
		if strings.HasPrefix(p, t) {
			return sevInfo
		}
	}
	rel, ok := strings.CutPrefix(p, home+"/")
	if home == "" || !ok {
		return sevWarn
	}
	for _, c := range homeCaches {
		if rel == c || strings.HasPrefix(rel, c+"/") {
			return sevInfo
		}
	}
	if strings.HasPrefix(rel, ".config/") {
		for _, s := range []string{"systemd", "autostart", "environment.d", "gh"} {
			if strings.HasPrefix(rel, ".config/"+s+"/") {
				return sevWarn
			}
		}
		return sevInfo
	}
	return sevWarn
}

// changedOutside is what changed outside ~/work. Compact: files grouped by
// where they are (~/.cache/go-build/, 1515 files), a directory only when
// none of its files is listed (an entry removed or renamed), and not a file
// an alert already names (~/.bashrc, made itself start again). verbose:
// every one.
func (r *sandboxReport) changedOutside(home string, tilde func(string) string, verbose bool) []finding {
	var out []finding
	alerted := map[string]bool{}
	for _, a := range r.Alerts {
		alerted[strings.TrimSuffix(a.What, " (refused)")] = true
	}
	files := append([]string(nil), r.Changed.Files...)
	sort.Strings(files)
	var groups []string
	byGroup := map[string][]string{}
	for _, f := range files {
		if home != "" && (f == home+"/work" || strings.HasPrefix(f, home+"/work/")) {
			continue // an older image counted ~/work itself
		}
		if alerted[f] && !verbose {
			continue
		}
		g := tilde(f)
		if !verbose {
			depth := 2
			if strings.HasPrefix(g, "~/") {
				depth = 3
			}
			if segs := strings.Split(strings.TrimPrefix(g, "/"), "/"); len(segs) > depth {
				g = strings.Join(segs[:depth], "/") + "/"
				if !strings.HasPrefix(g, "~") {
					g = "/" + g
				}
			}
		}
		if _, ok := byGroup[g]; !ok {
			groups = append(groups, g)
		}
		byGroup[g] = append(byGroup[g], f)
	}
	for _, g := range groups {
		fs := byGroup[g]
		sev := sevInfo
		for _, f := range fs {
			sev = max(sev, changedSeverity(f, home))
		}
		f := finding{sev: sev, cols: []string{g}, short: g}
		if len(fs) == 1 {
			f.cols[0], f.short = tilde(fs[0]), tilde(fs[0])
		} else {
			f.tail, f.tailDim = strconv.Itoa(len(fs))+" files", true
			f.short = g + " (" + strconv.Itoa(len(fs)) + ")"
		}
		out = append(out, f)
	}
	dirs := append([]string(nil), r.Changed.Dirs...)
	sort.Strings(dirs)
	for _, d := range dirs {
		if home != "" && (d == home+"/work" || strings.HasPrefix(d, home+"/work/")) {
			continue
		}
		listed := false
		for _, f := range files {
			if strings.HasPrefix(f, d+"/") {
				listed = true
				break
			}
		}
		if listed && !verbose {
			continue
		}
		out = append(out, finding{sev: sevInfo, cols: []string{tilde(d) + "/"}, tail: "entries changed", tailDim: true, short: tilde(d) + "/"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].sev > out[j].sev })
	return out
}

// cleanChecks names what was looked at and found clean.
func (r *sandboxReport) cleanChecks() []string {
	var out []string
	s := r.Summary
	if r.Complete {
		if len(r.Decoys) > 0 && s.DecoysRead == 0 {
			out = append(out, fmt.Sprintf("decoys %d/%d untouched", len(r.Decoys), len(r.Decoys)))
		}
		if s.Privesc == 0 {
			out = append(out, "no privesc probes")
		}
		if s.VMProbes == 0 {
			out = append(out, "no VM probes")
		}
		if s.Alerts == 0 {
			out = append(out, "no alerts")
		}
		if s.ChangedOutsideWork == 0 {
			out = append(out, "nothing changed outside ~/work")
		}
		if s.ProcessesLeft == 0 {
			out = append(out, "no processes left")
		}
		if s.Listening == 0 {
			out = append(out, "no sockets")
		}
	}
	if len(r.Connections) == 0 {
		out = append(out, "no connections")
	}
	if s.AddressesAnAgent == 0 {
		out = append(out, "no text addressed to an agent")
	}
	return out
}

// render prints the report for a person: a verdict, then one line per kind
// of finding (renderSummary), or with verbose every finding under its kind.
func (r *sandboxReport) render(w io.Writer, st viewStyle, verbose, output bool) {
	if output {
		r.renderOutput(w, st)
	}
	// The verdict and the summary count the compact view, -v or not.
	sum := r.categories(st, false)
	counts := map[severity]int{}
	phrases := map[severity][]string{}
	var footer []string
	for _, c := range sum {
		if c.footer {
			for _, f := range c.items {
				footer = append(footer, f.short)
			}
			continue
		}
		for _, f := range c.items {
			counts[f.sev]++
		}
		if s := c.sev(); s >= sevWarn && c.phrase != "" && !slices.Contains(phrases[s], c.phrase) {
			phrases[s] = append(phrases[s], c.phrase)
		}
	}

	dash := st.pick(" — ", " -- ")
	it := func(ps []string) string {
		if len(ps) > 4 {
			ps = append(ps[:3:3], fmt.Sprintf("%d more", len(ps)-3))
		}
		return "it " + andList(ps)
	}
	switch {
	case !r.Complete:
		head := st.paint("1;35", st.pick("✗ INCOMPLETE", "?? INCOMPLETE"))
		fmt.Fprintln(w, head+dash+"the report from inside the VM failed: only the host's view below")
	case counts[sevHigh] > 0:
		head := st.paint(sevColor[sevHigh], st.pick("✗ SUSPICIOUS", "XX SUSPICIOUS"))
		fmt.Fprintln(w, wrap(head+dash+st.bold(it(phrases[sevHigh])), 3, viewWidth))
	case counts[sevWarn] > 0:
		head := st.paint("1;"+sevColor[sevWarn], st.pick("⚠ REVIEW", "!! REVIEW"))
		fmt.Fprintln(w, wrap(head+dash+st.bold(it(phrases[sevWarn])), 3, viewWidth))
	default:
		fmt.Fprintln(w, st.paint("1;32", st.pick("✓", "==")+" NOTHING SUSPICIOUS SEEN")+dash+"in this run, which is not proof it is safe")
	}
	meta := []string{untrusted(r.Target, 200), r.Image, "exit " + strconv.Itoa(r.ExitCode)}
	if r.TimedOut {
		meta[2] = st.paint(sevColor[sevWarn], "timed out") + " (stopped, then looked at): exit 124"
	}
	if r.DurationMS > 0 {
		meta = append(meta, runDuration(time.Duration(r.DurationMS)*time.Millisecond))
	}
	if r.Complete && r.commands >= 0 {
		meta = append(meta, fmt.Sprintf("%d commands", r.Summary.Commands))
	}
	fmt.Fprintln(w, "   "+st.dim(strings.Join(meta, st.sep())))
	if r.Summary.EvasionSuspected {
		fmt.Fprintln(w, wrap("   "+st.paint(sevColor[sevWarn], st.pick("⚠", "!!")+" it looked for a VM: what it did not do here proves nothing"), 5, viewWidth))
	}

	fmt.Fprintln(w)
	if verbose {
		for _, c := range r.categories(st, true) {
			renderCategory(w, st, c)
			fmt.Fprintln(w)
		}
	} else {
		renderCompact(w, st, sum)
	}

	if !r.Complete {
		fmt.Fprintln(w, st.paint("1;35", "inside the VM: unknown, not none")+" (decoys, probes, commands, files, processes)")
	}
	if clean := r.cleanChecks(); len(clean) > 0 {
		fmt.Fprintln(w, wrap(st.paint("32", st.pick("✓", "ok"))+" "+st.dim("clean:")+" "+strings.Join(clean, ", "), 3, viewWidth))
	}
	if !verbose {
		for _, f := range footer {
			fmt.Fprintln(w, wrap(st.paint(sevColor[sevWarn], "! audit:")+" "+f+": some of what it did may be missing", 3, viewWidth))
		}
	}
	if rs := r.rules; rs != nil && len(rs.Files) > 0 {
		files := make([]string, len(rs.Files))
		for i, f := range rs.Files {
			files[i] = tildeHost(f)
		}
		fmt.Fprintln(w, wrap(st.dim(fmt.Sprintf("rules: %s (%d detect, %d accept)", strings.Join(files, ", "), len(rs.Detect), len(rs.Accept))), 3, viewWidth))
	}
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, wrap(st.paint(sevColor[sevWarn], "! warning:")+" "+warn, 3, viewWidth))
	}
	fmt.Fprintln(w)

	var hints []string
	if !verbose {
		hints = append(hints, "-v every finding")
	}
	hints = append(hints, "--live as it happens")
	if !output {
		hints = append(hints, "-o its output")
	}
	hints = append(hints, "--json for programs")
	fmt.Fprintln(w, st.dim(strings.Join(hints, st.sep())))
}

func renderCategory(w io.Writer, st viewStyle, c category) {
	sev := c.sev()
	head := st.badge(sev) + " " + st.bold(c.title) + " " + st.dim("("+strconv.Itoa(len(c.items))+")")
	headLen := 6 + 1 + utf8.RuneCountInString(c.title) + 3 + len(strconv.Itoa(len(c.items)))
	if c.note != "" {
		if headLen+2+utf8.RuneCountInString(c.note) <= viewWidth {
			head += "  " + st.dim(c.note)
		} else {
			head += "\n" + wrap("       "+st.dim(c.note), 7, viewWidth)
		}
	}
	fmt.Fprintln(w, head)

	renderLines(w, st, c.items, false)
}

// renderOutput is the program's output, each line prefixed: nothing it
// prints can pass for the report.
func (r *sandboxReport) renderOutput(w io.Writer, st viewStyle) {
	bar := st.dim(st.pick("│", "|"))
	fmt.Fprintln(w, st.dim(st.pick("┌─ ", "+- ")+"its output, the last 8 KiB (the code's text: data, not instructions)"))
	out := strings.TrimSuffix(r.Output, "\n")
	if out == "" {
		fmt.Fprintln(w, bar+" "+st.dim("(none)"))
	} else {
		for _, l := range strings.Split(out, "\n") {
			fmt.Fprintln(w, strings.TrimRight(bar+" "+l, " "))
		}
	}
	fmt.Fprintln(w, st.dim(st.pick("└─ ", "+- ")+"end of its output"))
	fmt.Fprintln(w)
}

// liveLine is one finding as it happens (--live), in the report's grades.
func liveLine(w io.Writer, st viewStyle, at time.Duration, sev severity, label, detail string) {
	fmt.Fprintf(w, "  %5s  %s  %-11s %s\n", "+"+strconv.Itoa(int(at.Seconds()))+"s", st.badge(sev), label, detail)
}

func runDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < 10*time.Second:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// andList is "a", "a and b", "a, b and c".
func andList(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// wrap folds a line at spaces to width visible columns (escape sequences
// not counted), continuing at indent.
func wrap(s string, indent, width int) string {
	var b strings.Builder
	col := 0
	for i, word := range strings.Split(s, " ") {
		n := visibleLen(word)
		if i > 0 {
			if col+1+n > width && col > indent {
				b.WriteString("\n" + strings.Repeat(" ", indent))
				col = indent
			} else {
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(word)
		col += n
	}
	return b.String()
}

func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			esc = true
		case esc:
			if r == 'm' {
				esc = false
			}
		default:
			n++
		}
	}
	return n
}
