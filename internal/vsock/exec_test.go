package vsock

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveOne stands up a fake Firecracker vsock UDS that accepts exactly one
// connection, answers the CONNECT handshake, and hands the connection to
// handle. hold=true reproduces the post-snapshot-restore behavior this package
// must survive: the response is written but the connection is never closed
// from the far side (no EOF ever reaches the client). The far end drains until
// the client closes, so the goroutine always exits.
func serveOne(t *testing.T, hold bool, handle func(r *bufio.Reader, w net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "CONNECT ") {
			return
		}
		fmt.Fprintf(conn, "OK 52\n")
		handle(r, conn)
		if hold {
			// Never close: block until the CLIENT closes (its deferred
			// conn.Close after parsing the marker), like a restored VM whose
			// guest-side close never propagates.
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	return sock
}

// TestExecNoEOF is the regression test for exec on snapshot-restored VMs: the
// full response arrives but EOF never does. Exec must return promptly from the
// marker alone.
func TestExecNoEOF(t *testing.T) {
	sock := serveOne(t, true, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil { // the command line
			return
		}
		fmt.Fprintf(w, "hello\n%s0\n", exitMarker)
	})

	start := time.Now()
	out, code, err := Exec(sock, "echo hello")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hello\n" || code != 0 {
		t.Fatalf("got output %q code %d", out, code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Exec took %s — still waiting for EOF instead of stopping at the marker", d)
	}
}

// TestExecWithEOF covers the classic path (fresh-booted VM: response, then the
// guest closes) — stopping at the marker must not change its result.
func TestExecWithEOF(t *testing.T) {
	sock := serveOne(t, false, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprintf(w, "line1\nline2\n%s3\n", exitMarker)
	})

	out, code, err := Exec(sock, "whatever")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "line1\nline2\n" || code != 3 {
		t.Fatalf("got output %q code %d", out, code)
	}
}

// TestExecOutputWithoutTrailingNewline: the agent's echo glues the marker to
// output that didn't end in \n ("foo___MICROHOSTED_EXIT___:7\n").
func TestExecOutputWithoutTrailingNewline(t *testing.T) {
	sock := serveOne(t, true, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprintf(w, "foo%s7\n", exitMarker)
	})

	out, code, err := Exec(sock, "printf foo; exit 7")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "foo" || code != 7 {
		t.Fatalf("got output %q code %d", out, code)
	}
}

// TestExecMarkerLikeTextMidLine: marker text NOT terminating a line (no int
// right before the newline) must not end the read.
func TestExecMarkerLikeTextMidLine(t *testing.T) {
	sock := serveOne(t, true, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprintf(w, "echo \"%s$?\"\n%s0\n", exitMarker, exitMarker)
	})

	out, code, err := Exec(sock, "cat /usr/local/bin/microhosted-exec")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := "echo \"" + exitMarker + "$?\"\n"
	if out != want || code != 0 {
		t.Fatalf("got output %q code %d, want %q code 0", out, code, want)
	}
}

// TestExecMissingMarker: EOF with no marker is still the "agent missing" error.
func TestExecMissingMarker(t *testing.T) {
	sock := serveOne(t, false, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprintf(w, "garbage with no marker\n")
	})

	_, _, err := Exec(sock, "true")
	if err == nil || !strings.Contains(err.Error(), "missing exit marker") {
		t.Fatalf("want missing-marker error, got %v", err)
	}
}

// flood writes past the response ceiling and stops. A real compromised guest
// streams forever; a test goroutine must not, so it gives up once the client has
// had more than enough to refuse — or as soon as the client closes on it.
func flood(w net.Conn, unit string) {
	for sent := 0; sent < maxAgentResponse+(1<<20); sent += len(unit) {
		if _, err := io.WriteString(w, unit); err != nil {
			return
		}
	}
}

// TestExecFloodIsCapped is the containment test for a compromised guest: the
// agent answers an exec with an endless stream and never sends an exit marker.
// The host must refuse at maxAgentResponse instead of buffering it into its own
// heap — the daemon runs as root and holds every other VM on the machine, so an
// unbounded read here turns one compromised parser into a gateway outage.
func TestExecFloodIsCapped(t *testing.T) {
	sock := serveOne(t, false, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		flood(w, strings.Repeat("A", 1023)+"\n")
	})

	out, _, err := Exec(sock, "cat /dev/urandom")
	if err == nil {
		t.Fatal("Exec accepted an unbounded agent response")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("got %v, want the response-ceiling error", err)
	}
	if len(out) != 0 {
		t.Fatalf("Exec returned %d bytes of the response it had just refused", len(out))
	}
}

// TestExecFloodWithoutNewlinesIsCapped covers the shape a line-oriented read
// would still have grown an unbounded buffer for: one endless "line" the guest
// never terminates. The ceiling has to hold without any delimiter arriving.
func TestExecFloodWithoutNewlinesIsCapped(t *testing.T) {
	sock := serveOne(t, false, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		flood(w, strings.Repeat("A", 64<<10))
	})

	out, _, err := Exec(sock, "yes | tr -d '\\n'")
	if err == nil {
		t.Fatal("Exec accepted an unterminated agent response")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("got %v, want the response-ceiling error", err)
	}
	if len(out) != 0 {
		t.Fatalf("Exec returned %d bytes of the response it had just refused", len(out))
	}
}

// TestPutFileNoEOF: PUT must also complete from the marker alone on a
// connection that never closes.
func TestPutFileNoEOF(t *testing.T) {
	payload := "sample-data"
	sock := serveOne(t, true, func(r *bufio.Reader, w net.Conn) {
		header, err := r.ReadString('\n') // "PUT <path> <len>\n"
		if err != nil || !strings.HasPrefix(header, "PUT ") {
			return
		}
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		fmt.Fprintf(w, "%s0\n", exitMarker)
	})

	start := time.Now()
	if err := PutFile(sock, "/tmp/x", strings.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("PutFile took %s — still waiting for EOF instead of stopping at the marker", d)
	}
}

// TestPutFileGuestFailure: non-zero marker code surfaces as an error with the
// guest's output.
func TestPutFileGuestFailure(t *testing.T) {
	sock := serveOne(t, true, func(r *bufio.Reader, w net.Conn) {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		fmt.Fprintf(w, "%s1\n", exitMarker)
	})

	err := PutFile(sock, "/readonly/x", strings.NewReader("a"), 1)
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("want guest-failure error, got %v", err)
	}
}
