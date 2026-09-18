package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGuestPathParamRefusesInjection: a URL-encoded newline decodes to a real
// one, and on the offline path that would be a second debugfs command. The API
// must turn it away as a client error before any channel sees it.
func TestGuestPathParamRefusesInjection(t *testing.T) {
	for query, wantOK := range map[string]bool{
		"path=/root/a.bin":                    true,
		"path=/root/my%20file.txt":            true,
		"":                                    false,
		"path=relative":                       false,
		"path=/x%0Adump%20/secret%20/tmp/out": false,
		"path=/x%0D":                          false,
		"path=/a%22b":                         false,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/vms/x/files?"+query, nil)
		_, ok := guestPathParam(rec, req)
		if ok != wantOK {
			t.Errorf("%q: ok = %v, want %v", query, ok, wantOK)
		}
		if !ok && rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", query, rec.Code)
		}
	}
}
