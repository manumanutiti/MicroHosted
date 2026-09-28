// Package engine is the orchestrator's client of the MicroHosted engine API.
// It speaks only the public wire types of pkg/types: the orchestrator is a
// client like any other and imports nothing from the engine's internals
// (docs/orchestrator.md §3).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"microhosted/pkg/types"
)

// DefaultSocket is where the engine serves its API.
const DefaultSocket = "/run/microhosted.sock"

// ResolveSocket picks the engine endpoint the way the mh CLI does: the flag,
// then $MICROHOSTED_HOST, then $MICROHOSTED_SOCKET, then the default socket.
func ResolveSocket(flag string) string {
	for _, v := range []string{flag, os.Getenv("MICROHOSTED_HOST"), os.Getenv("MICROHOSTED_SOCKET")} {
		if v != "" {
			return strings.TrimPrefix(v, "unix://")
		}
	}
	return DefaultSocket
}

// Error is a non-2xx answer from the engine.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("engine %d: %s", e.Status, e.Msg) }

// StatusOf returns the HTTP status of an engine error, 0 for anything else.
func StatusOf(err error) int {
	if e, ok := err.(*Error); ok {
		return e.Status
	}
	return 0
}

// Client talks to one engine.
type Client struct {
	http *http.Client
	base string
}

// New returns a client for the engine's Unix socket.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}, base: "http://microhosted"}
}

// NewHTTP returns a client for an engine at a base URL (tests).
func NewHTTP(base string) *Client {
	return &Client{http: http.DefaultClient, base: strings.TrimSuffix(base, "/")}
}

// maxResponse bounds what one answer may cost the orchestrator in memory.
const maxResponse = 16 << 20

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	data, err := c.raw(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// raw performs a request and returns the body of a 2xx answer.
func (c *Client) raw(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &Error{Status: res.StatusCode, Msg: msg}
	}
	return data, nil
}

func selector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	q := url.Values{}
	for k, v := range labels {
		q.Add("label", k+"="+v)
	}
	return "?" + q.Encode()
}

// ListVMs returns the VMs carrying every one of labels.
func (c *Client) ListVMs(ctx context.Context, labels map[string]string) ([]types.VMResponse, error) {
	var out []types.VMResponse
	return out, c.do(ctx, "GET", "/v1/vms"+selector(labels), nil, &out)
}

// CreateVM creates and boots a VM.
func (c *Client) CreateVM(ctx context.Context, req types.CreateVMRequest) (*types.VMResponse, error) {
	var out types.VMResponse
	return &out, c.do(ctx, "POST", "/v1/vms", req, &out)
}

// DestroyVM deletes a VM, disk included. An unknown VM is not an error.
func (c *Client) DestroyVM(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/v1/vms/"+url.PathEscape(id), nil, nil)
	if StatusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// WaitReady blocks until the VM's guest agent answers, or timeout.
func (c *Client) WaitReady(ctx context.Context, id string, timeout time.Duration) error {
	ms := max(timeout.Milliseconds(), 1)
	return c.do(ctx, "GET", "/v1/vms/"+url.PathEscape(id)+"/ready?timeout_ms="+strconv.FormatInt(ms, 10), nil, nil)
}

// Exec runs cmd in the VM through its guest agent, bounded by timeout.
func (c *Client) Exec(ctx context.Context, id, cmd string, timeout time.Duration) (*types.ExecResponse, error) {
	var out types.ExecResponse
	req := types.ExecRequest{Cmd: cmd, TimeoutMS: max(timeout.Milliseconds(), 1)}
	return &out, c.do(ctx, "POST", "/v1/vms/"+url.PathEscape(id)+"/exec", req, &out)
}

// PatchVMLabels merge-patches a VM's labels (a nil value removes the key).
func (c *Client) PatchVMLabels(ctx context.Context, id string, labels map[string]*string) error {
	return c.do(ctx, "PATCH", "/v1/vms/"+url.PathEscape(id)+"/labels", types.UpdateVMLabelsRequest{Labels: labels}, nil)
}

// PatchNetworkLabels merges labels into a network's (nil removes a key).
func (c *Client) PatchNetworkLabels(ctx context.Context, name string, labels map[string]*string) error {
	return c.do(ctx, "PATCH", "/v1/networks/"+url.PathEscape(name)+"/labels", types.UpdateNetworkLabelsRequest{Labels: labels}, nil)
}

// ListNetworks returns the networks carrying every one of labels.
func (c *Client) ListNetworks(ctx context.Context, labels map[string]string) ([]types.NetworkResponse, error) {
	var out []types.NetworkResponse
	return out, c.do(ctx, "GET", "/v1/networks"+selector(labels), nil, &out)
}

// CreateNetwork creates a network with its whole policy.
func (c *Client) CreateNetwork(ctx context.Context, req types.CreateNetworkRequest) error {
	return c.do(ctx, "POST", "/v1/networks", req, nil)
}

// DeleteNetwork deletes a network with no VMs on it.
func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", "/v1/networks/"+url.PathEscape(name), nil, nil)
	if StatusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// SetEgress replaces a network's egress policy.
func (c *Client) SetEgress(ctx context.Context, name string, req types.UpdateNetworkEgressRequest) error {
	return c.do(ctx, "PUT", "/v1/networks/"+url.PathEscape(name)+"/egress", req, nil)
}

// SetIngress replaces a network's ingress policy.
func (c *Client) SetIngress(ctx context.Context, name string, rules []types.IngressRule) error {
	if rules == nil {
		rules = []types.IngressRule{}
	}
	return c.do(ctx, "PUT", "/v1/networks/"+url.PathEscape(name)+"/ingress", types.UpdateNetworkIngressRequest{AllowedIngress: rules}, nil)
}

// SetIntra flips VM↔VM reachability within a network.
func (c *Client) SetIntra(ctx context.Context, name string, intra bool) error {
	return c.do(ctx, "PUT", "/v1/networks/"+url.PathEscape(name)+"/intra", types.UpdateNetworkIntraRequest{Intra: intra}, nil)
}

// GetImage resolves an image reference in the engine's store.
func (c *Client) GetImage(ctx context.Context, ref string) (*types.ImageResponse, error) {
	var out types.ImageResponse
	return &out, c.do(ctx, "GET", "/v1/images/"+url.PathEscape(ref), nil, &out)
}

// Console returns the last tail bytes of a VM's console log. The bytes are
// the guest's: untrusted data.
func (c *Client) Console(ctx context.Context, id string, tail int) ([]byte, error) {
	return c.raw(ctx, "GET", "/v1/vms/"+url.PathEscape(id)+"/console?tail="+strconv.Itoa(tail), nil)
}
