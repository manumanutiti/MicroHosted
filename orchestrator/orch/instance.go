package orch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"microhosted/orchestrator/spec"
	"microhosted/pkg/types"
)

// Inside the guest: where a detached command writes its output and its pid.
const (
	guestLog = "/var/log/mh-function.log"
	guestPID = "/run/mh-function.pid"
)

// Bounds on what the orchestrator waits for and keeps.
var (
	// readyTimeout bounds a boot, from create to the guest agent answering.
	readyTimeout = 60 * time.Second
	// settle is how long a launched command must stay up before a
	// persistent instance counts as started (when it has no health check).
	settle = 2 * time.Second
	// healthWait is how long a new persistent instance has to pass its
	// health check before it counts as failed.
	healthWait = 30 * time.Second
	// healthRetry spaces a new instance's health attempts.
	healthRetry = time.Second
	// pollEvery is how often a window cycle checks its command is still up.
	pollEvery = time.Second
	// destroyTimeout bounds a destroy, which runs even during shutdown.
	destroyTimeout = 60 * time.Second
)

// OutputTail is how much of a command's output a result keeps: the end,
// where the error usually is.
const OutputTail = 4096

// shellQuote makes s one single-quoted sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// launchCmd starts cmd detached from the agent's connection — its own session,
// stdin from /dev/null, output to guestLog — and records its pid, so the
// exec returns at once and the command outlives it.
func launchCmd(cmd string) string {
	return "setsid sh -c " + shellQuote(cmd) + " </dev/null >" + guestLog + " 2>&1 & echo $! >" + guestPID
}

// aliveCmd exits 0 while the launched command runs.
const aliveCmd = `kill -0 "$(cat ` + guestPID + `)" 2>/dev/null`

// tailCmd prints the end of the launched command's output.
var tailCmd = fmt.Sprintf("tail -c %d %s 2>/dev/null", OutputTail, guestLog)

// tail keeps the last OutputTail bytes of s.
func tail(s string) string {
	if len(s) <= OutputTail {
		return s
	}
	return s[len(s)-OutputTail:]
}

// createVM creates one instance of a function, named
// <project>-<function>-<generation>.
func (o *Orchestrator) createVM(ctx context.Context, name string, f *spec.Function) (*types.VMResponse, error) {
	gen := o.nextGeneration(name)
	req := types.CreateVMRequest{
		Image:  f.Image,
		Name:   o.VMName(name, gen),
		Labels: functionLabels(o.project, name, f, gen),
		VCPUs:  f.Resources.VCPUs,
		MemMB:  f.Resources.MemMB,
		DiskMB: f.Resources.DiskMB,
		Files:  fileSpecs(f),
	}
	if f.Network == spec.NoNetwork {
		req.NoNetwork = true
	} else {
		req.Network = f.Network
		req.GuestIP = f.IP
	}
	return o.eng.CreateVM(ctx, req)
}

// destroy removes a VM the orchestrator owns. It runs on its own context:
// a cycle's VM is destroyed even when the orchestrator is shutting down.
func (o *Orchestrator) destroy(id, why string) error {
	ctx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
	defer cancel()
	if err := o.eng.DestroyVM(ctx, id); err != nil {
		o.log.Printf("destroy vm %s (%s): %v", id, why, err)
		return err
	}
	return nil
}

// startPersistent creates a persistent function's VM and returns once it is
// serving: agent up, command launched and still running, health passing. On
// any failure the VM is destroyed — a half-started instance is never left
// behind — and the error says why.
func (o *Orchestrator) startPersistent(ctx context.Context, name string, f *spec.Function) (*types.VMResponse, error) {
	born := time.Now()
	vm, err := o.createVM(ctx, name, f)
	if err != nil {
		if ctx.Err() == nil {
			o.recordFailure(name, f, nil, born, "create: "+err.Error(), nil, "")
		}
		return nil, fmt.Errorf("create: %w", err)
	}
	fail := func(err error) (*types.VMResponse, error) {
		if ctx.Err() == nil {
			var out string
			if f.Command != "" {
				out = o.commandOutput(context.Background(), vm.ID)
			}
			o.recordFailure(name, f, vm, born, "start: "+err.Error(), nil, out)
		}
		o.destroy(vm.ID, "failed to start")
		return nil, fmt.Errorf("vm %s (%s): %w", vm.Name, vm.ID, err)
	}
	if err := o.eng.WaitReady(ctx, vm.ID, readyTimeout); err != nil {
		return fail(fmt.Errorf("guest agent: %w", err))
	}
	if f.Command != "" {
		if err := o.launch(ctx, vm.ID, f.Command); err != nil {
			return fail(err)
		}
	}
	switch {
	case f.Health != nil:
		if err := o.awaitHealthy(ctx, vm.ID, f); err != nil {
			return fail(err)
		}
	case f.Command != "":
		if err := sleep(ctx, settle); err != nil {
			return fail(err)
		}
		if ok, out := o.commandAlive(ctx, vm.ID); !ok {
			return fail(fmt.Errorf("command exited within %s: %q", settle, out))
		}
	}
	return vm, nil
}

// launch starts a function's command detached in the VM.
func (o *Orchestrator) launch(ctx context.Context, id, cmd string) error {
	res, err := o.eng.Exec(ctx, id, launchCmd(cmd), 10*time.Second)
	if err != nil {
		return fmt.Errorf("launching command: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("launching command: exit %d: %s", res.ExitCode, tail(res.Output))
	}
	return nil
}

// commandAlive reports whether the launched command still runs; when it does
// not, it returns the end of its output.
func (o *Orchestrator) commandAlive(ctx context.Context, id string) (bool, string) {
	res, err := o.eng.Exec(ctx, id, aliveCmd, 5*time.Second)
	if err == nil && res.ExitCode == 0 {
		return true, ""
	}
	return false, o.commandOutput(ctx, id)
}

// commandOutput returns the end of the launched command's output.
func (o *Orchestrator) commandOutput(ctx context.Context, id string) string {
	res, err := o.eng.Exec(ctx, id, tailCmd, 5*time.Second)
	if err != nil {
		return fmt.Sprintf("(output unavailable: %v)", err)
	}
	return res.Output
}

// checkHealth runs a health check once.
func (o *Orchestrator) checkHealth(ctx context.Context, id string, h *spec.Health) error {
	res, err := o.eng.Exec(ctx, id, h.Command, h.Timeout.D())
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(tail(res.Output)))
	}
	return nil
}

// awaitHealthy retries a new instance's health check until it passes, for up
// to healthWait — or until the function's command has exited, which no
// amount of waiting fixes.
func (o *Orchestrator) awaitHealthy(ctx context.Context, id string, f *spec.Function) error {
	deadline := time.Now().Add(healthWait)
	for {
		err := o.checkHealth(ctx, id, f.Health)
		if err == nil {
			return nil
		}
		if f.Command != "" {
			if ok, out := o.commandAlive(ctx, id); !ok {
				return fmt.Errorf("command exited before its health check passed: %s", strings.TrimSpace(tail(out)))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check still failing after %s: %w", healthWait, err)
		}
		if err := sleep(ctx, healthRetry); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
