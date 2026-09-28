// Package spec reads and validates a plant spec: the one file that declares
// which networks and functions the orchestrator keeps running (see
// docs/orchestrator.md §5).
//
// Parsing is strict — an unknown field, a duplicate key or a second document is
// an error — and the whole file is validated before anything is applied: a
// mistyped policy line must never degrade silently into "no rule".
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the only plant spec version this orchestrator reads.
const Version = 1

// Lifecycle modes (docs/orchestrator.md §7).
const (
	ModeTransaction = "transaction" // boot → run command once → destroy, every interval
	ModeWindow      = "window"      // boot → run command for a bounded time → destroy, every interval
	ModePersistent  = "persistent"  // always on; a fresh VM takes its place on failure
)

// NoNetwork is the network value of a function that gets no network at all.
const NoNetwork = "none"

// Labels the orchestrator sets itself and a spec may not.
var ReservedLabels = []string{"managed-by", "function", "generation", "spec"}

// Spec is a parsed, validated plant spec.
type Spec struct {
	Version   int                  `yaml:"version"`
	Budget    Budget               `yaml:"budget"`
	Networks  map[string]*Network  `yaml:"networks"`
	Functions map[string]*Function `yaml:"functions"`

	// FunctionOrder lists the functions as they appear in the file: updates
	// go one at a time in this order.
	FunctionOrder []string `yaml:"-"`
}

// File is a file written into a function's VM disk before its first boot
// (files: and secrets:). From is a path on the orchestrator's host, relative
// to the spec's directory; its content is read and checked when the spec is
// loaded, so a missing file fails the whole spec before any change.
type File struct {
	From string `yaml:"from"`
	// Mode is octal, e.g. "0644" (default) or "0400" (a secret's default).
	Mode string `yaml:"mode"`
	UID  int    `yaml:"uid"`
	GID  int    `yaml:"gid"`

	// Content, read by Load.
	Content []byte `yaml:"-"`
}

// Budget is the hard ceiling the spec's worst case must fit (§11).
type Budget struct {
	MaxVMs   int   `yaml:"max_vms"`
	MaxMemMB int64 `yaml:"max_mem_mb"`
	// Workers bounds how many cycles and recreations run at once.
	Workers int `yaml:"workers"`
}

// Network mirrors the engine's CreateNetworkRequest field for field (§5).
type Network struct {
	Subnet         string        `yaml:"subnet"`
	Intra          bool          `yaml:"intra"`
	Egress         bool          `yaml:"egress"`
	EgressIface    string        `yaml:"egress_iface"`
	AllowedEgress  []EgressRule  `yaml:"allowed_egress"`
	AllowedIngress []IngressRule `yaml:"allowed_ingress"`
}

