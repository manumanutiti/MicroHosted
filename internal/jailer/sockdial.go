package jailer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// chrootDirName is the chroot subtree Jailer creates under each InstanceDir
// (see WorkspaceRoot). Everything from here down is owned by the VM's jailed
// uid, so a compromised Firecracker can rename, replace or link anything in it.
const chrootDirName = "root"

// DialSocket connects to a Unix socket that lives inside a VM's chroot (the
// Firecracker API socket or the vsock UDS) without trusting anything the
// jailed process could have planted there.
//
// The daemon runs as root, and a plain connect(path) follows symlinks: a
// compromised VMM could swap its v.sock for a link to a host socket (the
// daemon's own API, systemd, Docker) and have root talk to it. So:
//
//   - the path is split at the chroot: the part above it (InstanceDir) is
//     root-owned and trusted, and is verified to be so;
//   - the part below it is resolved with openat2 refusing every symlink,
//     magic link, mount crossing or escape from that directory;
//   - the result must be a socket owned by the VM's own uid, which also rules
//     out a hard link to some host socket;
//   - the connect goes through /proc/self/fd/N of that very inode, so nothing
//     can be swapped in between the check and the connect.
//
// uid is the VM's jailed identity (VMConfig.JailUID); root (0) is refused.
func DialSocket(ctx context.Context, path string, uid int) (net.Conn, error) {
	if uid <= 0 {
		return nil, fmt.Errorf("dialing %s: refusing jail socket without an unprivileged owner (uid %d)", path, uid)
	}
	trusted, rel, err := splitJailPath(path)
	if err != nil {
		return nil, err
	}

	dirfd, err := unix.Open(trusted, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: opening jail dir: %w", path, &os.PathError{Op: "open", Path: trusted, Err: err})
	}
	defer unix.Close(dirfd)
	var dst unix.Stat_t
	if err := unix.Fstat(dirfd, &dst); err != nil {
		return nil, fmt.Errorf("dialing %s: stat jail dir: %w", path, err)
	}
	if dst.Uid != 0 || dst.Mode&0o022 != 0 {
		return nil, fmt.Errorf("dialing %s: jail dir %s is not root-only (uid %d, mode %#o)", path, trusted, dst.Uid, dst.Mode&0o7777)
	}

	fd, err := unix.Openat2(dirfd, rel, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", path, &os.PathError{Op: "openat2", Path: rel, Err: err})
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("dialing %s: stat: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, fmt.Errorf("dialing %s: not a socket (mode %#o)", path, st.Mode)
	}
	if int(st.Uid) != uid {
		return nil, fmt.Errorf("dialing %s: socket owned by uid %d, want the VM's uid %d", path, st.Uid, uid)
	}

	var d net.Dialer
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
}

// splitJailPath splits an absolute path under a VM's chroot into the trusted
// InstanceDir above it and the relative path inside the chroot, which keeps
// the leading "root/" so openat2 resolves the chroot directory itself under
// the same no-symlink rules.
func splitJailPath(path string) (trusted, rel string, err error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean != path {
		return "", "", fmt.Errorf("dialing %s: jail socket path must be absolute and clean", path)
	}
	sep := string(filepath.Separator) + chrootDirName + string(filepath.Separator)
	i := strings.LastIndex(clean, sep)
	if i <= 0 {
		return "", "", errors.New("dialing " + path + ": not inside a jail chroot")
	}
	return clean[:i], clean[i+1:], nil
}
