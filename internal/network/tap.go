package network

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var tapNameRe = regexp.MustCompile(`^tap[0-9a-f]{8}$`)

// SweepOrphans removes leftover TAP devices from a previous run of the daemon
// that crashed or was killed before it could call DeleteTap itself, without
// which a restart hands out an IP block that collides with a route to a
// now-dead interface and traffic to the new VM silently goes nowhere.
//
// keep is the set of tap names belonging to VMs the manager adopted at startup
// (still-running processes recovered from the store — see
// vm.Manager.Reconcile). Those are live, not orphans, so they must survive the
// sweep or reconnecting to a persisted VM would tear down its networking.
func SweepOrphans(keep map[string]bool) error {
	out, err := exec.Command("ip", "-o", "link", "show").CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing links: %w (%s)", err, out)
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if tapNameRe.MatchString(name) && !keep[name] {
			if err := DeleteTap(name); err != nil {
				return fmt.Errorf("sweeping orphaned tap %s: %w", name, err)
			}
		}
	}
	return nil
}

// DeleteTap removes a TAP device. Safe to call even if it doesn't exist.
func DeleteTap(name string) error {
	cmd := exec.Command("ip", "link", "del", name)
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "Cannot find device") {
		return fmt.Errorf("deleting tap %s: %w (%s)", name, err, out)
	}
	return nil
}
