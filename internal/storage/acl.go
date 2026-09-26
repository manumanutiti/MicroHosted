package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

// Shared, sealed snapshot memory.
//
// Every restore of a snapshot maps the SAME memory file (MAP_PRIVATE), so the
// kernel shares the page cache of the pages no fork has modified — N forks of
// one snapshot cost roughly one copy of its untouched memory, not N. That file
// is sealed: owned by root, mode 0400, writable by no one (see
// snapshot_seal.go). A fork's Firecracker, which runs as the fork's own
// identity, gets read access through a POSIX ACL entry for its group for only
// as long as it takes to load the snapshot: Firecracker opens the files
// read-only and maps the memory during the load, and permissions are checked
// at open(2), so once the load returns the entry is revoked and the mapping
// keeps working. No identity keeps a standing grant on a snapshot — one that is
// later reissued to another VM inherits nothing — and a descriptor opened
// read-only can never be turned into a writable one.
//
// On a filesystem without ACL support the restore falls back to a private copy
// per fork (InstallSnapshotFile): the same isolation, without the page-cache
// sharing.

const aclXattr = "system.posix_acl_access"

// POSIX ACL xattr encoding (linux/posix_acl_xattr.h).
const (
	aclVersion   = 2
	aclUserObj   = 0x01
	aclUser      = 0x02
	aclGroupObj  = 0x04
	aclGroup     = 0x08
	aclMask      = 0x10
	aclOther     = 0x20
	aclUndefined = 0xFFFFFFFF
	aclRead      = 4
)

type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

// aclMu serialises the read-modify-write of snapshot ACLs: concurrent forks of
// one snapshot each add and remove their own entry on the same inode.
var aclMu sync.Mutex

// ErrACLUnsupported reports a filesystem that can't hold POSIX ACLs.
var ErrACLUnsupported = errors.New("filesystem does not support POSIX ACLs")

func encodeACL(entries []aclEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].tag != entries[j].tag {
			return entries[i].tag < entries[j].tag
		}
		return entries[i].id < entries[j].id
	})
	b := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(b, aclVersion)
	for i, e := range entries {
		o := 4 + 8*i
		binary.LittleEndian.PutUint16(b[o:], e.tag)
		binary.LittleEndian.PutUint16(b[o+2:], e.perm)
		binary.LittleEndian.PutUint32(b[o+4:], e.id)
	}
	return b
}

func decodeACL(b []byte) ([]aclEntry, error) {
	if len(b) < 4 || (len(b)-4)%8 != 0 || binary.LittleEndian.Uint32(b) != aclVersion {
		return nil, fmt.Errorf("malformed ACL xattr (%d bytes)", len(b))
	}
	var out []aclEntry
	for o := 4; o < len(b); o += 8 {
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(b[o:]),
			perm: binary.LittleEndian.Uint16(b[o+2:]),
			id:   binary.LittleEndian.Uint32(b[o+4:]),
		})
	}
	return out, nil
}

// readACL returns path's access ACL entries, or nil when it has none.
func readACL(path string) ([]aclEntry, error) {
	buf := make([]byte, 1024)
	for {
		n, err := syscall.Getxattr(path, aclXattr, buf)
		switch {
		case err == nil:
			return decodeACL(buf[:n])
		case errors.Is(err, syscall.ENODATA):
			return nil, nil
		case errors.Is(err, syscall.ERANGE):
			buf = make([]byte, len(buf)*4)
		case errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP):
			return nil, ErrACLUnsupported
		default:
			return nil, fmt.Errorf("reading ACL of %s: %w", path, err)
		}
	}
}

// grantGroupRead adds a read-only entry for gid to path's ACL. The owner keeps
// read, the owning group and others nothing; no entry ever grants write.
func grantGroupRead(path string, gid int) error {
	aclMu.Lock()
	defer aclMu.Unlock()
	cur, err := readACL(path)
	if err != nil {
		return err
	}
	var entries []aclEntry
	for _, e := range cur {
		// Rebuilt below: the base entries and the mask always read-only,
		// and any stale entry for gid replaced.
		if e.tag == aclGroup && e.id != uint32(gid) {
			entries = append(entries, aclEntry{aclGroup, aclRead, e.id})
		}
	}
	entries = append(entries,
		aclEntry{aclUserObj, aclRead, aclUndefined},
		aclEntry{aclGroupObj, 0, aclUndefined},
		aclEntry{aclGroup, aclRead, uint32(gid)},
		aclEntry{aclMask, aclRead, aclUndefined},
		aclEntry{aclOther, 0, aclUndefined},
	)
	if err := syscall.Setxattr(path, aclXattr, encodeACL(entries), 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return ErrACLUnsupported
		}
		return fmt.Errorf("granting gid %d read on %s: %w", gid, path, err)
	}
	return nil
}

// revokeGroupRead removes gid's entry; with no named entry left the ACL is
// removed entirely and the file is back to its sealed mode.
func revokeGroupRead(path string, gid int) error {
	aclMu.Lock()
	defer aclMu.Unlock()
	cur, err := readACL(path)
	if err != nil {
		return err
	}
	var named []aclEntry
	for _, e := range cur {
		if e.tag == aclGroup && e.id != uint32(gid) {
			named = append(named, aclEntry{aclGroup, aclRead, e.id})
		}
	}
	if len(named) == 0 {
		return clearACLLocked(path)
	}
	entries := append(named,
		aclEntry{aclUserObj, aclRead, aclUndefined},
		aclEntry{aclGroupObj, 0, aclUndefined},
		aclEntry{aclMask, aclRead, aclUndefined},
		aclEntry{aclOther, 0, aclUndefined},
	)
	if err := syscall.Setxattr(path, aclXattr, encodeACL(entries), 0); err != nil {
		return fmt.Errorf("revoking gid %d on %s: %w", gid, path, err)
	}
	return nil
}

// clearACL drops every ACL entry from path and restores the sealed mode. Used
// at startup, so a grant left behind by a daemon that died mid-restore does
// not outlive it.
func clearACL(path string) error {
	aclMu.Lock()
	defer aclMu.Unlock()
	return clearACLLocked(path)
}

func clearACLLocked(path string) error {
	err := syscall.Removexattr(path, aclXattr)
	if err != nil && !errors.Is(err, syscall.ENODATA) &&
		!errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return fmt.Errorf("clearing ACL of %s: %w", path, err)
	}
	if err := os.Chmod(path, snapshotSealedMode); err != nil {
		return fmt.Errorf("restoring mode of %s: %w", path, err)
	}
	return nil
}

// ShareSnapshotFile makes the sealed snapshot artifact src available at dst,
// inside a restoring VM's chroot, to that VM's identity gid. Preferably as a
// hard link to the shared inode with a temporary read grant; the returned
// revoke must be called as soon as Firecracker has loaded the snapshot (and is
// safe to call more than once). Where ACLs are unsupported it installs a
// private copy instead, and revoke does nothing. dst must not exist.
func ShareSnapshotFile(src, dst string, gid int) (revoke func() error, err error) {
	noop := func() error { return nil }
	if err := grantGroupRead(src, gid); err != nil {
		if !errors.Is(err, ErrACLUnsupported) {
			return noop, err
		}
		return noop, InstallSnapshotFile(src, dst, gid)
	}
	var once sync.Once
	var revokeErr error
	revoke = func() error {
		once.Do(func() { revokeErr = revokeGroupRead(src, gid) })
		return revokeErr
	}
	if err := os.Link(src, dst); err != nil {
		_ = revoke()
		return noop, fmt.Errorf("linking %s into %s: %w", src, filepath.Dir(dst), err)
	}
	return revoke, nil
}
