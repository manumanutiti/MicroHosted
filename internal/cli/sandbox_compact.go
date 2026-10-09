package cli

import (
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
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
	{label: "GnuPG", dirs: []string{"~/.gnupg/"}},
	{label: "cloud: AWS, k8s", dirs: []string{"~/.aws/", "~/.kube/", "~/.config/gcloud/", "~/.azure/"}},
	{label: "npm, PyPI, Docker", dirs: []string{"~/.docker/", "~/.npmrc", "~/.pypirc"}},
	{label: "GitHub, git, .netrc", dirs: []string{"~/.config/gh/", "~/.git-credentials", "~/.netrc"}},
	{label: "AI agents: Claude, Gemini, Codex, Copilot", dirs: []string{"~/.claude/", "~/.gemini/", "~/.codex/", "~/.config/github-copilot/"}},
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
var commandKinds = map[string]bool{"credential_search": true, "dev_tcp": true, "reverse_shell": true, "pipe_to_shell": true, "hook_pipe_to_shell": true,
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

// groupAgentText is one line per phrase (case aside) or way of hiding text,
// with how often and where: a repository of skills says "system prompt" in
// forty files, which is one thing to read, not forty.
func groupAgentText(as []sandboxAgentText, tilde func(string) string, x string) []finding {
	type group struct {
		text  string
		sev   severity
		n     int
		where []string
	}
	var groups []*group
	at := map[string]*group{}
	for _, a := range as {
		key := strings.ToLower(a.Text)
		g := at[key]
		if g == nil {
			g = &group{text: a.Text, sev: a.severity()}
			at[key] = g
			groups = append(groups, g)
		}
		g.n++
		w := a.Where
		if a.File != "" {
			w = tilde(a.File)
		}
		if !slices.Contains(g.where, w) {
			g.where = append(g.where, w)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].sev > groups[j].sev })
	var out []finding
	for _, g := range groups {
		where := g.where
		more := ""
		if len(where) > 3 {
			where, more = where[:3], fmt.Sprintf(" … %d files", len(g.where))
		}
		out = append(out, finding{sev: g.sev, cols: []string{x + strconv.Itoa(g.n), strconv.Quote(g.text)},
			tail: "in " + strings.Join(where, ", ") + more, tailDim: true, short: strconv.Quote(g.text)})
	}
	return out
}

// sandboxResolver is mh-sandbox-dns's address in the guest (mh-sandbox-prepare).
const sandboxResolver = "127.53.0.1"

// dstNote says what a destination is, where it is known.
func dstNote(dst string, port int) string {
	switch {
	case dst == "169.254.169.254" || dst == "[fd00:ec2::254]":
		return "cloud metadata: an instance's credentials"
	case dst == sandboxResolver && port == 53:
		return "the sandbox's resolver: the names are above"
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
	if r.Net == "sinkhole" {
		c.note = "the sandbox's own network answered: nothing left the VM"
		if r.Answers {
			c.note = "the sandbox's own network answered, as GitHub, npm and AWS would a logged-in user: nothing left the VM"
		}
	}
	// who: the programs that connected to each destination (audit, inside),
	// the sinkhole's addresses by the name they were given to
	who := map[string]map[string]int{}
	var inside []string
	for _, a := range r.Alerts {
		if a.Kind != "connect" {
			continue
		}
		dst := r.sinkName(strings.TrimSuffix(a.What, " (dns)"))
		if who[dst] == nil {
			who[dst] = map[string]int{}
			inside = append(inside, dst)
		}
		who[dst][a.By] += a.Count
	}
	by := func(dst string) string {
		if len(who[dst]) == 0 {
			return ""
		}
		return "by " + topNames(who[dst], 3)
	}
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, "; ")
	}
	seen := map[string]bool{}
	secretSent := false // the phrase names the first request that carried one
	pubPhrase := ""     // and the first that published
	// what it sent first: a secret going out is what matters most
	for _, q := range r.Requests {
		host := urlHost(q.URL)
		dst := host + ":" + strconv.Itoa(defaultPort(q.Scheme, q.Port))
		seen[dst] = true
		answered := ""
		if q.Answer != "" {
			answered = "answered: " + q.Answer
		}
		f := finding{sev: sevWarn, cols: []string{q.Method, q.URL, x + strconv.Itoa(q.Count)}, tail: join(sizeOf(q.Bytes), answered, by(dst)), tailDim: true, short: q.Method + " " + host}
		if q.TokenToItsService {
			f.tail = join("a decoy's credential, to its own service: used, not sent out", sizeOf(q.Bytes), answered, by(dst))
		}
		if q.CarriesToken {
			f.sev, f.tailDim = sevHigh, false
			f.tail = join("carries this run's decoy token: a secret sent out", sizeOf(q.Bytes), answered, by(dst))
			if !secretSent {
				c.phrase = "sent a decoy's secret to " + host
			}
			secretSent = true
		}
		// publishing goes first: what it did with the credential
		if pub := published(q.Method, q.URL); pub != "" {
			f.sev, f.tailDim = sevHigh, false
			f.tail = pub + "; " + f.tail
			if pubPhrase == "" {
				pubPhrase = pub
			}
		}
		c.items = append(c.items, f)
	}
	if pubPhrase != "" {
		if secretSent {
			c.phrase = pubPhrase + " and " + c.phrase
		} else {
			c.phrase = pubPhrase
		}
	}
	for _, t := range r.TLSRefused {
		dst := t.Name + ":443"
		seen[dst] = true
		c.items = append(c.items, finding{sev: sevWarn, cols: []string{"https", t.Name, x + strconv.Itoa(t.Count)}, tail: join("refused the sinkhole's certificate (its own list of authorities): what it would send is unknown", by(dst)), tailDim: true, short: t.Name})
	}
	// the names: what it meant to reach, where an address says little; one
	// line a name, its types together (A and AAAA are one lookup)
	var names []string
	types := map[string][]string{}
	counts := map[string]int{}
	for _, d := range r.DNS {
		if _, ok := types[d.Name]; !ok {
			names = append(names, d.Name)
		}
		types[d.Name] = append(types[d.Name], d.Type)
		counts[d.Name] += d.Count
	}
	for _, name := range names {
		answer := "not answered"
		if r.Net == "sinkhole" {
			answer = "no answer"
			if slices.Contains(types[name], "A") {
				answer = "answered into the sinkhole"
			}
		}
		t := strings.Join(types[name], ", ")
		f := finding{sev: sevWarn, cols: []string{"dns", name, x + strconv.Itoa(counts[name])}, tail: t + "; " + answer, tailDim: true, short: name}
		if r.tokenIn(name) {
			f.sev, f.tailDim = sevHigh, false
			f.tail = "carries this run's decoy token: a secret sent out in a name; " + t
			if !secretSent {
				c.phrase = "sent a decoy's secret out in a DNS name"
			}
		}
		c.items = append(c.items, f)
	}
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
		if b := by(dst); b != "" {
			tail = append(tail, b)
		}
		if n.Reason != "egress" {
			tail = append(tail, n.Reason)
		}
		c.items = append(c.items, finding{sev: sevWarn, cols: []string{n.Protocol, dst, x + strconv.FormatUint(n.Count, 10)}, tail: strings.Join(tail, "; "), tailDim: true, short: dst})
	}
	for _, dst := range inside {
		// the resolver's own: the names it was asked are above
		if seen[dst] || dst == sandboxResolver+":53" && len(r.DNS) > 0 {
			continue
		}
		n := 0
		for _, k := range who[dst] {
			n += k
		}
		host, port := dst, 0
		if i := strings.LastIndex(dst, ":"); i >= 0 {
			host = dst[:i]
			port, _ = strconv.Atoi(dst[i+1:])
		}
		if r.isSinkName(host) {
			// a name the sinkhole answered, on a port it does not serve
			c.items = append(c.items, finding{sev: sevWarn, cols: []string{"tcp", dst, x + strconv.Itoa(n)}, tail: join(dstNote("", port), "nothing listens there in the sinkhole", by(dst)), tailDim: true, short: dst})
			continue
		}
		tail := []string{by(dst), "seen inside only"}
		if note := dstNote(strings.Trim(host, "[]"), port); note != "" {
			tail = append([]string{note}, tail...)
		}
		c.items = append(c.items, finding{sev: sevInfo, cols: []string{"?", dst, x + strconv.Itoa(n)}, tail: strings.Join(tail, "; "), tailDim: true, short: dst})
	}
	return c
}

