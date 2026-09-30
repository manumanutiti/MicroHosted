// Package build turns an image spec — a YAML file naming a base, packages,
// files and build steps, like a Dockerfile — into a kernel and a root
// filesystem the engine's image store imports (mh build; docs/orchestrator.md
// §4). The build runs on the engine's host, as root through sudo: it unpacks
// the base, installs packages in a chroot and writes an ext4 image.
package build

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is a parsed, validated image spec.
//
//	base: alpine:3.22
//	packages: [nginx]
//	files:
//	  /srv/www/: out/            # a directory: its contents
//	  /etc/nginx/http.d/default.conf: nginx.conf
//	run:
//	  - mkdir -p /run/nginx
//	# or, in the order they run (each a layer, as a Dockerfile's COPY and RUN):
//	# steps:
//	#   - copy: {/srv/www/: out/}
//	#   - run: mkdir -p /run/nginx
//	command: nginx -g 'daemon off;'
//	health: { command: "wget -qO- -T 2 http://127.0.0.1/", every: 10s, timeout: 3s }
//	mem_mb: 64
type Spec struct {
	// Name is the image's name, what mh build tags it with when -t does not
	// say: "name:sha-<fingerprint>". Optional; the default is the build
	// context directory's name.
	Name string `yaml:"name"`
	// Base is the distribution the image starts from: "alpine:3.22" or
	// "alpine:3.22.0", a pinned minirootfs (see Bases).
	Base string `yaml:"base"`
	// Kernel is the guest kernel version (see Kernels); empty takes the
	// default.
	Kernel string `yaml:"kernel"`
	// Packages are installed with apk, in the image, before files and run.
	Packages []string `yaml:"packages"`
	// Files maps a guest path to a source relative to the build context,
	// copied as Docker's COPY does: a file to the path (into it, when the
	// path ends in "/"), a directory's contents into the path. Owned by root,
	// modes kept.
	Files map[string]string `yaml:"files"`
	// Run are shell commands run in the image, in order, after packages and
	// files, with the build host's network — Docker's RUN.
	Run []string `yaml:"run"`
	// Steps are copies and commands in the order they run, instead of Files
	// and Run (which copy every file first): so that what changes least —
	// a dependency list and its install — comes before what changes most,
	// and a change to the code does not rerun the install.
	Steps []Step `yaml:"steps"`

	// Defaults of the VMs created from the image. Command and Health are for
	// whoever runs them (mh run starts the command, the orchestrator uses
	// both); the engine runs neither.
	Command string  `yaml:"command"`
	Health  *Health `yaml:"health"`
	VCPUs   int64   `yaml:"vcpus"`
	MemMB   int64   `yaml:"mem_mb"`
	DiskMB  int64   `yaml:"disk_mb"`
	// SizeMB is the root filesystem's size; 0 sizes it to its content plus
	// headroom.
	SizeMB int64 `yaml:"size_mb"`

	// Ops are the build's copies and commands in order: Steps as written,
	// or Files sorted by guest path (a directory before what goes inside
	// it) and then Run.
	Ops []Op `yaml:"-" json:"-"`
}

// Step is one entry of steps: exactly one of copy (guest path ← source, as
// Files; several are copied in guest path order) and run.
type Step struct {
	Copy map[string]string `yaml:"copy"`
	Run  string            `yaml:"run"`
}

// Op is one copy (Guest ← Source) or one command (Run) of a build.
type Op struct {
	Guest, Source string
	Run           string
	// Where is where the spec says it, for errors: "files[/etc/x]",
	// "steps[2].copy[/opt/app/]", "run[0]".
	Where string
}

// IsCopy tells a copy from a command.
func (o Op) IsCopy() bool { return o.Run == "" }

// Health is the image's default health check.
type Health struct {
	Command  string `yaml:"command"`
	Every    string `yaml:"every"`
	Timeout  string `yaml:"timeout"`
	Failures int    `yaml:"failures"`
}

// Defaults of a spec that leaves them out: those of make prepare-image's
// Alpine image.
const (
	DefaultVCPUs = 1
	DefaultMemMB = 128
)

// maxCommand matches the guest agent's single line (and the engine's limit).
const maxCommand = 4096

