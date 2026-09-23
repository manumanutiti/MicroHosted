// Package faults provides failpoints: named places in the engine where a test
// can make the operation fail, or make the daemon die, on purpose. It exists
// to prove the failure paths (rollback, startup recovery) on real hardware —
// see scripts/fault-test.sh and docs/roadmap.md Phase 0b.
//
// Inert unless MICROHOSTED_FAULTS is set when the daemon starts:
//
//	MICROHOSTED_FAULTS=vm.create.after-tap:error,vm.create.after-boot:crash
//
// "error" makes Check return an error at that point every time it is reached;
// "crash" SIGKILLs the daemon there — a crash, not an exit: no deferred
// cleanup, no graceful shutdown, exactly what a kill -9 or an OOM kill does.
// The variable is read once; a daemon started without it pays one map lookup
// per point and nothing else.
package faults

import (
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
)

// EnvVar names the environment variable read at startup.
const EnvVar = "MICROHOSTED_FAULTS"

// Points lists every failpoint compiled into the engine, in the order an
// operation reaches them. Parse rejects any other name, so a typo in a fault
// test fails loudly instead of testing nothing.
var Points = []string{
	"vm.create.after-record",
	"vm.create.after-clone",
	"vm.create.after-network",
	"vm.create.after-boot",
	"vm.create.before-save",
	"vm.fork.after-clone",
	"vm.fork.after-boot",
	"network.create.after-bridge",
	"network.create.after-apply",
}

type mode int

const (
	modeError mode = iota + 1
	modeCrash
)

var active = mustParse(os.Getenv(EnvVar))

func mustParse(spec string) map[string]mode {
	m, err := Parse(spec)
	if err != nil {
		// A fault spec only exists in a test; a bad one must stop the test,
		// not run it with no faults and report success.
		log.Fatalf("%s: %v", EnvVar, err)
	}
	if len(m) > 0 {
		log.Printf("WARNING: failpoints active (%s=%s) — this daemon fails on purpose; never set this in production", EnvVar, spec)
	}
	return m
}

// Parse reads a comma-separated list of point:mode.
func Parse(spec string) (map[string]mode, error) {
	out := make(map[string]mode)
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}
	for _, item := range strings.Split(spec, ",") {
		name, how, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok {
			return nil, fmt.Errorf("%q: want point:error or point:crash", item)
		}
		if !known(name) {
			return nil, fmt.Errorf("unknown failpoint %q (known: %s)", name, strings.Join(Points, ", "))
		}
		switch how {
		case "error":
			out[name] = modeError
		case "crash":
			out[name] = modeCrash
		default:
			return nil, fmt.Errorf("%q: mode must be error or crash", item)
		}
	}
	return out, nil
}

func known(name string) bool {
	for _, p := range Points {
		if p == name {
			return true
		}
	}
	return false
}

// Check is called at a failpoint. It returns nil unless the point is active.
func Check(point string) error {
	switch active[point] {
	case modeError:
		return fmt.Errorf("failpoint %s: injected error", point)
	case modeCrash:
		log.Printf("failpoint %s: crashing the daemon (SIGKILL)", point)
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // SIGKILL is not deliverable-later; never returns
	}
	return nil
}