// sinkName is ADDRESS:PORT with the sinkhole's address as the name it was
// given to (evil.example:4444), or as it was.
func (r *sandboxReport) sinkName(dst string) string {
	i := strings.LastIndex(dst, ":")
	if i < 0 {
		return dst
	}
	if name, ok := r.sinkAddr[dst[:i]]; ok {
		return name + dst[i:]
	}
	return dst
}

func (r *sandboxReport) isSinkName(host string) bool {
	for _, n := range r.sinkAddr {
		if n == host {
			return true
		}
	}
	return false
}

// published says what a request to the sinkhole published, in the
// registries' and GitHub's own APIs: a package (npm publish, twine upload),
// a repository created, a file or a branch written, a self-hosted runner
// registered. A worm spreads that way, with the credentials it found; a
// test, a build, an install has no reason to. "" for anything else.
func published(method, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host, p := strings.ToLower(u.Hostname()), u.EscapedPath()
	switch {
	case (host == "registry.npmjs.org" || host == "registry.yarnpkg.com") && method == "PUT" && len(p) > 1 && !strings.HasPrefix(p, "/-/"):
		name, err := url.PathUnescape(strings.TrimPrefix(p, "/"))
		if err != nil || strings.Contains(name, "/-rev/") {
			name = "a package"
		}
		return "published " + name + " to npm"
	case (host == "upload.pypi.org" || host == "test.pypi.org") && method == "POST" && strings.HasPrefix(p, "/legacy"):
		return "uploaded a package to PyPI"
	case host != "api.github.com" && host != "uploads.github.com":
		return ""
	case method == "POST" && (p == "/user/repos" || ghOrgRepos.MatchString(p)):
		return "created a GitHub repository"
	}
	m := ghRepoPath.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	repo, rest := m[1], m[2]
	switch {
	case (method == "PUT" || method == "POST") && strings.HasPrefix(rest, "/contents/"):
		return "wrote " + strings.TrimPrefix(rest, "/contents/") + " to GitHub's " + repo
	case method == "POST" && (rest == "/git/refs" || rest == "/git/commits" || rest == "/forks"):
		return "wrote to GitHub's " + repo
	case method == "POST" && rest == "/actions/runners/registration-token":
		return "registered a self-hosted runner in GitHub's " + repo
	case method == "POST" && strings.HasPrefix(rest, "/releases"):
		return "made a release in GitHub's " + repo
	}
	return ""
}

var (
	ghOrgRepos = regexp.MustCompile(`^/orgs/[^/]+/repos$`)
	ghRepoPath = regexp.MustCompile(`^/repos/([^/]+/[^/]+)(/.*)?$`)
)

// urlHost is the host of a URL the sinkhole wrote down, without its port.
func urlHost(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return u
	}
	host, _, _ := strings.Cut(rest, "/")
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.HasPrefix(host, "[") {
		host = h
	}
	return host
}

// defaultPort is the port a request was sent to: the one the sinkhole saw.
func defaultPort(scheme string, port int) int {
	if port != 0 {
		return port
	}
	if scheme == "https" {
		return 443
	}
	return 80
}

func sizeOf(n int) string {
	switch {
	case n == 0:
		return ""
	case n < 1024:
		return strconv.Itoa(n) + " bytes"
	}
	return strconv.Itoa(n/1024) + " KiB"
}
