package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"microhosted/internal/events"
	"microhosted/pkg/types"
)

// frame is one SSE block: an event (with data) or a bare id / comment.
type frame struct {
	id, typ, comment string
	ev               *types.Event
}

type sseStream struct {
	t    *testing.T
	res  *http.Response
	sc   *bufio.Scanner
	next chan frame
}

func openStream(t *testing.T, url string) *sseStream {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	s := &sseStream{t: t, res: res, sc: bufio.NewScanner(res.Body), next: make(chan frame, 64)}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		return s
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	go func() {
		defer close(s.next)
		var f frame
		var data string
		for s.sc.Scan() {
			line := s.sc.Text()
			switch {
			case line == "":
				if data != "" {
					var ev types.Event
					if err := json.Unmarshal([]byte(data), &ev); err == nil {
						f.ev = &ev
					}
				}
				s.next <- f
				f, data = frame{}, ""
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ": "):
				f.comment = strings.TrimPrefix(line, ": ")
			}
		}
	}()
	return s
}

func (s *sseStream) read() (frame, bool) {
	select {
	case f, ok := <-s.next:
		return f, ok
	case <-time.After(5 * time.Second):
		s.t.Fatal("no frame within 5s")
		return frame{}, false
	}
}

// untilLive returns the events before the end of the backlog, and the bare id
// that marks it.
func (s *sseStream) untilLive() (evs []types.Event, pos string) {
	for {
		f, ok := s.read()
		if !ok {
			s.t.Fatal("stream ended before the backlog did")
		}
		if f.comment == "live" {
			return evs, f.id
		}
		if f.ev != nil {
			if want := fmt.Sprintf("%s:%d", f.ev.Epoch, f.ev.Seq); f.id != want || f.typ != f.ev.Type {
				s.t.Errorf("frame id %q event %q for %+v", f.id, f.typ, f.ev)
			}
			evs = append(evs, *f.ev)
		}
	}
}

func types_(evs []types.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type+":"+e.VM)
	}
	return strings.Join(out, " ")
}

func newEventServer(t *testing.T) (*httptest.Server, *events.Bus) {
	bus := events.NewBus(0)
	mux := http.NewServeMux()
	registerEventRoutes(mux, bus)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	bus.Publish(types.Event{Type: types.EventVMCreated, VM: "a0000001", Labels: map[string]string{"sensor": "ts-01"}})
	bus.Publish(types.Event{Type: types.EventVMDied, VM: "b0000001", Labels: map[string]string{"sensor": "ts-02"}})
	bus.Publish(types.Event{Type: types.EventRulesetFailed, Reason: "nft: syntax error"})
	return ts, bus
}

func TestEventsBacklogAndFilters(t *testing.T) {
	ts, bus := newEventServer(t)
	ep := bus.Epoch()

	evs, pos := openStream(t, ts.URL+"/v1/events").untilLive()
	if got := types_(evs); got != "vm.created:a0000001 vm.died:b0000001 network.ruleset_failed:" {
		t.Errorf("full backlog: %s", got)
	}
	if pos != ep+":3" {
		t.Errorf("position after the backlog = %q, want %s:3", pos, ep)
	}

	// Label filter: other VMs' events go, host events stay.
	evs, _ = openStream(t, ts.URL+"/v1/events?label=sensor=ts-01").untilLive()
	if got := types_(evs); got != "vm.created:a0000001 network.ruleset_failed:" {
		t.Errorf("label filter: %s", got)
	}
	// Type filter plus resuming after seq 1.
	evs, _ = openStream(t, ts.URL+"/v1/events?type=vm.died&since="+ep+":1").untilLive()
	if got := types_(evs); got != "vm.died:b0000001" {
		t.Errorf("type filter since 1: %s", got)
	}
	// Last-Event-ID, what an SSE client sends on its own when reconnecting.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/events", nil)
	req.Header.Set("Last-Event-ID", ep+":2")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("Last-Event-ID resume = %d", res.StatusCode)
	}

	if s := openStream(t, ts.URL+"/v1/events?since=garbage"); s.res.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed position = %d, want 400", s.res.StatusCode)
	}
}

// A position from another daemon run cannot be resumed: reset first, then
// everything kept, so the subscriber can resync and carry on.
func TestEventsResetOnNewEpoch(t *testing.T) {
	ts, _ := newEventServer(t)
	s := openStream(t, ts.URL+"/v1/events?since=0badc0de:7")
	f, _ := s.read()
	if f.ev == nil || f.ev.Type != types.EventReset || f.ev.Reason != events.ResetNewEpoch {
		t.Fatalf("first frame = %+v, want a reset (new epoch)", f.ev)
	}
	if evs, _ := s.untilLive(); len(evs) != 3 {
		t.Errorf("after the reset: %d events, want the 3 kept", len(evs))
	}
}

func TestEventsLiveAndShutdown(t *testing.T) {
	ts, bus := newEventServer(t)
	s := openStream(t, ts.URL+"/v1/events?since=now")
	if evs, _ := s.untilLive(); len(evs) != 0 {
		t.Errorf("since=now replayed %d events", len(evs))
	}
	bus.Publish(types.Event{Type: types.EventVMQuarantined, VM: "a0000001"})
	f, _ := s.read()
	if f.ev == nil || f.ev.Type != types.EventVMQuarantined || f.ev.Seq != 4 {
		t.Fatalf("live frame = %+v", f.ev)
	}
	bus.Shutdown()
	if _, ok := s.read(); ok {
		t.Error("stream still open after the bus shut down")
	}
}
