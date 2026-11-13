package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy"
	dolsync "github.com/fl0wo/docker-offload-lan/internal/sync"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// ProxyDir is the state subdirectory holding per-lease proxy sockets.
const ProxyDir = "proxy"

// proxySocketPath is <state>/proxy/<lease>.sock, or /tmp/dol-<uid>/<lease>.sock
// when the state dir is too deep for a unix socket path.
func (c *Core) proxySocketPath(id string) string {
	if p := c.store.Path(ProxyDir, id+".sock"); len(p) <= dockerproxy.MaxSocketPath {
		return p
	}
	return filepath.Join("/tmp", "dol-"+strconv.Itoa(os.Getuid()), id+".sock")
}

// startProxy serves the Docker API proxy for a lease (proxy.enabled,
// ADR-0040) and returns its DOCKER_HOST value.
func (c *Core) startProxy(ctx context.Context, l v1.ClientLease) (string, error) {
	var (
		dial dockerproxy.DialFunc
		maps []dockerproxy.PathMap
	)
	if l.Placement == v1.PlacementRemote {
		n, err := c.node(l.Node)
		if err != nil {
			return "", err
		}
		opts := c.ssh
		// dial-stdio connections are opened lazily per API connection; give
		// a cold (non-multiplexed) ssh more than the health-check budget.
		opts.ConnectTimeout = max(opts.ConnectTimeout, 10*time.Second)
		dial = dockerproxy.SSHDialer(opts, n.ref.SSHHost)
		maps = []dockerproxy.PathMap{{
			Local:  c.projectDir,
			Remote: dolsync.RemoteDir(c.cfg.Sync.RemoteRoot, l.ID, filepath.Base(c.projectDir)),
		}}
	} else {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ci, err := c.docker("").FindContext(dctx, l.DockerContext)
		cancel()
		if err != nil {
			return "", err
		}
		if ci == nil {
			return "", fmt.Errorf("docker context %q not found", l.DockerContext)
		}
		if dial, err = dockerproxy.EndpointDialer(c.ssh, ci.DockerEndpoint); err != nil {
			return "", err
		}
	}
	p, err := dockerproxy.New(dockerproxy.Options{
		Dial: dial, Labels: l.Labels, PathMaps: maps, AllowExternalBinds: c.cfg.Proxy.AllowExternalBinds, Logger: c.log,
		OnPorts: func(ps []dockerproxy.PortBinding) {
			c.log.Info("published ports via docker proxy", "lease", l.ID, "ports", ps)
		},
	})
	if err != nil {
		return "", err
	}
	c.stopProxy(l.ID)
	srv, err := dockerproxy.Listen(c.proxySocketPath(l.ID), p)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.proxies[l.ID] = srv
	c.mu.Unlock()
	c.log.Info("docker API proxy started", "lease", l.ID, "socket", srv.Path)
	return srv.DockerHost(), nil
}

// proxyHost returns the DOCKER_HOST of a running lease proxy, or "".
func (c *Core) proxyHost(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.proxies[id]; s != nil {
		return s.DockerHost()
	}
	return ""
}

func (c *Core) stopProxy(id string) {
	c.mu.Lock()
	s := c.proxies[id]
	delete(c.proxies, id)
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

func (c *Core) stopAllProxies() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.proxies))
	for id := range c.proxies {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.stopProxy(id)
	}
}