// EgressRule mirrors the engine's EgressRule.
type EgressRule struct {
	Iface    string `yaml:"iface"`
	IP       string `yaml:"ip"`
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

// IngressRule mirrors the engine's IngressRule.
type IngressRule struct {
	Iface    string `yaml:"iface"`
	SrcIP    string `yaml:"src_ip"`
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	ToIP     string `yaml:"to_ip"`
}

// Function is one workload the orchestrator keeps in its declared state.
type Function struct {
	// Image is name:version@sha256:<digest>; the digest is mandatory.
	Image string `yaml:"image"`
	// Network is a network of this spec, or "none".
	Network string `yaml:"network"`
	// IP pins the function's address on its network.
	IP string `yaml:"ip"`
	// Command runs inside the VM: once per cycle (transaction), for the
	// window (window), or detached for the VM's life (persistent, optional).
	Command   string            `yaml:"command"`
	Health    *Health           `yaml:"health"`
	Lifecycle Lifecycle         `yaml:"lifecycle"`
	Resources Resources         `yaml:"resources"`
	Labels    map[string]string `yaml:"labels"`
	// Files and Secrets map a guest path to its source. A secret's content
	// never appears in a plan, a log or the engine's record.
	Files   map[string]*File `yaml:"files"`
	Secrets map[string]*File `yaml:"secrets"`
}

// Health is a command run inside a persistent VM; exit 0 means healthy.
type Health struct {
	Command  string   `yaml:"command"`
	Every    Duration `yaml:"every"`
	Timeout  Duration `yaml:"timeout"`
	Failures int      `yaml:"failures"`
}

// Lifecycle says how long each of a function's VMs lives.
type Lifecycle struct {
	Mode     string   `yaml:"mode"`
	Every    Duration `yaml:"every"`
	Timeout  Duration `yaml:"timeout"`
	Duration Duration `yaml:"duration"`
	// Recycle (persistent only): "never" (the default) or an interval after
	// which a fresh VM takes the running one's place.
	Recycle Recycle `yaml:"recycle"`
}

// Resources override the image's defaults; zero keeps them.
type Resources struct {
	VCPUs  int64 `yaml:"vcpus"`
	MemMB  int64 `yaml:"mem_mb"`
	DiskMB int64 `yaml:"disk_mb"`
}

// Duration is a time.Duration written as "30s", "5m", "24h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: duration %q: want e.g. 30s, 5m, 24h", n.Line, s)
	}
	if v <= 0 {
		return fmt.Errorf("line %d: duration %q must be positive", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Recycle is "never" or a duration; zero means never.
type Recycle time.Duration

func (r *Recycle) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if s == "never" {
		*r = 0
		return nil
	}
	var d Duration
	if err := d.UnmarshalYAML(n); err != nil {
		return fmt.Errorf("%w (or \"never\")", err)
	}
	*r = Recycle(d)
	return nil
}

// D returns the recycle interval; 0 means never.
func (r Recycle) D() time.Duration { return time.Duration(r) }

// Load reads and validates the spec at path, and reads the content of every
// file and secret it declares (relative paths from the spec's directory).
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.ReadFiles(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Engine limits on the files of one VM (see the engine's CreateVMRequest).
const (
	MaxFiles     = 64
	MaxFileBytes = 512 << 10
)

// ReadFiles reads every declared file and secret, relative to dir, and checks
// them: regular files, within the engine's limits, and — for a secret — not
// readable by anyone but its owner.
func (s *Spec) ReadFiles(dir string) error {
	var errs []error
	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		total := 0
		for _, set := range []struct {
			what   string
			files  map[string]*File
			secret bool
		}{{"files", f.Files, false}, {"secrets", f.Secrets, true}} {
			for guest, src := range set.files {
				where := fmt.Sprintf("functions.%s.%s[%s]", name, set.what, guest)
				p := src.From
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				fi, err := os.Stat(p)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", where, err))
					continue
				}
				if !fi.Mode().IsRegular() {
					errs = append(errs, fmt.Errorf("%s: %s is not a regular file", where, p))
					continue
				}
				if set.secret && fi.Mode().Perm()&0o077 != 0 {
					errs = append(errs, fmt.Errorf("%s: %s is readable by group or others (mode %04o): a secret's source must be private (chmod 600)", where, p, fi.Mode().Perm()))
					continue
				}
				if fi.Size() > MaxFileBytes {
					errs = append(errs, fmt.Errorf("%s: %s is %d bytes, over %d", where, p, fi.Size(), MaxFileBytes))
					continue
				}
				data, err := os.ReadFile(p)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", where, err))
					continue
				}
				src.Content = data
				total += len(data)
			}
		}
		if total > MaxFileBytes {
			errs = append(errs, fmt.Errorf("functions.%s: files and secrets add up to %d bytes, over %d", name, total, MaxFileBytes))
		}
	}
	return errors.Join(errs...)
}

