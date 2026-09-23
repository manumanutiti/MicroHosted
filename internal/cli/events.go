package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"microhosted/pkg/types"
)

// sysEvents follows GET /v1/events. By default it shows what happens from now
// on and keeps following across daemon restarts (reconnecting, resuming from
// the last event it printed); --all starts with every event the daemon keeps,
// --no-follow stops once those are printed.
func sysEvents(e *env, cmd *command, p string, args []string) error {
	var labels, typeList []string
	var all, noFollow, asJSON bool
	var since string
	fs := newCmdFlags(e, p, cmd)
	fs.listVar(&labels, "label", "l", "only events of VMs labelled `KEY=VALUE` (repeatable); host events always show")
	fs.listVar(&typeList, "type", "t", "only events of `TYPE`, e.g. vm.died (repeatable)")
	fs.boolVar(&all, "all", "a", "start with every event the daemon still keeps, not just new ones")
	fs.stringVar(&since, "since", "", "", "resume after event `ID` (the id --json prints as epoch:seq)")
	fs.boolVar(&noFollow, "no-follow", "", "print the kept events and exit instead of following")
	fs.boolVar(&asJSON, "json", "", "one JSON event per line")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef(p, "unexpected argument %q", pos[0])
	}
	if since != "" && all {
		return usagef(p, "--since and --all are mutually exclusive")
	}
	if noFollow && since == "" {
		all = true
	}
	c, err := e.api()
	if err != nil {
		return err
	}

	q := url.Values{}
	for _, l := range labels {
		q.Add("label", l)
	}
	for _, t := range typeList {
		q.Add("type", t)
	}
	last := since
	if last == "" && !all {
		last = "now"
	}
	show := func(ev types.Event) {
		if asJSON {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(e.stdout, string(b))
			return
		}
		fmt.Fprintln(e.stdout, fmtEvent(ev))
	}

	backoff := time.Second
	for {
		if last != "" {
			q.Set("since", last)
		}
		live, err := readEvents(c, "/v1/events?"+q.Encode(), noFollow, func(id string) { last = id }, show)
		if noFollow {
			return err
		}
		if live {
			backoff = time.Second
		}
		if err != nil {
			// A 4xx is the request's fault: retrying cannot fix it.
			if ae, ok := err.(*APIError); ok && ae.Status < 500 {
				return err
			}
			fmt.Fprintf(e.stderr, "mh events: %v; reconnecting in %s\n", err, backoff)
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 10*time.Second)
	}
}

// readEvents reads one SSE stream, calling at with every position the stream
// hands out (an event's id, or the bare id after the backlog) and got with
// each event. It returns when the stream ends, or — with stopAtLive — once
// the backlog is done. live reports whether it got as far as live events.
func readEvents(c *Client, path string, stopAtLive bool, at func(id string), got func(types.Event)) (live bool, err error) {
	resp, err := c.Raw(path)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return false, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var id, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if id != "" {
				at(id)
			}
			if data != "" {
				var ev types.Event
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					return live, fmt.Errorf("bad event from the daemon: %w", err)
				}
				got(ev)
			}
			id, data = "", ""
		case line == ": live":
			live = true
			if stopAtLive {
				return true, nil
			}
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return live, err
	}
	return live, fmt.Errorf("event stream closed by the daemon")
}

// fmtEvent renders one event as a line: time, type, VM, network, reason, data.
func fmtEvent(ev types.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-22s", ev.Time.Local().Format("2006-01-02 15:04:05"), ev.Type)
	if ev.VM != "" {
		who := ev.VM
		if ev.Name != "" {
			who += " (" + ev.Name + ")"
		}
		fmt.Fprintf(&b, "  %s", who)
	}
	if ev.Network != "" {
		fmt.Fprintf(&b, "  net=%s", ev.Network)
	}
	keys := make([]string, 0, len(ev.Data))
	for k := range ev.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s=%s", k, ev.Data[k])
	}
	if ev.Reason != "" {
		fmt.Fprintf(&b, "  — %s", ev.Reason)
	}
	return b.String()
}
