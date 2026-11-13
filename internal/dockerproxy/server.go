package dockerproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
)

// MaxSocketPath is the longest unix socket path accepted by every supported
// OS (macOS sun_path is 104 bytes including the terminating NUL).
const MaxSocketPath = 103

// Server serves a Proxy on a unix socket.
type Server struct {
	Path  string
	Proxy *Proxy

	ln   net.Listener
	srv  *http.Server
	once sync.Once
	done chan struct{}
}

// Listen creates the socket at path (parent dir 0700, socket 0600; a stale
// socket left by a crashed process is replaced, any other file is an error)
// and serves p on it until Close.
func Listen(path string, p *Proxy) (*Server, error) {
	if len(path) > MaxSocketPath {
		return nil, fmt.Errorf("socket path %s is too long (%d > %d bytes)", path, len(path), MaxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The directory is 0700, so there is no window in which another user
	// can connect before the chmod.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &Server{
		Path: path, Proxy: p, ln: ln, done: make(chan struct{}),
		srv: &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second},
	}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// checkPrivateDir refuses directories other users can write to or enter.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs the x bit; 0700 is owner-only
			return fmt.Errorf("%s is accessible by other users (%v) and cannot be fixed: %w", dir, fi.Mode().Perm(), err)
		}
	}
	return nil
}

// Close stops serving, closes hijacked streams and removes the socket.
func (s *Server) Close() error {
	s.once.Do(func() {
		_ = s.srv.Close() // closes the listener and non-hijacked connections
		s.Proxy.Close()
		<-s.done
		_ = os.Remove(s.Path)
	})
	return nil
}

// DockerHost returns the DOCKER_HOST / -H value for the socket.
func (s *Server) DockerHost() string { return "unix://" + s.Path }

// UnixDialer dials a local engine socket.
func UnixDialer(path string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

// SSHDialer reaches the engine on host through `ssh host docker system
// dial-stdio`, like the docker CLI does for ssh:// contexts (ADR-0002). Each
// upstream connection is one ssh process; ssh multiplexing (ControlMaster)
// in the user's config makes this cheap.
func SSHDialer(o sshx.Options, host string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		return o.DialStdio(ctx, host, "docker", "system", "dial-stdio")
	}
}

// EndpointDialer returns a dialer for a docker context endpoint
// (unix:///path or ssh://[user@]host[:port]).
func EndpointDialer(o sshx.Options, endpoint string) (DialFunc, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("docker endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "unix":
		if u.Path == "" {
			return nil, fmt.Errorf("docker endpoint %q has no socket path", endpoint)
		}
		return UnixDialer(u.Path), nil
	case "ssh":
		host := u.Hostname()
		if host == "" || strings.HasPrefix(host, "-") {
			return nil, fmt.Errorf("docker endpoint %q has no valid host", endpoint)
		}
		if u.User != nil {
			host = u.User.Username() + "@" + host
		}
		if p := u.Port(); p != "" {
			o.ExtraArgs = append(append([]string(nil), o.ExtraArgs...), "-p", p)
		}
		return SSHDialer(o, host), nil
	}
	return nil, errors.New("docker endpoint " + endpoint + " is not supported by the proxy (only unix:// and ssh://)")
}
