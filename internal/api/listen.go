package api

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Socket permissions: rw for owner and group once a group owns it, rw for root
// alone otherwise. Never any bits for "other" — this socket is root-equivalent.
const (
	socketModeOwner = 0o600
	socketModeGroup = 0o660
)

// ListenUnix opens the API's listening socket at path, with the filesystem as
// its access control. That is the whole authentication story, and it is a
// deliberate one: this API creates VMs, runs commands inside them and rewrites
// the netfilter policy, all from a daemon running as root, so "who may call it"
// is exactly the question "who may open this file" — which the host already
// knows how to express, audit with ls, and revoke. No shared secret to
// distribute, rotate or leak into a shell history.
//
// group, when set, owns the socket at 0660 so a named group of operators can
// drive the daemon without sudo. Empty leaves it root-only.
func ListenUnix(path, group string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the socket's directory: %w", err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	// Create the socket already unreachable instead of widening it afterwards.
	// net.Listen would otherwise create it at 0777&^umask, leaving a window —
	// however short — in which any user on the host can call a root API. The
	// mask can only make the socket MORE restrictive than its final mode, so
	// the window errs in the safe direction. Umask is process-global and this
	// runs once at startup, before anything else here creates files.
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}

	mode := os.FileMode(socketModeOwner)
	if group != "" {
		gid, gerr := lookupGID(group)
		if gerr != nil {
			ln.Close()
			return nil, gerr
		}
		if err := os.Chown(path, -1, gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("handing %s to group %q: %w", path, group, err)
		}
		mode = socketModeGroup
	}
	// Explicit rather than relying on the mask: the mode is a security
	// boundary, so it gets stated, not inherited.
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("setting permissions on %s: %w", path, err)
	}
	return ln, nil
}

// clearStaleSocket removes a leftover socket file, but only after proving that
// nobody is serving on it. Unlinking a live socket is worse than failing to
// start: the running daemon keeps serving its existing clients while every new
// one silently reaches the newcomer, so two daemons end up managing the same
// VMs. A successful dial means a real instance is up and the correct answer is
// to refuse.
func clearStaleSocket(path string) error {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket — refusing to remove it", path)
	}
	if conn, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
		conn.Close()
		return fmt.Errorf("another microhosted is already serving on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing the stale socket %s: %w", path, err)
	}
	return nil
}

// lookupGID resolves a group name to its numeric id, failing loudly: a typo in
// --socket-group must not silently leave the socket root-only, because the
// operator would read that as "the daemon is broken" and reach for chmod.
func lookupGID(group string) (int, error) {
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("looking up group %q: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %q has a non-numeric gid %q: %w", group, g.Gid, err)
	}
	return gid, nil
}
