package network

import (
	"fmt"
	"os/exec"
	"strings"
)

// This is the segmented-network data plane (Fase 1, see docs/networking.md):
// one Linux bridge per named network, each VM's TAP enslaved to its network's
// bridge. It replaces the old /30 point-to-point model in tap.go, where the
// TAP itself carried the host IP and the host was every VM's gateway.

// BridgeExists reports whether a bridge device is already present — used at
// startup to recreate only the network bridges that a host reboot wiped,
// leaving intact any the daemon's previous run left running.
func BridgeExists(name string) bool {
	err := exec.Command("ip", "link", "show", "dev", name).Run()
	return err == nil
}

// CreateBridge creates a bridge device, assigns it the network's gateway
// address (e.g. "172.16.0.1/24" — the .1 guests route through) and brings it
// up. Idempotent-ish: safe to call for a bridge that doesn't exist yet; if
// creation fails partway it best-effort removes what it made.
func CreateBridge(name, gatewayCIDR string) error {
	steps := [][]string{
		{"ip", "link", "add", "name", name, "type", "bridge"},
		{"ip", "addr", "add", gatewayCIDR, "dev", name},
		{"ip", "link", "set", name, "up"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = DeleteBridge(name)
			return fmt.Errorf("running %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// DeleteBridge removes a bridge device. Safe to call even if it's absent.
func DeleteBridge(name string) error {
	out, err := exec.Command("ip", "link", "del", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "Cannot find device") {
		return fmt.Errorf("deleting bridge %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// TapExists reports whether a TAP device with this name is present on the
// host — used by fork-from-snapshot on Firecracker < 1.12 to know whether the
// snapshot's original TAP name (the only name such a host can restore onto)
// is free to take.
func TapExists(name string) bool {
	return exec.Command("ip", "link", "show", "dev", name).Run() == nil
}

// CreateTapQuarantined creates a TAP device enslaved to nothing, up but going
// nowhere. Firecracker refuses to restore a snapshot that had a network device
// unless a host TAP backs it, and a quarantined fork wants exactly that: the
// guest wakes up believing it still has its network (IP/MAC frozen in the
// restored memory) while every frame it emits dies at a TAP with no bridge —
// no path to the host, other VMs, or the internet. vsock exec still works.
func CreateTapQuarantined(tap string) error {
	steps := [][]string{
		{"ip", "tuntap", "add", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "up"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = DeleteTap(tap) // best-effort rollback
			return fmt.Errorf("running %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// CreateTapEnslaved creates a TAP device and enslaves it to bridge, bringing it
// up. Unlike the old CreateTap, the TAP carries no IP of its own: guests get
// their address from the network's subnet and reach the host/gateway through
// the bridge, and VMs on the same bridge see each other at L2.
func CreateTapEnslaved(tap, bridge string) error {
	steps := [][]string{
		{"ip", "tuntap", "add", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "master", bridge},
		{"ip", "link", "set", tap, "up"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = DeleteTap(tap) // best-effort rollback
			return fmt.Errorf("running %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
