package cli

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
	"system_write": {sevWarn, "tried to write a system file",
		"a write in /etc, as the sandbox's user (refused unless the file is writable by anyone)"},
	"io_uring": {sevWarn, "used io_uring",
		"files opened and read through io_uring are not recorded: what it did through it is missing here. Node turns it off here; little else uses it"},
	"io_uring_epoll": {sevInfo, "set up Node's io_uring ring",
		"libuv's ring of 256 for epoll_ctl, which Node sets up with io_uring off for files: what a native module did through it would not be recorded"},
	"connect": {sevInfo, "tried to connect",
		"which program tried to reach what, seen inside the VM (connections lists what the network refused); :53 (dns) is a name lookup"},
	// From the command lines run (execve).
	"reverse_shell": {sevHigh, "reverse shell",
		"a shell joined to a network connection (bash -i >& /dev/tcp/…, nc -e, socat exec:, a Python socket dup2'd onto a shell): someone else's shell on this machine"},
	"dev_tcp": {sevWarn, "connected through bash's /dev/tcp",
		"a connection without a network tool: connectivity checks do it, and hand-made requests that avoid curl"},
	"pipe_to_shell": {sevWarn, "ran what it downloaded",
		"curl … | sh: installers (rustup, nvm) do it; so does malware's first stage"},
	"obfuscated_exec": {sevHigh, "ran hidden code",
		"decoded and ran at once (base64 -d | sh, exec(b64decode(…))): code that hides what it runs from whoever reads it"},
	"dropper": {sevHigh, "ran a binary it dropped in /tmp",
		"a program written (not compiled) after the sandbox was prepared, in /tmp, /var/tmp or /dev/shm, then run: a downloaded payload"},
	"dropped_exec": {sevWarn, "ran a program it wrote",
		"a binary written (not by a compiler or linker) after the sandbox was prepared, then run: unpacked releases do it; so do payloads"},
	"dropped_script": {sevInfo, "ran a script it wrote in /tmp",
		"test suites (pytest's tmp_path) and git hooks do it; what the script ran is recorded, command by command"},
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