// nameRE is an image name: a DNS label (the engine's rule for a tag's name).
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]*(([<>]?=|~|>|<)[A-Za-z0-9._+-]+)?$`)

// Load reads and validates the spec at p.
func Load(p string) (*Spec, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// Parse decodes a spec strictly — an unknown field, a duplicate key or a
// second document is an error — and validates it in full.
func Parse(data []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty image spec")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document: an image spec is exactly one")
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Spec) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if s.Name != "" && !nameRE.MatchString(s.Name) {
		add("name: %q: lowercase letters, digits and '-', starting and ending with a letter or digit, at most 63 characters", s.Name)
	}
	if s.Base == "" {
		add("base: required, e.g. alpine:3.22 (known: %s)", strings.Join(knownBases(), ", "))
	} else if _, err := resolveBase(s.Base); err != nil {
		add("base: %v", err)
	}
	if s.Kernel == "" {
		s.Kernel = DefaultKernel
	}
	if _, ok := Kernels[s.Kernel]; !ok {
		add("kernel: %q is not a known kernel (known: %s)", s.Kernel, strings.Join(sortedKeys(Kernels), ", "))
	}
	for i, p := range s.Packages {
		if !packageRE.MatchString(p) {
			add("packages[%d]: %q is not an apk package name (optionally with a version: nginx=1.28.0-r3)", i, p)
		}
	}
	checkCopy := func(w, guest, src string) {
		if !path.IsAbs(guest) || strings.ContainsAny(guest, "\n\x00") || hasDotDot(guest) {
			add("%s: the guest path must be absolute, without '..'", w)
		}
		if path.Clean(guest) == "/" {
			add("%s: the root directory cannot be replaced", w)
		}
		if src == "" || strings.ContainsAny(src, "\n\x00") {
			add("%s: the source must be a path in the build context", w)
		} else if path.IsAbs(src) || hasDotDot(src) {
			add("%s: %q: the source must be relative to the build context, without '..' (as COPY takes it)", w, src)
		}
	}
	// copies are a map's entries in guest path order.
	copies := func(where string, m map[string]string) {
		for _, guest := range sortedKeys(m) {
			w := fmt.Sprintf("%s[%s]", where, guest)
			checkCopy(w, guest, m[guest])
			s.Ops = append(s.Ops, Op{Guest: guest, Source: m[guest], Where: w})
		}
	}
	if s.Steps != nil && (s.Files != nil || s.Run != nil) {
		add("steps: replaces files: and run: — put the copies and commands in steps:, in the order they run")
	}
	s.Ops = nil
	if s.Steps == nil {
		copies("files", s.Files)
		for i, c := range s.Run {
			w := fmt.Sprintf("run[%d]", i)
			if strings.TrimSpace(c) == "" {
				add("%s: empty", w)
			}
			s.Ops = append(s.Ops, Op{Run: c, Where: w})
		}
	}
	for i, st := range s.Steps {
		w := fmt.Sprintf("steps[%d]", i)
		switch {
		case st.Copy != nil && st.Run != "":
			add("%s: either copy: or run:, not both (make them two steps)", w)
		case st.Copy != nil && len(st.Copy) == 0:
			add("%s.copy: empty", w)
		case st.Copy != nil:
			copies(w+".copy", st.Copy)
		case strings.TrimSpace(st.Run) == "":
			add("%s: needs copy: {GUEST_PATH: SOURCE} or run: COMMAND", w)
		default:
			s.Ops = append(s.Ops, Op{Run: st.Run, Where: w + ".run"})
		}
	}
	if err := checkCommand(s.Command); err != nil {
		add("command: %v", err)
	}
	if h := s.Health; h != nil {
		if h.Command == "" {
			add("health.command: required")
		} else if err := checkCommand(h.Command); err != nil {
			add("health.command: %v", err)
		}
		var every, timeout time.Duration
		for _, d := range []struct {
			name string
			v    string
			dst  *time.Duration
		}{{"every", h.Every, &every}, {"timeout", h.Timeout, &timeout}} {
			if d.v == "" {
				continue
			}
			v, err := time.ParseDuration(d.v)
			if err != nil || v <= 0 {
				add("health.%s: %q: a positive duration such as 10s", d.name, d.v)
			}
			*d.dst = v
		}
		if every > 0 && timeout >= every {
			add("health: timeout must be shorter than every")
		}
		if h.Failures < 0 {
			add("health.failures: must be > 0")
		}
	}
	if s.VCPUs == 0 {
		s.VCPUs = DefaultVCPUs
	}
	if s.VCPUs < 1 || s.VCPUs > 32 {
		add("vcpus: 1 to 32")
	}
	if s.MemMB == 0 {
		s.MemMB = DefaultMemMB
	}
	if s.MemMB < 32 {
		add("mem_mb: at least 32")
	}
	if s.DiskMB < 0 {
		add("disk_mb: must be >= 0")
	}
	if s.SizeMB < 0 || (s.SizeMB > 0 && s.SizeMB < 32) {
		add("size_mb: 0 (sized to the content) or at least 32")
	}
	return errors.Join(errs...)
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func checkCommand(c string) error {
	if len(c) > maxCommand {
		return fmt.Errorf("longer than %d characters", maxCommand)
	}
	if strings.ContainsAny(c, "\n\r\x00") {
		return errors.New("must be a single line (use && or ; to chain)")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
