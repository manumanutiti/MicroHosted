package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"
)

// Offline I/O reads and writes files inside an ext4 image WITHOUT MOUNTING IT.
//
// This is the security-critical primitive of the data plane. Mounting an
// untrusted guest filesystem with mount(2) runs the host *kernel's* ext4 parser
// over attacker-controlled bytes — the classic filesystem-image escape surface
// (a long history of ext4/journal CVEs). Instead every host-side access goes
// through debugfs (from e2fsprogs, already a host dependency), a userspace ext4
// tool: a malformed image can at worst make debugfs itself misbehave in its own
// process, never touch the host kernel.
//
// And that process is NOT the daemon's root: when the daemon runs as root,
// every debugfs invocation drops to the jailer uid/gid (OfflineIO.UID/GID) —
// the same unprivileged identity the jailed Firecracker runs as, and the owner
// of every image file it touches (clones and volumes are chowned to it at
// creation). A debugfs parser exploit triggered by a malicious image then
// lands in an unprivileged process with no capabilities, not in root: it can
// scribble on the store's images (which it could anyway — it IS the parser
// writing them) but not on the host. This is mitigation, not a jail — the
// process still shares the host's namespaces — but it removes the root-shell
// prize from the ext4-parser attack surface.
//
// Used for two cases the live vsock channel can't serve: injecting a sample into
// a detached volume with no VM running, and extracting artifacts from a stopped
// VM's disk post-mortem.
//
// Everything here works in CONSTANT MEMORY regardless of file size: debugfs
// can't read/write a stream (its `write`/`dump` verbs take real host files), so
// bytes are staged to a temp file and copied with io.Copy's fixed buffer — a
// 4 GB inject costs a few KB of RAM, not 4 GB. The staging directory must be a
// real on-disk location on the store (never /tmp, which is often tmpfs = RAM),
// which is why OfflineIO carries a StagingDir the caller roots on the CoW
// store.

// debugfsTimeout caps any single debugfs invocation. A corrupt or adversarial
// image shouldn't be able to wedge the daemon; debugfs on a sane image is
// fast even for a large dump (it streams to the output file).
const debugfsTimeout = 30 * time.Minute

// OfflineIO carries the host-side context every offline operation needs: where
// to stage temp files (on the store, never /tmp) and the unprivileged identity
// debugfs drops to (see the package comment). UID/GID are the jailer's; the
// drop only happens when the daemon itself runs as root — in unprivileged runs
// (tests) debugfs simply runs as the daemon's own user, which already owns the
// images it creates.
type OfflineIO struct {
	StagingDir string
	UID        int
	GID        int
}

// dropPrivs reports whether debugfs should switch to UID/GID: only meaningful
// (and only permitted — setuid needs CAP_SETUID) when running as root, and
// never to uid 0 itself.
func (o OfflineIO) dropPrivs() bool {
	return os.Geteuid() == 0 && o.UID > 0
}

// grant hands a staged file or directory to the debugfs identity. The staging
// dir itself stays root-owned 0755 (traversal is enough); the files debugfs
// must open — its command file, an inject source, an extract destination —
// get chowned so the 0600 temp perms keep excluding everyone else.
func (o OfflineIO) grant(path string) error {
	if !o.dropPrivs() {
		return nil
	}
	if err := os.Chown(path, o.UID, o.GID); err != nil {
		return fmt.Errorf("granting %s to debugfs uid %d:%d: %w", path, o.UID, o.GID, err)
	}
	return nil
}

// InjectFile writes data to guestPath inside the ext4 image at imagePath,
// creating parent directories as needed and overwriting any existing file. It
// streams data to a staged file under StagingDir first (debugfs needs a real
// host file as its write source), so memory stays constant for any size.
// imagePath must be a detached volume or a stopped VM's disk — never a mounted
// image, or the write races the guest's own view of the filesystem.
func (o OfflineIO) InjectFile(imagePath, guestPath string, data io.Reader) error {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return err
	}

	tmp, err := o.stageFile("inject-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("staging inject data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("staging inject data: %w", err)
	}

	// A single debugfs command file: create each parent dir (harmless if it
	// already exists), drop any existing file, then write. debugfs keeps going
	// past a failed command, so the mkdir/rm lines are best-effort by design and
	// the write is what matters; success is confirmed by the stat check below.
	var s debugfsScript
	for _, dir := range parentDirs(clean) {
		s.add("mkdir", dir)
	}
	s.add("rm", clean)
	s.add("write", tmp.Name(), clean)
	script, err := s.script()
	if err != nil {
		return err
	}

	if _, err := o.runDebugfs(imagePath, true, script); err != nil {
		return err
	}

	// debugfs exits 0 even when the write failed (e.g. no space), so verify the
	// file is actually there rather than trusting the exit code.
	if err := o.statInImage(imagePath, clean); err != nil {
		return fmt.Errorf("injecting %s: %w", guestPath, err)
	}
	return nil
}

