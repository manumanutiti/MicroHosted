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
	cfg, err := BuildConfig(vm, fc.JailerConfig{})
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
	cfg, err := BuildConfig(vm, fc.JailerConfig{})
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if len(cfg.NetworkInterfaces) != 0 {
		t.Fatalf("expected no network interfaces for a no-network VM, got %d", len(cfg.NetworkInterfaces))
	}
}
