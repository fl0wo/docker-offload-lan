// Package nodeclient is the Go client of the node API (pkg/api/v1). It
// reaches the node through `ssh <host> dol node dial-stdio` (ADR-0002), or a
// local unix socket / arbitrary dialer for tests and node-local tools.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// DialFunc opens a byte stream to the node API.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client talks to one node.
type Client struct {
	Node string
	hc   *http.Client
	tr   *http.Transport
}

// New builds a client using dial for every connection.
func New(node string, dial DialFunc) *Client {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	return &Client{Node: node, tr: tr, hc: &http.Client{Transport: tr}}
}

// DefaultRemoteCommand is what the client runs on the node over ssh.
var DefaultRemoteCommand = []string{"dol", "node", "dial-stdio"}

// NewSSH builds a client that dials through `ssh host dol node dial-stdio`.
func NewSSH(node, sshHost string, opts sshx.Options, remoteCmd ...string) *Client {
	if len(remoteCmd) == 0 {
		remoteCmd = DefaultRemoteCommand
	}
	return New(node, func(ctx context.Context) (net.Conn, error) {
		return opts.DialStdio(ctx, sshHost, remoteCmd...)
	})
}

// NewUnix builds a client for a unix socket on this machine.
func NewUnix(node, socket string) *Client {
	return New(node, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	})
}

// Close releases idle connections (and their ssh processes).
func (c *Client) Close() { c.tr.CloseIdleConnections() }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://dol-node"+v1.PathPrefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportError(ctx, err, time.Since(start))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return c.transportError(ctx, err, time.Since(start))
	}
	if resp.StatusCode >= 300 {
		var er v1.ErrorResponse
		if json.Unmarshal(b, &er) == nil && er.Error != nil && er.Error.Code != "" {
			return er.Error
		}
		return v1.Errorf(v1.CodeInternal, "check the node agent logs: `journalctl -u dol-node` on the node",
			"node %s returned HTTP %d: %s", c.Node, resp.StatusCode, truncate(string(b), 200))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return v1.Errorf(v1.CodeVersionMismatch, "make sure client and node run the same dol version (`dol --version` on both)",
				"node %s sent an unreadable response: %v", c.Node, err)
		}
	}
	return nil
}

func (c *Client) transportError(ctx context.Context, err error, took time.Duration) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return v1.Errorf(v1.CodeTimeout, "the node may be asleep, overloaded or off the LAN; run `dol doctor`",
			"node %s did not answer within %s", c.Node, took.Round(time.Millisecond))
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Err.Error()
	}
	hint := sshx.Hint(msg)
	if hint == "" {
		hint = "run `dol doctor` to check each link (ssh, node agent, docker)"
	}
	return v1.Errorf(v1.CodeUnreachable, hint, "cannot reach node %s: %s", c.Node, msg)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Health calls GET /v1/healthz.
func (c *Client) Health(ctx context.Context) (*v1.Health, error) {
	var h v1.Health
	return &h, c.do(ctx, http.MethodGet, "/healthz", nil, &h)
}

// Capacity calls GET /v1/capacity.
func (c *Client) Capacity(ctx context.Context) (*v1.Capacity, error) {
	var cp v1.Capacity
	return &cp, c.do(ctx, http.MethodGet, "/capacity", nil, &cp)
}

// CreateLease calls POST /v1/leases.
func (c *Client) CreateLease(ctx context.Context, req v1.LeaseRequest) (*v1.Lease, error) {
	var l v1.Lease
	return &l, c.do(ctx, http.MethodPost, "/leases", req, &l)
}

// ListLeases calls GET /v1/leases. Empty filters are omitted.
func (c *Client) ListLeases(ctx context.Context, owner string, state v1.LeaseState) ([]v1.Lease, error) {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if state != "" {
		q.Set("state", string(state))
	}
	p := "/leases"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var ll v1.LeaseList
	return ll.Leases, c.do(ctx, http.MethodGet, p, nil, &ll)
}

