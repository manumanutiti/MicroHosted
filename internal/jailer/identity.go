package jailer

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Per-VM identities.
//
// Every VM's Firecracker runs as a uid of its own, never shared with another
// VM, with a gid of the same number. Its disk is owned by that uid, as are the
// volumes attached to it, and debugfs, when it parses that VM's disk, drops to
// the same identity. A compromised VMM or a debugfs exploited through a crafted
// image therefore holds the permissions of one VM's files and nothing else: not
// the other VMs' disks, not their volumes, not the ability to signal their
// processes or attach to their TAPs.
//
// The identities come from a range reserved for the daemon, which must not
// overlap any account, group or subordinate id range on the host: a VM sharing
// a uid with a real account would hand that account its disk (and the account
// the VM's processes). The daemon refuses to start on an overlapping range.

const (
	// DefaultIDBase is the first identity handed out. It sits in
	// 1879048192–2147483647, the band systemd leaves unallocated (above the
	// container ranges it hands to nspawn, below the ids that turn negative as
	// a signed 32-bit value), and far above the regular user and subordinate
	// ranges (100000+ by default).
	DefaultIDBase = 1_900_000_000
	// DefaultIDCount bounds the VMs and volumes the daemon can track at once.
	DefaultIDCount = 65536
)

// ErrIDRangeOverlap is returned when the reserved range collides with an id the
// host already uses.
var ErrIDRangeOverlap = errors.New("identity range overlaps ids in use on the host")

// Files consulted by ValidateIDRange; variables so tests can point them at
// fixtures.
var (
	passwdFile = "/etc/passwd"
	groupFile  = "/etc/group"
	subuidFile = "/etc/subuid"
	subgidFile = "/etc/subgid"
)

// ContainsID reports whether id is inside the reserved range.
func (d Defaults) ContainsID(id int) bool {
	return id >= d.IDBase && id < d.IDBase+d.IDCount
}

// ValidateIDRange checks that [IDBase, IDBase+IDCount) is a sane range that no
// local user, group or subordinate id range already uses. Accounts served by
// NSS modules other than files (LDAP, SSSD) are not visible here: on such
// hosts the operator must reserve the range there as well.
func (d Defaults) ValidateIDRange() error {
	if d.IDBase < 65536 || d.IDCount < 1 || int64(d.IDBase)+int64(d.IDCount) > 1<<31-1 {
		return fmt.Errorf("identity range [%d, +%d) must start at or above 65536 and end below 2^31-1", d.IDBase, d.IDCount)
	}
	var clashes []string
	for _, f := range []string{passwdFile, groupFile} {
		ids, err := readIDField(f)
		if err != nil {
			return err
		}
		for name, id := range ids {
			if d.ContainsID(id) {
				clashes = append(clashes, fmt.Sprintf("%s: %s (%d)", f, name, id))
			}
		}
	}
	for _, f := range []string{subuidFile, subgidFile} {
		ranges, err := readSubIDs(f)
		if err != nil {
			return err
		}
		for _, r := range ranges {
			if r.start < int64(d.IDBase+d.IDCount) && int64(d.IDBase) < r.start+r.count {
				clashes = append(clashes, fmt.Sprintf("%s: %s (%d+%d)", f, r.owner, r.start, r.count))
			}
		}
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%w [%d, %d): %s — choose another --jailer-id-base", ErrIDRangeOverlap, d.IDBase, d.IDBase+d.IDCount, strings.Join(clashes, "; "))
	}
	return nil
}

// readIDField returns name → id (third field) from a passwd- or group-style
// file. A missing file contributes nothing.
func readIDField(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	ids := make(map[string]int)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if id, err := strconv.Atoi(fields[2]); err == nil {
			ids[fields[0]] = id
		}
	}
	return ids, sc.Err()
}

type subIDRange struct {
	owner        string
	start, count int64
}

// readSubIDs parses /etc/subuid-style "owner:start:count" lines. A missing
// file contributes nothing.
func readSubIDs(path string) ([]subIDRange, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	var out []subIDRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(fields) != 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		start, err1 := strconv.ParseInt(fields[1], 10, 64)
		count, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 == nil && err2 == nil {
			out = append(out, subIDRange{fields[0], start, count})
		}
	}
	return out, sc.Err()
}

// LiveUIDs returns the real, effective, saved and filesystem uids of every
// process on the host. The allocator never hands out an identity some process
// still runs as — a VMM that survived its VM's teardown, or anything else —
// so a reused uid can never inherit a live process's reach.
func LiveUIDs() (map[int]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	uids := make(map[int]bool)
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue // exited meanwhile
		}
		for _, line := range strings.Split(string(b), "\n") {
			rest, ok := strings.CutPrefix(line, "Uid:")
			if !ok {
				continue
			}
			for _, f := range strings.Fields(rest) {
				if id, err := strconv.Atoi(f); err == nil {
					uids[id] = true
				}
			}
			break
		}
	}
	return uids, nil
}
