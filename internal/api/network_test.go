package api

import (
	"bytes"
	"net/http"
	"testing"
)

// putJSON is a tiny helper: http.Post has no PUT sibling.
func putJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The validation paths of PUT /v1/networks/{name}/egress are exercisable
// without root (no bridge or nft is touched before they reject); the happy
// path needs a live network and is covered by on-device testing.
func TestUpdateNetworkEgressValidation(t *testing.T) {
	ts := newTestServer(t)

	cases := []struct {
		name string
		url  string
		body string
		want int
	}{
		{"unknown network", "/v1/networks/nope/egress", `{"allowed_egress":[{"ip":"192.168.0.15","protocol":"icmp"}]}`, http.StatusNotFound},
		{"bad ip", "/v1/networks/nope/egress", `{"allowed_egress":[{"ip":"evil.example.com","protocol":"tcp","port":80}]}`, http.StatusBadRequest},
		{"injection attempt", "/v1/networks/nope/egress", `{"allowed_egress":[{"ip":"1.2.3.4 accept; ip daddr 0.0.0.0/0","protocol":"tcp","port":80}]}`, http.StatusBadRequest},
		{"egress and rules together", "/v1/networks/nope/egress", `{"egress":true,"allowed_egress":[{"ip":"1.2.3.4","protocol":"tcp","port":80}]}`, http.StatusBadRequest},
		{"garbage body", "/v1/networks/nope/egress", `{not json`, http.StatusBadRequest},
		{"intra unknown network", "/v1/networks/nope/intra", `{"intra":true}`, http.StatusNotFound},
		{"intra garbage body", "/v1/networks/nope/intra", `{not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		res := putJSON(t, ts.URL+c.url, c.body)
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: PUT %s = %d, want %d", c.name, c.url, res.StatusCode, c.want)
		}
	}
}

// Rule validation must reject bad input on create too, with the caller's 400.
func TestCreateNetworkEgressValidation(t *testing.T) {
	ts := newTestServer(t)

	for name, body := range map[string]string{
		"bad ip":     `{"name":"x","allowed_egress":[{"ip":"nope","protocol":"tcp","port":80}]}`,
		"both flags": `{"name":"x","egress":true,"allowed_egress":[{"ip":"1.2.3.4","protocol":"tcp","port":80}]}`,
	} {
		res, err := http.Post(ts.URL+"/v1/networks", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: POST /v1/networks = %d, want 400", name, res.StatusCode)
		}
	}
}

// PUT /v1/networks/{name}/ingress: same split as egress. The test daemon
// manages no interface, so every rule is refused before a bridge or nft is
// touched; an empty list is valid and reaches the not-found check.
func TestUpdateNetworkIngressValidation(t *testing.T) {
	ts := newTestServer(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown network", `{"allowed_ingress":[]}`, http.StatusNotFound},
		{"unmanaged iface", `{"allowed_ingress":[{"iface":"wlan0","src_ip":"192.168.50.60","protocol":"tcp","port":1883,"to_ip":"172.16.0.2"}]}`, http.StatusBadRequest},
		{"no iface", `{"allowed_ingress":[{"src_ip":"192.168.50.60","protocol":"tcp","port":1883,"to_ip":"172.16.0.2"}]}`, http.StatusBadRequest},
		{"injection attempt", `{"allowed_ingress":[{"iface":"wlan0","src_ip":"1.2.3.4 accept;","protocol":"tcp","port":1883,"to_ip":"172.16.0.2"}]}`, http.StatusBadRequest},
		{"garbage body", `{not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		res := putJSON(t, ts.URL+"/v1/networks/nope/ingress", c.body)
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: PUT /v1/networks/nope/ingress = %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
}

func TestCreateNetworkIngressValidation(t *testing.T) {
	ts := newTestServer(t)
	body := `{"name":"x","subnet":"10.99.0.0/24","allowed_ingress":[{"iface":"wlan0","src_ip":"192.168.50.60","protocol":"tcp","port":1883,"to_ip":"10.99.0.2"}]}`
	res, err := http.Post(ts.URL+"/v1/networks", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/networks with an unmanaged ingress iface = %d, want 400", res.StatusCode)
	}
}

// Names, labels and error kinds on networks: all refused or mapped before a
// bridge or nft is touched, so no root is needed.
func TestNetworkIdentityStatuses(t *testing.T) {
	ts := newTestServer(t)
	do := func(method, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/networks", `{}`, http.StatusBadRequest},
		{"POST", "/v1/networks", `{"name":"Bad_Name"}`, http.StatusBadRequest},
		{"POST", "/v1/networks", `{"name":"lab","labels":{"Bad":"x"}}`, http.StatusBadRequest},
		{"POST", "/v1/networks", `{"name":"lab","labels":{"k":"has space"}}`, http.StatusBadRequest},
		{"GET", "/v1/networks?label=nokey", "", http.StatusBadRequest},
		{"GET", "/v1/networks?label=managed-by=mh-orchestrator", "", http.StatusOK},
		{"PATCH", "/v1/networks/nope/labels", `{"labels":{"k":"v"}}`, http.StatusNotFound},
		{"PATCH", "/v1/networks/nope/labels", `{"labels":{"Bad":"v"}}`, http.StatusBadRequest},
		{"PATCH", "/v1/networks/nope/labels", `{not json`, http.StatusBadRequest},
		{"DELETE", "/v1/networks/nope", "", http.StatusNotFound},
	} {
		if got := do(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s = %d, want %d", c.method, c.path, c.body, got, c.want)
		}
	}
}

// A VM the daemon does not know, and the flow log holds nothing of, is a 404:
// not an empty list that would read as "it tried nothing".
func TestFlowsUnknownVM(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/v1/vms/nope/flows")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", res.StatusCode)
	}
}