func leasePath(id string, rest ...string) string {
	return "/leases/" + url.PathEscape(id) + strings.Join(rest, "")
}

// GetLease calls GET /v1/leases/{id}.
func (c *Client) GetLease(ctx context.Context, id string) (*v1.Lease, error) {
	var l v1.Lease
	return &l, c.do(ctx, http.MethodGet, leasePath(id), nil, &l)
}

// Heartbeat calls POST /v1/leases/{id}/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, id string) (*v1.HeartbeatResponse, error) {
	var h v1.HeartbeatResponse
	return &h, c.do(ctx, http.MethodPost, leasePath(id, "/heartbeat"), nil, &h)
}

// PatchLease calls PATCH /v1/leases/{id}.
func (c *Client) PatchLease(ctx context.Context, id string, p v1.LeasePatch) (*v1.Lease, error) {
	var l v1.Lease
	return &l, c.do(ctx, http.MethodPatch, leasePath(id), p, &l)
}

// ReleaseLease calls DELETE /v1/leases/{id}. keepSyncDir maps to
// ?keep_sync_dir=true.
func (c *Client) ReleaseLease(ctx context.Context, id string, keepSyncDir bool) (*v1.ReleaseResponse, error) {
	p := leasePath(id)
	if keepSyncDir {
		p += "?keep_sync_dir=true"
	}
	var r v1.ReleaseResponse
	return &r, c.do(ctx, http.MethodDelete, p, nil, &r)
}

// Resources calls GET /v1/leases/{id}/resources.
func (c *Client) Resources(ctx context.Context, id string) (*v1.Inventory, error) {
	var inv v1.Inventory
	return &inv, c.do(ctx, http.MethodGet, leasePath(id, "/resources"), nil, &inv)
}

// RegisterCompose calls POST /v1/leases/{id}/compose/{project}: the node then
// tracks the project and tears it down with the lease.
func (c *Client) RegisterCompose(ctx context.Context, id, project string) error {
	return c.do(ctx, http.MethodPost, leasePath(id, "/compose/", url.PathEscape(project)), nil, nil)
}

// RegisterSyncDir calls POST /v1/leases/{id}/sync: the node tracks the
// directory and deletes it with the lease. The path must be under the node's
// sync_root.
func (c *Client) RegisterSyncDir(ctx context.Context, id, path string) error {
	return c.do(ctx, http.MethodPost, leasePath(id, "/sync"), v1.SyncRegisterRequest{Path: path}, nil)
}

// KindCreate calls POST /v1/leases/{id}/kind.
func (c *Client) KindCreate(ctx context.Context, id string, req v1.KindCreateRequest) (*v1.KindCreateResponse, error) {
	var r v1.KindCreateResponse
	return &r, c.do(ctx, http.MethodPost, leasePath(id, "/kind"), req, &r)
}

// KindLoad calls POST /v1/leases/{id}/kind/{name}/load.
func (c *Client) KindLoad(ctx context.Context, id, name string, images []string) (*v1.KindLoadResponse, error) {
	var r v1.KindLoadResponse
	return &r, c.do(ctx, http.MethodPost, leasePath(id, "/kind/", url.PathEscape(name), "/load"), v1.KindLoadRequest{Images: images}, &r)
}

// KindDelete calls DELETE /v1/leases/{id}/kind/{name}.
func (c *Client) KindDelete(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodDelete, leasePath(id, "/kind/", url.PathEscape(name)), nil, nil)
}

// Drain calls POST /v1/admin/drain.
func (c *Client) Drain(ctx context.Context, draining bool) error {
	return c.do(ctx, http.MethodPost, "/admin/drain", v1.DrainRequest{Draining: draining}, nil)
}

// String implements fmt.Stringer.
func (c *Client) String() string { return fmt.Sprintf("nodeclient(%s)", c.Node) }
