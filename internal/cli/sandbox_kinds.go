package cli

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// The kinds of alert the image's tools report (mh-sandbox-report --tsv,
// mh-sandbox-watch: "alert KIND COUNT WHAT BY"). The guest names the kind;
// this table is what the CLI knows about it: how serious, under which
// heading, in one line for a person. A kind not in it is still shown, under
// its own name: a newer image may know more than this CLI.

type severity int

const (
	sevInfo severity = iota // worth knowing: ordinary code does it too
	sevWarn                 // unusual for ordinary code
	sevHigh                 // what malicious code does
)

func (s severity) String() string {
	switch s {
	case sevHigh:
		return "high"
	case sevWarn:
		return "warn"
	}
	return "info"
}

type alertKind struct {
	Severity severity
	Title    string // the heading it is shown under
	Means    string // one line: why it matters
}

// alertKinds: how each is found is in classify (sandbox/sbin/mh-sandbox-lib);
// docs/sandbox.md lists them for a person.
var alertKinds = map[string]alertKind{
	// From audit's syscall rules (auditrules, mh-sandbox-lib).
	"privesc_attempt": {sevHigh, "tried to become root (or the kernel)",
		"a syscall that changes who the process is (setuid(0)…) refused, or one ordinary programs never make as a user: mount, chroot, bpf, a kernel module, keyctl, userfaultfd, perf_event_open, ASLR turned off"},
	"namespace": {sevWarn, "entered a new namespace",
		"unshare or setns: container tools and browsers' sandboxes (Chromium, Playwright) do it; so do container escapes and kernel exploits, for the privileges a user namespace gives"},
	"ptrace": {sevWarn, "traced a process",
		"ptrace: debuggers and strace do it; malware does it to read or inject into another process, or to detect a debugger"},
	"persistence": {sevHigh, "made itself start again",
		"wrote (or tried to) where code is run later without being asked: ~/.ssh/authorized_keys, a systemd user unit, autostart, cron, /etc's startup files"},
	"shell_rc": {sevWarn, "changed the shell's startup files",
		"~/.bashrc, ~/.profile…: run by every new shell. Installers (nvm, rustup) add a line too; malware hides a command there"},
	// From the startup files themselves (rclines, mh-sandbox-lib): what
	// was added to them, which every new shell will run.
	"shell_rc_hostile": {sevHigh, "left a harmful command for every new shell",
		"a line added to ~/.bashrc, ~/.zshrc… that shuts down or wipes the machine, runs a download or decoded text, joins a shell to a connection, preloads a library, or replaces sudo, su or ssh"},
	"shell_rc_line": {sevInfo, "added a line to the shell's startup files",
		"what every new shell will now run: installers add their PATH (nvm, rustup, cargo)"},
	"system_write": {sevWarn, "tried to write a system file",
		"a write in /etc, as the sandbox's user (refused unless the file is writable by anyone)"},
	"io_uring": {sevWarn, "used io_uring",
		"files opened and read through io_uring are not recorded: what it did through it is missing here. Node turns it off here; little else uses it"},
	"io_uring_epoll": {sevInfo, "set up Node's io_uring ring",
		"libuv's ring of 256 for epoll_ctl, which Node sets up with io_uring off for files: what a native module did through it would not be recorded"},
	"listen": {sevWarn, "opened a port to the network",
		"bind on an address others reach (0.0.0.0, ::), not loopback: a server for whoever finds it. Dev servers do it; so do backdoors and exfiltration over HTTP"},
	"connect": {sevInfo, "tried to connect",
		"which program tried to reach what, seen inside the VM (connections lists what the network refused); :53 (dns) is a name lookup"},
	// From the command lines run (execve).
	"reverse_shell": {sevHigh, "reverse shell",
		"a shell joined to a network connection (bash -i >& /dev/tcp/…, nc -e, socat exec:, a Python socket dup2'd onto a shell): someone else's shell on this machine"},
	"dev_tcp": {sevWarn, "connected through bash's /dev/tcp",
		"a connection without a network tool: connectivity checks do it, and hand-made requests that avoid curl"},
	"pipe_to_shell": {sevWarn, "ran what it downloaded",
		"curl … | sh: installers (rustup, nvm) do it; so does malware's first stage"},
	// pipe_to_shell, regraded by the CLI (hookAlerts): the process tree
	// says who ran it
	"hook_pipe_to_shell": {sevHigh, "an install script ran what it downloaded",
		"curl … | sh run by a package's install script (npm's preinstall, install, postinstall; pip's setup.py, a build backend), not by the command given: a dependency fetching and running code of its own on install, as Shai-Hulud 2.0 brought in Bun"},
	"obfuscated_exec": {sevHigh, "ran hidden code",
		"decoded and ran at once (base64 -d | sh, exec(b64decode(…))): code that hides what it runs from whoever reads it"},
	"dropper": {sevHigh, "ran a binary it dropped in /tmp",
		"a program written (not compiled) after the sandbox was prepared, in /tmp, /var/tmp or /dev/shm, then run: a downloaded payload"},
	"dropped_exec": {sevWarn, "ran a program it wrote",
		"a binary written (not by a compiler or linker) after the sandbox was prepared, then run: unpacked releases do it; so do payloads"},
	"dropped_script": {sevInfo, "ran a script it wrote in /tmp",
		"test suites (pytest's tmp_path) and git hooks do it; what the script ran is recorded, command by command"},
	"preload": {sevWarn, "loaded its own library into a system program",
		"a library from a temporary directory or a home, loaded before the loader's cache (LD_PRELOAD, LD_LIBRARY_PATH) into a program of /usr/bin: its functions replace libc's in it"},
	"masquerade": {sevWarn, "ran a program dressed as a document",
		"a file it wrote, named as a document, a picture or an archive (report.pdf.sh, image.png), then run or handed to an interpreter: made for a person to open without a second look"},
	// From the files left, read at the end (mh-sandbox-report).
	"pth_hook": {sevHigh, "hooked every start of Python",
		"a .pth file in a site-packages whose import line runs a shell, a subprocess, a connection or decoded code: Python runs it at every start, of every program"},
	"antiforensics": {sevHigh, "covered its tracks",
		"history turned off or cleared, a file's times reset outside ~/work (touch -d/-r), shred, logs or the program it dropped removed"},
	"miner": {sevHigh, "cryptocurrency miner",
		"a miner by name or by its arguments (xmrig, stratum+tcp://, --donate-level, a mining pool)"},
	"credential_search": {sevHigh, "searched for credentials",
		"grep, find or locate for passwords, keys, tokens (id_rsa, .pem, AKIA…) outside ~/work: across home, /etc or the whole disk"},
	// From the shells' trace (traced, mh-sandbox-lib): set -x of every
	// shell the code started.
	"trace_off": {sevInfo, "turned off its shell's trace",
		"set +x in a script: CI scripts do it to keep secrets out of logs; what that shell did after is only in audit (programs run, not builtins)"},
	"trace_full": {sevWarn, "flooded the shell trace",
		"the trace of its shells reached its cap: what scripts did after (builtins, eval) is only in audit"},
	// The kernel's, at the end.
	"oom_kill": {sevWarn, "ran the VM out of memory",
		"the kernel killed its processes out of memory: a fork bomb (a .pth that starts Python, which reads the .pth…) or a build bigger than the VM (--mem). What the killed ones would have done next is not here"},
	// Audit's own state, at the end.
	"audit_health": {sevWarn, "audit may have missed some",
		"the disk filled up (audit stops recording), auditd stopped, or the kernel held the code back for audit to keep up: what is missing is unknown"},
}

