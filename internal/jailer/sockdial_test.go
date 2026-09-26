package jailer

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitJailPath(t *testing.T) {
	cases := []struct {
		path, trusted, rel string
		ok                 bool
	}{
		{"/srv/jailer/firecracker/ab12/root/v.sock", "/srv/jailer/firecracker/ab12", "root/v.sock", true},
		{"/srv/jailer/firecracker/ab12/root/run/firecracker.socket", "/srv/jailer/firecracker/ab12", "root/run/firecracker.socket", true},
		// The last chroot boundary wins, so a "root" higher up in the host
		// path is still part of the trusted prefix.
		{"/root/jail/firecracker/ab12/root/v.sock", "/root/jail/firecracker/ab12", "root/v.sock", true},
		{"relative/root/v.sock", "", "", false},
		{"/srv/jailer/firecracker/ab12/root/../../x/root/v.sock", "", "", false}, // not clean
		{"/srv/jailer/v.sock", "", "", false},                                    // no chroot
		{"/root/v.sock", "", "", false},                                          // empty trusted prefix
	}
	for _, c := range cases {
		trusted, rel, err := splitJailPath(c.path)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.path, err, c.ok)
			continue
		}
		if c.ok && (trusted != c.trusted || rel != c.rel) {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", c.path, trusted, rel, c.trusted, c.rel)
		}
	}
}

func TestDialSocketRefusesPrivilegedOwner(t *testing.T) {
	for _, uid := range []int{0, -1} {
		if _, err := DialSocket(context.Background(), "/srv/j/firecracker/x/root/v.sock", uid); err == nil ||
			!strings.Contains(err.Error(), "unprivileged owner") {
			t.Errorf("uid %d: err = %v, want refusal", uid, err)
		}
	}
}

// jailTree builds <tmp>/<id>/root/v.sock with a listening socket and returns
// the socket path.
func jailTree(t *testing.T) (instance, sock string) {
	t.Helper()
	instance = filepath.Join(t.TempDir(), "ab12")
	if err := os.MkdirAll(filepath.Join(instance, chrootDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(instance, chrootDirName, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return instance, sock
}

func TestDialSocketRefusesNonRootJailDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the temp dir is root-owned when running as root")
	}
	_, sock := jailTree(t)
	if _, err := DialSocket(context.Background(), sock, os.Getuid()); err == nil ||
		!strings.Contains(err.Error(), "not root-only") {
		t.Fatalf("err = %v, want jail dir refusal", err)
	}
}

// The remaining cases need the root-owned jail dir and a chown to a VM-like
// uid, as on a real host: run with sudo go test ./internal/jailer.
const testVMUID = 1_999_999_999

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root (sudo go test ./internal/jailer)")
	}
}

func TestDialSocketConnectsToOwnedSocket(t *testing.T) {
	requireRoot(t)
	_, sock := jailTree(t)
	if err := os.Lchown(sock, testVMUID, testVMUID); err != nil {
		t.Fatal(err)
	}
	c, err := DialSocket(context.Background(), sock, testVMUID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
}

func TestDialSocketRefusesWrongOwner(t *testing.T) {
	requireRoot(t)
	_, sock := jailTree(t) // still root-owned: e.g. a host socket linked in
	if _, err := DialSocket(context.Background(), sock, testVMUID); err == nil ||
		!strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("err = %v, want owner refusal", err)
	}
}

func TestDialSocketRefusesSymlinks(t *testing.T) {
	requireRoot(t)
	instance, sock := jailTree(t)
	if err := os.Lchown(sock, testVMUID, testVMUID); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(instance, chrootDirName)

	// A planted link to the socket itself — the attack target would be a host
	// socket, but any symlink must be refused.
	link := filepath.Join(root, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	// A planted directory link on the way to the socket.
	if err := os.Symlink(root, filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, filepath.Join(root, "run", "v.sock")} {
		if _, err := DialSocket(context.Background(), p, testVMUID); err == nil {
			t.Errorf("%s: dial through a symlink succeeded", p)
		}
	}
}
