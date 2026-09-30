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
	"time"

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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	digest := "sha256:" + strings.Repeat("cd", 32)
	served := map[string]string{} // a tag bound again: the digest it now names
	built := map[string]bool{"site:0.9": true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/images/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !built[r.PathValue("ref")] {
			replyStatus(http.StatusNotFound, map[string]string{"error": "image not found"})(w, r)
			return
		}
		d := digest
		if s, ok := served[r.PathValue("ref")]; ok {
			d = s
		}
		reply(types.ImageResponse{Digest: d})(w, r)
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

	// The tag removed and imported again with other bytes: refused, not
	// taken as the cached build — by --no-build too.
	_, out, _ = f.run("", "build", "-q", dir)
	ref, _, _ = strings.Cut(strings.TrimSpace(out), "@")
	served[ref] = "sha256:" + strings.Repeat("ee", 32)
	for _, args := range [][]string{{"build", "-q", dir}, {"build", "--no-build", dir}} {
		if code, out2, errOut := f.run("", args...); code == 0 || out2 != "" || !strings.Contains(errOut, "imported again with other bytes") || builds != 2 {
			t.Errorf("%v of a rebound tag: exit %d, stdout %q, stderr %q, %d builds", args, code, out2, errOut, builds)
		}
	}
	// A tag this user has no record of building: refused until adopted.
	rec, err := buildRecordPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.run("", "build", "-q", dir); code == 0 || !strings.Contains(errOut, "--adopt") {
		t.Errorf("unrecorded tag: exit %d, stderr %q", code, errOut)
	}
	if code, out2, _ := f.run("", "build", "-q", "--adopt", dir); code != 0 || !strings.HasSuffix(out2, "@"+served[ref]+"\n") || builds != 2 {
		t.Errorf("--adopt: exit %d, %q, %d builds", code, out2, builds)
	}
	if code, _, errOut := f.run("", "build", "-q", dir); code != 0 {
		t.Errorf("after --adopt: exit %d, %s", code, errOut)
	}

	// An explicit tag that exists is refused before building.
	if code, _, errOut := f.run("", "build", "-t", "site:0.9", dir); code == 0 || !strings.Contains(errOut, "tags never move") || builds != 2 {
		t.Errorf("taken tag: exit %d, stderr %q, %d builds", code, errOut, builds)
	}
	// --no-cache reuses no layer either.
	if got.NoCache {
		t.Error("a build without --no-cache skipped the layer cache")
	}
	if code, _, errOut := f.run("", "build", "-q", "--no-cache", dir); code != 0 || !got.NoCache || builds != 3 {
		t.Errorf("--no-cache: exit %d, %s, NoCache %v", code, errOut, got.NoCache)
	}
}

// mh builder prune prunes the layer store under the daemon's store.
func TestBuilderPrune(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/system", reply(types.SystemResponse{Daemon: types.DaemonInfo{Paths: types.DaemonPaths{Store: "/store", Kernels: "/store/kernels"}}}))
	f := newFakeAPI(t, mux)
	oldPrune, oldRunner := runPrune, newBuildRunner
	t.Cleanup(func() { runPrune, newBuildRunner = oldPrune, oldRunner })
	newBuildRunner = func(io.Writer, bool) buildRunner { return noAuthRunner{nopRunner{}} }
	var store string
	var opts build.PruneOptions
	runPrune = func(_ context.Context, _ build.Runner, s string, o build.PruneOptions) (build.PruneResult, error) {
		store, opts = s, o
		return build.PruneResult{Layers: 3, Caches: o.All}, nil
	}
	if code, out, errOut := f.run("", "builder", "prune", "--older-than", "48h"); code != 0 || out != "removed 3 layers\n" || store != "/store" || opts.OlderThan != 48*time.Hour || opts.All {
		t.Errorf("prune: exit %d, %q %q, %s %+v", code, out, errOut, store, opts)
	}
	if code, out, _ := f.run("", "builder", "prune", "--all"); code != 0 || !strings.Contains(out, "package caches") || !opts.All {
		t.Errorf("prune --all: exit %d, %q", code, out)
	}
	if code, _, _ := f.run("", "builder", "prune", "--older-than", "soon"); code == 0 {
		t.Error("a malformed --older-than was accepted")
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
