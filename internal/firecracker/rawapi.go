package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// This file talks to Firecracker's REST API directly over its Unix socket,
// bypassing the SDK's generated client, for the snapshot operations the SDK
// can't express:
//
//   - The SDK (v1.0.0, models generated from a 2021-era Firecracker) predates
//     `network_overrides` in PUT /snapshot/load — the field that lets a
//     restored VM come back on a *different* host TAP device. Forking a
//     snapshot into a new VM is impossible without it: the new VM needs its
//     own TAP name, but the snapshot's vmstate remembers the original one.
//   - Pause/snapshot/resume must also work on *adopted* VMs (re-tracked after
//     a daemon restart), where there is no SDK Machine handle at all — only
//     the socket path persisted in the VM record. Speaking to the socket
//     directly gives one code path for both live and adopted VMs.
//
// Paths passed to these calls are interpreted BY FIRECRACKER, which under
// Jailer runs chrooted — so they're chroot-relative (e.g. "/snap.mem" lands in
// <chroot>/root/snap.mem on the host).

// versionRe extracts the major/minor from `firecracker --version` output,
// whose first line looks like "Firecracker v1.10.1".
var versionRe = regexp.MustCompile(`v(\d+)\.(\d+)`)

// Version returns the firecracker binary's version string (e.g. "v1.10.1")
// for the observability report, or "" if the binary can't be run or its
// output not parsed. Same probe SupportsNetworkOverrides does, kept separate
// because one answers "what do we have" and the other "what can it do".
func Version(execFile string) string {
	out, err := exec.Command(execFile, "--version").Output()
	if err != nil {
		return ""
	}
	m := fullVersionRe.Find(out)
	if m == nil {
		return ""
	}
	return string(m)
}

// fullVersionRe captures the complete version tag from `firecracker
// --version`, first line "Firecracker v1.10.1".
var fullVersionRe = regexp.MustCompile(`v\d+\.\d+\.\d+`)

// SupportsNetworkOverrides reports whether the firecracker binary at execFile
// accepts `network_overrides` in PUT /snapshot/load — added in Firecracker
// v1.12.0. Probed once at startup by running the binary with --version; a
// binary that can't be run or parsed counts as not supporting it (the caller
// then sticks to override-free restores, which work everywhere).
func SupportsNetworkOverrides(execFile string) bool {
	out, err := exec.Command(execFile, "--version").Output()
	if err != nil {
		return false
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	return major > 1 || (major == 1 && minor >= 12)
}

// httpOverUDS returns an http.Client whose every request is dialed to the
// given Unix socket, whatever the URL's host says.
func httpOverUDS(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}
}

// apiCall sends one JSON request to Firecracker's API socket and fails on any
// non-2xx response, surfacing Firecracker's own fault message.
func apiCall(ctx context.Context, socketPath, method, path string, body interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling %s %s body: %w", method, path, err)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpOverUDS(socketPath).Do(req)
	if err != nil {
		return fmt.Errorf("%s %s on %s: %w", method, path, socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: firecracker returned %d: %s", method, path, resp.StatusCode, string(msg))
	}
	return nil
}

// PauseVM freezes a running microVM's vCPUs. Required before snapshotting —
// Firecracker rejects snapshot creation on a running VM.
func PauseVM(ctx context.Context, socketPath string) error {
	return apiCall(ctx, socketPath, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// ResumeVM unfreezes a paused microVM's vCPUs.
func ResumeVM(ctx context.Context, socketPath string) error {
	return apiCall(ctx, socketPath, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// SnapshotCreate asks a (paused) VM to write a full snapshot: device/vCPU
// state to statePath and guest memory to memPath, both chroot-relative.
func SnapshotCreate(ctx context.Context, socketPath, statePath, memPath string) error {
	return apiCall(ctx, socketPath, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	})
}

// NetworkOverride remaps one snapshotted network interface onto a different
// host TAP device at load time. IfaceID is the interface's ID as configured at
// boot — the SDK numbers interfaces from "1".
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// SnapshotLoad restores a snapshot into a freshly started (not yet booted)
// Firecracker process and resumes it. statePath/memPath are chroot-relative.
// The mem backend is File: guest pages are mapped copy-on-write from the file,
// so the snapshot's memory is never written to — any number of restored VMs
// can share one mem file.
func SnapshotLoad(ctx context.Context, socketPath, statePath, memPath string, overrides []NetworkOverride) error {
	body := map[string]interface{}{
		"snapshot_path": statePath,
		"mem_backend": map[string]string{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"resume_vm": true,
	}
	if len(overrides) > 0 {
		body["network_overrides"] = overrides
	}
	return apiCall(ctx, socketPath, http.MethodPut, "/snapshot/load", body)
}
