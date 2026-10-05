package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The user's own rules for mh sandbox (docs/sandbox.md, "Your own rules"):
// detections of their own — a path opened, a command line, a connection —
// and findings they have checked and accept. They come only from the files
// named on the command line (--rules FILE): no file is read on its own, so
// nothing left somewhere can slip rules into a run; and never from the code
// under test, which could otherwise accept itself. In the VM they are
// root's (/var/lib/mh-sandbox/rules): the code cannot read or change them.
// sandbox/rules/ has examples.
//
//	detect:
//	  - name: brave-passwords        # the alert's title: rule:brave-passwords
//	    severity: high               # high, warn (default) or info
//	    means: Brave's saved passwords
//	    path: ~/.config/BraveSoftware/   # opened, run or asked about; / at the end: what is inside too
//	  - name: solana-transfer
//	    command: 'solana .*transfer'     # a POSIX extended regex, on every command line run
//	  - name: known-c2
//	    connect: 203.0.113.0/24          # an address, a range, :PORT or both
//	accept:
//	  - kind: dropped_exec               # an alert's kind, or decoy, vm_probe, privesc, changed, connection, process, listening
//	    what: '^~/work/node_modules/'    # a regex, on what the report says (-v), ~ for the home
//	    by: node                         # the program, exactly
//	    why: esbuild's own binary        # required: whoever reads the report sees it
//
// An accepted finding is not dropped: it moves, graded info, under
// "accepted by your rules", with its why.

type sandboxRules struct {
	Files  []string
	Detect []detectRule
	Accept []acceptRule
}

type detectRule struct {
	Name     string `yaml:"name"`
	Severity string `yaml:"severity"`
	Means    string `yaml:"means"`
	Path     string `yaml:"path"`
	Command  string `yaml:"command"`
	Connect  string `yaml:"connect"`

	sev  severity
	conn connRule
}

type acceptRule struct {
	Kind string `yaml:"kind"`
	What string `yaml:"what"`
	By   string `yaml:"by"`
	Why  string `yaml:"why"`

	what *regexp.Regexp
}

// connRule is a connect: an address or a range, a port, or both.
type connRule struct {
	prefix netip.Prefix // invalid: any address
	port   int          // 0: any port
}

