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
// artifacts back out — moving a lot of data over vsock — without a network.
package vsock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"microhosted/internal/jailer"
)

// AgentPort is the fixed vsock port the guest-side listener listens on.
// Arbitrary but fixed for this whole project — one place to change it.
const AgentPort = 52

const exitMarker = "___MICROHOSTED_EXIT___:"

// dialTimeout bounds establishing the UDS connection and the vsock handshake.
const dialTimeout = 5 * time.Second

// DefaultExecTimeout bounds a command from dial to full response when the
// caller asks for no particular deadline.
const DefaultExecTimeout = 30 * time.Second

// MaxExecTimeout is the longest deadline a caller may ask an exec for. Each
// exec in flight holds a connection and a goroutine in the daemon, so the
// ceiling keeps a caller from parking them indefinitely; work longer than this
// belongs in the guest, polled or fetched as a file.
const MaxExecTimeout = 10 * time.Minute

// ErrTimeout reports an exec that did not return its exit marker before its
// deadline. The host stops waiting and closes the connection; the command
// itself is not guaranteed to stop inside the guest.
var ErrTimeout = errors.New("exec timed out")

// transferTimeout bounds a file PUT/GET. Larger than DefaultExecTimeout because a bulk
// artifact (a memory dump, a pcap) can be big and slow to stream over vsock.
const transferTimeout = 10 * time.Minute

// maxAgentResponse bounds how much of a single agent response the host will
// buffer (an exec's combined output, or a PUT's ack). The guest is untrusted by
// design — containing it is the entire point of the microVM — so reading its
// reply without a ceiling is a path from a compromised guest straight into the
// daemon's heap: it answers with an endless stream and the host OOMs while
// holding every other VM on the machine. 8 MiB is orders of magnitude above any
// real command's output; bulk data belongs in a file and travels through
// GetFileStream, which streams instead of buffering and is deliberately not
// subject to this cap.
const maxAgentResponse = 8 << 20

// maxHeaderLine bounds the single-line control messages the host reads before
// any payload: the CONNECT ack and a GET's "OK <len>"/"ERR <msg>" header. Both
// cross the trust boundary — the ack comes from the Firecracker process (which
// runs as the VM's unprivileged identity and is assumed compromisable), the GET
// header from the guest agent — so neither may grow a buffer without limit by
// simply never sending a newline. Real headers are a few dozen bytes.
const maxHeaderLine = 4096

// dialJail connects to the vsock UDS inside the VM's chroot without following
// anything the jailed Firecracker could have planted there (see
// jailer.DialSocket). A variable only so tests can point it at a plain socket.
var dialJail = jailer.DialSocket

// dial opens the Firecracker vsock UDS and performs the CONNECT handshake,
// returning a live duplex stream to the guest's AgentPort plus a buffered
// reader positioned right after the "OK <port>" ack. uid is the VM's jailed
// identity, which must own the socket. deadline is applied to the whole
// connection, handshake included; establishing it is further bounded by
// dialTimeout and by ctx. The caller owns closing conn.
func dial(ctx context.Context, sockPath string, uid int, deadline time.Time) (net.Conn, *bufio.Reader, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := dialJail(dctx, sockPath, uid)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("dialing vsock uds %s: %w", sockPath, err)
	}
	_ = conn.SetDeadline(deadline)

	// Firecracker's vsock UDS handshake: ask to connect to a guest port and
	// wait for its "OK <port>" ack; the same fd then becomes a raw duplex
	// stream to that guest port.
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", AgentPort); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sending CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	ack, err := readLine(reader, maxHeaderLine)
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

// readLine reads one '\n'-terminated line of at most max bytes (terminator
// included). Like readToMarker it uses ReadSlice so a peer that never sends a
// newline costs at most max bytes of buffering before the read is refused.
func readLine(reader *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return "", fmt.Errorf("line exceeds %d bytes", max)
		}
		buf = append(buf, chunk...)
		switch err {
		case nil:
			return string(buf), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return "", err
		}
	}
}

// readToMarker consumes the agent's response until its terminating exit-marker
// line and returns the output before the marker plus the exit code. It
// deliberately does NOT read to EOF: after a snapshot restore, Firecracker
// (observed on v1.16.1) stops propagating the guest-side close of a
// host-initiated vsock connection as EOF on the host UDS — every byte of the
// response arrives, the connection just never reads as closed, so anything
// blocking on EOF hangs until its deadline. The marker line the agent always
// emits last is the protocol's own explicit terminator; stopping there behaves
// identically on fresh-booted and restored VMs (and also unhangs execs whose
// command leaked the connection fd to a background process). EOF before a
// marker is still an error: agent missing, or it died mid-response. So is a
// response that never terminates — see maxAgentResponse.
func readToMarker(reader *bufio.Reader) (output string, exitCode int, err error) {
	var buf strings.Builder
	for {
		// ReadSlice, not ReadString: it reads into bufio's fixed buffer and
		// reports ErrBufferFull rather than growing one, so a guest that simply
		// never sends a newline cannot make a single read allocate without
		// bound. The bytes are copied out immediately (the slice is only valid
		// until the next read) and a partial line just continues next pass.
		chunk, rerr := reader.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			rerr = nil
		}
		buf.Write(chunk)
		if out, code, ok := parseMarkerTail(buf.String()); ok {
			return out, code, nil
		}
		// Checked after the marker so a response that ends exactly at the
		// ceiling still parses, and the output is dropped rather than returned:
		// handing the caller the 8 MiB we just refused to accept would defeat
		// the point.
		if buf.Len() > maxAgentResponse {
			return "", 0, fmt.Errorf("guest agent response exceeded %d bytes with no exit marker — output that large belongs in a file, fetched with GetFile", maxAgentResponse)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return buf.String(), 0, fmt.Errorf("guest agent response missing exit marker — is scripts/prepare-image.sh applied to this VM's template?")
			}
			return buf.String(), 0, fmt.Errorf("reading agent response: %w", rerr)
		}
	}
}

