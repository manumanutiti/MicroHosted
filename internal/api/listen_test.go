package api

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestListenUnixIsRootOnly pins the default the whole authentication model
// rests on: with no owning group the socket must be unreachable to every other
// user on the host. A regression here is silent — the daemon still works, it is
// just callable by anyone.
func TestListenUnixIsRootOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenUnix(path, "")
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer ln.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != socketModeOwner {
		t.Fatalf("socket mode %#o, want %#o — it is reachable beyond its owner", got, socketModeOwner)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", path, fi.Mode())
	}
}

// TestListenUnixRefusesLiveSocket covers the split-brain case: a second daemon
// must not unlink a socket someone is serving on, because both would then
// manage the same VMs while clients silently reach one or the other.
func TestListenUnixRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenUnix(path, "")
	if err != nil {
		t.Fatalf("first ListenUnix: %v", err)
	}
	defer ln.Close()

	second, err := ListenUnix(path, "")
	if err == nil {
		second.Close()
		t.Fatal("ListenUnix stole a socket another instance was serving on")
	}
	if !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("got %v, want the already-serving refusal", err)
	}
}

// TestListenUnixReplacesStaleSocket is the other half: a socket left behind by
// a daemon that died must not block the next start, or a crash turns into an
// outage that needs a human with rm.
func TestListenUnixReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("staging a stale socket: %v", err)
	}
	// Keep the file after closing, which is exactly what a killed process
	// leaves behind.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	ln, err := ListenUnix(path, "")
	if err != nil {
		t.Fatalf("ListenUnix over a stale socket: %v", err)
	}
	ln.Close()
}

// TestListenUnixRefusesNonSocket keeps the stale-socket cleanup from becoming
// an arbitrary delete: point --socket at a real file by accident and the daemon
// must complain, not remove it.
func TestListenUnixRefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := ListenUnix(path, "")
	if err == nil {
		ln.Close()
		t.Fatal("ListenUnix accepted a regular file as its socket path")
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the regular file was removed: %v", serr)
	}
}

// TestListenUnixRejectsUnknownGroup: a typo in --socket-group must fail the
// start, not quietly leave a root-only socket the operator then "fixes" with
// chmod.
func TestListenUnixRejectsUnknownGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenUnix(path, "no-such-group-exists-here")
	if err == nil {
		ln.Close()
		t.Fatal("ListenUnix accepted a group that does not exist")
	}
	if _, serr := os.Stat(path); serr == nil {
		t.Fatal("a socket was left behind after the group lookup failed")
	}
}
