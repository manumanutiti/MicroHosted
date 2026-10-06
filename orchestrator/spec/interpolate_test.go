package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mapLookup(m map[string]string) Lookup {
	return func(name string) (string, string, bool) {
		v, ok := m[name]
		return v, FromEnvironment, ok
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"IMG": "site:1.0@sha256:ab", "EMPTY": ""}
	for in, want := range map[string]string{
		"${IMG}":                     "site:1.0@sha256:ab",
		"a ${IMG} b":                 "a site:1.0@sha256:ab b",
		"${MISSING:-x}":              "x",
		"${EMPTY:-x}":                "x",
		"${EMPTY-x}":                 "",
		"${MISSING-x:-y}":            "x:-y",
		"$${IMG}":                    "${IMG}",
		"echo $(hostname) $ip $HOME": "echo $(hostname) $ip $HOME",
		"${IMG:?build it first}":     "site:1.0@sha256:ab",
		`printf "%s" "$$${IMG}"`:     `printf "%s" "$${IMG}"`,
	} {
		got, err := expand(in, mapLookup(vars), map[string]string{})
		if err != nil || got != want {
			t.Errorf("expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"${MISSING}":         "is not set",
		"${EMPTY:?build it}": "build it",
		"${MISSING?}":        "required",
		"${1BAD}":            "not a variable name",
		"${IMG":              "unclosed",
	} {
		if _, err := expand(in, mapLookup(vars), map[string]string{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expand(%q) = %v, want an error with %q", in, err, want)
		}
	}
}

// Load takes ${NAME} from the environment first, then from .env next to the
// spec, and records where each came from; comments and keys are left alone.
func TestLoadInterpolates(t *testing.T) {
	dir := t.TempDir()
	spec := strings.Replace(valid, "image: alpine:1.0@"+digest+"\n    network: web\n    ip:", "image: ${SERVER_IMAGE}\n    network: web\n    ip:", 1)
	spec = strings.Replace(spec, "command: whoami", "command: echo ${GREETING:-hi} $$ $HOME   # ${NOT_READ}", 1)
	spec = "# built with: SERVER_IMAGE=$(mh build -q -t server .) — ${IGNORED}\n" + spec
	p := filepath.Join(dir, "microse.yml")
	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "${SERVER_IMAGE} is not set") {
		t.Fatalf("Load without the variable = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, EnvFile), []byte("# from mh build\nexport SERVER_IMAGE=\"alpine:1.0@"+digest+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Functions["server"].Image; got != "alpine:1.0@"+digest {
		t.Errorf("image = %q", got)
	}
	if got := s.Functions["check"].Command; got != "echo hi $$ $HOME" {
		t.Errorf("command = %q", got)
	}
	if s.Vars["SERVER_IMAGE"] != FromEnvFile || s.Vars["GREETING"] != FromDefault {
		t.Errorf("vars = %v", s.Vars)
	}

	t.Setenv("SERVER_IMAGE", "alpine:2.0@"+digest)
	if s, err := Load(p); err != nil || s.Functions["server"].Image != "alpine:2.0@"+digest || s.Vars["SERVER_IMAGE"] != FromEnvironment {
		t.Errorf("the environment must win over .env: %v, %v", s, err)
	}
}

func TestReadEnvFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, EnvFile), []byte("OK=1\nnot a line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(dir); err == nil || !strings.Contains(err.Error(), ".env:2") {
		t.Errorf("ReadEnvFile = %v, want an error at line 2", err)
	}
}