// ExtractFileStream reads guestPath out of the ext4 image at imagePath and
// returns an open reader over its contents plus its size. debugfs dumps to a
// staged file on the store; the returned file is that stage, already unlinked,
// so the caller just streams it to its destination and Close()s it — nothing is
// buffered in memory and no temp file is left behind. Used to pull an artifact
// off a detached volume or a stopped VM.
func (o OfflineIO) ExtractFileStream(imagePath, guestPath string) (*os.File, int64, error) {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return nil, 0, err
	}

	// dump on a missing file leaves the stage empty and still exits 0, so a
	// prior stat is what turns "not there" into a clean error.
	if err := o.statInImage(imagePath, clean); err != nil {
		return nil, 0, fmt.Errorf("extracting %s: %w", guestPath, err)
	}

	tmp, err := o.stageFile("extract-*")
	if err != nil {
		return nil, 0, err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()

	script, err := debugfsLine("dump", clean, tmpName)
	if err == nil {
		_, err = o.runDebugfs(imagePath, false, script)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return nil, 0, err
	}

	f, err := os.Open(tmpName)
	if err != nil {
		_ = os.Remove(tmpName)
		return nil, 0, fmt.Errorf("opening extracted %s: %w", guestPath, err)
	}
	// Unlink now: on Linux the open fd stays valid, so the bytes live only as
	// long as the caller holds the reader and vanish on Close with no cleanup.
	_ = os.Remove(tmpName)

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("sizing extracted %s: %w", guestPath, err)
	}
	return f, fi.Size(), nil
}

// ExtractDir recursively copies the directory tree at guestPath out of the image
// into destDir on the host (destDir gets a subdirectory named after guestPath's
// last component). The one-shot way to collect a whole artifact tree — pcaps,
// memory dumps — from a stopped VM's disk. debugfs streams straight to destDir,
// so this is constant-memory too. destDir is chowned to the debugfs identity
// (rdump has to create entries in it), so expect its contents owned by the
// jailer uid, not root.
func (o OfflineIO) ExtractDir(imagePath, guestPath, destDir string) error {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating extract dest %s: %w", destDir, err)
	}
	if err := o.grant(destDir); err != nil {
		return err
	}
	if err := o.statInImage(imagePath, clean); err != nil {
		return fmt.Errorf("extracting dir %s: %w", guestPath, err)
	}
	script, err := debugfsLine("rdump", clean, destDir)
	if err != nil {
		return err
	}
	_, err = o.runDebugfs(imagePath, false, script)
	return err
}

