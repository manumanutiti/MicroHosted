package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"microhosted/orchestrator/spec"
)

// Whether resolveBuilds may build what is missing.
const (
	buildMissing = true  // plan, apply, run: build what the store lacks
	lookOnly     = false // status, down, failures, reloads: never build
)

// resolveBuilds gives every function with build: the image mh build makes of
// it: the pinned reference mh build prints, which is the existing image when
// the store already holds one of the same inputs (mh build tags by their
// fingerprint). The orchestrator imports nothing of the engine's, so it runs
// the mh client. Each image spec is resolved once, however many functions
// use it. With lookOnly it only asks (mh build --no-build): a spec not built
// leaves its functions without an image, and notBuilt names them.
func resolveBuilds(s *spec.Spec, host string, build bool) (notBuilt []string, err error) {
	refs := map[string]string{}
	var errs []error
	for _, name := range s.FunctionOrder {
		f := s.Functions[name]
		if f.BuildFile == "" {
			continue
		}
		ref, done := refs[f.BuildFile]
		if !done {
			ref, err = mhBuild(f.BuildFile, host, build)
			if err != nil {
				errs = append(errs, fmt.Errorf("functions.%s.build: %w", name, err))
				continue
			}
			refs[f.BuildFile] = ref
		}
		if ref == "" {
			notBuilt = append(notBuilt, name)
			continue
		}
		f.Image = ref
	}
	sort.Strings(notBuilt)
	return notBuilt, errors.Join(errs...)
}

// errNotBuilt is mh build --no-build's exit code for a spec not built yet.
const exitNotBuilt = 3

func mhBuild(file, host string, build bool) (string, error) {
	mh, err := mhPath()
	if err != nil {
		return "", err
	}
	args := []string{}
	if host != "" {
		args = append(args, "-H", host)
	}
	args = append(args, "build", "-f", file)
	if !build {
		args = append(args, "--no-build", "-q")
	}
	cmd := exec.Command(mh, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	// The build's progress and sudo's prompt reach the terminal; only the
	// reference is read.
	// Looking only, "not built" is not news; any other message (a tag that
	// names bytes this user did not build) is kept for the error.
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	if !build {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !build && errors.As(err, &ee) && ee.ExitCode() == exitNotBuilt {
			return "", nil
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("mh build -f %s: %s", file, msg)
		}
		return "", fmt.Errorf("mh build -f %s: %w", file, err)
	}
	ref := strings.TrimSpace(out.String())
	if !strings.Contains(ref, "@sha256:") || strings.ContainsAny(ref, " \n") {
		return "", fmt.Errorf("mh build -f %s printed %q, not a pinned reference", file, ref)
	}
	return ref, nil
}

// mhPath finds the mh client: $MH, else the mh next to this binary (make
// build puts both in build/), else the one on PATH.
func mhPath() (string, error) {
	if p := os.Getenv("MH"); p != "" {
		return p, nil
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "mh")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("mh")
	if err != nil {
		return "", errors.New("build: needs the mh client: none next to mh-orchestrator, none on PATH (make install-cli, or MH=/path/to/mh)")
	}
	return p, nil
}
