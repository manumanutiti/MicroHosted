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
// only what was found, grouped by category and graded high, warn or info, then
// one line naming what was checked and found clean — so an empty section
// never reads as an unchecked one. -v adds everything: every probe by every
// program, every command, what changed in ~/work. --output adds the program's
// own output, every line of it prefixed so it cannot pass for the report.
//
// Severities (the alerts' come from their kind, sandbox_kinds.go):
//
//	high  a decoy READ, TAMPERED or DELETED; text addressed to an AI agent;
//	      privesc: find -perm for setuid/setgid files, /etc/shadow, /etc/gshadow,
//	      /etc/sudoers*, a container runtime's socket, /proc/PID/mem
//	warn  every other privesc probe (sudo, su, cron, root's home, the kernel's
//	      switches, find -perm for writable files); a VM probe; a connection
//	      refused; a file or directory changed outside ~/work; a process left;
//	      a listening socket
//	info  a VM probe ordinary programs make too (commonProbes); with -v, the
//	      commands run and what changed in ~/work
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

// compactLines is how many findings a category shows without -v.
const compactLines = 15

// finding is one line under a category: columns aligned across the
// category, then a tail.
type finding struct {
	sev     severity
	cols    []string
	tail    string
	tailDim bool
}

type category struct {
	title  string // ours
	note   string // ours: what it means, in a few words
	phrase string // ours: for the verdict, "read your credentials"
	detail bool   // -v's detail, not a finding: not counted in the verdict
	items  []finding
}

func (c category) sev() severity {
	s := sevInfo
	for _, f := range c.items {
		s = max(s, f.sev)
	}
	return s
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
// probe as recorded, and what is only detail (commands, ~/work).
func (r *sandboxReport) categories(st viewStyle, verbose bool) []category {
	x := st.times()
	home := ""
	if r.user != "" {
		home = "/home/" + r.user + "/"
	}
	tilde := func(p string) string {
		if home != "" && strings.HasPrefix(p, home) {
			return "~/" + p[len(home):]
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
			out = append(out, finding{sev: grade(p.Path), cols: []string{x + strconv.Itoa(p.Count), tilde(p.Path), state}, tail: "by " + p.By, tailDim: true})
		}
		if !verbose {
			sort.SliceStable(out, func(i, j int) bool { return out[i].sev > out[j].sev })
		}
		return out
	}

	var cs []category
	if r.Complete {
		c := category{title: "decoy credentials", note: "fake secrets planted for this run", phrase: "read your credentials"}
		for _, d := range r.Decoys {
			if d.State == "untouched" {
				continue
			}
			if d.State != "READ" {
				c.phrase = "tampered with your credentials"
			}
			c.items = append(c.items, finding{sev: sevHigh, cols: []string{d.State, tilde(d.Path)}, tail: "legitimately: " + d.Legitimately, tailDim: true})
		}
		cs = append(cs, c)
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
		c.items = append(c.items, finding{sev: sevHigh, cols: []string{where}, tail: strconv.Quote(a.Text)})
	}
	cs = append(cs, c)
	if r.Complete {
		phrase := "looked for a way to root"
		for _, p := range r.Privesc {
			if strings.HasPrefix(p.Path, "find -perm ") && privescSeverity(p.Path) == sevHigh {
				phrase = "searched for setuid binaries"
			}
		}
		cs = append(cs, category{title: "looking for a way to root", note: "absent ones count: asking is the tell", phrase: phrase,
			items: probes(r.Privesc, privescSeverity)})

		var kinds []string
		byKind := map[string][]sandboxAlert{}
		for _, a := range r.Alerts {
			if _, ok := byKind[a.Kind]; !ok {
				kinds = append(kinds, a.Kind)
			}
			byKind[a.Kind] = append(byKind[a.Kind], a)
		}
		for _, k := range kinds {
			kind := kindOf(k)
			c := category{title: kind.Title, note: kind.Means, phrase: "set off " + strings.ToLower(kind.Title)}
			for _, a := range byKind[k] {
				c.items = append(c.items, finding{sev: kind.Severity, cols: []string{x + strconv.Itoa(a.Count), tilde(a.What)}, tail: "by " + a.By, tailDim: true})
			}
			cs = append(cs, c)
		}

		cs = append(cs, category{title: "looking for a VM", note: "absent: not in this VM; asking is the tell", phrase: "looked for a VM",
			items: probes(r.VMProbes, vmProbeSeverity)})
	}
	c = category{title: "connections refused", note: "seen by the host; nothing got out", phrase: "tried to reach the network"}
	for _, n := range r.Connections {
		dst := n.Dst
		if n.DstPort != 0 {
			dst += ":" + strconv.Itoa(n.DstPort)
		}
		c.items = append(c.items, finding{sev: sevWarn, cols: []string{n.Protocol, dst, x + strconv.FormatUint(n.Count, 10)}, tail: n.Reason, tailDim: true})
	}
	cs = append(cs, c)
	if r.Complete {
		c := category{title: "changed outside ~/work", phrase: "changed files outside ~/work"}
		files := append([]string(nil), r.Changed.Files...)
		sort.Strings(files)
		for _, f := range files {
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{tilde(f)}})
		}
		dirs := append([]string(nil), r.Changed.Dirs...)
		sort.Strings(dirs)
		for _, d := range dirs {
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{tilde(d) + "/"}, tail: "entries changed", tailDim: true})
		}
		cs = append(cs, c)

		c = category{title: "processes left running", note: "as " + r.user, phrase: "left processes running"}
		for _, p := range r.Processes {
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{strconv.Itoa(p.PID)}, tail: p.Args})
		}
		cs = append(cs, c)
		c = category{title: "listening sockets", phrase: "opened listening sockets"}
		for _, l := range r.Listening {
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{l}})
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

