package events

import (
	"testing"
	"time"

	"microhosted/pkg/types"
)

func publishN(b *Bus, n int) {
	for i := 0; i < n; i++ {
		b.Publish(types.Event{Type: types.EventVMCreated})
	}
}

func seqs(evs []types.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPublishStamps(t *testing.T) {
	b := NewBus(4)
	b.Publish(types.Event{Type: types.EventVMDied, VM: "a0000001"})
	backlog, reset, at, sub := b.Subscribe("", 0, true)
	defer sub.Close()
	if reset != "" || at != 0 || len(backlog) != 1 {
		t.Fatalf("backlog %v reset %q at %d", backlog, reset, at)
	}
	e := backlog[0]
	if e.Epoch != b.Epoch() || e.Seq != 1 || e.Time.IsZero() || e.VM != "a0000001" {
		t.Errorf("event not stamped: %+v", e)
	}
	if NewBus(4).Epoch() == b.Epoch() {
		t.Error("two buses (two daemon runs) share an epoch")
	}
}

func TestSubscribeResumes(t *testing.T) {
	b := NewBus(4)
	publishN(b, 6) // ring keeps 3..6

	for _, tc := range []struct {
		name      string
		epoch     string
		seq       uint64
		fromStart bool
		reset     bool
		want      []uint64
		at        uint64
	}{
		{"from the start", "", 0, true, false, []uint64{3, 4, 5, 6}, 2},
		{"mid-ring", b.Epoch(), 4, false, false, []uint64{5, 6}, 4},
		{"just before the ring", b.Epoch(), 2, false, false, []uint64{3, 4, 5, 6}, 2},
		{"up to date", b.Epoch(), 6, false, false, nil, 6},
		{"fell off the ring", b.Epoch(), 1, false, true, []uint64{3, 4, 5, 6}, 2},
		{"another epoch", "deadbeef", 5, false, true, []uint64{3, 4, 5, 6}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backlog, reset, at, sub := b.Subscribe(tc.epoch, tc.seq, tc.fromStart)
			defer sub.Close()
			if (reset != "") != tc.reset {
				t.Errorf("reset = %q, want reset=%v", reset, tc.reset)
			}
			if !equal(seqs(backlog), tc.want) || at != tc.at {
				t.Errorf("backlog %v at %d, want %v at %d", seqs(backlog), at, tc.want, tc.at)
			}
		})
	}
}

// Backlog and live events meet with no gap and no duplicate.
func TestLiveAfterBacklog(t *testing.T) {
	b := NewBus(8)
	publishN(b, 2)
	backlog, _, _, sub := b.Subscribe(b.Epoch(), 1, false)
	defer sub.Close()
	publishN(b, 1)
	got := append(seqs(backlog), (<-sub.C).Seq)
	if !equal(got, []uint64{2, 3}) {
		t.Errorf("got %v, want [2 3]", got)
	}
}

// Publish never waits for a subscriber: a full one is dropped, and says why.
func TestSlowSubscriberIsDropped(t *testing.T) {
	b := NewBus(8)
	_, _, _, slow := b.Subscribe("", 0, true)
	_, _, _, fast := b.Subscribe("", 0, true)
	defer fast.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer+10; i++ {
			b.Publish(types.Event{Type: types.EventVMCreated})
			<-fast.C
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that does not read")
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != subBuffer || slow.Reason() != DropSlow {
		t.Errorf("slow subscriber got %d events, reason %q; want %d then DropSlow", n, slow.Reason(), subBuffer)
	}
}

func TestShutdownClosesSubscriptions(t *testing.T) {
	b := NewBus(8)
	_, _, _, sub := b.Subscribe("", 0, true)
	b.Shutdown()
	if _, ok := <-sub.C; ok || sub.Reason() != DropShutdown {
		t.Errorf("after Shutdown: open=%v reason=%q", ok, sub.Reason())
	}
	sub.Close() // idempotent after a drop
	var nilBus *Bus
	nilBus.Publish(types.Event{Type: types.EventVMDied}) // must not panic
}
