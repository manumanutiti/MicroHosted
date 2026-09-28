package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const valid = `
version: 1
budget: { max_vms: 10, max_mem_mb: 2048, workers: 2 }
networks:
  web:
    subnet: 172.30.1.0/24
    allowed_egress:
      - { ip: 10.0.0.5, protocol: tcp, port: 443 }
      - { ip: 10.0.0.9, protocol: icmp }
    allowed_ingress:
      - { iface: wlan0, src_ip: 192.168.1.0/24, protocol: tcp, port: 8080, to_ip: 172.30.1.10 }
functions:
  server:
    image: alpine:1.0@` + digest + `
    network: web
    ip: 172.30.1.10
    command: httpd -f -p 8080
    health: { command: "wget -qO- localhost:8080" }
    lifecycle: { mode: persistent, recycle: 6h }
  check:
    image: alpine:1.0@` + digest + `
    network: none
    command: whoami
    lifecycle: { mode: transaction, every: 30s, timeout: 10s }
  sampler:
    image: alpine:1.0@` + digest + `
    network: web
    command: "while true; do date; sleep 1; done"
    lifecycle: { mode: window, every: 1m, duration: 20s }
`

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.FunctionOrder, ","); got != "server,check,sampler" {
		t.Errorf("order %s, want the file's", got)
	}
	h := s.Functions["server"].Health
	if h.Every.D() != 10*time.Second || h.Timeout.D() != 2*time.Second || h.Failures != 3 {
		t.Errorf("health defaults: %+v", h)
	}
	if r := s.Functions["server"].Lifecycle.Recycle.D(); r != 6*time.Hour {
		t.Errorf("recycle %s", r)
	}
}

// Every mistake is refused, and named — none degrades into a default.
func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"unknown field":        {"    ip: 172.30.1.10\n", "    ip: 172.30.1.10\n    replicas: 2\n", "replicas"},
		"typo in a rule":       {"protocol: icmp }", "protocl: icmp }", "protocl"},
		"no digest":            {"alpine:1.0@" + digest + "\n    network: none", "alpine:1.0\n    network: none", "digest is mandatory"},
		"unknown network":      {"network: none", "network: nope", `"nope" is not declared`},
		"ip outside subnet":    {"    ip: 172.30.1.10", "    ip: 172.30.2.10", "not a usable guest address"},
		"ip is the gateway":    {"    ip: 172.30.1.10", "    ip: 172.30.1.1\n", "not a usable guest address"},
		"ingress to nobody":    {"to_ip: 172.30.1.10", "to_ip: 172.30.1.11", "no function on web declares ip: 172.30.1.11"},
		"timeout over every":   {"every: 30s, timeout: 10s", "every: 30s, timeout: 30s", "timeout must be shorter"},
		"window over every":    {"duration: 20s", "duration: 2m", "duration must be shorter"},
		"no mode":              {"mode: transaction, ", "", "lifecycle.mode: required"},
		"bad mode":             {"mode: transaction", "mode: cron", `"cron"`},
		"no command":           {"    command: whoami\n", "", "command: required for transaction"},
		"multi-line command":   {"command: whoami", "command: \"who\\nami\"", "single line"},
		"reserved label":       {"network: none\n", "network: none\n    labels: { function: x }\n", "set by the orchestrator"},
		"health on a cycle":    {"command: whoami\n", "command: whoami\n    health: { command: true }\n", "only persistent"},
		"bad duration":         {"every: 30s", "every: 30 seconds", "want e.g. 30s"},
		"recycle too short":    {"recycle: 6h", "recycle: 10s", "at least 1m"},
		"duplicate key":        {"  check:\n", "  server:\n", "already"},
		"version":              {"version: 1", "version: 2", "version: must be 1"},
		"egress without iface": {"    subnet: 172.30.1.0/24\n", "    subnet: 172.30.1.0/24\n    egress: true\n", "needs egress_iface"},
		"no budget":            {"budget: { max_vms: 10, max_mem_mb: 2048, workers: 2 }", "", "max_vms: required"},
		"uppercase name":       {"  check:", "  Check:", "DNS label"},
		"subnet not a network": {"172.30.1.0/24", "172.30.1.5/24", "subnet"},
		"second document":      {"version: 1", "version: 1\n---\nversion: 1", "more than one YAML document"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(valid, tc.from) {
				t.Fatalf("test broken: %q not in the valid spec", tc.from)
			}
			_, err := Parse([]byte(strings.Replace(valid, tc.from, tc.to, 1)))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The examples shipped with the orchestrator stay valid.
func TestExamplesValid(t *testing.T) {
	files, _ := filepath.Glob("../examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, f := range files {
		if _, err := Load(f); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// Files and secrets are read at load, relative to the spec; a missing file, a
// secret others can read or a bad guest path fails the whole spec.
func TestLoadFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "app.conf"), []byte("port=8080\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "token"), []byte("s3cret"), 0o600)
	os.WriteFile(filepath.Join(dir, "open-token"), []byte("s3cret"), 0o644)
	write := func(files string) string {
		y := strings.Replace(valid, "    command: whoami\n", "    command: whoami\n"+files, 1)
		p := filepath.Join(dir, "plant.yaml")
		os.WriteFile(p, []byte(y), 0o644)
		return p
	}

	s, err := Load(write("    files: { /etc/app.conf: { from: conf/app.conf, mode: \"0640\" } }\n    secrets: { /etc/token: { from: token } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := s.Functions["check"]
	if string(f.Files["/etc/app.conf"].Content) != "port=8080\n" || string(f.Secrets["/etc/token"].Content) != "s3cret" {
		t.Errorf("contents not read: %+v %+v", f.Files, f.Secrets)
	}

	for name, tc := range map[string]struct{ files, want string }{
		"missing":          {"    files: { /etc/x: { from: nope } }\n", "no such file"},
		"open secret":      {"    secrets: { /etc/t: { from: open-token } }\n", "readable by group or others"},
		"relative guest":   {"    files: { etc/x: { from: token } }\n", "must be absolute"},
		"dotdot guest":     {"    files: { /etc/../x: { from: token } }\n", "must be absolute"},
		"setuid":           {"    files: { /etc/x: { from: token, mode: \"4755\" } }\n", "no setuid"},
		"no from":          {"    files: { /etc/x: { mode: \"0644\" } }\n", "from: required"},
		"file and secret":  {"    files: { /etc/x: { from: token } }\n    secrets: { /etc/x: { from: token } }\n", "also declared"},
		"directory source": {"    files: { /etc/x: { from: conf } }\n", "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(tc.files)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
