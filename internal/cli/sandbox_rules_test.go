package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeRules(t *testing.T, text string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "rules.yml")
	must(t, os.WriteFile(f, []byte(text), 0o600))
	return f
}

func TestSandboxRulesLoad(t *testing.T) {
	f := writeRules(t, `
detect:
  - name: brave
    severity: high
    means: Brave's saved passwords
    path: ~/.config/BraveSoftware/
  - name: solana
    command: 'solana .*transfer'
  - name: c2
    connect: 203.0.113.0/24:4444
accept:
  - kind: dropped_exec
    what: '^~/work/node_modules/'
    by: node
    why: esbuild's own binary
  - kind: rule:solana
    what: balance
    why: reading a balance is fine
`)
	rs, err := loadSandboxRules([]string{f})
	must(t, err)
	if len(rs.Detect) != 3 || len(rs.Accept) != 2 || rs.Files[0] != f {
		t.Fatalf("%+v", rs)
	}
	if rs.Detect[0].sev != sevHigh || rs.Detect[1].sev != sevWarn {
		t.Errorf("severities %v %v", rs.Detect[0].sev, rs.Detect[1].sev)
	}
	want := "path\tbrave\t^~/[.]config/BraveSoftware(/|$)\ncommand\tsolana\tsolana .*transfer\n"
	if g := rs.guest(); g != want {
		t.Errorf("guest rules:\n%q\nwant\n%q", g, want)
	}
	if k := rs.kindOf("rule:brave"); k.Severity != sevHigh || k.Title != "your rule brave" || k.Means != "Brave's saved passwords" {
		t.Errorf("kindOf %+v", k)
	}
	if k := rs.kindOf("rule:solana"); k.Means != "command /solana .*transfer/" {
		t.Errorf("kindOf %+v", k)
	}

	// an empty file is no rules; none at all is too
	rs, err = loadSandboxRules([]string{writeRules(t, "")})
	if err != nil || len(rs.Detect)+len(rs.Accept) != 0 {
		t.Errorf("empty: %+v %v", rs, err)
	}
	var none *sandboxRules
	if none.guest() != "" || none.kindOf("dropper").Severity != sevHigh {
		t.Error("nil rules")
	}
}

// Only the files given are read — none is no rules, whatever is in the
// user's config — in order, a name once across them.
func TestSandboxRulesOnlyGiven(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	must(t, os.MkdirAll(filepath.Join(cfg, "mh"), 0o700))
	must(t, os.WriteFile(filepath.Join(cfg, "mh", "sandbox-rules.yml"), []byte("accept:\n  - kind: decoy\n    by: cat\n    why: planted\n"), 0o600))
	rs, err := loadSandboxRules(nil)
	if err != nil || len(rs.Files)+len(rs.Detect)+len(rs.Accept) != 0 {
		t.Errorf("no --rules: %+v %v", rs, err)
	}
	a := writeRules(t, "detect:\n  - name: a\n    path: /opt/a\n")
	b := writeRules(t, "detect:\n  - name: b\n    path: /opt/b\n")
	rs, err = loadSandboxRules([]string{a, b})
	must(t, err)
	if !slices.Equal(rs.Files, []string{a, b}) || len(rs.Detect) != 2 || rs.Detect[0].Name != "a" {
		t.Errorf("%+v", rs)
	}
	if _, err := loadSandboxRules([]string{a, writeRules(t, "detect:\n  - name: a\n    path: /x\n")}); err == nil || !strings.Contains(err.Error(), "already a rule") {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := loadSandboxRules([]string{filepath.Join(cfg, "missing.yml")}); err == nil {
		t.Error("a missing file was no rules")
	}
}

func TestSandboxRulesRefused(t *testing.T) {
	for _, c := range []struct{ rules, err string }{
		{"detect:\n  - name: Bad Name\n    path: /x\n", "name"},
		{"detect:\n  - name: a\n", "a path, a command or a connect"},
		{"detect:\n  - name: a\n    severity: critical\n    path: /x\n", "severity"},
		{"detect:\n  - name: a\n    path: relative/x\n", "absolute or ~/"},
		{"detect:\n  - name: a\n    path: '/x\\y'\n", "absolute or ~/"},
		{"detect:\n  - name: a\n    command: '\\d+'\n", "POSIX"},
		{"detect:\n  - name: a\n    command: '(unclosed'\n", "POSIX"},
		{"detect:\n  - name: a\n    connect: 1.2.3.4:99999\n", "port"},
		{"detect:\n  - name: a\n    connect: example.com\n", "an address"},
		{"detect:\n  - name: a\n    pth: /x\n", "field pth not found"},
		{"accept:\n  - kind: dropper\n    why: x\n", "a what or a by"},
		{"accept:\n  - kind: dropper\n    by: x\n", "why"},
		{"accept:\n  - kind: droper\n    by: x\n    why: x\n", "an alert's kind"},
		{"accept:\n  - kind: rule:nope\n    by: x\n    why: x\n", "no detect rule"},
		{"accept:\n  - kind: changed\n    what: '(?<x>'\n    why: x\n", "what"},
	} {
		_, err := loadSandboxRules([]string{writeRules(t, c.rules)})
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%q: %v, want %q", c.rules, err, c.err)
		}
	}
}

