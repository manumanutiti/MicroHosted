package orch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"microhosted/orchestrator/spec"
)

// secretCommandTimeout bounds a secret's command: it mints one credential,
// typically with one API call.
const secretCommandTimeout = 60 * time.Second

// runSecretCommand runs a secret's command on this host, in the spec's
// directory, with env added to the orchestrator's environment, and returns
// its standard output: the secret. Its standard error is only reported on a
// failure, and only its last line — a command must not print the secret there.
// A variable so tests can count calls.
var runSecretCommand = func(ctx context.Context, src *spec.File, env []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, secretCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", src.Command)
	cmd.Dir = src.Dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second // a child left holding stdout does not hang the create
	out := &capped{max: spec.MaxFileBytes}
	errOut := &capped{max: 4096}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		if msg := lastLine(errOut.String()); msg != "" {
			return nil, fmt.Errorf("command %q: %w: %s", src.Command, err, msg)
		}
		return nil, fmt.Errorf("command %q: %w", src.Command, err)
	}
	switch {
	case out.over:
		return nil, fmt.Errorf("command %q: printed more than %d bytes", src.Command, spec.MaxFileBytes)
	case out.Len() == 0:
		return nil, fmt.Errorf("command %q: printed nothing", src.Command)
	}
	return out.Bytes(), nil
}

// capped keeps the first max bytes written to it and notes whether more came.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.Len(); len(p) > room {
		p, c.over = p[:max(room, 0)], true
	}
	c.Buffer.Write(p)
	return n, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return Printable(s)
}