// render prints the report for a person.
func (r *sandboxReport) render(w io.Writer, st viewStyle, verbose, output bool) {
	if output {
		r.renderOutput(w, st)
	}
	cs := r.categories(st, verbose)
	// The verdict counts the compact view's lines, -v or not.
	counts := map[severity]int{}
	var phrases []string
	for _, c := range r.categories(st, false) {
		for _, f := range c.items {
			counts[f.sev]++
		}
		if c.sev() >= sevWarn && c.phrase != "" && !slices.Contains(phrases, c.phrase) {
			phrases = append(phrases, c.phrase)
		}
	}

	// The verdict.
	var tally []string
	for _, s := range []severity{sevHigh, sevWarn, sevInfo} {
		if counts[s] > 0 {
			tally = append(tally, fmt.Sprintf("%d %s", counts[s], strings.ToLower(sevLabel[s])))
		}
	}
	dash := st.pick(" — ", " -- ")
	switch {
	case !r.Complete:
		head := st.paint("1;35", st.pick("✗ INCOMPLETE", "?? INCOMPLETE"))
		fmt.Fprintln(w, head+dash+"the report from inside the VM failed: only the host's view below")
	case counts[sevHigh]+counts[sevWarn] > 0:
		sgr := sevColor[sevWarn]
		if counts[sevHigh] > 0 {
			sgr = sevColor[sevHigh]
		}
		what := phrases
		if len(what) > 3 {
			what = append(what[:2:2], fmt.Sprintf("%d more", len(phrases)-2))
		}
		line := strings.Join(tally, ", ") + dash + "it " + andList(what)
		fmt.Fprintln(w, wrap(st.paint(sgr, st.pick("⚠", "!!"))+" "+st.bold(line), 3, viewWidth))
	default:
		line := "nothing seen in this run (not proof it is safe)"
		if len(tally) > 0 {
			line = "nothing high or warn in this run, " + strings.Join(tally, ", ") + " (not proof it is safe)"
		}
		fmt.Fprintln(w, st.paint("1;32", st.pick("✓", "==")+" "+line))
	}
	meta := []string{untrusted(r.Target, 200), r.Image, "exit " + strconv.Itoa(r.ExitCode)}
	if r.TimedOut {
		meta[2] = st.paint(sevColor[sevWarn], "timed out") + " (stopped, then looked at): exit 124"
	}
	if r.DurationMS > 0 {
		meta = append(meta, runDuration(time.Duration(r.DurationMS)*time.Millisecond))
	}
	if r.Complete && r.commands >= 0 {
		meta = append(meta, fmt.Sprintf("%d commands run", r.Summary.Commands))
	}
	fmt.Fprintln(w, "   "+st.dim(strings.Join(meta, st.sep())))
	if r.Summary.EvasionSuspected {
		fmt.Fprintln(w, wrap("   "+st.paint(sevColor[sevWarn], st.pick("⚠", "!!")+" it looked for a VM: what it did not do here proves nothing"), 5, viewWidth))
	}

	for _, c := range cs {
		fmt.Fprintln(w)
		renderCategory(w, st, c, verbose)
	}

	fmt.Fprintln(w)
	if !r.Complete {
		fmt.Fprintln(w, st.paint("1;35", "inside the VM: unknown, not none")+" (decoys, probes, commands, files, processes)")
	}
	if clean := r.cleanChecks(); len(clean) > 0 {
		fmt.Fprintln(w, wrap(st.paint("32", st.pick("✓", "ok"))+" "+st.dim("clean:")+" "+strings.Join(clean, ", "), 3, viewWidth))
	}
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, wrap(st.paint(sevColor[sevWarn], "! warning:")+" "+warn, 3, viewWidth))
	}
	fmt.Fprintln(w)

	var hints []string
	if !output {
		hints = append(hints, "--output its output")
	}
	if !verbose {
		hints = append(hints, "-v everything")
	}
	hints = append(hints, "--json for agents")
	fmt.Fprintln(w, st.dim(strings.Join(hints, st.sep())))
}

