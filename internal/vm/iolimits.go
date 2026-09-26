package vm

import (
	"fmt"

	"microhosted/pkg/types"
)

// Default throughput ceiling for every VM (Limits.IO). A compromised VM
// otherwise saturates the storage device or its bridge and degrades every
// other VM on the host; CPU, memory and PIDs are already capped by the cgroup.
// The operator lifts a limit by setting it to 0 explicitly.
const (
	DefaultDiskMiBs = 100
	DefaultDiskIOPS = 4000
	DefaultNetMbit  = 100
)

// EffectiveIO is what a VM gets: per field, the lower of what it asked for
// and the ceiling, where zero is "not asked" in req and "no limit" in ceil.
// Worked out at every boot, so a lowered ceiling reaches every VM, including
// records written before limits existed (req nil).
func EffectiveIO(req *types.IOLimits, ceil types.IOLimits) types.IOLimits {
	if req == nil {
		return ceil
	}
	return types.IOLimits{
		DiskMiBs: lowerLimit(req.DiskMiBs, ceil.DiskMiBs),
		DiskIOPS: lowerLimit(req.DiskIOPS, ceil.DiskIOPS),
		NetMbit:  lowerLimit(req.NetMbit, ceil.NetMbit),
	}
}

func lowerLimit(req, ceil int64) int64 {
	switch {
	case req == 0:
		return ceil
	case ceil == 0 || req < ceil:
		return req
	default:
		return ceil
	}
}

// ValidateIOLimits refuses a request that is negative or asks for more than
// the ceiling: a VM can lower its own limits, never raise them. nil is valid.
func ValidateIOLimits(req *types.IOLimits, ceil types.IOLimits) error {
	if req == nil {
		return nil
	}
	for _, f := range []struct {
		name      string
		req, ceil int64
	}{
		{"disk_mib_s", req.DiskMiBs, ceil.DiskMiBs},
		{"disk_iops", req.DiskIOPS, ceil.DiskIOPS},
		{"net_mbit", req.NetMbit, ceil.NetMbit},
	} {
		if f.req < 0 {
			return fmt.Errorf("%w: io_limits.%s must not be negative", ErrInvalid, f.name)
		}
		if f.ceil > 0 && f.req > f.ceil {
			return fmt.Errorf("%w: io_limits.%s %d exceeds the daemon's ceiling %d", ErrInvalid, f.name, f.req, f.ceil)
		}
	}
	return nil
}

// cloneIOLimits copies a request so the record never shares it with the
// caller or with another record.
func cloneIOLimits(l *types.IOLimits) *types.IOLimits {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

// effectiveIO is EffectiveIO against the ceiling in force.
func (m *Manager) effectiveIO(req *types.IOLimits) types.IOLimits {
	return EffectiveIO(req, m.Limits().IO)
}

// lowerIOLimits combines two requests field by field, keeping the lower of
// what each asks (zero is "not asked"). How a fork keeps its snapshot's limits
// and can only tighten them. nil when neither asks for anything.
func lowerIOLimits(a, b *types.IOLimits) *types.IOLimits {
	switch {
	case a == nil:
		return cloneIOLimits(b)
	case b == nil:
		return cloneIOLimits(a)
	}
	return &types.IOLimits{
		DiskMiBs: lowerAsked(a.DiskMiBs, b.DiskMiBs),
		DiskIOPS: lowerAsked(a.DiskIOPS, b.DiskIOPS),
		NetMbit:  lowerAsked(a.NetMbit, b.NetMbit),
	}
}

func lowerAsked(a, b int64) int64 {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// clampIOLimits brings a request recorded under an older, higher ceiling
// within the current one, so it passes ValidateIOLimits again (a replacement
// re-asks for its predecessor's limits).
func clampIOLimits(req *types.IOLimits, ceil types.IOLimits) *types.IOLimits {
	if req == nil {
		return nil
	}
	c := *req
	for _, f := range []struct {
		v    *int64
		ceil int64
	}{
		{&c.DiskMiBs, ceil.DiskMiBs}, {&c.DiskIOPS, ceil.DiskIOPS}, {&c.NetMbit, ceil.NetMbit},
	} {
		if f.ceil > 0 && *f.v > f.ceil {
			*f.v = f.ceil
		}
	}
	return &c
}
