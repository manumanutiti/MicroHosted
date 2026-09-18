package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// DefaultSocket is where the daemon listens unless told otherwise (see
// cmd/microhosted --socket).
const DefaultSocket = "/run/microhosted.sock"

// Client talks to the daemon's HTTP API over its Unix socket (or a TCP address
// when the daemon was started with --addr). It is a thin layer: one method per
// verb, JSON in and out, and API errors surfaced with the daemon's own message.
type Client struct {
	http *http.Client
	// base is the URL prefix every path is appended to. Over a Unix socket the
	// host part is irrelevant — the dialer ignores it — so it is a fixed
	// placeholder.
	base string
	// where describes the endpoint for error messages ("unix /run/...").
	where string
	// socket is the socket path, empty over TCP: permission errors only have
	// an actionable fix (join the socket's group) in the socket case.
	socket string
}

// ResolveHost picks the endpoint: the -H flag, then $MICROHOSTED_HOST, then
// $MICROHOSTED_SOCKET (the variable the older curl helper in docs/api.md used),
// then the default socket.
func ResolveHost(flagValue string) string {
	for _, v := range []string{flagValue, os.Getenv("MICROHOSTED_HOST"), os.Getenv("MICROHOSTED_SOCKET")} {
		if v != "" {
			return v
		}
	}
	return DefaultSocket
}

// NewClient builds a client for host, which is one of:
//
//	unix:///run/microhosted.sock   /run/microhosted.sock   ./dev.sock
//	tcp://127.0.0.1:8080           127.0.0.1:8080
func NewClient(host string) (*Client, error) {
	switch {
	case strings.HasPrefix(host, "unix://"):
		return unixClient(strings.TrimPrefix(host, "unix://")), nil
	case strings.HasPrefix(host, "tcp://"):
		return tcpClient(strings.TrimPrefix(host, "tcp://")), nil
	case strings.HasPrefix(host, "http://"):
		return tcpClient(strings.TrimPrefix(host, "http://")), nil
	case strings.HasPrefix(host, "/"), strings.HasPrefix(host, "."):
		return unixClient(host), nil
	case strings.Contains(host, ":"):
		return tcpClient(host), nil
	}
	return nil, fmt.Errorf("host %q: use a socket path (/run/microhosted.sock, unix://...) or tcp://host:port", host)
}

func unixClient(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	// No timeout: creating a VM, snapshotting or copying a multi-GB file take
	// as long as they take, and Ctrl-C is the operator's timeout.
	return &Client{http: &http.Client{Transport: tr}, base: "http://microhosted", where: "unix " + path, socket: path}
}

func tcpClient(addr string) *Client {
	return &Client{http: &http.Client{}, base: "http://" + strings.TrimSuffix(addr, "/"), where: "tcp " + addr}
}

// APIError is a non-2xx answer from the daemon, carrying its "error" message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("daemon answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return e.Message
}

// IsNotFound reports whether err is the daemon's 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Do sends a JSON request (body may be nil) and decodes a JSON answer into out
// (may be nil). A non-2xx status becomes an *APIError.
func (c *Client) Do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	resp, err := c.send(method, path, rd, -1, body != nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream sends a raw body (a file upload) and discards the answer. size is the
// body's length, or -1 when unknown (stdin); the daemon copes with both.
func (c *Client) Stream(method, path string, body io.Reader, size int64) error {
	resp, err := c.send(method, path, body, size, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp)
}

// Raw performs a GET and hands back the response unchecked — for downloads,
// and for /v1/health, whose 503 is an answer rather than a failure. The caller
// closes the body.
func (c *Client) Raw(path string) (*http.Response, error) {
	return c.send(http.MethodGet, path, nil, -1, false)
}

func (c *Client) send(method, path string, body io.Reader, size int64, isJSON bool) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.explain(err)
	}
	return resp, nil
}

// explain turns a transport failure into what the operator should do about it.
// curl reports all of these as "Could not connect to server", which makes a
// missing group membership look like a dead daemon — the one confusion worth
// a dedicated message.
func (c *Client) explain(err error) error {
	switch {
	case c.socket != "" && errors.Is(err, syscall.EACCES):
		return socketAccessError(c.socket)
	case c.socket != "" && errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("%s does not exist: is the daemon running? (systemctl status microhosted)", c.socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("nothing is listening on %s: is the daemon running? (systemctl status microhosted)", c.where)
	}
	return fmt.Errorf("reaching microhosted at %s: %w", c.where, err)
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

// socketAccessError explains a permission denied on the socket by looking at
// it: "join the group" is the wrong advice when the socket has no group at all
// (the daemon was installed without --socket-group) or when the caller is
// already in it but in a session opened before usermod.
func socketAccessError(path string) error {
	var st *syscall.Stat_t
	fi, err := os.Stat(path)
	if err == nil {
		st, _ = fi.Sys().(*syscall.Stat_t)
	}
	if st == nil {
		return fmt.Errorf("permission denied on %s: run with sudo, or see docs/api.md § Calling the API", path)
	}
	if fi.Mode().Perm()&0o060 != 0o060 || st.Gid == 0 {
		return fmt.Errorf("permission denied on %s: the socket is root-only (%s %s), the daemon was installed without a socket group — "+
			"reinstall with: make install-service SOCKET_GROUP=microhosted  (or use sudo mh)", path, fi.Mode().Perm(), groupName(st.Gid))
	}
	gid := int(st.Gid)
	groups, _ := os.Getgroups()
	for _, g := range groups {
		if g == gid {
			return fmt.Errorf("permission denied on %s even though this session is in group %s: check the socket's mode (%s)", path, groupName(st.Gid), fi.Mode().Perm())
		}
	}
	return fmt.Errorf("permission denied on %s: this session is not in group %s — sudo usermod -aG %s $USER, then log in again (or use sudo mh)",
		path, groupName(st.Gid), groupName(st.Gid))
}

func groupName(gid uint32) string {
	if g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10)); err == nil {
		return g.Name
	}
	return strconv.FormatUint(uint64(gid), 10)
}
