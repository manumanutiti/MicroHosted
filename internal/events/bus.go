// Package events is the daemon's event bus: what happened to VMs and to the
// host, pushed to whoever subscribes (GET /v1/events) instead of polled.
//
// Two rules shape it. The engine never waits for a subscriber: Publish only
// appends to a ring and hands the event to buffered channels, and a subscriber
// whose buffer is full is dropped (its stream ends). And no subscriber misses
// an event without being told: the ring keeps the recent past so a
// reconnecting subscriber — dropped, or just disconnected — resumes where it
// left off, and when that is impossible (the daemon restarted, the gap fell
// off the ring) it gets a reset and must re-read the state instead.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"microhosted/pkg/types"
)

const (
	// DefaultCapacity is how many past events the ring keeps for resuming.
	DefaultCapacity = 1024
	// subBuffer is how far a subscriber may lag behind live events before it
	// is dropped.
	subBuffer = 256
)

// Why a subscriber must resync: the reason of a reset.
const (
	ResetNewEpoch = "daemon restarted: events before this point are gone"
	ResetGap      = "the events since that point are no longer kept"
)

// Why a subscription was closed from the bus's side (Subscription.Reason). The
// subscriber just resumes from the last event it got: if what it missed is
// still in the ring it gets it, and a reset otherwise.
const (
	DropSlow     = "subscriber too slow"
	DropShutdown = "daemon shutting down"
)

// Bus fans events out to subscribers and keeps the recent ones.
type Bus struct {
	epoch string
	now   func() time.Time

	mu   sync.Mutex
	seq  uint64
	ring []types.Event // oldest first, at most cap(ring)
	subs map[*Subscription]struct{}
}

// NewBus returns a bus keeping the last capacity events (DefaultCapacity if
// capacity <= 0), under a fresh random epoch.
func NewBus(capacity int) *Bus {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &Bus{
		epoch: hex.EncodeToString(b),
		now:   time.Now,
		ring:  make([]types.Event, 0, capacity),
		subs:  make(map[*Subscription]struct{}),
	}
}

// Epoch identifies this run of the daemon.
func (b *Bus) Epoch() string { return b.epoch }

// Publish stamps e (epoch, seq, time) and delivers it. Safe on a nil Bus, which
// drops everything — code paths that run without a bus (tests) need no guard.
func (b *Bus) Publish(e types.Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Epoch, e.Seq, e.Time = b.epoch, b.seq, b.now().UTC()
	if len(b.ring) == cap(b.ring) {
		copy(b.ring, b.ring[1:])
		b.ring = b.ring[:len(b.ring)-1]
	}
	b.ring = append(b.ring, e)
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			b.dropLocked(s, DropSlow)
		}
	}
}

// Subscription is one subscriber's live feed. Events arrive on C; when C is
// closed, Reason says why (DropSlow, DropShutdown), or is empty after Close.
type Subscription struct {
	C      <-chan types.Event
	ch     chan types.Event
	reason string
	bus    *Bus
}

// Reason is why C was closed; read it only after C is closed.
func (s *Subscription) Reason() string {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.reason
}

// Close unsubscribes. Idempotent.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if _, ok := s.bus.subs[s]; ok {
		delete(s.bus.subs, s)
		close(s.ch)
	}
}

func (b *Bus) dropLocked(s *Subscription, reason string) {
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	s.reason = reason
	close(s.ch)
}

// Subscribe starts a feed resuming after the event (epoch, seq). It returns
// the events the subscriber missed, then live ones on the Subscription. With
// fromStart (no position given) the backlog is the whole ring. When resuming
// is impossible — another epoch, or seq older than the ring reaches — reset is
// the reason and backlog is the whole ring: the subscriber must re-read the
// state, and can then carry on from these events. at is the position the
// backlog starts after (the last seq before it), for the reset's id.
func (b *Bus) Subscribe(epoch string, seq uint64, fromStart bool) (backlog []types.Event, reset string, at uint64, sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch {
	case fromStart:
		backlog = append(backlog, b.ring...)
	case epoch != b.epoch:
		reset = ResetNewEpoch
		backlog = append(backlog, b.ring...)
	case seq >= b.seq:
		// Up to date (or ahead, which only a forged id can be): nothing missed.
	case len(b.ring) == 0 || seq+1 < b.ring[0].Seq:
		reset = ResetGap
		backlog = append(backlog, b.ring...)
	default:
		first := int(seq + 1 - b.ring[0].Seq)
		backlog = append(backlog, b.ring[first:]...)
	}

	at = b.seq
	if len(backlog) > 0 {
		at = backlog[0].Seq - 1
	}
	ch := make(chan types.Event, subBuffer)
	sub = &Subscription{C: ch, ch: ch, bus: b}
	b.subs[sub] = struct{}{}
	return backlog, reset, at, sub
}

// Shutdown closes every live subscription (DropShutdown), so streaming
// handlers return and the HTTP server can stop.
func (b *Bus) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		b.dropLocked(s, DropShutdown)
	}
}
