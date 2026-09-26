package vm

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
)

// consoleLogMax caps one console log segment. When the live log would grow
// past it, it is rotated to <log>.1 (replacing any previous one) and a fresh
// segment starts, so a VM costs at most 2×consoleLogMax of store space for its
// console, however much it prints.
//
// The cap is a containment boundary, not housekeeping: the console is written
// by the guest (and by the Firecracker process, assumed compromisable), and the
// log lives on the same filesystem as every VM's disk. Handing Firecracker the
// file itself — as the daemon used to — let one VM print until the store hit
// ENOSPC and every other VM's writes failed.
const consoleLogMax = 2 << 20

// consoleLogRotated is the path a rotated segment moves to.
func consoleLogRotated(path string) string { return path + ".1" }

// consoleLog is the bounded sink Firecracker's stdout/stderr pipe drains into.
// Firecracker never gets a file: it gets the write end of a pipe and the daemon
// copies from the read end into this writer, which enforces the cap.
//
// Write never fails and never blocks on a full disk: data it can't store is
// dropped. A sink that returned errors would stop exec's copy goroutine and
// leave Firecracker blocked on a full pipe — the guest's serial writes stall
// its own vCPU — turning a logging problem into a hung VM.
type consoleLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
}

// openConsoleLog starts a VM's console log from empty (a boot or restore): the
// live segment is truncated and a stale rotated one removed.
func openConsoleLog(path string) (*consoleLog, error) {
	if err := removeIfExists(consoleLogRotated(path)); err != nil {
		return nil, fmt.Errorf("removing old console log %s: %w", consoleLogRotated(path), err)
	}
	f, err := openLogSegment(path, os.O_TRUNC)
	if err != nil {
		return nil, err
	}
	return &consoleLog{path: path, f: f, max: consoleLogMax}, nil
}

// reopenConsoleLog continues an existing console log (re-attaching to a VM
// adopted after a daemon restart), counting what it already holds against the
// cap.
func reopenConsoleLog(path string) (*consoleLog, error) {
	f, err := openLogSegment(path, os.O_APPEND)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sizing console log %s: %w", path, err)
	}
	return &consoleLog{path: path, f: f, size: fi.Size(), max: consoleLogMax}, nil
}

// openLogSegment opens path write-only as a root-private file. O_CREATE's mode
// only applies to a new file, so the mode is forced afterwards too: a log left
// 0644 by an older daemon must not stay world-readable — the console carries
// whatever the guest prints, secrets included.
func openLogSegment(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|flag, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening console log %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("restricting console log %s: %w", path, err)
	}
	return f, nil
}

func (c *consoleLog) Write(p []byte) (int, error) {
	n := len(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.f == nil {
		return n, nil // closed: the VM is going away, drop the tail
	}
	if int64(len(p)) > c.max {
		p = p[int64(len(p))-c.max:] // one oversized write keeps only its tail
	}
	if c.size+int64(len(p)) > c.max {
		c.rotate()
		if c.f == nil {
			return n, nil
		}
	}
	w, _ := c.f.Write(p)
	c.size += int64(w)
	return n, nil
}

// rotate moves the live segment to <path>.1 and starts an empty one. If the
// new segment can't be opened the log is closed and further output dropped:
// losing console output is acceptable, an uncapped file is not.
func (c *consoleLog) rotate() {
	_ = c.f.Close()
	c.f = nil
	if err := os.Rename(c.path, consoleLogRotated(c.path)); err != nil {
		log.Printf("console log %s: rotating: %v", c.path, err)
		return
	}
	f, err := openLogSegment(c.path, os.O_TRUNC)
	if err != nil {
		log.Printf("console log %s: %v", c.path, err)
		return
	}
	c.f = f
	c.size = 0
}

func (c *consoleLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.f == nil {
		return nil
	}
	err := c.f.Close()
	c.f = nil
	return err
}

// attachConsole re-connects the console log of a Firecracker process that
// outlived a daemon restart (KillMode=process keeps VMs running). Its stdout is
// a pipe whose read end died with the previous daemon; opening
// /proc/<pid>/fd/1 read-only yields a new read end on that same pipe, so the
// adopted VM's console is captured — and capped — again from here on. Output
// the VM printed while no daemon was reading was dropped by the kernel (EPIPE
// on Firecracker's side), not buffered.
//
// A process whose stdout is not a pipe was started by a daemon that predates
// the capped log and wrote the file directly; there is nothing to attach to,
// and it stays uncapped until its next boot. Reported as an error so the caller
// logs it.
func attachConsole(pid int, path string) (io.Closer, error) {
	src, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/1", pid), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("opening console pipe of pid %d: %w", pid, err)
	}
	fi, err := src.Stat()
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		_ = src.Close()
		return nil, fmt.Errorf("console of pid %d is not a pipe (started by an older daemon): log stays uncapped until the VM is restarted", pid)
	}
	dst, err := reopenConsoleLog(path)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	go func() {
		// Ends at EOF, when Firecracker exits and its write end closes.
		_, _ = io.Copy(dst, src)
		_ = src.Close()
		_ = dst.Close()
	}()
	return dst, nil
}

// removeConsoleLog deletes both segments of a VM's console log.
func removeConsoleLog(path string) error {
	if path == "" {
		return nil
	}
	return errors.Join(removeIfExists(path), removeIfExists(consoleLogRotated(path)))
}
