package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// startDetached leaves a run of file supervising the project in the
// background — docker compose up -d — with its log in the project's state
// directory. It returns once the run holds the project's lock, or with the
// end of its log if it exits first.
func startDetached(file, host, stateDir, projectDir, project string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(projectDir, "run.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	args := []string{"run", "-f", absPath(file), "-state", stateDir}
	if host != "" {
		args = append(args, "-H", host)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives this terminal
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(2 * time.Minute)
	for {
		if h := lockHolder(project); h != nil && h.pid == cmd.Process.Pid {
			fmt.Printf("Project %s is up: pid %d keeps it running.\n  logs:   tail -f %s\n  status: mh status   stop: mh down\n", project, h.pid, logPath)
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("the background run ended (%v); its log, %s:\n%s", err, logPath, tailFile(logPath, 2048))
		case <-deadline:
			return fmt.Errorf("the background run (pid %d) did not start in 2 minutes; see %s", cmd.Process.Pid, logPath)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stopRunner stops the run keeping project up, if there is one, and waits
// for it to let go of the project (it cleans up cycles in flight; persistent
// VMs stay for down to remove).
func stopRunner(project string) error {
	h := lockHolder(project)
	if h == nil {
		return nil
	}
	fmt.Printf("Stopping the orchestrator keeping project %s up (pid %d)...\n", project, h.pid)
	if err := syscall.Kill(h.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping pid %d: %w", h.pid, err)
	}
	for i := 0; i < 600; i++ {
		if lockHolder(project) == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still holds project %s after 60 s", h.pid, project)
}

func tailFile(p string, n int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		f.Seek(-n, 2)
	}
	b := make([]byte, n)
	k, _ := f.Read(b)
	return string(b[:k])
}