func TestParseConnRule(t *testing.T) {
	for _, c := range []struct {
		rule string
		dst  string
		port int
		ok   bool
	}{
		{"1.2.3.4", "1.2.3.4", 80, true},
		{"1.2.3.4", "1.2.3.5", 80, false},
		{"10.0.0.0/8", "10.9.9.9", 443, true},
		{":4444", "8.8.8.8", 4444, true},
		{":4444", "8.8.8.8", 443, false},
		{"1.2.3.4:53", "1.2.3.4", 53, true},
		{"1.2.3.4:53", "1.2.3.4", 54, false},
		{"10.0.0.0/8:22", "10.1.1.1", 22, true},
		{"[2001:db8::1]:443", "2001:db8::1", 443, true},
		{"2001:db8::/32", "[2001:db8::7]", 80, true},
		{"2001:db8::/32", "1.2.3.4", 80, false},
	} {
		cr, err := parseConnRule(c.rule)
		must(t, err)
		if got := cr.match(c.dst, c.port); got != c.ok {
			t.Errorf("%s ~ %s:%d = %v", c.rule, c.dst, c.port, got)
		}
	}
}

// What accept rules match leaves its list for Accepted, graded info, with
// its why; what connect rules match is an alert of the rule's severity.
func TestSandboxApplyRules(t *testing.T) {
	rs, err := loadSandboxRules([]string{writeRules(t, `
detect:
  - name: c2
    severity: high
    connect: :4444
  - name: brave
    path: ~/.config/BraveSoftware/
accept:
  - kind: changed
    what: '^~/\.local/lib/python'
    why: pip install --user
  - kind: vm_probe
    what: ^/sys/firmware/dmi/
    by: lscpu
    why: our CI prints lscpu
  - kind: decoy
    what: ^~/\.netrc$
    by: python3
    why: requests reads it
  - kind: connection
    what: ^1\.1\.1\.1:53$
    why: name lookups
`)})
	must(t, err)
	r := parseSandboxReport(strings.Join([]string{
		"user\tdev",
		"rules\t1",
		"decoy\tREAD\t/home/dev/.netrc\tcurl reads it\tcurl",
		"decoyby\topen\t1\t/home/dev/.netrc\tpython3",
		"decoy\tREAD\t/home/dev/.aws/credentials\tthe AWS CLI reads it\taws",
		"decoyby\topen\t1\t/home/dev/.aws/credentials\tpython3",
		"probe\tfound\t1\t/sys/firmware/dmi/tables/DMI\tlscpu",
		"probe\tfound\t1\t/sys/class/dmi/id/product_name\tcat",
		"alert\tconnect\t4\t1.1.1.1:53 (dns)\tpython3",
		"alert\tconnect\t1\t10.0.0.1:4444\tbash",
		"alert\trule:brave\t2\t/home/dev/.config/BraveSoftware/Default/Login Data\tpython3",
		"file\t/home/dev/.local/lib/python3.12/site-packages/x.py",
		"file\t/home/dev/.bashrc",
	}, "\n"))
	r.Complete = true
	r.Connections = []sandboxConn{{Protocol: "udp", Dst: "1.1.1.1", DstPort: 53, Count: 4, Reason: "egress"}, {Protocol: "tcp", Dst: "10.0.0.1", DstPort: 4444, Count: 1, Reason: "egress"}}
	r.applyRules(rs)
	r.summarize()

	var kinds []string
	for _, a := range r.Accepted {
		kinds = append(kinds, a.Kind+" "+a.What)
	}
	want := []string{"connection 1.1.1.1:53", "vm_probe /sys/firmware/dmi/tables/DMI", "changed ~/.local/lib/python3.12/site-packages/x.py", "decoy READ ~/.netrc"}
	if !slices.Equal(kinds, want) {
		t.Errorf("accepted:\n%q\nwant\n%q", kinds, want)
	}
	if len(r.Connections) != 1 || len(r.VMProbes) != 1 || len(r.Changed.Files) != 1 || len(r.Decoys) != 2 {
		t.Errorf("left: %+v %+v %+v", r.Connections, r.VMProbes, r.Changed.Files)
	}
	if r.Decoys[0].Severity != "info" || r.Decoys[0].Accepted != "requests reads it" || r.Decoys[1].Severity != "high" {
		t.Errorf("decoys %+v", r.Decoys)
	}
	sev := map[string]string{}
	for _, a := range r.Alerts {
		sev[a.Kind+" "+a.What] = a.Severity + " by " + a.By
	}
	if sev["rule:c2 10.0.0.1:4444"] != "high by bash" || sev["rule:brave /home/dev/.config/BraveSoftware/Default/Login Data"] != "warn by python3" {
		t.Errorf("alerts %v", sev)
	}
	if _, ok := sev["connect 1.1.1.1:53 (dns)"]; ok {
		t.Error("an accepted connection's connect alert stayed")
	}

	out := renderReport(r, false, false)
	for _, w := range []string{"and matched your rule c2", "your rule c2", "your rule brave", "accepted by your rules (4)", "rules: "} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
	out = renderReport(r, true, false)
	for _, w := range []string{"why: requests reads it", "by lscpu; why: our CI prints lscpu"} {
		if !strings.Contains(out, w) {
			t.Errorf("-v: no %q in:\n%s", w, out)
		}
	}
}

