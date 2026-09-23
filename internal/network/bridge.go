package network

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// SetTapIsolation (re)applies the isolated-port flag on a live, enslaved TAP.
// Reconcile calls it for every adopted VM so a TAP created under an older
// policy (daemon upgrade, network flag changed while the daemon was down)
// converges to the network's CURRENT intra setting without restarting the VM.
func SetTapIsolation(tap string, isolated bool) error {
	state := "on"
	if !isolated {
		state = "off"
	}
	if out, err := exec.Command("bridge", "link", "set", "dev", tap, "isolated", state).CombinedOutput(); err != nil {
		return fmt.Errorf("setting isolation %s on %s: %w (%s)", state, tap, err, strings.TrimSpace(string(out)))
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
// the bridge.
//
// isolated is the deny-by-default for VM↔VM traffic: an isolated bridge port
// (kernel feature, `bridge link set ... isolated on`) exchanges no frames with
// other isolated ports, only with the bridge itself — so the guest still
// reaches its gateway (and whatever the egress policy allows) but never a
// sibling VM. Same-bridge traffic never traverses nftables (it's pure L2
// switching), so this is the layer where intra-network isolation must happen;
// a forward-chain rule could not do it.
func CreateTapEnslaved(tap, bridge string, isolated bool) error {
	steps := [][]string{
		{"ip", "tuntap", "add", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "master", bridge},
		{"ip", "link", "set", tap, "up"},
	}
	if isolated {
		// After master: the flag lives on the bridge port, which only exists
		// once the TAP is enslaved.
		steps = append(steps, []string{"bridge", "link", "set", "dev", tap, "isolated", "on"})
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = DeleteTap(tap) // best-effort rollback
			return fmt.Errorf("running %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// DetachTap takes a live TAP off its bridge, leaving it up and enslaved to
// nothing — the state CreateTapQuarantined builds, reached without touching
// the VM behind it. Idempotent: detaching a TAP with no master succeeds.
func DetachTap(tap string) error {
	if out, err := exec.Command("ip", "link", "set", "dev", tap, "nomaster").CombinedOutput(); err != nil {
		return fmt.Errorf("detaching %s from its bridge: %w (%s)", tap, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnslaveTap puts a live TAP on bridge with the given port isolation — the
// converse of DetachTap. Idempotent: re-enslaving to the same bridge is a no-op
// for the kernel, so it also converges a TAP that may or may not be detached.
func EnslaveTap(tap, bridge string, isolated bool) error {
	if out, err := exec.Command("ip", "link", "set", "dev", tap, "master", bridge).CombinedOutput(); err != nil {
		return fmt.Errorf("enslaving %s to %s: %w (%s)", tap, bridge, err, strings.TrimSpace(string(out)))
	}
	return SetTapIsolation(tap, isolated)
}

// listLinks returns the names of the host's network devices starting with
// prefix — how startup and the doctor find this daemon's own devices by their
// naming convention.
func listLinks(prefix string) ([]string, error) {
	out, err := exec.Command("ip", "-o", "link", "show").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing links: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return parseLinkNames(string(out), prefix), nil
}

// parseLinkNames extracts device names from `ip -o link show` output. A device
// with a parent prints as "name@parent:", so only the part before '@' counts.
func parseLinkNames(out, prefix string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		name, _, _ = strings.Cut(name, "@")
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names
}

// ListTaps returns the TAP devices on the host that follow this daemon's
// naming ("tap" + 8 hex chars).
func ListTaps() ([]string, error) {
	all, err := listLinks("tap")
	if err != nil {
		return nil, err
	}
	var taps []string
	for _, n := range all {
		if tapNameRe.MatchString(n) {
			taps = append(taps, n)
		}
	}
	return taps, nil
}

// ListBridges returns the bridges on the host that follow this daemon's naming
// (BridgePrefix + id).
func ListBridges() ([]string, error) {
	return listLinks(BridgePrefix)
}

// defaultRouteIface returns the interface of the host's IPv4 default route
// with the lowest metric, read from /proc/net/route.
func defaultRouteIface() (string, error) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", err
	}
	return parseDefaultRoute(string(data))
}

func parseDefaultRoute(table string) (string, error) {
	best, bestMetric := "", -1
	for i, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if i == 0 || len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		metric, err := strconv.Atoi(f[6])
		if err != nil {
			continue
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	if best == "" {
		return "", fmt.Errorf("no IPv4 default route")
	}
	return best, nil
}
