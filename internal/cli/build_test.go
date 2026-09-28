package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"microhosted/internal/build"
	"microhosted/pkg/types"
)

type noAuthRunner struct{ build.Runner }

func (noAuthRunner) Authenticate(context.Context) error { return nil }

type nopRunner struct{}

func (nopRunner) Run(context.Context, io.Reader, string, ...string) error { return nil }
func (nopRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

// mh build builds with the daemon's store paths, imports the result with the
// spec's defaults and prints only the pinned reference on stdout. Without a
// version it tags by fingerprint, and a fingerprint the store holds is not
// built again.
func TestImageBuild(t *testing.T) {
	digest := "sha256:" + strings.Repeat("cd", 32)
	built := map[string]bool{"site:0.9": true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !built[r.PathValue("ref")] {
			replyStatus(http.StatusNotFound, map[string]string{"error": "image not found"})(w, r)
			return
		}
		reply(types.ImageResponse{Digest: digest})(w, r)
	})
	mux.HandleFunc("GET /v1/system", reply(types.SystemResponse{Daemon: types.DaemonInfo{Paths: types.DaemonPaths{Store: "/store", Kernels: "/store/kernels"}}}))
	mux.HandleFunc("POST /v1/images", func(w http.ResponseWriter, r *http.Request) {
		var req types.ImportImageRequest
		json.NewDecoder(r.Body).Decode(&req)
		built[req.Name] = true
		replyStatus(http.StatusCreated, types.ImageResponse{Digest: digest, Tags: []string{req.Name}})(w, r)
	})
	f := newFakeAPI(t, mux)

	dir := t.TempDir()
	spec := "name: site\nbase: alpine:3.22\npackages: [nginx]\nfiles: {/srv/www/index.html: index.html}\ncommand: nginx -g 'daemon off;'\nhealth: {command: 'wget -qO- http://127.0.0.1/', every: 10s}\nmem_mb: 64\n"
	if err := os.WriteFile(filepath.Join(dir, "build.yml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got build.Options
	builds, cleaned := 0, false
	oldBuild, oldRunner := runBuild, newBuildRunner
	t.Cleanup(func() { runBuild, newBuildRunner = oldBuild, oldRunner })
	newBuildRunner = func(io.Writer, bool) buildRunner { return noAuthRunner{nopRunner{}} }
	runBuild = func(_ context.Context, o build.Options) (*build.Result, error) {
		got = o
		builds++
		return build.NewResult("/store/kernels/vmlinux-6.1.102", "/store/build/x/rootfs.ext4", func(context.Context) error { cleaned = true; return nil }), nil
	}

	code, out, errOut := f.run("", "build", "-q", dir)
	if code != 0 {
		t.Fatalf("build: exit %d, stderr %q", code, errOut)
	}
	ref, _, _ := strings.Cut(strings.TrimSpace(out), "@")
	if !strings.HasPrefix(ref, "site:sha-") || !strings.HasSuffix(out, "@"+digest+"\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("stdout = %q, want only site:sha-…@digest", out)
	}
	if got.Store != "/store" || got.Kernels != "/store/kernels" || got.Context != dir || got.Spec.MemMB != 64 {
		t.Errorf("build options %+v", got)
	}
	var req types.ImportImageRequest
	if err := json.Unmarshal(f.last("POST").body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != ref || req.RootfsPath != "/store/build/x/rootfs.ext4" || req.Command != "nginx -g 'daemon off;'" ||
		req.Health == nil || req.Health.Every != "10s" || req.MemMB != 64 {
		t.Errorf("import request %+v", req)
	}
	if !cleaned {
		t.Error("the build's files were not removed after the import")
	}

	// Same inputs: the same reference, nothing built.
	if code, out2, _ := f.run("", "build", "-q", "-f", filepath.Join(dir, "build.yml")); code != 0 || out2 != out || builds != 1 {
		t.Errorf("second build: exit %d, %q (first %q), %d builds", code, out2, out, builds)
	}
	if code, out2, _ := f.run("", "build", "--no-build", dir); code != 0 || out2 != out {
		t.Errorf("--no-build of a built spec: exit %d, %q", code, out2)
	}
	// A changed file: another fingerprint, a build.
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run("", "build", "--no-build", dir); code != 3 || builds != 1 {
		t.Errorf("--no-build of a changed spec: exit %d, %d builds; want 3 and no build", code, builds)
	}
	if code, out2, _ := f.run("", "build", "-q", dir); code != 0 || out2 == out || builds != 2 {
		t.Errorf("changed file: exit %d, %q, %d builds", code, out2, builds)
	}

	// An explicit tag that exists is refused before building.
	if code, _, errOut := f.run("", "build", "-t", "site:0.9", dir); code == 0 || !strings.Contains(errOut, "tags never move") || builds != 2 {
		t.Errorf("taken tag: exit %d, stderr %q, %d builds", code, errOut, builds)
	}
}

// mh up, down… hand the verb and its arguments to mh-orchestrator.
func TestProjectVerbs(t *testing.T) {
	var got []string
	old := execProgram
	t.Cleanup(func() { execProgram = old })
	execProgram = func(path string, argv, env []string) error { got = argv; return nil }
	t.Setenv("PATH", t.TempDir()) // a fake mh-orchestrator on PATH
	bin := filepath.Join(os.Getenv("PATH"), "mh-orchestrator")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := Main([]string{"-H", "/tmp/x.sock", "up", "-d", "-f", "a.yml"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("mh up: exit %d, %s", code, errb.String())
	}
	if strings.Join(got[1:], " ") != "up -H /tmp/x.sock -d -f a.yml" || !strings.HasSuffix(got[0], "mh-orchestrator") {
		t.Errorf("exec %q", got)
	}
	Main([]string{"failures", "web"}, strings.NewReader(""), &out, &errb)
	if strings.Join(got[1:], " ") != "failures web" {
		t.Errorf("exec %q", got)
	}
}
