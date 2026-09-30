package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tcpTestServer is newTestServer as `--addr` serves it: behind GuardTCP.
func tcpTestServer(t *testing.T, extraHosts ...string) *httptest.Server {
	t.Helper()
	ts := newTestServer(t)
	ts.Config.Handler = GuardTCP(ts.Config.Handler, extraHosts)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, host, body string, header map[string]string) int {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// The audit's cases (NV-2): a page of another origin posting a "simple"
// request — no preflight — to the API. Refused on every listener.
func TestCrossOriginWritesRefused(t *testing.T) {
	for name, ts := range map[string]*httptest.Server{"unix": newTestServer(t), "tcp": tcpTestServer(t)} {
		for _, h := range []map[string]string{
			{"Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"},
			{"Origin": "http://attacker.example", "Content-Type": "text/plain"}, // a browser without Sec-Fetch-Site
			{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/x-www-form-urlencoded"},
		} {
			if got := do(t, ts, "POST", "/v1/vms/deadbeef/exec", "", `{"cmd":"true"}`, h); got != http.StatusForbidden {
				t.Errorf("%s: cross-origin POST exec with %v = %d, want 403", name, h, got)
			}
			if got := do(t, ts, "POST", "/v1/vms", "", `{"template":"t1"}`, h); got != http.StatusForbidden {
				t.Errorf("%s: cross-origin POST /v1/vms with %v = %d, want 403", name, h, got)
			}
		}
	}
}

// Over TCP a JSON body must say so: text/plain and forms are what a browser
// sends cross-origin without a preflight.
func TestTCPRequiresJSONContentType(t *testing.T) {
	ts := tcpTestServer(t)
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		if got := do(t, ts, "POST", "/v1/networks", "", `{}`, map[string]string{"Content-Type": ct}); got != http.StatusUnsupportedMediaType {
			t.Errorf("POST /v1/networks with Content-Type %q = %d, want 415", ct, got)
		}
	}
	// A proper one reaches the handler (which refuses the empty network: 400).
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if got := do(t, ts, "POST", "/v1/networks", "", `{}`, map[string]string{"Content-Type": ct}); got != http.StatusBadRequest {
			t.Errorf("POST /v1/networks with Content-Type %q = %d, want the handler's 400", ct, got)
		}
	}
	// A body-less action has no Content-Type to check.
	if got := do(t, ts, "POST", "/v1/vms/deadbeef/stop", "", "", nil); got == http.StatusUnsupportedMediaType || got == http.StatusForbidden {
		t.Errorf("POST stop with no body = %d, want it to reach the handler", got)
	}
	// Uploads are raw bytes, not JSON.
	if got := do(t, ts, "PUT", "/v1/volumes/deadbeef/files?path=/a", "", "data", map[string]string{"Content-Type": "application/octet-stream"}); got == http.StatusUnsupportedMediaType || got == http.StatusForbidden {
		t.Errorf("PUT volume file = %d, want it to reach the handler", got)
	}
}

// On the Unix socket `curl -d` (form-encoded) keeps working, as the docs use it.
func TestUnixSocketAcceptsCurlStyleJSON(t *testing.T) {
	ts := newTestServer(t)
	if got := do(t, ts, "POST", "/v1/networks", "", `{}`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); got != http.StatusBadRequest {
		t.Errorf("curl -d style POST on the socket = %d, want the handler's 400", got)
	}
}

// DNS rebinding: a page under its own name pointed at 127.0.0.1 sends its
// own name as Host. Reads are refused too.
func TestTCPRefusesForeignHost(t *testing.T) {
	ts := tcpTestServer(t, "mh.lab.internal")
	for _, host := range []string{"rebind.attacker.example", "rebind.attacker.example:8080", "localhost.attacker.example"} {
		for _, path := range []string{"/v1/vms", "/v1/system", "/v1/events"} {
			if got := do(t, ts, "GET", path, host, "", nil); got != http.StatusForbidden {
				t.Errorf("GET %s with Host %q = %d, want 403", path, host, got)
			}
		}
	}
	for _, host := range []string{"", "127.0.0.1:2375", "localhost:2375", "LOCALHOST", "[::1]:2375", "10.0.0.5", "mh.lab.internal:2375", "MH.lab.internal."} {
		if got := do(t, ts, "GET", "/v1/vms", host, "", nil); got != http.StatusOK {
			t.Errorf("GET /v1/vms with Host %q = %d, want 200", host, got)
		}
	}
}

func TestServerBoundsHeadersAndIdle(t *testing.T) {
	srv := newTestAPI(t)
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout %v, IdleTimeout %v: both must be set", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatal("ReadTimeout/WriteTimeout would cut exec, file transfers and the event stream")
	}
}
