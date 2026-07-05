// Package vsock talks to the microhosted-exec listener that
// scripts/prepare-image.sh installs inside a VM's guest (a systemd unit
// running `socat VSOCK-LISTEN:52,fork EXEC:/usr/local/bin/microhosted-exec`).
// It's the programmatic access path: no SSH keys, no IP, no TAP device — it
// works even on a VM created with no_network:true, since vsock isn't a
// network interface in that sense.
//
// The agent multiplexes three verbs over the same port, dispatched on the first
// line of each connection:
//
//   - bare command (no verb)  → run it, return combined output + exit marker
//   - PUT <path> <len>\n+bytes → write the file, return the exit marker
//   - GET <path>\n            → return "OK <len>\n"+bytes, or "ERR <msg>\n"
//
// PUT/GET are the bulk data channel: pushing a sample into a live VM or pulling
// artifacts back out — "sacar muchos datos por el vsock" — without a network.
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

// dialTimeout bounds establishing the UDS connection and the vsock handshake.
const dialTimeout = 5 * time.Second

// execTimeout bounds a command from send to full response.
const execTimeout = 30 * time.Second

// transferTimeout bounds a file PUT/GET. Larger than execTimeout because a bulk
// artifact (a memory dump, a pcap) can be big and slow to stream over vsock.
const transferTimeout = 10 * time.Minute

// dial opens the Firecracker vsock UDS and performs the CONNECT handshake,
// returning a live duplex stream to the guest's AgentPort plus a buffered
// reader positioned right after the "OK <port>" ack. deadline is applied to the
// whole connection. The caller owns closing conn.
func dial(sockPath string, deadline time.Duration) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("unix", sockPath, dialTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing vsock uds %s: %w", sockPath, err)
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))

	// Firecracker's vsock UDS handshake: ask to connect to a guest port and
	// wait for its "OK <port>" ack; the same fd then becomes a raw duplex
	// stream to that guest port.
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", AgentPort); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sending CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	ack, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("reading CONNECT ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK ") {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("unexpected vsock handshake response: %q", strings.TrimSpace(ack))
	}
	return conn, reader, nil
}

// Exec runs cmd inside the guest and returns its combined stdout+stderr and
// exit code. sockPath is the host-side path to Firecracker's vsock UDS
// (computed with jailer.WorkspaceRoot + the device path from
// internal/firecracker.BuildConfig).
func Exec(sockPath, cmd string) (output string, exitCode int, err error) {
	conn, reader, err := dial(sockPath, execTimeout)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()

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

// PutFile streams data (size bytes) into the guest at guestPath, creating parent
// directories as needed. It's the write half of the bulk channel — pushing a
// sample or corpus into a live VM without a network. Returns an error if the
// guest agent reports a non-zero exit writing the file.
func PutFile(sockPath, guestPath string, data io.Reader, size int64) error {
	conn, reader, err := dial(sockPath, transferTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "PUT %s %d\n", guestPath, size); err != nil {
		return fmt.Errorf("sending PUT header: %w", err)
	}
	if _, err := io.CopyN(conn, data, size); err != nil {
		return fmt.Errorf("streaming file to guest: %w", err)
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("reading PUT result: %w", err)
	}
	full := string(rest)
	idx := strings.LastIndex(full, exitMarker)
	if idx == -1 {
		return fmt.Errorf("guest agent response missing exit marker on PUT — is scripts/prepare-image.sh applied to this VM's template?")
	}
	codeStr := strings.TrimSpace(full[idx+len(exitMarker):])
	code, convErr := strconv.Atoi(codeStr)
	if convErr != nil {
		return fmt.Errorf("parsing PUT exit code %q: %w", codeStr, convErr)
	}
	if code != 0 {
		return fmt.Errorf("guest failed to write %s (exit %d): %s", guestPath, code, strings.TrimSpace(full[:idx]))
	}
	return nil
}

// fileStream is a streaming reader over a GET's payload: it reads exactly the
// advertised number of bytes off the vsock connection and closes it on Close.
// Nothing is buffered — the caller copies straight from the guest to wherever
// the bytes are going, so a multi-GB download costs a fixed buffer, not its size.
type fileStream struct {
	conn net.Conn
	body io.Reader // io.LimitReader over the connection's buffered reader
}

func (fs *fileStream) Read(p []byte) (int, error) { return fs.body.Read(p) }
func (fs *fileStream) Close() error               { return fs.conn.Close() }

// GetFileStream reads guestPath out of the live guest and returns a streaming
// reader over its contents plus the total size. The read half of the bulk
// channel — pulling artifacts off a running VM over vsock. The caller must Close
// the returned reader (it owns the underlying connection).
func GetFileStream(sockPath, guestPath string) (io.ReadCloser, int64, error) {
	conn, reader, err := dial(sockPath, transferTimeout)
	if err != nil {
		return nil, 0, err
	}

	if _, err := fmt.Fprintf(conn, "GET %s\n", guestPath); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("sending GET: %w", err)
	}

	header, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("reading GET header: %w", err)
	}
	header = strings.TrimRight(header, "\n")
	switch {
	case strings.HasPrefix(header, "OK "):
		size, convErr := strconv.ParseInt(strings.TrimSpace(header[len("OK "):]), 10, 64)
		if convErr != nil {
			_ = conn.Close()
			return nil, 0, fmt.Errorf("parsing GET length %q: %w", header, convErr)
		}
		return &fileStream{conn: conn, body: io.LimitReader(reader, size)}, size, nil
	case strings.HasPrefix(header, "ERR "):
		_ = conn.Close()
		return nil, 0, fmt.Errorf("guest could not read %s: %s", guestPath, strings.TrimSpace(header[len("ERR "):]))
	default:
		_ = conn.Close()
		return nil, 0, fmt.Errorf("unexpected GET response: %q", header)
	}
}
