package jailer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
)

// Defaults holds the operator-configured, host-wide Jailer settings — the
// same for every VM launched by this daemon.
type Defaults struct {
	// UID/GID the jailed Firecracker process switches to. Firecracker's own
	// getting-started docs use 123/100; the operator is responsible for
	// making sure that uid/gid exists and can access /dev/kvm.
	UID int
	GID int

	// ChrootBaseDir is the root under which Jailer builds
	// <ChrootBaseDir>/<exec-file basename>/<id>/root/. Defaults to
	// /srv/jailer per docs/architecture.md; scripts/setup-host.sh creates it.
	ChrootBaseDir string

	// JailerBinary/ExecFile are absolute paths to the jailer and firecracker
	// binaries. Both must be the exact same release — scripts/install-fc.sh
	// installs them together for this reason.
	JailerBinary string
	ExecFile     string

	// CgroupVersion is passed as jailer's --cgroup-version. Jailer defaults
	// to "1" on its own, which panics with "Hierarchy not found" on hosts
	// that only mount cgroup2 (the norm since systemd's unified hierarchy
	// became default) — so this must match what the host actually has
	// mounted. DetectCgroupVersion below figures this out automatically;
	// it's only a field here so it can still be forced via a flag.
	CgroupVersion string
}

// DetectCgroupVersion inspects the host and returns "2" if the unified
// cgroup2 hierarchy is mounted, "1" otherwise. Used as the flag default in
// cmd/microhosted so the operator doesn't have to know this about their own
// host ahead of time — it automates the same check
// scripts/setup-host.sh does by hand (`mount | grep cgroup2`).
func DetectCgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	return "1"
}

// DefaultDefaults returns the conventional paths/ids from Firecracker's own
// getting-started guide, plus the host's actual cgroup version. Overridable
// via flags in cmd/microhosted.
func DefaultDefaults() Defaults {
	return Defaults{
		UID:           123,
		GID:           100,
		ChrootBaseDir: "/srv/jailer",
		JailerBinary:  "/usr/local/bin/jailer",
		ExecFile:      "/usr/local/bin/firecracker",
		CgroupVersion: DetectCgroupVersion(),
	}
}

// InstanceDir returns the host-side path of a VM's whole jail directory, i.e.
// <ChrootBaseDir>/<basename(ExecFile)>/<vmID> — the parent of the chroot's
// `root` subtree. This is the directory Jailer creates per VM and, crucially,
// never cleans up on its own: it stays behind (rootfs/kernel hardlinks, the
// api socket, cgroup leftovers) after the Firecracker process is gone, so the
// manager has to remove it explicitly on Destroy (see RemoveInstanceDir).
func InstanceDir(d Defaults, vmID string) string {
	return filepath.Join(d.ChrootBaseDir, filepath.Base(d.ExecFile), vmID)
}

// WorkspaceRoot returns the host-side path of a VM's chroot, i.e.
// <ChrootBaseDir>/<basename(ExecFile)>/<vmID>/root — the same formula the
// SDK computes internally for the API socket (see its jail() function) but
// doesn't expose. Anything we need to reach from the host, inside the jail,
// that the SDK itself doesn't already resolve for us (like the vsock UDS
// path — see internal/vsock) has to be built with this.
func WorkspaceRoot(d Defaults, vmID string) string {
	return filepath.Join(InstanceDir(d, vmID), "root")
}

// RemoveInstanceDir deletes a VM's whole jail directory (see InstanceDir)
// and its cgroup (see RemoveCgroup) — the two pieces of per-VM host residue
// Jailer creates and never cleans up itself. Only safe once the Firecracker
// process is gone — Destroy calls it after firecracker.Stop. Safe to call
// even if neither exists. The daemon runs as root, so it can remove files
// Jailer left owned by the jailed uid/gid.
func RemoveInstanceDir(d Defaults, vmID string) error {
	dir := InstanceDir(d, vmID)
	var dirErr error
	if err := os.RemoveAll(dir); err != nil {
		dirErr = fmt.Errorf("removing jail dir %s: %w", dir, err)
	}
	return errors.Join(dirErr, RemoveCgroup(d, vmID))
}

// Build constructs the per-VM JailerConfig. kernelPath is needed up front
// because the SDK's NaiveChrootStrategy hard-links the kernel image by name
// into the chroot and has to know that name at config-build time.
//
// stdout/stderr should be a per-VM log file, never the daemon's own
// os.Stdout/os.Stderr: if those happen to be a TTY (e.g. the daemon is run
// in a foreground terminal), Firecracker attaches the guest's serial console
// directly to it and that terminal stops being usable for anything else.
// The daemon just creates the VM; whether and how an operator looks at its
// console afterwards is a separate decision (tail the log file, SSH, or
// vsock exec — not the process's controlling terminal).
//
// Jailer's own --daemonize isn't used here even though it also avoids TTY
// takeover: it makes Jailer setsid()+double-fork, which detaches the actual
// Firecracker process from the one the SDK spawned and is watching — the
// SDK then loses track of it entirely (it looks like Firecracker exited
// immediately and the API socket never gets created).
func Build(vmID, kernelPath string, d Defaults, stdout, stderr io.Writer) fc.JailerConfig {
	return fc.JailerConfig{
		ID:             vmID,
		UID:            fc.Int(d.UID),
		GID:            fc.Int(d.GID),
		NumaNode:       fc.Int(0),
		ChrootBaseDir:  d.ChrootBaseDir,
		JailerBinary:   d.JailerBinary,
		ExecFile:       d.ExecFile,
		CgroupVersion:  d.CgroupVersion,
		ChrootStrategy: fc.NewNaiveChrootStrategy(kernelPath),
		Stdout:         stdout,
		Stderr:         stderr,
	}
}