// kindOf is what the CLI knows of kind, or a generic entry for one it does
// not: shown, never dropped.
func kindOf(kind string) alertKind {
	if k, ok := alertKinds[kind]; ok {
		return k
	}
	return alertKind{Severity: sevWarn, Title: kind, Means: "reported by the image (unknown to this mh: update it)"}
}

// hookAlerts regrades each pipe_to_shell alert whose command ran under a
// package manager installing (npm install, pip install…: its install
// scripts, a build) as hook_pipe_to_shell. A person types curl … | sh; a
// dependency doing it on install fetches code nobody chose. The tree is the
// commands' pid and ppid (an image before them: none, nothing regraded),
// their first run's: a command line run by the command given first and by
// an install script after is the first's. Up from the alert's process,
// never its own: sh -c "npm i x; curl … | sh" is the command given.
func (r *sandboxReport) hookAlerts() {
	args := map[int][]string{} // pid: the command lines it ran
	parent := map[int]int{}
	for _, c := range r.Commands {
		if c.PID == 0 {
			continue
		}
		args[c.PID] = append(args[c.PID], c.Args)
		if _, ok := parent[c.PID]; !ok {
			parent[c.PID] = c.PPID
		}
	}
	if len(args) == 0 {
		return
	}
	// a package manager installing above pid
	underInstall := func(pid int) bool {
		seen := map[int]bool{pid: true}
		for p := parent[pid]; p > 1 && !seen[p]; p = parent[p] {
			seen[p] = true
			if slices.ContainsFunc(args[p], installing) {
				return true
			}
		}
		return false
	}
	for i := range r.Alerts {
		a := &r.Alerts[i]
		if a.Kind != "pipe_to_shell" {
			continue
		}
		// WHAT is the command line, cut at 200 (…...); its exec record's
		// at 300
		cut, long := strings.CutSuffix(a.What, "...")
		for _, c := range r.Commands {
			if c.PID == 0 || !(c.Args == a.What || long && strings.HasPrefix(c.Args, cut)) {
				continue
			}
			if underInstall(c.PID) {
				a.Kind = "hook_pipe_to_shell"
				a.Severity = kindOf(a.Kind).Severity.String()
				break
			}
		}
	}
}

