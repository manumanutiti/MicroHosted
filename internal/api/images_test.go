package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"microhosted/internal/images"
	"microhosted/internal/storage"
	"microhosted/internal/store"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

func TestImageRoutes(t *testing.T) {
	var storeDir string
	var mgr *vm.Manager
	ts := newTestServer(t, func(m *vm.Manager, st *store.Store, dir string) {
		s, err := images.Open(dir, st)
		if err != nil {
			t.Fatal(err)
		}
		m.SetImages(s)
		mgr, storeDir = m, dir
	})
	for name, content := range map[string]string{"vmlinux": "kernel", "rootfs.ext4": "rootfs"} {
		if err := os.WriteFile(filepath.Join(storeDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	do := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewBufferString(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(res.Body)
		return res.StatusCode, buf.Bytes()
	}

	body, _ := json.Marshal(types.ImportImageRequest{Name: "parser:1.0",
		KernelPath: filepath.Join(storeDir, "vmlinux"), RootfsPath: filepath.Join(storeDir, "rootfs.ext4"), VCPUs: 1, MemMB: 64})
	code, out := do("POST", "/v1/images", string(body))
	if code != http.StatusCreated {
		t.Fatalf("import = %d %s", code, out)
	}
	var img types.ImageResponse
	if err := json.Unmarshal(out, &img); err != nil {
		t.Fatal(err)
	}

	outside, _ := json.Marshal(types.ImportImageRequest{Name: "leak:1", KernelPath: "/etc/passwd", RootfsPath: "/etc/passwd", VCPUs: 1, MemMB: 64})
	pinned := url.PathEscape("parser:1.0@" + img.Digest)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/v1/images", "", http.StatusOK},
		{"GET", "/v1/images/parser:1.0", "", http.StatusOK},
		{"GET", "/v1/images/" + pinned, "", http.StatusOK},
		{"GET", "/v1/images/" + img.Digest, "", http.StatusOK},
		{"GET", "/v1/images/parser:9.9", "", http.StatusNotFound},
		{"GET", "/v1/images/Bad:1", "", http.StatusBadRequest},
		{"POST", "/v1/images", string(outside), http.StatusBadRequest},
		{"POST", "/v1/images", `{"name":"x:1","kernel":"/k"}`, http.StatusBadRequest},
		{"POST", "/v1/images/parser:1.0/verify", "", http.StatusOK},
		// A create by an unknown image is refused before anything happens.
		{"POST", "/v1/vms", `{"image":"parser:9.9"}`, http.StatusBadRequest},
		{"POST", "/v1/vms", `{"image":"parser:1.0","template":"t1"}`, http.StatusBadRequest},
	} {
		if got, out := do(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s = %d, want %d (%s)", c.method, c.path, c.body, got, c.want, out)
		}
	}

	// In use by a snapshot: the delete is refused.
	snapDir := t.TempDir()
	for _, n := range []string{storage.SnapshotStateFile, storage.SnapshotMemFile, storage.SnapshotDiskFile} {
		if err := os.WriteFile(filepath.Join(snapDir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mgr.LoadSnapshots([]*types.Snapshot{{ID: "s0000001", Image: img.Digest, Dir: snapDir}})
	if code, out := do("DELETE", "/v1/images/parser:1.0", ""); code != http.StatusConflict {
		t.Errorf("delete in use = %d %s, want 409", code, out)
	}
}