var ruleName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,39}$`)

// acceptKinds are what an accept rule may name besides an alert's kind.
var acceptKinds = []string{"decoy", "vm_probe", "privesc", "changed", "connection", "process", "listening"}

// loadSandboxRules reads each of files, in order; none is no rules.
func loadSandboxRules(files []string) (*sandboxRules, error) {
	rs := &sandboxRules{}
	names := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Detect []detectRule `yaml:"detect"`
			Accept []acceptRule `yaml:"accept"`
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		// an empty file is no rules
		if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for i := range doc.Detect {
			d := &doc.Detect[i]
			if err := d.check(); err != nil {
				return nil, fmt.Errorf("%s: detect #%d: %w", f, i+1, err)
			}
			if prev, ok := names[d.Name]; ok {
				return nil, fmt.Errorf("%s: detect %q: already a rule in %s", f, d.Name, prev)
			}
			names[d.Name] = f
			rs.Detect = append(rs.Detect, *d)
		}
		for i := range doc.Accept {
			a := &doc.Accept[i]
			if err := a.check(names); err != nil {
				return nil, fmt.Errorf("%s: accept #%d: %w", f, i+1, err)
			}
			rs.Accept = append(rs.Accept, *a)
		}
		rs.Files = append(rs.Files, f)
	}
	return rs, nil
}

func (d *detectRule) check() error {
	if !ruleName.MatchString(d.Name) {
		return fmt.Errorf("name %q: lowercase letters, digits, - _ . (at most 40)", d.Name)
	}
	switch d.Severity {
	case "", "warn":
		d.sev = sevWarn
	case "high":
		d.sev = sevHigh
	case "info":
		d.sev = sevInfo
	default:
		return fmt.Errorf("%s: severity %q: high, warn or info", d.Name, d.Severity)
	}
	if d.Path == "" && d.Command == "" && d.Connect == "" {
		return fmt.Errorf("%s: a path, a command or a connect to look for", d.Name)
	}
	if strings.ContainsAny(d.Means, "\t\n") {
		return fmt.Errorf("%s: means: one line", d.Name)
	}
	if p := d.Path; p != "" {
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/") || strings.ContainsAny(p, "\t\n\\^\"") || len(p) > 300 {
			return fmt.Errorf("%s: path %q: absolute or ~/…, without \\ ^ \" (at most 300)", d.Name, p)
		}
	}
	if c := d.Command; c != "" {
		if strings.ContainsAny(c, "\t\n") || len(c) > 300 {
			return fmt.Errorf("%s: command: one line, at most 300", d.Name)
		}
		if _, err := regexp.CompilePOSIX(c); err != nil {
			return fmt.Errorf("%s: command %q: not a POSIX extended regex (grep -E: no \\d, \\b…): %v", d.Name, c, err)
		}
	}
	if c := d.Connect; c != "" {
		cr, err := parseConnRule(c)
		if err != nil {
			return fmt.Errorf("%s: connect %q: %v", d.Name, c, err)
		}
		d.conn = cr
	}
	return nil
}

// parseConnRule reads 1.2.3.4, 10.0.0.0/8, :4444, 1.2.3.4:4444,
// 10.0.0.0/8:4444, [2001:db8::1]:443, 2001:db8::/32.
func parseConnRule(s string) (connRule, error) {
	var cr connRule
	addr, port := s, ""
	switch {
	case strings.HasPrefix(s, ":"):
		addr, port = "", s[1:]
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return cr, errors.New("no ] after the address")
		}
		addr = s[1:end]
		if rest := s[end+1:]; rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return cr, errors.New("[ADDRESS]:PORT")
			}
			port = rest[1:]
		}
	case strings.Count(s, ":") == 1:
		addr, port, _ = strings.Cut(s, ":")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return cr, errors.New("a port is 1-65535")
		}
		cr.port = n
	}
	if addr != "" {
		if !strings.Contains(addr, "/") {
			a, err := netip.ParseAddr(addr)
			if err != nil {
				return cr, errors.New("an address (1.2.3.4), a range (10.0.0.0/8), :PORT or ADDRESS:PORT")
			}
			addr = a.String() + "/" + strconv.Itoa(a.BitLen())
		}
		p, err := netip.ParsePrefix(addr)
		if err != nil {
			return cr, errors.New("an address (1.2.3.4), a range (10.0.0.0/8), :PORT or ADDRESS:PORT")
		}
		cr.prefix = p.Masked()
	}
	return cr, nil
}

func (cr connRule) match(dst string, port int) bool {
	if cr.port != 0 && cr.port != port {
		return false
	}
	if !cr.prefix.IsValid() {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(dst, "[]"))
	return err == nil && cr.prefix.Contains(a.Unmap())
}

func (a *acceptRule) check(detects map[string]string) error {
	switch {
	case slices.Contains(acceptKinds, a.Kind):
	case strings.HasPrefix(a.Kind, "rule:"):
		if _, ok := detects[strings.TrimPrefix(a.Kind, "rule:")]; !ok {
			return fmt.Errorf("kind %q: no detect rule of that name before it", a.Kind)
		}
	default:
		if _, ok := alertKinds[a.Kind]; !ok {
			return fmt.Errorf("kind %q: an alert's kind (%s), rule:NAME, or one of %s", a.Kind, strings.Join(sortedKinds(), ", "), strings.Join(acceptKinds, ", "))
		}
	}
	if a.What == "" && a.By == "" {
		return fmt.Errorf("%s: a what or a by: accepting every %s would hide what this rule is not about", a.Kind, a.Kind)
	}
	if strings.TrimSpace(a.Why) == "" {
		return fmt.Errorf("%s: why: whoever reads the report sees it", a.Kind)
	}
	if a.What != "" {
		re, err := regexp.Compile(a.What)
		if err != nil {
			return fmt.Errorf("%s: what %q: %v", a.Kind, a.What, err)
		}
		a.what = re
	}
	return nil
}

func sortedKinds() []string {
	ks := make([]string, 0, len(alertKinds))
	for k := range alertKinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (a acceptRule) match(kind, what, by string) bool {
	return a.Kind == kind && (a.By == "" || a.By == by) && (a.what == nil || a.what.MatchString(what))
}

// accepts is the accept rule that matches a finding, if any.
func (rs *sandboxRules) accepts(kind, what, by string) (acceptRule, bool) {
	if rs == nil {
		return acceptRule{}, false
	}
	for _, a := range rs.Accept {
		if a.match(kind, what, by) {
			return a, true
		}
	}
	return acceptRule{}, false
}

// kindOf is alertKinds' entry, or a detect rule's for rule:NAME.
func (rs *sandboxRules) kindOf(kind string) alertKind {
	if name, ok := strings.CutPrefix(kind, "rule:"); ok && rs != nil {
		for _, d := range rs.Detect {
			if d.Name == name {
				return alertKind{Severity: d.sev, Title: "your rule " + d.Name, Means: d.means()}
			}
		}
	}
	return kindOf(kind)
}

func (d detectRule) means() string {
	if d.Means != "" {
		return d.Means
	}
	var on []string
	if d.Path != "" {
		on = append(on, "path "+d.Path)
	}
	if d.Command != "" {
		on = append(on, "command /"+d.Command+"/")
	}
	if d.Connect != "" {
		on = append(on, "connect "+d.Connect)
	}
	return strings.Join(on, ", ")
}

// guest is the rules the VM applies (classify, mh-sandbox-lib): path NAME
// ERE, command NAME ERE, one a line. "" when there are none. A path's ERE
// starts with ^~ for the home, which the guest knows.
func (rs *sandboxRules) guest() string {
	if rs == nil {
		return ""
	}
	var b strings.Builder
	for _, d := range rs.Detect {
		if d.Path != "" {
			p, prefix := strings.CutSuffix(d.Path, "/")
			ere := "^" + ereLiteral(p) + "$"
			if prefix {
				ere = "^" + ereLiteral(p) + "(/|$)"
			}
			fmt.Fprintf(&b, "path\t%s\t%s\n", d.Name, ere)
		}
		if d.Command != "" {
			fmt.Fprintf(&b, "command\t%s\t%s\n", d.Name, d.Command)
		}
	}
	return b.String()
}

// ereLiteral matches s itself in an ERE, every special character in
// brackets: no backslash, which awk's strings would read first.
func ereLiteral(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '[', '(', ')', '*', '+', '?', '{', '}', '|', '$':
			b.WriteString("[" + string(r) + "]")
		case ']':
			b.WriteString("[]]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sandboxAccepted is a finding an accept rule matched: out of its list,
// here, graded info. What and By are as the report shows them (~ for the
// home); untrusted.
type sandboxAccepted struct {
	Kind  string `json:"kind"`
	What  string `json:"what"`
	By    string `json:"by"`
	Count int    `json:"count"`
	Why   string `json:"why"`
}

// applyRules adds what the connect rules match, as alerts (path and command
// rules are the guest's), grades every rule:NAME alert by its rule, then
// moves what the accept rules match under Accepted.
func (r *sandboxReport) applyRules(rs *sandboxRules) {
	r.rules = rs
	if rs == nil {
		return
	}
	r.Rules = rs.Files

	// who tried each destination, seen inside (audit's connect)
	who := map[string]map[string]int{}
	for _, a := range r.Alerts {
		if a.Kind == "connect" {
			dst := strings.TrimSuffix(a.What, " (dns)")
			if who[dst] == nil {
				who[dst] = map[string]int{}
			}
			who[dst][a.By] += a.Count
		}
	}
	byOf := func(dst string) string {
		if len(who[dst]) == 0 {
			return ""
		}
		return topNames(who[dst], 3)
	}
	for _, d := range rs.Detect {
		if d.Connect == "" {
			continue
		}
		for _, c := range r.Connections {
			if d.conn.match(c.Dst, c.DstPort) {
				dst := connDst(c)
				r.Alerts = append(r.Alerts, sandboxAlert{Kind: "rule:" + d.Name, Count: int(c.Count), What: dst, By: byOf(dst)})
			}
		}
	}
	for i := range r.Alerts {
		r.Alerts[i].Severity = rs.kindOf(r.Alerts[i].Kind).Severity.String()
	}
	if len(rs.Accept) == 0 {
		return
	}

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
	accept := func(kind, what, by string, n int) bool {
		a, ok := rs.accepts(kind, what, by)
		if ok {
			r.Accepted = append(r.Accepted, sandboxAccepted{Kind: kind, What: what, By: by, Count: n, Why: a.Why})
		}
		return ok
	}

	// a connection: refused by the host, or seen inside only; its connect
	// alerts go with it
	gone := map[string]bool{}
	r.Connections = slices.DeleteFunc(r.Connections, func(c sandboxConn) bool {
		dst := connDst(c)
		if accept("connection", dst, byOf(dst), int(c.Count)) {
			gone[dst] = true
		}
		return gone[dst]
	})
	r.Alerts = slices.DeleteFunc(r.Alerts, func(a sandboxAlert) bool {
		if a.Kind != "connect" {
			return accept(a.Kind, tilde(a.What), a.By, a.Count)
		}
		dst := strings.TrimSuffix(a.What, " (dns)")
		if !gone[dst] && !slices.ContainsFunc(r.Connections, func(c sandboxConn) bool { return connDst(c) == dst }) {
			gone[dst] = accept("connection", dst, byOf(dst), a.Count)
		}
		return gone[dst]
	})
	probes := func(kind string, ps []sandboxProbe) []sandboxProbe {
		return slices.DeleteFunc(ps, func(p sandboxProbe) bool { return accept(kind, tilde(p.Path), p.By, p.Count) })
	}
	r.VMProbes = probes("vm_probe", r.VMProbes)
	r.Privesc = probes("privesc", r.Privesc)
	changed := func(fs []string) []string {
		return slices.DeleteFunc(fs, func(f string) bool { return accept("changed", tilde(f), "", 1) })
	}
	r.Changed.Files = changed(r.Changed.Files)
	r.Changed.Dirs = changed(r.Changed.Dirs)
	r.Processes = slices.DeleteFunc(r.Processes, func(p sandboxProc) bool { return accept("process", p.Args, "", 1) })
	r.Listening = slices.DeleteFunc(r.Listening, func(l string) bool { return accept("listening", l, "", 1) })
	// a decoy stays in its list (17 planted, 17 accounted for), marked; by
	// is its reader when it had one only
	for i := range r.Decoys {
		d := &r.Decoys[i]
		if d.State == "untouched" {
			continue
		}
		by, n := "", 0
		for name, k := range r.decoyBy[d.Path] {
			by, n = name, n+k
		}
		if len(r.decoyBy[d.Path]) != 1 {
			by = ""
		}
		if a, ok := rs.accepts("decoy", tilde(d.Path), by); ok {
			d.Accepted = a.Why
			r.Accepted = append(r.Accepted, sandboxAccepted{Kind: "decoy", What: d.State + " " + tilde(d.Path), By: by, Count: max(n, 1), Why: a.Why})
		}
	}
}

// connDst is a connection as the report shows it: 1.2.3.4:443, [::1]:53.
func connDst(c sandboxConn) string {
	dst := c.Dst
	if strings.Contains(dst, ":") {
		dst = "[" + dst + "]"
	}
	if c.DstPort != 0 {
		dst += ":" + strconv.Itoa(c.DstPort)
	}
	return dst
}

func (rs *sandboxRules) detects() []detectRule {
	if rs == nil {
		return nil
	}
	return rs.Detect
}

// tildeHost is a host path with ~ for the host user's home.
func tildeHost(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h+"/") {
		return "~" + p[len(h):]
	}
	return p
}

// within says whether file is target or inside it, links resolved.
func within(file, target string) bool {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		a, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return a
	}
	f, t := real(file), real(target)
	return f == t || strings.HasPrefix(f, t+string(filepath.Separator))
}
