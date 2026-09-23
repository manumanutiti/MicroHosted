package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"microhosted/internal/events"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// heartbeat is how often an idle event stream sends an SSE comment, so both
// ends notice a dead connection instead of waiting on it forever.
const heartbeat = 15 * time.Second

// registerEventRoutes serves GET /v1/events: the bus as Server-Sent Events.
//
// Each event goes out as `id: <epoch>:<seq>`, `event: <type>`, `data: <JSON>`.
// A subscriber resumes by sending the last id it got back — as Last-Event-ID
// (what an SSE client does by itself on reconnect) or ?since= — and receives
// what it missed first. When that is impossible it gets a `reset` event and
// must re-read the state. Without a position the stream starts with every
// event the daemon still keeps; ?since=now starts with live events only. An
// `id:` without data plus a `: live` comment mark where the backlog ends.
//
// Filters: ?type=T (repeatable) keeps those types; ?label=k=v (repeatable)
// keeps vm.* events of VMs carrying those labels. Host events (no VM) and
// resets always pass the label filter: a subscriber interested in some VMs
// still needs to know that the firewall failed or that it must resync.
func registerEventRoutes(mux *http.ServeMux, bus *events.Bus) {
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sel, err := vm.ParseLabelSelector(q["label"])
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		want := make(map[string]bool)
		for _, t := range q["type"] {
			for _, t := range strings.Split(t, ",") {
				if t != "" {
					want[t] = true
				}
			}
		}
		pos := r.Header.Get("Last-Event-ID")
		if s := q.Get("since"); s != "" {
			pos = s
		}
		var (
			epoch     string
			seq       uint64
			fromStart bool
		)
		if pos == "now" {
			// Positioned at the end: nothing to catch up on.
			epoch, seq = bus.Epoch(), math.MaxUint64
		} else if epoch, seq, fromStart, err = parseEventID(pos); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
			return
		}

		backlog, reset, at, sub := bus.Subscribe(epoch, seq, fromStart)
		defer sub.Close()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		pass := func(e types.Event) bool {
			if len(want) > 0 && !want[e.Type] && e.Type != types.EventReset {
				return false
			}
			return e.VM == "" || sel.Matches(e.Labels)
		}
		send := func(e types.Event) error {
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "id: %s:%d\nevent: %s\ndata: %s\n\n", e.Epoch, e.Seq, e.Type, data)
			return err
		}

		if reset != "" {
			if send(types.Event{Epoch: bus.Epoch(), Seq: at, Time: time.Now().UTC(), Type: types.EventReset, Reason: reset}) != nil {
				return
			}
		}
		for _, e := range backlog {
			if pass(e) && send(e) != nil {
				return
			}
		}
		// The position the backlog brought the subscriber to, as an id with no
		// data: SSE clients adopt it as their Last-Event-ID without an event,
		// so one that reconnects before any event reaches it (filtered out, or
		// just a quiet host) still resumes here instead of from scratch.
		if _, err := fmt.Fprintf(w, "id: %s:%d\n: live\n\n", bus.Epoch(), at+uint64(len(backlog))); err != nil {
			return
		}
		fl.Flush()

		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-sub.C:
				// Closed by the bus (slow, shutdown): end the stream; the
				// client resumes from the last id it got.
				if !ok {
					return
				}
				if !pass(e) {
					continue
				}
				if send(e) != nil {
					return
				}
				fl.Flush()
			case <-tick.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	})
}

// parseEventID reads "<epoch>:<seq>"; empty means "from the start".
func parseEventID(s string) (epoch string, seq uint64, fromStart bool, err error) {
	if s == "" {
		return "", 0, true, nil
	}
	e, n, ok := strings.Cut(s, ":")
	if ok {
		seq, err = strconv.ParseUint(n, 10, 64)
	}
	if !ok || err != nil || e == "" {
		return "", 0, false, fmt.Errorf("event id %q: want <epoch>:<seq>, as sent in the stream's id field", s)
	}
	return e, seq, false, nil
}
