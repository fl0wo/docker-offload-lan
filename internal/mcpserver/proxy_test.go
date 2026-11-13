package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func proxyEnabled(c *config.ClientConfig) { c.Proxy.Enabled = true }

// pingSocket sends GET /_ping through a proxy socket and returns the status
// and the Docker-style error message, if any.
func pingSocket(t *testing.T, host string) (int, string) {
	t.Helper()
	path := strings.TrimPrefix(host, "unix://")
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	defer cl.CloseIdleConnections()
	resp, err := cl.Get("http://docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var e struct{ Message string }
	_ = json.Unmarshal(b, &e)
	return resp.StatusCode, e.Message
}

func TestProxyIsOptIn(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(1))
	if r.DockerHost != "" || r.Commands.DockerProxied != "" || r.Commands.ComposeProxied != "" {
		t.Fatalf("proxy must be off by default: %+v", r)
	}
	if _, err := os.Stat(h.store.Path(ProxyDir)); !os.IsNotExist(err) {
		t.Fatalf("proxy dir created while disabled: %v", err)
	}
}

func TestProxyRemoteLeaseLifecycle(t *testing.T) {
	h := newHarnessWith(t, 40, func(o *Options) { o.SSH.Binary = "false" }, proxyEnabled)
	r := ok[v1.AcquireResult](h, h.acquire(1))
	if r.Placement != v1.PlacementRemote || !strings.HasPrefix(r.DockerHost, "unix://") || !strings.HasSuffix(r.DockerHost, r.LeaseID+".sock") {
		t.Fatalf("docker_host %q placement %s", r.DockerHost, r.Placement)
	}
	// Existing contract fields are unchanged; the proxy is additive.
	if r.Commands.Docker != "docker --context dol-tower" || r.Commands.LabelFlags == "" {
		t.Fatalf("commands changed: %+v", r.Commands)
	}
	if r.Commands.DockerProxied != "docker -H "+r.DockerHost || r.Commands.ComposeProxied != r.Commands.DockerProxied+" compose" {
		t.Fatalf("proxied commands %+v", r.Commands)
	}
	if !strings.Contains(strings.Join(r.NextSteps, " "), r.Commands.DockerProxied) {
		t.Fatalf("next steps do not mention the proxy: %v", r.NextSteps)
	}
	path := strings.TrimPrefix(r.DockerHost, "unix://")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %s: %v %v", path, fi, err)
	}
	// The upstream is `ssh tower docker system dial-stdio` (here a failing
	// ssh): the proxy answers with a Docker-style error, not a hang.
	if code, msg := pingSocket(t, r.DockerHost); code != http.StatusBadGateway || !strings.Contains(msg, "upstream engine") {
		t.Fatalf("ping via proxy: %d %q", code, msg)
	}
	ok[v1.ReleaseResult](h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}))
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket not removed on release: %v", err)
	}
	// Close removes the sockets of leases that are still active.
	r2 := ok[v1.AcquireResult](h, h.acquire(1))
	h.core.Close()
	if _, err := os.Lstat(strings.TrimPrefix(r2.DockerHost, "unix://")); !os.IsNotExist(err) {
		t.Fatalf("socket not removed on exit: %v", err)
	}
}

func TestProxyLocalLease(t *testing.T) {
	h := newHarness(t, 1, proxyEnabled) // node too small → local fallback
	r := ok[v1.AcquireResult](h, h.acquire(4))
	if r.Placement != v1.PlacementLocal || r.DockerHost == "" {
		t.Fatalf("placement %s docker_host %q warnings %v", r.Placement, r.DockerHost, r.Warnings)
	}
	// The fake local context points at a socket that does not exist.
	if code, _ := pingSocket(t, r.DockerHost); code != http.StatusBadGateway {
		t.Fatalf("ping via proxy: %d", code)
	}
}
