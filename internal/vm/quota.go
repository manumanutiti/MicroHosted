package vm

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"microhosted/internal/labels"
	"microhosted/pkg/types"
)

// Per-consumer quotas bound what each owner — the value of a VM's managed-by
// label — may hold at once, so one orchestrator gone wrong cannot take the
// whole host from another. They sit inside host admission (memory, disk,
// --max-vms): a launch must pass both.
//
// What counts is what holds memory: running VMs, quarantined ones included,
// and launches in progress; mem is the sum of their configured mem_mb, the
// worst case a guest may touch. A VM without managed-by belongs to no
// consumer and is bounded by host admission only. The label is fixed at
// create (see labels.ManagedBy), so a VM cannot leave its quota.
//
// Anyone who can reach the API is root-equivalent: quotas contain a consumer's
// bugs and floods, they are not a security boundary between consumers.

// ErrQuota is a launch refused because its consumer is at its quota. The API
// answers 429: the host may have room, this consumer does not.
var ErrQuota = errors.New("consumer quota exceeded")

// Quota caps one consumer. A zero field is no cap on that dimension.
type Quota struct {
	MaxVMs   int
	MaxMemMB int64
}

// ParseQuota reads "vms:N,mem:MB" (either part may be left out).
func ParseQuota(s string) (Quota, error) {
	var q Quota
	if s == "" {
		return q, fmt.Errorf("empty quota: want vms:N,mem:MB")
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, ":")
		n, err := strconv.ParseInt(v, 10, 64)
		if !ok || err != nil || n < 1 {
			return q, fmt.Errorf("quota %q: %q: want vms:N or mem:MB with N, MB at least 1", s, part)
		}
		switch k {
		case "vms":
			q.MaxVMs = int(n)
		case "mem":
			q.MaxMemMB = n
		default:
			return q, fmt.Errorf("quota %q: unknown field %q (vms, mem)", s, k)
		}
	}
	return q, nil
}

// ParseConsumerQuota reads "CONSUMER=vms:N,mem:MB", CONSUMER being a
// managed-by value.
func ParseConsumerQuota(s string) (string, Quota, error) {
	consumer, spec, ok := strings.Cut(s, "=")
	if !ok {
		return "", Quota{}, fmt.Errorf("quota %q: want CONSUMER=vms:N,mem:MB", s)
	}
	if consumer == "" || labels.Validate(map[string]string{labels.ManagedBy: consumer}) != nil {
		return "", Quota{}, fmt.Errorf("quota %q: %q is not a valid %s value", s, consumer, labels.ManagedBy)
	}
	q, err := ParseQuota(spec)
	return consumer, q, err
}

// consumerOf is rec's consumer, read under the lock (labels are replaced
// whole by SetLabels and Quarantine, never written into).
func (m *Manager) consumerOf(rec *types.VM) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return rec.Config.Labels[labels.ManagedBy]
}

// quotaForLocked is the quota that applies to consumer: its own, else the
// default for labelled VMs, else none. A VM with no consumer has none.
func (m *Manager) quotaForLocked(consumer string) (Quota, bool) {
	if consumer == "" {
		return Quota{}, false
	}
	if q, ok := m.limits.Quotas[consumer]; ok {
		return q, true
	}
	if m.limits.DefaultQuota != nil {
		return *m.limits.DefaultQuota, true
	}
	return Quota{}, false
}

// usageLocked is what consumer holds now: running VMs plus launches in
// progress, and their mem_mb.
func (m *Manager) usageLocked(consumer string) (vms int, memMB int64) {
	for id, v := range m.vms {
		if _, launching := m.inflight[id]; launching {
			continue
		}
		if v.State == types.VMStateRunning && v.Config.Labels[labels.ManagedBy] == consumer {
			vms++
			memMB += v.Config.MemMB
		}
	}
	for _, l := range m.inflight {
		if l.consumer == consumer {
			vms++
			memMB += l.memMB
		}
	}
	return vms, memMB
}

// checkQuotaLocked refuses a launch of memMB for consumer that would take it
// over its quota.
func (m *Manager) checkQuotaLocked(consumer string, memMB int64) error {
	q, ok := m.quotaForLocked(consumer)
	if !ok {
		return nil
	}
	vms, used := m.usageLocked(consumer)
	if q.MaxVMs > 0 && vms+1 > q.MaxVMs {
		return fmt.Errorf("%w: %s=%s holds %d VMs running or launching, its quota is %d", ErrQuota, labels.ManagedBy, consumer, vms, q.MaxVMs)
	}
	if q.MaxMemMB > 0 && used+memMB > q.MaxMemMB {
		return fmt.Errorf("%w: %s=%s holds %d MB, a %d MB VM would take it to %d MB, over its %d MB quota", ErrQuota, labels.ManagedBy, consumer, used, memMB, used+memMB, q.MaxMemMB)
	}
	return nil
}

// QuotaUsage reports every consumer that has a quota or holds a running VM,
// with its quota (zero = none) and what it holds, sorted by consumer.
func (m *Manager) QuotaUsage() []types.QuotaUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	consumers := make(map[string]bool)
	for c := range m.limits.Quotas {
		consumers[c] = true
	}
	for _, v := range m.vms {
		if c := v.Config.Labels[labels.ManagedBy]; c != "" && v.State == types.VMStateRunning {
			consumers[c] = true
		}
	}
	for _, l := range m.inflight {
		if l.consumer != "" {
			consumers[l.consumer] = true
		}
	}
	out := make([]types.QuotaUsage, 0, len(consumers))
	for c := range consumers {
		q, _ := m.quotaForLocked(c)
		vms, mem := m.usageLocked(c)
		out = append(out, types.QuotaUsage{Consumer: c, MaxVMs: q.MaxVMs, MaxMemMB: q.MaxMemMB, VMs: vms, MemMB: mem})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Consumer < out[j].Consumer })
	return out
}