// classify (sandbox/sbin/mh-sandbox-lib), run on audit records as auditd
// writes them, with every awk the images may have.
func TestGuestClassify(t *testing.T) {
	sys := func(serial int, call, comm, exe string, ok bool, pid int) string {
		succ := "yes"
		if !ok {
			succ = "no"
		}
		return fmt.Sprintf(`type=SYSCALL msg=audit(1.000:%d): arch=c000003e syscall=0 success=%s exit=0 a0=ffffff9c a1=0 a2=0 a3=0 items=1 ppid=1 pid=%d auid=4294967295 uid=1000 comm="%s" exe="%s" key="mh-sandbox"`+"\035"+`ARCH=x86_64 SYSCALL=%s UID="dev"`, serial, succ, pid, comm, exe, call)
	}
	path := func(serial int, item int, name, nt string) string {
		return fmt.Sprintf(`type=PATH msg=audit(1.000:%d): item=%d name="%s" inode=1 dev=fe:00 mode=0100644 ouid=0 ogid=0 rdev=00:00 nametype=%s cap_fp=0`, serial, item, name, nt)
	}
	cwd := func(serial int) string {
		return fmt.Sprintf(`type=CWD msg=audit(1.000:%d): cwd="/home/dev/work"`, serial)
	}
	execve := func(serial, pid int, exe string, args ...string) []string {
		ev := fmt.Sprintf(`type=EXECVE msg=audit(1.000:%d): argc=%d`, serial, len(args))
		for i, a := range args {
			ev += fmt.Sprintf(` a%d="%s"`, i, a)
		}
		return []string{sys(serial, "execve", filepath.Base(exe), exe, true, pid), ev, cwd(serial), path(serial, 0, args[0], "NORMAL")}
	}
	var log []string
	add := func(l ...string) { log = append(log, l...) }
	// perl's getpwuid: /etc/passwd, then /etc/shadow — not a probe
	add(sys(10, "openat", "perl", "/usr/bin/perl", true, 100), cwd(10), path(10, 0, "/etc/passwd", "NORMAL"))
	add(sys(11, "openat", "perl", "/usr/bin/perl", false, 100), cwd(11), path(11, 0, "/etc/shadow", "NORMAL"))
	// perl opening /etc/shadow itself, and cat after /etc/passwd: probes
	add(sys(12, "openat", "perl", "/usr/bin/perl", false, 101), cwd(12), path(12, 0, "/etc/shadow", "NORMAL"))
	add(sys(13, "openat", "cat", "/usr/bin/cat", true, 102), cwd(13), path(13, 0, "/etc/passwd", "NORMAL"))
	add(sys(14, "openat", "cat", "/usr/bin/cat", false, 102), cwd(14), path(14, 0, "/etc/shadow", "NORMAL"))
	// a script written in /tmp and run: dropped_script; a binary: dropper
	add(sys(20, "openat", "python3", "/usr/bin/python3.12", true, 103), cwd(20), path(20, 0, "/tmp/t/helper.sh", "CREATE"))
	add(sys(21, "execve", "helper.sh", "/usr/bin/dash", true, 104), `type=EXECVE msg=audit(1.000:21): argc=1 a0="/tmp/t/helper.sh"`, cwd(21), path(21, 0, "/tmp/t/helper.sh", "NORMAL"))
	add(sys(22, "openat", "cp", "/usr/bin/cp", true, 105), cwd(22), path(22, 0, "/tmp/.x", "CREATE"))
	add(sys(23, "execve", ".x", "/tmp/.x", true, 106), `type=EXECVE msg=audit(1.000:23): argc=1 a0="/tmp/.x"`, cwd(23), path(23, 0, "/tmp/.x", "NORMAL"))
	// the user's rules: a path opened (a stat alone is not), a command
	add(sys(30, "openat", "python3", "/usr/bin/python3.12", true, 107), cwd(30), path(30, 0, "/home/dev/.config/BraveSoftware/Default/Login Data", "NORMAL"))
	add(sys(31, "newfstatat", "ls", "/usr/bin/ls", true, 108), cwd(31), path(31, 0, "/home/dev/.config/BraveSoftware", "NORMAL"))
	add(sys(32, "openat", "cat", "/usr/bin/cat", true, 109), cwd(32), path(32, 0, "/opt/wallet.dat", "NORMAL"))
	add(execve(40, 110, "/usr/bin/solana", "solana", "transfer", "ADDR", "1")...)
	add(execve(41, 111, "/usr/bin/solana", "solana", "balance")...)

	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "decoys"), nil, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules"), []byte(
		"path\tbrave\t^~/[.]config/BraveSoftware(/|$)\n"+
			"path\twallet\t^/opt/wallet[.]dat$\n"+
			"command\tsolana\tsolana .*transfer\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "audit.log"), []byte(strings.Join(log, "\n")+"\n"), 0o600))
	lib, err := filepath.Abs("../../sandbox/sbin/mh-sandbox-lib")
	must(t, err)

	want := []string{
		"privesc\tfound\t1\t/etc/shadow\tperl\t1.000\t1.000",
		"privesc\tfound\t1\t/etc/shadow\tcat\t1.000\t1.000",
		"alert\tdropped_script\t1\t/tmp/t/helper.sh (written by python3.12)\thelper.sh\t1.000\t1.000",
		"alert\tdropper\t1\t/tmp/.x (written by cp)\t.x\t1.000\t1.000",
		"alert\trule:brave\t1\t/home/dev/.config/BraveSoftware/Default/Login Data\tpython3\t1.000\t1.000",
		"alert\trule:wallet\t1\t/opt/wallet.dat\tcat\t1.000\t1.000",
		"alert\trule:solana\t1\tsolana transfer ADDR 1\tsolana\t1.000\t1.000",
	}
	for _, awk := range []string{"mawk", "gawk", "busybox"} {
		bin, err := exec.LookPath(awk)
		if err != nil {
			t.Logf("no %s here", awk)
			continue
		}
		path := t.TempDir()
		if awk == "busybox" {
			must(t, os.WriteFile(filepath.Join(path, "awk"), []byte("#!/bin/sh\nexec "+bin+" awk \"$@\"\n"), 0o755))
		} else {
			must(t, os.Symlink(bin, filepath.Join(path, "awk")))
		}
		cmd := exec.Command("/bin/sh", "-c", `. "$1"; S=$2; H=/home/dev; classify < "$2/audit.log"`, "sh", lib, dir)
		cmd.Env = append(os.Environ(), "PATH="+path+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", awk, err, out)
		}
		var got []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if !strings.HasPrefix(l, "exec\t") {
				got = append(got, l)
			}
		}
		for _, w := range want {
			if n := strings.Count(strings.Join(got, "\n")+"\n", w+"\n"); n != 1 {
				t.Errorf("%s: %q %d times in:\n%s", awk, w, n, strings.Join(got, "\n"))
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d lines, want %d:\n%s", awk, len(got), len(want), strings.Join(got, "\n"))
		}
	}
}

