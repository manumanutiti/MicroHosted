package firecracker

import (
	"context"
	"net/http"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"microhosted/pkg/types"
)

// diskBootBurst is the one-time allowance on top of a drive's bandwidth
// limit: the guest reads a good part of its rootfs while booting, and a boot
// throttled to the steady-state rate would cost the deploy time VMs are
// meant for. It is spent once per boot and never refills.
const (
	diskBootBurstBytes = 128 << 20
	diskBootBurstOps   = 8192
)

// refillMs makes every bucket a per-second rate: size tokens per second.
const refillMs = 1000

func bucket(size, burst int64) *models.TokenBucket {
	b := &models.TokenBucket{Size: fc.Int64(size), RefillTime: fc.Int64(refillMs)}
	if burst > 0 {
		b.OneTimeBurst = fc.Int64(burst)
	}
	return b
}

// DiskRateLimiter is the limiter for each of a VM's drives, nil when its
// limits leave the disk unlimited.
func DiskRateLimiter(l types.IOLimits) *models.RateLimiter {
	if l.DiskMiBs <= 0 && l.DiskIOPS <= 0 {
		return nil
	}
	rl := &models.RateLimiter{}
	if l.DiskMiBs > 0 {
		rl.Bandwidth = bucket(l.DiskMiBs<<20, diskBootBurstBytes)
	}
	if l.DiskIOPS > 0 {
		rl.Ops = bucket(l.DiskIOPS, diskBootBurstOps)
	}
	return rl
}

// NetRateLimiter is the limiter for each direction of a VM's NIC, nil when
// unlimited. No burst: nothing at boot needs one, and a burst is exactly what
// a flood from a compromised VM would spend first.
func NetRateLimiter(l types.IOLimits) *models.RateLimiter {
	if l.NetMbit <= 0 {
		return nil
	}
	return &models.RateLimiter{Bandwidth: bucket(l.NetMbit*1_000_000/8, 0)}
}

// PatchDriveRateLimiter sets a drive's limiter on a VM that was not booted
// with it — a restored one, whose devices come from the vmstate.
func PatchDriveRateLimiter(ctx context.Context, socketPath string, uid int, driveID string, rl *models.RateLimiter) error {
	return apiCall(ctx, socketPath, uid, http.MethodPatch, "/drives/"+driveID, map[string]interface{}{
		"drive_id":     driveID,
		"rate_limiter": rl,
	})
}

// PatchNetRateLimiter sets both directions of a NIC's limiter: rx is what
// reaches the guest, tx what it sends.
func PatchNetRateLimiter(ctx context.Context, socketPath string, uid int, ifaceID string, rl *models.RateLimiter) error {
	return apiCall(ctx, socketPath, uid, http.MethodPatch, "/network-interfaces/"+ifaceID, map[string]interface{}{
		"iface_id":        ifaceID,
		"rx_rate_limiter": rl,
		"tx_rate_limiter": rl,
	})
}
