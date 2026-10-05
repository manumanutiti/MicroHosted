package cli

import (
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The default report's body: one block per kind of finding, its lines what
// the findings amount to rather than each of them — the privesc probes by
// what they go for (the password files, sudo, cron…), the VM probes by what
// they tell apart (DMI, container markers, modules…), the same command run on
// fifteen directories as one line, every connection with the program that
// tried it. -v prints each finding instead (renderCategory).

// compactLines is how many lines a block shows without -v.
const compactLines = 8

// theme is a group of probes that go for the same thing.
type theme struct {
	label string
	dirs  []string // path prefixes
	tools []string // programs, looked up or run, by name
	match func(string) bool
}

func (t theme) has(p string) bool {
	for _, d := range t.dirs {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	for _, n := range t.tools {
		if p == n || strings.HasSuffix(p, "/"+n) {
			return true
		}
	}
	return t.match != nil && t.match(p)
}

var privescThemes = []theme{
	{label: "password files", dirs: []string{"/etc/shadow", "/etc/gshadow", "/etc/passwd"}},
	{label: "sudo, doas rules", dirs: []string{"/etc/sudoers", "/etc/doas"}},
	{label: "setuid search", dirs: []string{"find -perm"}},
	{label: "container sockets", match: func(p string) bool {
		for _, rt := range []string{"docker", "containerd", "podman", "lxd", "snapd"} {
			if strings.Contains(p, rt) {
				return true
			}
		}
		return false
	}},
	{label: "cron", dirs: []string{"/etc/cron", "/etc/anacrontab", "/var/spool/cron"}},
	{label: "root's home", dirs: []string{"/root"}},
	{label: "process memory", dirs: []string{"/proc/PID/mem"}},
	{label: "kernel hardening", dirs: []string{"/proc/sys/kernel/"}},
	{label: "polkit, NFS", dirs: []string{"/etc/polkit-1", "/etc/exports"}},
	{label: "su, sudo… tools", match: func(p string) bool { return !strings.Contains(p, "/") }},
}

var decoyThemes = []theme{
	{label: "SSH", dirs: []string{"~/.ssh/"}},
	{label: "cloud: AWS, k8s", dirs: []string{"~/.aws/", "~/.kube/", "~/.config/gcloud/", "~/.azure/"}},
	{label: "npm, PyPI, Docker", dirs: []string{"~/.docker/", "~/.npmrc", "~/.pypirc"}},
	{label: "GitHub, git, .netrc", dirs: []string{"~/.config/gh/", "~/.git-credentials", "~/.netrc"}},
	{label: "a project's .env", dirs: []string{"~/projects/"}},
	{label: "shell history", dirs: []string{"~/.bash_history", "~/.zsh_history"}},
	{label: "browsers", dirs: []string{"~/.config/google-chrome/", "~/.config/chromium/", "~/.mozilla/"}},
	{label: "crypto wallet", dirs: []string{"~/.electrum/", "~/.bitcoin/", "~/.ethereum/"}},
}

var vmThemes = []theme{
	{label: "DMI, BIOS", dirs: []string{"/sys/class/dmi", "/sys/devices/virtual/dmi", "/sys/firmware/dmi"}, tools: []string{"dmidecode"}},
	{label: "container markers", dirs: []string{"/.dockerenv", "/run/.containerenv", "/proc/1/cgroup", "/proc/self/cgroup"}},
	{label: "CPU", dirs: []string{"/proc/cpuinfo"}, tools: []string{"lscpu"}},
	{label: "kernel modules", dirs: []string{"/proc/modules", "/sys/module"}, tools: []string{"lsmod"}},
	{label: "ACPI tables", dirs: []string{"/sys/firmware/acpi"}},
	{label: "PCI, disks, SCSI", dirs: []string{"/sys/bus/pci", "/proc/bus/pci", "/proc/scsi", "/dev/disk/by-id"}, tools: []string{"lspci", "lshw"}},
	{label: "hypervisor", dirs: []string{"/sys/hypervisor", "/proc/xen"}},
	{label: "MAC address", dirs: []string{"/sys/class/net/"}},
	{label: "battery, sensors", dirs: []string{"/sys/class/power_supply", "/sys/class/thermal"}},
	{label: "boot, kernel log", dirs: []string{"/proc/cmdline"}, tools: []string{"dmesg"}},
	{label: "detection tools", tools: []string{"systemd-detect-virt", "virt-what", "hostnamectl"}},
}

// themed is the probes as one line per theme, in the themes' order: the
// paths asked for (a common directory said once), how many, the programs
// that asked most. A probe no theme has is a line of its own.
// stateOf, when given, says what happened to a path (a decoy's READ), in
// place of how many paths.
func themed(ps []sandboxProbe, themes []theme, grade func(string) severity, tilde func(string) string, stateOf func(string) string) []finding {
	type group struct {
		label string
		sev   severity
		paths []string
		seen  map[string]bool
		by    map[string]int
	}
	var groups []*group
	at := map[string]*group{}
	for _, p := range ps {
		label := ""
		for _, t := range themes {
			if t.has(p.Path) || t.has(tilde(p.Path)) {
				label = t.label
				break
			}
		}
		key := label
		if key == "" {
			key = "\x00" + p.Path
		}
		g := at[key]
		if g == nil {
			g = &group{label: label, sev: sevInfo, seen: map[string]bool{}, by: map[string]int{}}
			at[key] = g
			groups = append(groups, g)
		}
		g.sev = max(g.sev, grade(p.Path))
		if !g.seen[p.Path] {
			g.seen[p.Path] = true
			g.paths = append(g.paths, p.Path)
		}
		g.by[p.By] += p.Count
	}
	order := map[string]int{}
	for i, t := range themes {
		order[t.label] = i
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].sev != groups[j].sev {
			return groups[i].sev > groups[j].sev
		}
		oi, ok := order[groups[i].label]
		if !ok {
			oi = len(themes)
		}
		oj, ok := order[groups[j].label]
		if !ok {
			oj = len(themes)
		}
		return oi < oj
	})
	var out []finding
	for _, g := range groups {
		var names []string
		for _, p := range g.paths {
			n := tilde(p)
			if isTool(p) {
				n = path.Base(p) // /usr/bin/dmesg and /bin/dmesg: dmesg
			}
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		label := g.label
		if label == "" {
			label = "other"
		}
		col := strconv.Itoa(len(names))
		if stateOf != nil {
			var states []string
			for _, p := range g.paths {
				if s := stateOf(p); !slices.Contains(states, s) {
					states = append(states, s)
				}
			}
			col = strings.Join(states, ", ")
		}
		out = append(out, finding{sev: g.sev, cols: []string{label, col, joinPaths(names)}, tail: "by " + topNames(g.by, 3), tailDim: true})
	}
	return out
}

// isTool: a program looked up or run, which reads better by its name.
func isTool(p string) bool {
	for _, d := range []string{"/usr/bin/", "/usr/sbin/", "/bin/", "/sbin/", "/usr/local/bin/", "/usr/local/sbin/"} {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	return false
}

// joinPaths says a directory several paths share once:
// /sys/class/dmi/id/{product_name, sys_vendor}.
func joinPaths(ps []string) string {
	if len(ps) < 2 {
		return strings.Join(ps, ", ")
	}
	prefix := ps[0]
	for _, p := range ps[1:] {
		for !strings.HasPrefix(p, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if i := strings.LastIndexAny(prefix, "/ "); i > 0 {
		prefix = prefix[:i+1]
	} else {
		prefix = ""
	}
	rest := make([]string, len(ps))
	for i, p := range ps {
		rest[i] = strings.TrimPrefix(p, prefix)
		if rest[i] == "" {
			rest[i] = "."
		}
	}
	if prefix == "" || prefix == "/" {
		return strings.Join(ps, ", ")
	}
	return prefix + "{" + strings.Join(rest, ", ") + "}"
}

// topNames is the n names counted most, most first, "…" for the rest.
func topNames(by map[string]int, n int) string {
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if by[names[i]] != by[names[j]] {
			return by[names[i]] > by[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = append(names[:n:n], "…")
	}
	return strings.Join(names, ", ")
}

// commandKinds are the alerts whose WHAT is a command line.
var commandKinds = map[string]bool{"credential_search": true, "dev_tcp": true, "reverse_shell": true, "pipe_to_shell": true,
	"obfuscated_exec": true, "antiforensics": true, "miner": true}

// groupCommands is the same command run on several directories as one line:
// find / -name id_rsa and find /etc -name id_rsa are "find -name id_rsa", in
// /, /etc.
func groupCommands(as []sandboxAlert, x string) []finding {
	type group struct {
		cmd   string
		first string
		in    []string
		n     int
		by    map[string]int
	}
	var groups []*group
	at := map[string]*group{}
	for _, a := range as {
		var keep, in []string
		for i, t := range strings.Fields(a.What) {
			if i > 0 && strings.HasPrefix(t, "/") && !strings.ContainsAny(t, "*?<>|&;") {
				in = append(in, t)
			} else {
				keep = append(keep, t)
			}
		}
		// the image cuts a command at 200 bytes, where depends on how long
		// its directories are: the start says which command it is
		key := strings.Join(keep, " ")
		if r := []rune(key); len(r) > 80 {
			key = string(r[:80])
		}
		g := at[key]
		if g == nil {
			g = &group{cmd: key, first: a.What, by: map[string]int{}}
			at[key] = g
			groups = append(groups, g)
		}
		g.in = append(g.in, in...)
		g.n += a.Count
		g.by[a.By] += a.Count
	}
	var out []finding
	for _, g := range groups {
		f := finding{cols: []string{x + strconv.Itoa(g.n), g.first}, tail: "by " + topNames(g.by, 3), tailDim: true}
		if len(g.in) > 1 {
			in := g.in
			more := ""
			if len(in) > 4 {
				in, more = in[:4], fmt.Sprintf(" … %d places", len(g.in))
			}
			f.cols[1] = g.cmd
			f.tail = "in " + strings.Join(in, " ") + more + "; " + f.tail
		}
		out = append(out, f)
	}
	return out
}

// dstNote says what a destination is, where it is known.
func dstNote(dst string, port int) string {
	switch {
	case dst == "169.254.169.254" || dst == "[fd00:ec2::254]":
		return "cloud metadata: an instance's credentials"
	case port == 53:
		return "DNS"
	case port == 4444 || port == 1337 || port == 31337:
		return "a port reverse shells use"
	}
	return ""
}

// renderCompact is the default body: per kind, a heading with how many and
// what it means, then its lines (at most compactLines, -v for the rest);
// the kinds only info on one line at the end. false: nothing to print.
func renderCompact(w io.Writer, st viewStyle, cs []category) bool {
	var infos []string
	printed := false
	for _, c := range cs {
		if c.footer || c.detail {
			continue
		}
		if c.count(sevWarn) == 0 {
			infos = append(infos, c.title+" ("+strconv.Itoa(len(c.items))+")")
			continue
		}
		head := st.badge(c.sev()) + " " + st.bold(c.title) + " " + st.dim("("+strconv.Itoa(len(c.items))+")")
		if c.note != "" {
			n := 6 + 1 + utf8.RuneCountInString(c.title) + 3 + len(strconv.Itoa(len(c.items)))
			if n+2+utf8.RuneCountInString(c.note) <= viewWidth {
				head += "  " + st.dim(c.note)
			}
		}
		fmt.Fprintln(w, head)
		lines := c.compact
		if lines == nil {
			lines = c.items
		}
		more := 0
		if len(lines) > compactLines {
			more = len(lines) - compactLines
			lines = lines[:compactLines]
		}
		renderLines(w, st, lines, true)
		if more > 0 {
			fmt.Fprintln(w, "     "+st.dim(fmt.Sprintf("%s %d more (-v)", st.ell(), more)))
		}
		fmt.Fprintln(w)
		printed = true
	}
	if len(infos) > 0 {
		fmt.Fprintln(w, wrap(st.badge(sevInfo)+" "+st.dim("also, ordinary on its own: "+strings.Join(infos, ", ")), 7, viewWidth))
		fmt.Fprintln(w)
		printed = true
	}
	return printed
}

// renderLines prints findings with their columns aligned. clip: each line
// kept to viewWidth, the last column and the tail cut to fit.
func renderLines(w io.Writer, st viewStyle, items []finding, clip bool) {
	// A column is as wide as its widest value up to alignMax; a longer one
	// (an outlier path) runs on rather than pushing every line's tail out.
	const alignMax = 40
	var widths []int
	for _, f := range items {
		for i, col := range f.cols {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			if n := utf8.RuneCountInString(col); n <= alignMax && i < len(f.cols)-1 {
				widths[i] = max(widths[i], n)
			}
		}
	}
	cut := func(s string, n int) string {
		if !clip || utf8.RuneCountInString(s) <= n {
			return s
		}
		e := st.ell()
		return string([]rune(s)[:max(n-utf8.RuneCountInString(e), 1)]) + e
	}
	for _, f := range items {
		var b strings.Builder
		b.WriteString("  " + st.mark(f.sev))
		used := 3
		for i, col := range f.cols {
			b.WriteString("  ")
			used += 2
			if i == len(f.cols)-1 {
				room := viewWidth - used
				if f.tail != "" {
					room -= 2 + min(utf8.RuneCountInString(f.tail), 28)
				}
				col = cut(col, max(room, 24))
				b.WriteString(col)
				used += utf8.RuneCountInString(col)
				break
			}
			pad := widths[i] - utf8.RuneCountInString(col)
			b.WriteString(col + strings.Repeat(" ", max(pad, 0)))
			used += max(widths[i], utf8.RuneCountInString(col))
		}
		if f.tail != "" {
			tail := cut(f.tail, max(viewWidth-used-2, 20))
			if f.tailDim {
				tail = st.dim(tail)
			}
			b.WriteString("  " + tail)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// network is every connection the code tried: what the host refused, with
// the programs that tried it as audit saw them inside (connect alerts), and
// what audit saw that the host did not.
func (r *sandboxReport) network(x string) category {
	c := category{title: "tried to reach the network", note: "refused by the host: nothing got out", phrase: "tried to reach the network"}
	who := map[string]map[string]int{}
	var inside []string
	for _, a := range r.Alerts {
		if a.Kind != "connect" {
			continue
		}
		dst := strings.TrimSuffix(a.What, " (dns)")
		if who[dst] == nil {
			who[dst] = map[string]int{}
			inside = append(inside, dst)
		}
		who[dst][a.By] += a.Count
	}
	seen := map[string]bool{}
	for _, n := range r.Connections {
		dst := n.Dst
		if strings.Contains(dst, ":") {
			dst = "[" + dst + "]"
		}
		if n.DstPort != 0 {
			dst += ":" + strconv.Itoa(n.DstPort)
		}
		seen[dst] = true
		var tail []string
		if note := dstNote(n.Dst, n.DstPort); note != "" {
			tail = append(tail, note)
		}
		if by := who[dst]; len(by) > 0 {
			tail = append(tail, "by "+topNames(by, 3))
		}
		if n.Reason != "egress" {
			tail = append(tail, n.Reason)
		}
		c.items = append(c.items, finding{sev: sevWarn, cols: []string{n.Protocol, dst, x + strconv.FormatUint(n.Count, 10)}, tail: strings.Join(tail, "; "), tailDim: true, short: dst})
	}
	for _, dst := range inside {
		if seen[dst] {
			continue
		}
		port := 0
		if i := strings.LastIndex(dst, ":"); i >= 0 {
			port, _ = strconv.Atoi(dst[i+1:])
		}
		tail := []string{"by " + topNames(who[dst], 3), "seen inside only"}
		if note := dstNote(strings.Trim(dst[:max(strings.LastIndex(dst, ":"), 0)], "[]"), port); note != "" {
			tail = append([]string{note}, tail...)
		}
		n := 0
		for _, k := range who[dst] {
			n += k
		}
		c.items = append(c.items, finding{sev: sevInfo, cols: []string{"?", dst, x + strconv.Itoa(n)}, tail: strings.Join(tail, "; "), tailDim: true, short: dst})
	}
	return c
}
