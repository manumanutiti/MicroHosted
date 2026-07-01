// Package vsock talks to the microhosted-exec listener that
// scripts/prepare-image.sh installs inside a VM's guest (a systemd unit
// running `socat VSOCK-LISTEN:52,fork EXEC:/usr/local/bin/microhosted-exec`).
// It's the programmatic access path: no SSH keys, no IP, no TAP device — it
// works even on a VM created with no_network:true, since vsock isn't a
// network interface in that sense.
package vsock

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// AgentPort is the fixed vsock port the guest-side listener listens on.
// Arbitrary but fixed for this whole project — one place to change it.
const AgentPort = 52

const exitMarker = "___MICROHOSTED_EXIT___:"

const dialAndExecTimeout = 30 * time.Second

// Exec runs cmd inside the guest and returns its combined stdout+stderr and
// exit code. sockPath is the host-side path to Firecracker's vsock UDS
// (computed with jailer.WorkspaceRoot + the device path from
// internal/firecracker.BuildConfig).
func Exec(sockPath, cmd string) (output string, exitCode int, err error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return "", 0, fmt.Errorf("dialing vsock uds %s: %w", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialAndExecTimeout))

	// Firecracker's vsock UDS handshake: ask to connect to a guest port and
	// wait for its "OK <port>" ack; the same fd then becomes a raw duplex
	// stream to that guest port.
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", AgentPort); err != nil {
		return "", 0, fmt.Errorf("sending CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	ack, err := reader.ReadString('\n')
	if err != nil {
		return "", 0, fmt.Errorf("reading CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK ") {
		return "", 0, fmt.Errorf("unexpected vsock handshake response: %q", strings.TrimSpace(ack))
	}

	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", 0, fmt.Errorf("sending command: %w", err)
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		return "", 0, fmt.Errorf("reading command output: %w", err)
	}

	full := string(rest)
	idx := strings.LastIndex(full, exitMarker)
	if idx == -1 {
		return full, 0, fmt.Errorf("guest agent response missing exit marker — is scripts/prepare-image.sh applied to this VM's template?")
	}

	output = full[:idx]
	codeStr := strings.TrimSpace(full[idx+len(exitMarker):])
	code, convErr := strconv.Atoi(codeStr)
	if convErr != nil {
		return output, 0, fmt.Errorf("parsing exit code %q: %w", codeStr, convErr)
	}

	return output, code, nil
}