// classify says when each thing was first and last seen (audit's time) and
// which process first ran a command, and its parent.
func TestGuestClassifyTimes(t *testing.T) {
	ev := func(time string, serial, pid, ppid int, call, comm string, rest ...string) []string {
		h := fmt.Sprintf("msg=audit(%s:%d):", time, serial)
		l := []string{fmt.Sprintf(`type=SYSCALL %s arch=c000003e syscall=0 success=yes exit=0 a0=ffffff9c a1=0 a2=0 a3=0 items=1 ppid=%d pid=%d auid=4294967295 uid=1000 comm="%s" exe="/usr/bin/%s" key="mh-sandbox"`+"\035"+`ARCH=x86_64 SYSCALL=%s UID="dev"`, h, ppid, pid, comm, comm, call)}
		// as auditd writes them: EXECVE, then CWD, then PATH
		for _, r := range rest {
			if strings.HasPrefix(r, "type=PATH") {
				l = append(l, `type=CWD `+h+` cwd="/home/dev/work"`)
			}
			l = append(l, strings.Replace(r, "{H}", h, 1))
		}
		if !strings.HasPrefix(rest[len(rest)-1], "type=PATH") {
			l = append(l, `type=CWD `+h+` cwd="/home/dev/work"`)
		}
		return l
	}
	var log []string
	log = append(log, ev("1760000000.100", 10, 101, 100, "execve", "npm", `type=EXECVE {H} argc=2 a0="npm" a1="whoami"`)...)
	log = append(log, ev("1760000000.350", 11, 104, 101, "openat", "cat", `type=PATH {H} item=0 name="/home/dev/.npmrc" nametype=NORMAL`)...)
	log = append(log, ev("1760000001.500", 12, 102, 101, "execve", "npm", `type=EXECVE {H} argc=2 a0="npm" a1="whoami"`)...)
	log = append(log, ev("1760000002.000", 13, 103, 101, "execve", "crontab", `type=EXECVE {H} argc=2 a0="crontab" a1="x"`)...)
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "decoys"), []byte("/home/dev/.npmrc\tnpm reads it\tnpm\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules"), nil, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "audit.log"), []byte(strings.Join(log, "\n")+"\n"), 0o600))
	lib, err := filepath.Abs("../../sandbox/sbin/mh-sandbox-lib")
	must(t, err)
	out, err := exec.Command("/bin/sh", "-c", `. "$1"; S=$2; H=/home/dev; classify < "$2/audit.log"`, "sh", lib, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, w := range []string{
		"exec\t10\t2\tnpm whoami\t1760000000.100\t1760000001.500\t101\t100",
		"exec\t13\t1\tcrontab x\t1760000002.000\t1760000002.000\t103\t101",
		"decoyby\topen\t1\t/home/dev/.npmrc\tcat\t1760000000.350\t1760000000.350",
		"alert\tpersistence\t1\tcrontab x\tnpm\t1760000002.000\t1760000002.000",
	} {
		if !strings.Contains(string(out)+"\n", w+"\n") {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
}

// The examples in sandbox/rules/ are rules this CLI takes.
func TestSandboxRulesExamples(t *testing.T) {
	files, err := filepath.Glob("../../sandbox/rules/*.yml")
	must(t, err)
	if len(files) == 0 {
		t.Fatal("no example in sandbox/rules/")
	}
	rs, err := loadSandboxRules(files)
	must(t, err)
	if len(rs.Detect) == 0 || len(rs.Accept) == 0 || rs.guest() == "" {
		t.Errorf("examples: %d detect, %d accept", len(rs.Detect), len(rs.Accept))
	}
}
