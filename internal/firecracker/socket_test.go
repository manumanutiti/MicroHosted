package firecracker

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"

	"microhosted/internal/jailer"
	"microhosted/pkg/types"
)

// Regression (audit NV-1): when the VMM exits, the SDK unlinks the host-side
// API socket as root, by path. No directory between the root-owned
// InstanceDir and the socket may belong to the jailed uid, or a compromised
// VMM swaps it for a symlink and root deletes a file outside the jail. The
// socket must sit at the chroot's top level on both launch paths.
func TestAPISocketAtChrootTopLevel(t *testing.T) {
	const id = "a1b2c3d4"
	d := jailer.DefaultDefaults()
	d.ChrootBaseDir = t.TempDir()
	jcfg := jailer.Build(id, "/k", d, 100000, io.Discard, io.Discard)

	boot, err := BuildConfig(types.VMConfig{ID: id, VCPUs: 1, MemMB: 128, Rootfs: "/r", Kernel: "/k"}, types.IOLimits{}, jcfg)
	if err != nil {
		t.Fatal(err)
	}
	restore := BuildRestoreConfig(id, "/r", jcfg)
	for name, cfg := range map[string]fc.Config{"boot": boot, "restore": restore} {
		// Where Firecracker, inside the chroot, creates it (the SDK passes
		// this as --api-sock).
		if cfg.SocketPath != APISocketPath {
			t.Errorf("%s: SocketPath %q, want %q", name, cfg.SocketPath, APISocketPath)
		}
		m, err := fc.NewMachine(context.Background(), cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// What the SDK will os.Remove as root once the VMM exits.
		if got, want := filepath.Dir(m.Cfg.SocketPath), jailer.WorkspaceRoot(d, id); got != want {
			t.Errorf("%s: API socket %s is not at the chroot's top level %s", name, m.Cfg.SocketPath, want)
		}
	}
}
