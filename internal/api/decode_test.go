package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"microhosted/pkg/types"
)

func TestStrictDecode(t *testing.T) {
	for body, wantErr := range map[string]string{
		`{"name":"lab","intra":true}`:               "",
		`{"name":"lab","labels":{"a":"1","b":"2"}}`: "",
		`{"name":"lab","allowed_egress":[{"ip":"1.2.3.4","protocol":"icmp"},{"ip":"1.2.3.5","protocol":"icmp"}]}`: "",
		`{"name":"lab","alowed_egress":[]}`:                                                             "unknown field",
		`{"name":"lab","intra":false,"intra":true}`:                                                     `"intra" given twice`,
		`{"name":"lab","intra":false,"INTRA":true}`:                                                     `"INTRA" given twice`,
		`{"name":"lab","allowed_egress":[{"ip":"1.2.3.4","ip":"0.0.0.0/0","protocol":"tcp","port":1}]}`: `"allowed_egress.0.ip" given twice`,
		`{"name":"lab","labels":{"a":"1","a":"2"}}`:                                                     `"labels.a" given twice`,
		`{"name":"lab"}{"egress":true}`:                                                                 "after the JSON object",
		`{"name":"lab"} x`:                                                                              "after the JSON object",
		`null`:                                                                                          "must be a JSON object",
		`["lab"]`:                                                                                       "must be a JSON object",
		`"lab"`:                                                                                         "must be a JSON object",
		`{"name":"lab"`:                                                                                 "unexpected end",
	} {
		var req types.CreateNetworkRequest
		err := strictDecode([]byte(body), &req)
		switch {
		case wantErr == "" && err != nil:
			t.Errorf("%s: %v, want accepted", body, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("%s: %v, want an error containing %q", body, err, wantErr)
		}
	}
}

// Through the server: the status codes, an optional body, and the size cap.
func TestDecodeJSONStatuses(t *testing.T) {
	ts := newTestServer(t)
	post := func(path, body string) int {
		t.Helper()
		res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/networks", `{"name":"lab","egres_iface":"eth0"}`, http.StatusBadRequest},
		{"/v1/networks", ``, http.StatusBadRequest},
		{"/v1/vms", `{"template":"t1","template":"t2"}`, http.StatusBadRequest},
		{"/v1/vms/deadbeef/exec", `{"cmd":"id","timeout":5}`, http.StatusBadRequest},
		{"/v1/vms/deadbeef/exec", `{"cmd":"id","timeout_ms":-1}`, http.StatusBadRequest},
		{"/v1/vms/deadbeef/exec", `{"cmd":"id","timeout_ms":600001}`, http.StatusBadRequest},
		// A valid exec on an unknown VM is 404, not a daemon fault.
		{"/v1/vms/deadbeef/exec", `{"cmd":"id","timeout_ms":600000}`, http.StatusNotFound},
		// An optional body may be empty: the request reaches the manager,
		// which does not know the VM.
		{"/v1/vms/deadbeef/snapshot", ``, http.StatusNotFound},
		{"/v1/vms/deadbeef/snapshot", `{"nme":"clean"}`, http.StatusBadRequest},
		{"/v1/vms/deadbeef/replace", ``, http.StatusNotFound},
	} {
		if got := post(c.path, c.body); got != c.want {
			t.Errorf("POST %s %s = %d, want %d", c.path, c.body, got, c.want)
		}
	}

	huge := `{"name":"` + strings.Repeat("a", maxJSONBody) + `"}`
	res, err := http.Post(ts.URL+"/v1/networks", "application/json", bytes.NewBufferString(huge))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the cap = %d, want 413", res.StatusCode)
	}
}