// installVerbs: each package manager's commands that run its packages'
// install scripts or build them, by their first words; "" is the command
// with none (yarn).
var installVerbs = map[string][]string{
	"npm":    {"install", "i", "in", "ins", "inst", "insta", "instal", "isnt", "isnta", "isntal", "isntall", "add", "ci", "clean-install", "ic", "install-clean", "isntall-clean", "install-test", "it", "install-ci-test", "cit", "rebuild", "rb", "update", "up", "upgrade", "udpate"},
	"yarn":   {"", "install", "add", "upgrade", "up"},
	"pnpm":   {"install", "i", "add", "update", "up", "upgrade", "rebuild", "rb"},
	"bun":    {"install", "i", "add", "update"},
	"pip":    {"install", "download", "wheel"},
	"uv":     {"sync", "add", "pip install", "pip sync", "tool install"},
	"poetry": {"install", "add", "update", "lock"},
	"pdm":    {"install", "add", "sync", "update"},
	"pipx":   {"install", "inject", "upgrade"},
}

// a script the program is named by: npm-cli.js, yarn.cjs, pip3.12
var installerName = regexp.MustCompile(`^(npm|yarn|pnpm|bun|pip|uv|poetry|pdm|pipx)(-cli)?(\.c?js|\.mjs|[0-9.]*)$`)

// installing says whether a command line is a package manager installing,
// or a package's build run by one: setup.py, pip's and build's backend
// (pyproject_hooks' _in_process.py). By the program it runs, not by its
// words: bash -c "npm install" is a shell, its npm a process of its own.
func installing(cmd string) bool {
	t := strings.Fields(cmd)
	i := 0
	// env [-i] [VAR=…]: what it runs
	if i < len(t) && path.Base(t[i]) == "env" {
		for i++; i < len(t) && (strings.HasPrefix(t[i], "-") || strings.Contains(t[i], "=")); i++ {
		}
	}
	if i >= len(t) {
		return false
	}
	prog := path.Base(t[i])
	// an interpreter is named by its script, or python's -m module
	if prog == "node" || prog == "nodejs" || strings.HasPrefix(prog, "python") {
		for i++; i < len(t); i++ {
			if t[i] == "-m" && i+1 < len(t) {
				i++
				break
			}
			if t[i] == "-c" {
				// pip's legacy setup.py install: python -c "…setup.py…"
				return strings.Contains(cmd, "setup.py")
			}
			if !strings.HasPrefix(t[i], "-") {
				break
			}
		}
		if i >= len(t) {
			return false
		}
		prog = path.Base(t[i])
	}
	if prog == "setup.py" || prog == "_in_process.py" {
		return true
	}
	m := installerName.FindStringSubmatch(prog)
	if m == nil {
		return false
	}
	// its first two words not an option: uv's verbs are two
	var words []string
	for _, w := range t[i+1:] {
		if !strings.HasPrefix(w, "-") && len(words) < 2 {
			words = append(words, w)
		}
	}
	for _, v := range installVerbs[m[1]] {
		n := len(strings.Fields(v))
		if n == 0 && len(words) == 0 || n > 0 && n <= len(words) && v == strings.Join(words[:n], " ") {
			return true
		}
	}
	return false
}