func renderCategory(w io.Writer, st viewStyle, c category, verbose bool) {
	sev := c.sev()
	head := st.badge(sev) + " " + st.bold(c.title) + " " + st.dim("("+strconv.Itoa(len(c.items))+")")
	headLen := 6 + 1 + utf8.RuneCountInString(c.title) + 3 + len(strconv.Itoa(len(c.items)))
	if c.note != "" {
		if headLen+2+utf8.RuneCountInString(c.note) <= viewWidth {
			head += "  " + st.dim(c.note)
		} else {
			head += "\n       " + st.dim(c.note)
		}
	}
	fmt.Fprintln(w, head)

	items := c.items
	more := 0
	if !verbose && len(items) > compactLines {
		more = len(items) - compactLines
		items = items[:compactLines]
	}
	// Compact: each column at most colMax wide, the line at most viewWidth.
	const colMax = 60
	clipTo := func(s string, n int) string {
		if verbose || utf8.RuneCountInString(s) <= n {
			return s
		}
		rs := []rune(s)
		return string(rs[:n-1]) + "…"
	}
	// A column is as wide as its widest value up to alignMax; a longer one
	// (an outlier path) runs on rather than pushing every line's tail out.
	const alignMax = 40
	var widths []int
	for _, f := range items {
		for i, col := range f.cols {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			if n := utf8.RuneCountInString(col); n <= alignMax {
				widths[i] = max(widths[i], n)
			}
		}
	}
	for _, f := range items {
		var b strings.Builder
		b.WriteString("  " + st.mark(f.sev))
		used := 3
		for i, col := range f.cols {
			col = clipTo(col, colMax)
			b.WriteString("  ")
			used += 2
			if i == len(f.cols)-1 && f.tail == "" {
				b.WriteString(col)
				break
			}
			pad := widths[i] - utf8.RuneCountInString(col)
			b.WriteString(col + strings.Repeat(" ", max(pad, 0)))
			used += max(widths[i], utf8.RuneCountInString(col))
		}
		if f.tail != "" {
			tail := clipTo(f.tail, max(viewWidth-used-2, 24))
			if f.tailDim {
				tail = st.dim(tail)
			}
			b.WriteString("  " + tail)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	if more > 0 {
		fmt.Fprintln(w, "     "+st.dim(fmt.Sprintf("%s %d more (-v)", st.ell(), more)))
	}
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

// liveLine is one finding as it happens (-v), in the report's grades.
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
