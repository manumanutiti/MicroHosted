package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
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
// unprivileged process, never touch the host kernel.
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
// which is why every entry point takes a stagingDir the caller roots on the CoW
// store.

// debugfsTimeout caps any single debugfs invocation. A corrupt or adversarial
// image shouldn't be able to wedge the daemon; debugfs on a sane image is
// fast even for a large dump (it streams to the output file).
const debugfsTimeout = 30 * time.Minute

// InjectFile writes data to guestPath inside the ext4 image at imagePath,
// creating parent directories as needed and overwriting any existing file. It
// streams data to a staged file under stagingDir first (debugfs needs a real
// host file as its write source), so memory stays constant for any size.
// imagePath must be a detached volume or a stopped VM's disk — never a mounted
// image, or the write races the guest's own view of the filesystem.
func InjectFile(imagePath, guestPath string, data io.Reader, stagingDir string) error {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return err
	}

	tmp, err := stageFile(stagingDir, "inject-*")
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
	var script strings.Builder
	for _, dir := range parentDirs(clean) {
		fmt.Fprintf(&script, "mkdir %s\n", dir)
	}
	fmt.Fprintf(&script, "rm %s\n", clean)
	fmt.Fprintf(&script, "write %s %s\n", tmp.Name(), clean)

	if _, err := runDebugfs(imagePath, true, script.String(), stagingDir); err != nil {
		return err
	}

	// debugfs exits 0 even when the write failed (e.g. no space), so verify the
	// file is actually there rather than trusting the exit code.
	if err := statInImage(imagePath, clean, stagingDir); err != nil {
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
func ExtractFileStream(imagePath, guestPath, stagingDir string) (*os.File, int64, error) {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return nil, 0, err
	}

	// dump on a missing file leaves the stage empty and still exits 0, so a
	// prior stat is what turns "not there" into a clean error.
	if err := statInImage(imagePath, clean, stagingDir); err != nil {
		return nil, 0, fmt.Errorf("extracting %s: %w", guestPath, err)
	}

	tmp, err := stageFile(stagingDir, "extract-*")
	if err != nil {
		return nil, 0, err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()

	if _, err := runDebugfs(imagePath, false, fmt.Sprintf("dump %s %s\n", clean, tmpName), stagingDir); err != nil {
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
// so this is constant-memory too.
func ExtractDir(imagePath, guestPath, destDir, stagingDir string) error {
	clean, err := cleanGuestPath(guestPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating extract dest %s: %w", destDir, err)
	}
	if err := statInImage(imagePath, clean, stagingDir); err != nil {
		return fmt.Errorf("extracting dir %s: %w", guestPath, err)
	}
	if _, err := runDebugfs(imagePath, false, fmt.Sprintf("rdump %s %s\n", clean, destDir), stagingDir); err != nil {
		return err
	}
	return nil
}

// stageFile creates a temp file under stagingDir (rooted on the store by the
// caller, never /tmp), making the directory if needed.
func stageFile(stagingDir, pattern string) (*os.File, error) {
	if stagingDir == "" {
		return nil, fmt.Errorf("staging dir is required")
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating staging dir %s: %w", stagingDir, err)
	}
	f, err := os.CreateTemp(stagingDir, pattern)
	if err != nil {
		return nil, fmt.Errorf("creating staging file in %s: %w", stagingDir, err)
	}
	return f, nil
}

// runDebugfs runs a script of debugfs commands (one per line) against image.
// write enables -w (read-write mode); leave it false for read-only extraction so
// a bug can't mutate the image. The script is fed via a temp command file (-f)
// rather than -R so multi-command sequences work. The command file is small but
// still staged on the store, not /tmp, to keep the "never /tmp" rule uniform.
func runDebugfs(image string, write bool, script, stagingDir string) (string, error) {
	cmdFile, err := stageFile(stagingDir, "debugfs-*.cmd")
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

	out, err := exec.CommandContext(ctx, "debugfs", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("debugfs on %s: %v: %s", image, err, out)
	}
	return string(out), nil
}

// statInImage reports whether guestPath exists inside image, translating
// debugfs's "File not found" output into an error. debugfs exits 0 whether or
// not the file exists, so the output string is the only signal.
func statInImage(image, guestPath, stagingDir string) error {
	out, err := runDebugfs(image, false, fmt.Sprintf("stat %s\n", guestPath), stagingDir)
	if err != nil {
		return err
	}
	if strings.Contains(out, "File not found") || strings.Contains(out, "does not exist") {
		return fmt.Errorf("path %s not found in image", guestPath)
	}
	return nil
}

// cleanGuestPath validates and normalises an in-image path: it must be absolute
// and free of "." / ".." components, so a caller can't walk outside the intended
// target or feed debugfs a surprising relative path.
func cleanGuestPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("guest path is required")
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
