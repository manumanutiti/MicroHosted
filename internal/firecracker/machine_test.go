package firecracker

import (
	"net"
	"testing"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"

	"microhosted/pkg/types"
)

func TestDeriveMAC(t *testing.T) {
	cases := map[string]string{
		"172.16.1.2": "02:00:ac:10:01:02",
		"172.16.1.3": "02:00:ac:10:01:03",
		"10.0.0.1":   "02:00:0a:00:00:01",
	}
	for ipStr, want := range cases {
		got := deriveMAC(net.ParseIP(ipStr))
		if got != want {
			t.Fatalf("deriveMAC(%s) = %s, want %s", ipStr, got, want)
		}
	}
}

// Two different guest IPs must never yield the same MAC — the whole reason the
// field exists (shared-bridge collisions).
func TestDeriveMACUnique(t *testing.T) {
	a := deriveMAC(net.ParseIP("172.16.1.2"))
	b := deriveMAC(net.ParseIP("172.16.1.3"))
	if a == b {
		t.Fatalf("distinct IPs produced the same MAC: %s", a)
	}
}

// A networked VM must get nameservers, or DNS resolution silently fails in the
// guest even though IP-based traffic works fine — the bug this test guards
// against (google.com failed to resolve while 8.8.8.8 pinged fine).
func TestBuildConfigSetsNameservers(t *testing.T) {
	vm := types.VMConfig{
		ID:        "abc12345",
		Kernel:    "/img/vmlinux",
		Rootfs:    "/img/rootfs.ext4",
		VCPUs:     1,
		MemMB:     128,
		TapDevice: "tapabc12345",
		GuestIP:   "172.16.1.2",
		GatewayIP: "172.16.1.1",
		PrefixLen: 24,
	}
	cfg, err := BuildConfig(vm, types.IOLimits{}, fc.JailerConfig{})
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if len(cfg.NetworkInterfaces) != 1 {
		t.Fatalf("expected 1 network interface, got %d", len(cfg.NetworkInterfaces))
	}
	ns := cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration.Nameservers
	if len(ns) == 0 {
		t.Fatalf("expected nameservers to be set, got none")
	}
}

// NoNetwork VMs (no TapDevice) must not get a network interface at all.
func TestBuildConfigNoNetwork(t *testing.T) {
	vm := types.VMConfig{ID: "abc12345", Kernel: "/img/vmlinux", Rootfs: "/img/rootfs.ext4", VCPUs: 1, MemMB: 128}
	cfg, err := BuildConfig(vm, types.IOLimits{}, fc.JailerConfig{})
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if len(cfg.NetworkInterfaces) != 0 {
		t.Fatalf("expected no network interfaces for a no-network VM, got %d", len(cfg.NetworkInterfaces))
	}
}

// Every drive (rootfs and volumes) and both directions of the NIC carry the
// VM's limits; a boot burst keeps the disk from throttling the boot itself.
func TestBuildConfigRateLimiters(t *testing.T) {
	vm := types.VMConfig{
		ID: "abc12345", Kernel: "/img/vmlinux", Rootfs: "/img/rootfs.ext4", VCPUs: 1, MemMB: 128,
		TapDevice: "tapabc12345", GuestIP: "172.16.1.2", GatewayIP: "172.16.1.1", PrefixLen: 24,
		Volumes: []types.VolumeMount{{DriveID: "vol1", HostPath: "/v/1.ext4"}},
	}
	io := types.IOLimits{DiskMiBs: 50, DiskIOPS: 1000, NetMbit: 80}
	cfg, err := BuildConfig(vm, io, fc.JailerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Drives) != 2 {
		t.Fatalf("drives = %d, want 2", len(cfg.Drives))
	}
	for _, d := range cfg.Drives {
		rl := d.RateLimiter
		if rl == nil || rl.Bandwidth == nil || rl.Ops == nil {
			t.Fatalf("drive %s: limiter = %+v", *d.DriveID, rl)
		}
		if *rl.Bandwidth.Size != 50<<20 || *rl.Bandwidth.RefillTime != 1000 || *rl.Ops.Size != 1000 {
			t.Errorf("drive %s: bandwidth %d/%dms, ops %d", *d.DriveID, *rl.Bandwidth.Size, *rl.Bandwidth.RefillTime, *rl.Ops.Size)
		}
		if rl.Bandwidth.OneTimeBurst == nil || *rl.Bandwidth.OneTimeBurst != diskBootBurstBytes {
			t.Errorf("drive %s: no boot burst", *d.DriveID)
		}
	}
	nic := cfg.NetworkInterfaces[0]
	for _, rl := range []*struct{ in bool }{{true}, {false}} {
		lim := nic.OutRateLimiter
		if rl.in {
			lim = nic.InRateLimiter
		}
		if lim == nil || lim.Bandwidth == nil || *lim.Bandwidth.Size != 80*1_000_000/8 || lim.Bandwidth.OneTimeBurst != nil {
			t.Errorf("nic (in=%v): limiter = %+v", rl.in, lim)
		}
	}
}

// Zero limits (the operator lifted them) configure no limiter at all.
func TestBuildConfigUnlimited(t *testing.T) {
	vm := types.VMConfig{ID: "abc12345", Kernel: "/img/vmlinux", Rootfs: "/img/rootfs.ext4", VCPUs: 1, MemMB: 128,
		TapDevice: "tapabc12345", GuestIP: "172.16.1.2", GatewayIP: "172.16.1.1", PrefixLen: 24}
	cfg, err := BuildConfig(vm, types.IOLimits{}, fc.JailerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drives[0].RateLimiter != nil || cfg.NetworkInterfaces[0].InRateLimiter != nil || cfg.NetworkInterfaces[0].OutRateLimiter != nil {
		t.Error("limiters configured with no limits")
	}
}