// parseMarkerTail reports whether full ends with a complete exit-marker line
// ("<marker><int>\n", possibly glued to output that lacked a trailing newline)
// and, if so, returns the output preceding it. Only a bounded tail is
// inspected, so responses of any size stay O(1) per check. In-band framing has
// one inherent ambiguity: an output line that itself ends exactly like the
// terminator would end the read early — the same class of ambiguity the old
// read-to-EOF parser had, just resolved at the first candidate instead of the
// last.
func parseMarkerTail(full string) (string, int, bool) {
	if len(full) == 0 || full[len(full)-1] != '\n' {
		return "", 0, false
	}
	tailStart := len(full) - (len(exitMarker) + 16)
	if tailStart < 0 {
		tailStart = 0
	}
	idx := strings.LastIndex(full[tailStart:], exitMarker)
	if idx == -1 {
		return "", 0, false
	}
	idx += tailStart
	codeStr := full[idx+len(exitMarker) : len(full)-1]
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return "", 0, false
	}
	return full[:idx], code, true
}

// Exec runs cmd inside the guest and returns its combined stdout+stderr and
// exit code. sockPath is the host-side path to Firecracker's vsock UDS
// (computed with jailer.WorkspaceRoot + the device path from
// internal/firecracker.BuildConfig) and uid the VM's jailed identity. The
// whole exchange is bounded by DefaultExecTimeout.
func Exec(sockPath string, uid int, cmd string) (output string, exitCode int, err error) {
	return ExecContext(context.Background(), sockPath, uid, cmd, DefaultExecTimeout)
}

// ExecContext is Exec with a caller-chosen deadline, timeout, counted from the
// call and covering dial, handshake and the full response. Past it the host
// closes the connection and the error wraps ErrTimeout. Cancelling ctx (an API
// client that hung up) aborts the same way and returns ctx's error. timeout
// must be in (0, MaxExecTimeout]; the caller validates it.
func ExecContext(ctx context.Context, sockPath string, uid int, cmd string, timeout time.Duration) (output string, exitCode int, err error) {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	conn, reader, err := dial(ctx, sockPath, uid, deadline)
	if err != nil {
		return "", 0, execAbort(ctx, timeout, err)
	}
	defer conn.Close()
	// A cancelled ctx unblocks the read at once instead of at the deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", 0, execAbort(ctx, timeout, fmt.Errorf("sending command: %w", err))
	}

	output, exitCode, err = readToMarker(reader)
	if err != nil {
		// Partial output is dropped: a truncated answer must not pass for one.
		return "", 0, execAbort(ctx, timeout, err)
	}
	return output, exitCode, nil
}

// Probe reports whether the guest agent is accepting connections: the CONNECT
// handshake only succeeds once a listener inside the guest took the connection
// (Firecracker acks after the guest accepts, and closes it when nothing listens
// on the port). Nothing is sent after the handshake, so no command runs; the
// agent sees an empty request and ends it. The host dials, as on every vsock
// path — readiness never needs the guest to reach the host.
func Probe(ctx context.Context, sockPath string, uid int, timeout time.Duration) error {
	conn, _, err := dial(ctx, sockPath, uid, time.Now().Add(timeout))
	if err != nil {
		return err
	}
	return conn.Close()
}

// execAbort names why an exec stopped early: its own deadline (ErrTimeout), the
// caller's cancellation (ctx's error), or err itself when neither applies.
func execAbort(ctx context.Context, timeout time.Duration, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return ctx.Err()
	case ctx.Err() != nil, errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	return err
}

// PutFile streams data (size bytes) into the guest at guestPath, creating parent
// directories as needed. It's the write half of the bulk channel — pushing a
// sample or corpus into a live VM without a network. Returns an error if the
// guest agent reports a non-zero exit writing the file.
func PutFile(sockPath string, uid int, guestPath string, data io.Reader, size int64) error {
	conn, reader, err := dial(context.Background(), sockPath, uid, time.Now().Add(transferTimeout))
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

	out, code, err := readToMarker(reader)
	if err != nil {
		return fmt.Errorf("reading PUT result: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("guest failed to write %s (exit %d): %s", guestPath, code, strings.TrimSpace(out))
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
func GetFileStream(sockPath string, uid int, guestPath string) (io.ReadCloser, int64, error) {
	conn, reader, err := dial(context.Background(), sockPath, uid, time.Now().Add(transferTimeout))
	if err != nil {
		return nil, 0, err
	}

	if _, err := fmt.Fprintf(conn, "GET %s\n", guestPath); err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("sending GET: %w", err)
	}

	header, err := readLine(reader, maxHeaderLine)
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("reading GET header: %w", err)
	}
	header = strings.TrimRight(header, "\n")
	switch {
	case strings.HasPrefix(header, "OK "):
		size, convErr := strconv.ParseInt(strings.TrimSpace(header[len("OK "):]), 10, 64)
		if convErr == nil && size < 0 {
			convErr = fmt.Errorf("negative length")
		}
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