// Parse decodes a spec strictly and validates it in full.
func Parse(data []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty spec")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document: a plant spec is exactly one")
	}
	order, err := functionOrder(data)
	if err != nil {
		return nil, err
	}
	s.FunctionOrder = order
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// functionOrder reads the keys of "functions" in file order.
func functionOrder(data []byte) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("a plant spec is a mapping")
	}
	top := root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "functions" {
			continue
		}
		fns := top.Content[i+1]
		var out []string
		for j := 0; j+1 < len(fns.Content); j += 2 {
			out = append(out, fns.Content[j].Value)
		}
		return out, nil
	}
	return nil, nil
}

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	labelKeyRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`)
	labelValRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)
	// name:version@sha256:<64 hex> — the digest is what the engine boots.
	modeRE  = regexp.MustCompile(`^0?[0-7]{3}$`)
	imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
)

// MaxFunctionName leaves room for "-<generation>" in a 63-character VM name.
const MaxFunctionName = 50

// MaxCommand bounds a command line; the guest agent reads one line.
const MaxCommand = 4096

// validate checks everything that can be checked without the engine and
// collects every problem, so one run of `validate` shows them all.
func (s *Spec) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if s.Version != Version {
		add("version: must be %d", Version)
	}
	if s.Budget.MaxVMs <= 0 {
		add("budget.max_vms: required, > 0")
	}
	if s.Budget.MaxMemMB <= 0 {
		add("budget.max_mem_mb: required, > 0")
	}
	if s.Budget.Workers < 0 {
		add("budget.workers: must be >= 0")
	}
	if s.Budget.Workers == 0 {
		s.Budget.Workers = 2
	}

	subnets := map[string]netip.Prefix{}
	for name, n := range s.Networks {
		where := "networks." + name
		if n == nil {
			add("%s: empty", where)
			continue
		}
		if !nameRE.MatchString(name) || len(name) > 63 || name == NoNetwork || name == "default" {
			add("%s: name must be a DNS label (lowercase letters, digits, '-'), not %q or %q", where, NoNetwork, "default")
		}
		p, err := netip.ParsePrefix(n.Subnet)
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() > 29 {
			add("%s.subnet: required, an IPv4 network such as 172.16.10.0/24 (at most /29)", where)
			continue
		}
		for other, q := range subnets {
			if p.Overlaps(q) {
				add("%s.subnet: overlaps networks.%s", where, other)
			}
		}
		subnets[name] = p
		if n.Egress && n.EgressIface == "" {
			add("%s: egress: true needs egress_iface (the one host interface it leaves through)", where)
		}
		if !n.Egress && n.EgressIface != "" {
			add("%s: egress_iface without egress: true", where)
		}
		if n.Egress && len(n.AllowedEgress) > 0 {
			add("%s: egress: true and allowed_egress are exclusive", where)
		}
		for i, r := range n.AllowedEgress {
			w := fmt.Sprintf("%s.allowed_egress[%d]", where, i)
			if !validAddrOrPrefix(r.IP) {
				add("%s.ip: an IPv4 address or CIDR", w)
			}
			switch r.Protocol {
			case "tcp", "udp":
				if r.Port < 1 || r.Port > 65535 {
					add("%s.port: 1-65535 for %s", w, r.Protocol)
				}
			case "icmp":
				if r.Port != 0 {
					add("%s.port: not allowed for icmp", w)
				}
			default:
				add("%s.protocol: tcp, udp or icmp", w)
			}
		}
		for i, r := range n.AllowedIngress {
			w := fmt.Sprintf("%s.allowed_ingress[%d]", where, i)
			if r.Iface == "" {
				add("%s.iface: required (a managed interface)", w)
			}
			if !validAddrOrPrefix(r.SrcIP) {
				add("%s.src_ip: an IPv4 address or CIDR", w)
			}
			if r.Protocol != "tcp" && r.Protocol != "udp" {
				add("%s.protocol: tcp or udp", w)
			}
			if r.Port < 1 || r.Port > 65535 {
				add("%s.port: 1-65535", w)
			}
			if a, err := netip.ParseAddr(r.ToIP); err != nil || !p.Contains(a) {
				add("%s.to_ip: an address inside %s", w, p)
			}
		}
	}

	if len(s.Functions) == 0 {
		add("functions: at least one")
	}
	ips := map[string]string{} // network/ip → function
	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		where := "functions." + name
		if f == nil {
			add("%s: empty", where)
			continue
		}
		if !nameRE.MatchString(name) || len(name) > MaxFunctionName {
			add("%s: name must be a DNS label of at most %d characters", where, MaxFunctionName)
		}
		if !imageRE.MatchString(f.Image) {
			add("%s.image: name:version@sha256:<digest> — the digest is mandatory", where)
		}
		var subnet netip.Prefix
		switch {
		case f.Network == "":
			add("%s.network: required: a network of this spec, or %q", where, NoNetwork)
		case f.Network == NoNetwork:
		default:
			if _, ok := s.Networks[f.Network]; !ok {
				add("%s.network: %q is not declared under networks", where, f.Network)
			}
			subnet = subnets[f.Network]
		}
		if f.IP != "" {
			a, err := netip.ParseAddr(f.IP)
			switch {
			case f.Network == NoNetwork || f.Network == "":
				add("%s.ip: needs a network", where)
			case err != nil || !a.Is4():
				add("%s.ip: an IPv4 address", where)
			case subnet.IsValid() && !usableHost(subnet, a):
				add("%s.ip: %s is not a usable guest address of %s (not the network, gateway .1 or broadcast)", where, f.IP, subnet)
			}
			key := f.Network + "/" + f.IP
			if other, dup := ips[key]; dup {
				add("%s.ip: %s is already functions.%s's", where, f.IP, other)
			}
			ips[key] = name
		}
		if err := checkCommand(f.Command); err != nil {
			add("%s.command: %v", where, err)
		}

		lc := f.Lifecycle
		switch lc.Mode {
		case ModeTransaction:
			if lc.Every == 0 || lc.Timeout == 0 {
				add("%s.lifecycle: transaction needs every and timeout", where)
			} else if lc.Timeout >= lc.Every {
				add("%s.lifecycle: timeout must be shorter than every", where)
			}
			if lc.Duration != 0 || lc.Recycle != 0 {
				add("%s.lifecycle: duration and recycle do not apply to transaction", where)
			}
		case ModeWindow:
			if lc.Every == 0 || lc.Duration == 0 {
				add("%s.lifecycle: window needs every and duration", where)
			} else if lc.Duration >= lc.Every {
				add("%s.lifecycle: duration must be shorter than every", where)
			}
			if lc.Timeout != 0 || lc.Recycle != 0 {
				add("%s.lifecycle: timeout and recycle do not apply to window", where)
			}
		case ModePersistent:
			if lc.Every != 0 || lc.Timeout != 0 || lc.Duration != 0 {
				add("%s.lifecycle: every, timeout and duration do not apply to persistent (recycle does)", where)
			}
			if lc.Recycle != 0 && lc.Recycle.D() < time.Minute {
				add("%s.lifecycle.recycle: at least 1m, or never", where)
			}
		case "":
			add("%s.lifecycle.mode: required: %s, %s or %s", where, ModeTransaction, ModeWindow, ModePersistent)
		default:
			add("%s.lifecycle.mode: %q: want %s, %s or %s", where, lc.Mode, ModeTransaction, ModeWindow, ModePersistent)
		}
		if (lc.Mode == ModeTransaction || lc.Mode == ModeWindow) && f.Command == "" {
			add("%s.command: required for %s", where, lc.Mode)
		}

		if h := f.Health; h != nil {
			if lc.Mode != ModePersistent {
				add("%s.health: only persistent functions have a health check (a cycle's exit code is its check)", where)
			}
			if h.Command == "" {
				add("%s.health.command: required", where)
			} else if err := checkCommand(h.Command); err != nil {
				add("%s.health.command: %v", where, err)
			}
			if h.Every == 0 {
				h.Every = Duration(10 * time.Second)
			}
			if h.Timeout == 0 {
				h.Timeout = Duration(2 * time.Second)
			}
			if h.Failures == 0 {
				h.Failures = 3
			}
			if h.Failures < 0 {
				add("%s.health.failures: must be > 0", where)
			}
			if h.Timeout >= h.Every {
				add("%s.health: timeout must be shorter than every", where)
			}
		}

		seen := map[string]string{}
		for _, set := range []struct {
			what  string
			files map[string]*File
		}{{"files", f.Files}, {"secrets", f.Secrets}} {
			for guest, src := range set.files {
				w := fmt.Sprintf("%s.%s[%s]", where, set.what, guest)
				if other, dup := seen[guest]; dup {
					add("%s: also declared under %s", w, other)
				}
				seen[guest] = set.what
				if !strings.HasPrefix(guest, "/") || strings.Contains(guest, "/../") || strings.HasSuffix(guest, "/..") || strings.HasSuffix(guest, "/") || strings.ContainsAny(guest, "\n\x00") {
					add("%s: the guest path must be absolute and name a file", w)
				}
				if src == nil || src.From == "" {
					add("%s.from: required", w)
					continue
				}
				if src.Mode != "" && !modeRE.MatchString(src.Mode) {
					add("%s.mode: octal permission bits such as 0644, no setuid, setgid or sticky", w)
				}
				if src.UID < 0 || src.GID < 0 {
					add("%s: uid and gid must be >= 0", w)
				}
			}
		}
		if n := len(f.Files) + len(f.Secrets); n > MaxFiles {
			add("%s: %d files and secrets, at most %d", where, n, MaxFiles)
		}

		r := f.Resources
		if r.VCPUs < 0 || r.VCPUs > 32 {
			add("%s.resources.vcpus: 0 (image default) to 32", where)
		}
		if r.MemMB < 0 || (r.MemMB > 0 && r.MemMB < 64) {
			add("%s.resources.mem_mb: 0 (image default) or at least 64", where)
		}
		if r.DiskMB < 0 {
			add("%s.resources.disk_mb: must be >= 0", where)
		}

		for k, v := range f.Labels {
			if !labelKeyRE.MatchString(k) || len(k) > 63 || !labelValRE.MatchString(v) || len(v) > 63 {
				add("%s.labels: %s=%s: keys lowercase [a-z0-9._/-], values [A-Za-z0-9._-], at most 63 characters", where, k, v)
			}
			for _, r := range ReservedLabels {
				if k == r {
					add("%s.labels: %q is set by the orchestrator", where, k)
				}
			}
		}
	}

	// An ingress rule's to_ip belongs to exactly one function: automatic
	// allocation never hands it out, so nothing else would ever hold it.
	for name, n := range s.Networks {
		if n == nil {
			continue
		}
		for i, r := range n.AllowedIngress {
			if _, ok := ips[name+"/"+r.ToIP]; !ok {
				add("networks.%s.allowed_ingress[%d].to_ip: no function on %s declares ip: %s", name, i, name, r.ToIP)
			}
		}
	}

	return errors.Join(errs...)
}

// checkCommand keeps a command to what the guest agent can take: one line.
func checkCommand(c string) error {
	if len(c) > MaxCommand {
		return fmt.Errorf("longer than %d characters", MaxCommand)
	}
	if strings.ContainsAny(c, "\n\r\x00") {
		return errors.New("must be a single line (use && or ; to chain)")
	}
	return nil
}

func validAddrOrPrefix(s string) bool {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Is4()
	}
	p, err := netip.ParsePrefix(s)
	return err == nil && p.Addr().Is4()
}

// usableHost: inside p, and not its network address, its gateway (.1, the
// host's side of the bridge) or its broadcast.
func usableHost(p netip.Prefix, a netip.Addr) bool {
	if !p.Contains(a) {
		return false
	}
	base := p.Masked().Addr()
	if a == base || a == base.Next() {
		return false
	}
	last := base
	for i := 0; i < (1<<(32-p.Bits()))-1; i++ {
		last = last.Next()
	}
	return a != last
}
