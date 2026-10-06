package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// projectVerbs are docker compose's verbs on a project spec (./microse.yml):
// mh up, mh down… They run mh-orchestrator — a separate program by design
// (docs/orchestrator.md §3), a client of the same API — with the same
// arguments, as docker compose is a plugin of docker.
var projectVerbs = []struct{ name, summary string }{
	{"up", "Start the project in ./microse.yml and keep it running (-d: in the background)"},
	{"down", "Stop the project and remove its VMs and networks"},
	{"plan", "Show what up/apply would change (builds images that are missing)"},
	{"apply", "Converge the project once, or hand changes to its running up"},
	{"status", "The project's functions, VMs and health"},
	{"failures", "Why the project's VMs failed"},
	{"validate", "Check ./microse.yml on its own"},
}

func isProjectVerb(name string) bool {
	for _, v := range projectVerbs {
		if v.name == name {
			return true
		}
	}
	return false
}

// execProgram replaces this process; a variable so tests can record the call.
var execProgram = func(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}

// runProject hands verb and its arguments to mh-orchestrator, which gets
// the terminal, the signals and the exit code.
func runProject(e *env, hostFlag, verb string, args []string) error {
	bin, err := orchestratorPath()
	if err != nil {
		return err
	}
	argv := []string{bin, verb}
	if hostFlag != "" {
		argv = append(argv, "-H", hostFlag) // before a FUNCTION argument, where flags still parse
	}
	argv = append(argv, args...)
	return execProgram(bin, argv, os.Environ())
}

// orchestratorPath finds mh-orchestrator: next to mh (make build puts both in
// build/, make install-cli both in /usr/local/bin), else on PATH.
func orchestratorPath() (string, error) {
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "mh-orchestrator")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("mh-orchestrator")
	if err != nil {
		return "", errors.New("mh up, down, plan… run mh-orchestrator, which is not installed: make install-cli (or put build/mh-orchestrator on PATH)")
	}
	return p, nil
}

func printProjectHelp(w io.Writer) {
	fmt.Fprint(w, "\nProject commands (./microse.yml, like docker compose; -f FILE for another):\n")
	for _, v := range projectVerbs {
		fmt.Fprintf(w, "  %-11s%s\n", v.name, v.summary)
	}
}
