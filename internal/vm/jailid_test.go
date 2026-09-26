package vm

import (
	"errors"
	"testing"
	"time"

	"microhosted/internal/jailer"
	"microhosted/pkg/types"
)

func testPool(count int, live map[int]bool) *idPool {
	p := newIDPool(jailer.Defaults{IDBase: jailer.DefaultIDBase, IDCount: count})
	p.live = func() (map[int]bool, error) { return live, nil }
	return p
}

func TestIDPoolUniqueAndRoundRobin(t *testing.T) {
	p := testPool(3, nil)
	a, _ := p.alloc("vm:a")
	b, _ := p.alloc("vm:b")
	if a == b {
		t.Fatal("two owners got the same identity")
	}
	p.release(a, "vm:a")
	// A released identity is reused as late as possible: c takes the unused
	// third one first.
	c, _ := p.alloc("vm:c")
	if c == a {
		t.Fatalf("released identity %d reused while a never-used one was free", a)
	}
	d, err := p.alloc("vm:d")
	if err != nil || d != a {
		t.Fatalf("alloc = %d, %v; want the released %d once nothing else is free", d, err, a)
	}
	if _, err := p.alloc("vm:e"); !errors.Is(err, ErrConflict) {
		t.Fatalf("alloc on an exhausted range = %v, want ErrConflict", err)
	}
}

// An identity some process still runs as is never handed out.
func TestIDPoolSkipsLiveProcesses(t *testing.T) {
	p := testPool(2, map[int]bool{jailer.DefaultIDBase: true})
	uid, err := p.alloc("vm:a")
	if err != nil || uid != jailer.DefaultIDBase+1 {
		t.Fatalf("alloc = %d, %v", uid, err)
	}
	if _, err := p.alloc("vm:b"); err == nil {
		t.Fatal("handed out an identity a live process runs as")
	}
}

// Without a process table the allocator cannot rule out a live process: it
// refuses.
func TestIDPoolFailsClosed(t *testing.T) {
	p := testPool(2, nil)
	p.live = func() (map[int]bool, error) { return nil, errors.New("no /proc") }
	if _, err := p.alloc("vm:a"); err == nil {
		t.Fatal("allocated without checking live processes")
	}
}

func TestIDPoolTakeAndRelease(t *testing.T) {
	p := testPool(4, nil)
	uid := jailer.DefaultIDBase + 2
	if !p.take(uid, "vm:a") {
		t.Fatal("take of a free in-range identity failed")
	}
	if p.take(uid, "vm:b") {
		t.Fatal("took an identity another owner holds")
	}
	if p.take(123, "vm:c") || p.take(0, "vm:c") {
		t.Fatal("took an identity outside the range")
	}
	p.release(uid, "vm:b") // not the owner: no-op
	if p.take(uid, "vm:b") {
		t.Fatal("release by a non-owner freed the identity")
	}
}

// A record from before per-VM identities (JailUID 0, or the old shared 123)
// gets an identity of its own at startup, persisted; two such records never
// share one.
func TestReconcileAssignsIdentityToLegacyRecords(t *testing.T) {
	m := newTestManager(t)
	a := &types.VM{Config: types.VMConfig{ID: "aaaa0001"}, State: types.VMStateStopped, CreatedAt: time.Now()}
	b := &types.VM{Config: types.VMConfig{ID: "bbbb0002", JailUID: 123}, State: types.VMStateStopped, CreatedAt: time.Now()}
	m.Reconcile([]*types.VM{a, b})

	ga, _ := m.Get("aaaa0001")
	gb, _ := m.Get("bbbb0002")
	if !m.jailerCfg.ContainsID(ga.Config.JailUID) || !m.jailerCfg.ContainsID(gb.Config.JailUID) {
		t.Fatalf("identities not assigned: %d, %d", ga.Config.JailUID, gb.Config.JailUID)
	}
	if ga.Config.JailUID == gb.Config.JailUID {
		t.Fatal("two VMs share an identity")
	}
	recs, err := m.store.ListVMs()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if !m.jailerCfg.ContainsID(r.Config.JailUID) {
			t.Errorf("vm %s: identity %d not persisted", r.Config.ID, r.Config.JailUID)
		}
	}
}

// Two persisted records claiming the same identity (corruption, a copied
// database): the second gets a fresh one instead of sharing.
func TestReconcileSplitsDuplicateIdentity(t *testing.T) {
	m := newTestManager(t)
	uid := jailer.DefaultIDBase + 5
	a := &types.VM{Config: types.VMConfig{ID: "aaaa0001", JailUID: uid}, State: types.VMStateStopped, CreatedAt: time.Now()}
	b := &types.VM{Config: types.VMConfig{ID: "bbbb0002", JailUID: uid}, State: types.VMStateStopped, CreatedAt: time.Now()}
	m.Reconcile([]*types.VM{a, b})
	ga, _ := m.Get("aaaa0001")
	gb, _ := m.Get("bbbb0002")
	if ga.Config.JailUID != uid || gb.Config.JailUID == uid {
		t.Fatalf("identities = %d, %d", ga.Config.JailUID, gb.Config.JailUID)
	}
}

// Nothing boots without an identity from the range: jailer would run it as
// whatever it is handed, and 0 is root.
func TestBootRefusesMissingIdentity(t *testing.T) {
	m := newTestManager(t)
	for _, uid := range []int{0, 123, jailer.DefaultIDBase + 16} {
		rec := &types.VM{Config: types.VMConfig{ID: "cccc0003", JailUID: uid}}
		if err := m.boot(rec); !errors.Is(err, ErrConflict) {
			t.Errorf("boot with uid %d = %v, want ErrConflict", uid, err)
		}
	}
}