// stageFile creates a temp file under StagingDir (rooted on the store by the
// caller, never /tmp), making the directory if needed, and grants it to the
// debugfs identity — every staged file is either read or written by the
// dropped-privilege debugfs process.
func (o OfflineIO) stageFile(pattern string) (*os.File, error) {
	if o.StagingDir == "" {
		return nil, fmt.Errorf("staging dir is required")
	}
	if err := os.MkdirAll(o.StagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating staging dir %s: %w", o.StagingDir, err)
	}
	f, err := os.CreateTemp(o.StagingDir, pattern)
	if err != nil {
		return nil, fmt.Errorf("creating staging file in %s: %w", o.StagingDir, err)
	}
	if err := o.grant(f.Name()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// runDebugfs runs a script of debugfs commands (one per line) against image,
// as the dropped-privilege identity when the daemon is root (see dropPrivs).
// write enables -w (read-write mode); leave it false for read-only extraction so
// a bug can't mutate the image. The script is fed via a temp command file (-f)
// rather than -R so multi-command sequences work. The command file is small but
// still staged on the store, not /tmp, to keep the "never /tmp" rule uniform.
func (o OfflineIO) runDebugfs(image string, write bool, script string) (string, error) {
	cmdFile, err := o.stageFile("debugfs-*.cmd")
	if err != nil {
		return "", err
	}
	defer os.Remove(cmdFile.Name())
	if _, err := cmdFile.WriteString(script); err != nil {
		_ = cmdFile.Close()
		return "", fmt.Errorf("writing debugfs command file: %w", err)
	}
	if err := cmdFile.Close(); err != nil {
		return "", fmt.Errorf("writing debugfs command file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), debugfsTimeout)
	defer cancel()

	args := []string{}
	if write {
		args = append(args, "-w")
	}
	args = append(args, "-f", cmdFile.Name(), image)

	cmd := exec.CommandContext(ctx, "debugfs", args...)
	if o.dropPrivs() {
		// Groups is set to empty explicitly: without it the child inherits
		// root's supplementary groups, quietly keeping privileges the drop is
		// supposed to shed.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid:    uint32(o.UID),
				Gid:    uint32(o.GID),
				Groups: []uint32{},
			},
		}
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("debugfs on %s: %v: %s", image, err, out)
	}
	return string(out), nil
}

// statInImage reports whether guestPath exists inside image, translating
// debugfs's "File not found" output into an error. debugfs exits 0 whether or
// not the file exists, so the output string is the only signal.
func (o OfflineIO) statInImage(image, guestPath string) error {
	script, err := debugfsLine("stat", guestPath)
	if err != nil {
		return err
	}
	out, err := o.runDebugfs(image, false, script)
	if err != nil {
		return err
	}
	if strings.Contains(out, "File not found") || strings.Contains(out, "does not exist") {
		return fmt.Errorf("path %s not found in image", guestPath)
	}
	return nil
}

// ValidateGuestPath checks and normalises a path inside a guest filesystem, for
// every file channel (vsock and debugfs alike, so both accept the same paths):
// it must be absolute, free of ".." components, and free of control characters
// and double quotes.
//
// The last rule is a security boundary, not tidiness. debugfs reads one command
// per line and splits arguments on whitespace, so a path carrying a newline
// would smuggle in commands of its own — "dump" writes a host file, "write"
// reads one — running as the jailer uid, which owns every VM's disk and every
// volume. Paths often come from the guest itself (an automation listing a
// sample's output dir and downloading each file), so the guest would pick
// them. Double quotes are refused because they are how debugfsLine delimits
// arguments; spaces are fine.
func ValidateGuestPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("guest path is required")
	}
	if err := checkDebugfsArg(p); err != nil {
		return "", fmt.Errorf("guest path %q: %w", p, err)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("guest path %q must be absolute", p)
	}
	clean := path.Clean(p)
	if clean == "/" {
		return "", fmt.Errorf("guest path %q must name a file or directory, not the root", p)
	}
	if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.HasSuffix(clean, "/..") {
		return "", fmt.Errorf("guest path %q must not contain ..", p)
	}
	return clean, nil
}

// cleanGuestPath is the in-package name offline operations use.
func cleanGuestPath(p string) (string, error) { return ValidateGuestPath(p) }

// checkDebugfsArg refuses what cannot be passed safely as one debugfs argument:
// control characters (a newline ends the command and starts another) and the
// double quote (which would close debugfsLine's quoting early).
func checkDebugfsArg(s string) error {
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("must not contain control characters")
		case r == '"':
			return fmt.Errorf(`must not contain '"'`)
		}
	}
	return nil
}

// debugfsLine renders one debugfs command with every argument double-quoted,
// refusing any argument checkDebugfsArg rejects. Every line of every script
// goes through here, host paths included: quoting only some arguments is how
// the one unquoted path ends up being the injection.
func debugfsLine(verb string, args ...string) (string, error) {
	var b strings.Builder
	b.WriteString(verb)
	for _, a := range args {
		if err := checkDebugfsArg(a); err != nil {
			return "", fmt.Errorf("debugfs %s argument %q: %w", verb, a, err)
		}
		b.WriteString(` "`)
		b.WriteString(a)
		b.WriteString(`"`)
	}
	b.WriteString("\n")
	return b.String(), nil
}

// debugfsScript accumulates debugfsLine commands; the first refused argument
// sticks and is returned by script.
type debugfsScript struct {
	b   strings.Builder
	err error
}

func (s *debugfsScript) add(verb string, args ...string) {
	if s.err != nil {
		return
	}
	l, err := debugfsLine(verb, args...)
	if err != nil {
		s.err = err
		return
	}
	s.b.WriteString(l)
}

func (s *debugfsScript) script() (string, error) { return s.b.String(), s.err }

// parentDirs returns the ancestor directories of an absolute file path, shallow
// to deep (e.g. /a/b/c → /a, /a/b), so they can be created in order.
func parentDirs(p string) []string {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return nil
	}
	var parts []string
	for dir != "/" && dir != "." {
		parts = append([]string{dir}, parts...)
		dir = path.Dir(dir)
	}
	return parts
}
