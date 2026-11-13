// Package tunnel manages `ssh -L` port forwards as child processes
// (brief §7.8).
//
// Each forward is its own ssh connection (`-S none`, never multiplexed
// through a ControlMaster, whose forwards would outlive us). The remote side
// runs `cat >/dev/null` fed from a pipe we hold: if dol dies for any reason,
// even SIGKILL, the pipe closes, the remote cat exits and ssh exits. No
// orphaned tunnels.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Spec describes a forward to open.
type Spec struct {
	LeaseID    string
	SSHHost    string
	LocalPort  int    // 0 = pick a free port
	RemoteHost string // default 127.0.0.1
	RemotePort int
	Purpose    string
	// StrictLocalPort fails instead of picking another port when LocalPort is
	// taken (kind API tunnels need the exact port written in the kubeconfig).
	StrictLocalPort bool
}

// Forward is an open tunnel.
type Forward struct {
	v1.Forward
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	done  chan struct{}
	err   error
}

// Done is closed when the ssh process exits.
func (f *Forward) Done() <-chan struct{} { return f.done }

// Manager tracks forwards of one dol process.
type Manager struct {
	SSH sshx.Options
	// ReadyTimeout bounds how long Open waits for the forward to accept
	// connections. Default 10s.
	ReadyTimeout time.Duration

	mu  sync.Mutex
	fws map[int]*Forward // by local port
}

// NewManager returns a Manager.
func NewManager(opts sshx.Options) *Manager {
	return &Manager{SSH: opts, fws: map[int]*Forward{}}
}

// PortFree reports whether 127.0.0.1:port can be bound right now.
func PortFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// FreePort asks the kernel for a free port on 127.0.0.1.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Open starts a forward and waits until it accepts connections. It returns
// the forward with the actual local port, and a warning when the requested
// port was taken and another one was chosen.
func (m *Manager) Open(ctx context.Context, s Spec) (*Forward, string, error) {
	if s.RemotePort <= 0 || s.RemotePort > 65535 {
		return nil, "", v1.Errorf(v1.CodeBadRequest, "pass the port the service listens on (on the node)", "invalid remote_port %d", s.RemotePort)
	}
	if s.RemoteHost == "" {
		s.RemoteHost = "127.0.0.1"
	}
	if strings.ContainsAny(s.RemoteHost, " :/[]\t\n") && net.ParseIP(s.RemoteHost) == nil {
		return nil, "", v1.Errorf(v1.CodeBadRequest, "use an IP or simple hostname as seen from the node", "invalid remote_host %q", s.RemoteHost)
	}
	warning := ""
	m.mu.Lock()
	_, ours := m.fws[s.LocalPort]
	m.mu.Unlock()
	if s.LocalPort != 0 && (ours || !PortFree(s.LocalPort)) {
		if s.StrictLocalPort {
			return nil, "", v1.Errorf(v1.CodeConflict, "free the port or release the lease that uses it",
				"local port %d is already in use", s.LocalPort)
		}
		warning = fmt.Sprintf("local port %d was busy; using another port", s.LocalPort)
		s.LocalPort = 0
	}
	if s.LocalPort == 0 {
		p, err := FreePort()
		if err != nil {
			return nil, "", err
		}
		s.LocalPort = p
		if warning != "" {
			warning = strings.Replace(warning, "another port", "port "+strconv.Itoa(p), 1)
		}
	}
	spec := fmt.Sprintf("127.0.0.1:%d:%s:%d", s.LocalPort, s.RemoteHost, s.RemotePort)
	opts := m.SSH
	opts.ExtraArgs = append(append([]string{}, opts.ExtraArgs...),
		"-S", "none", "-o", "ExitOnForwardFailure=yes", "-L", spec)
	cmd := exec.Command(binOr(opts.Binary), opts.Args(s.SSHHost, "cat >/dev/null")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stderr := &limitedWriter{n: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting ssh: %w", err)
	}
	f := &Forward{
		Forward: v1.Forward{LeaseID: s.LeaseID, LocalPort: s.LocalPort, RemoteHost: s.RemoteHost,
			RemotePort: s.RemotePort, Purpose: s.Purpose, PID: cmd.Process.Pid},
		cmd: cmd, stdin: stdin, done: make(chan struct{}),
	}
	go func() { f.err = cmd.Wait(); close(f.done) }()

	if err := m.waitReady(ctx, f); err != nil {
		f.kill()
		se := strings.TrimSpace(stderr.String())
		msg := fmt.Sprintf("forward 127.0.0.1:%d → %s:%d via %s failed: %v", s.LocalPort, s.RemoteHost, s.RemotePort, s.SSHHost, err)
		if se != "" {
			msg += ": " + se
		}
		hint := sshx.Hint(se)
		if hint == "" {
			hint = "check the node is reachable (`dol doctor`); the ssh forward itself failed to start"
		}
		return nil, "", v1.Errorf(v1.CodeUnreachable, hint, "%s", msg)
	}
	m.mu.Lock()
	m.fws[s.LocalPort] = f
	m.mu.Unlock()
	go func() { // forget forwards whose ssh died
		<-f.done
		m.mu.Lock()
		if m.fws[f.LocalPort] == f {
			delete(m.fws, f.LocalPort)
		}
		m.mu.Unlock()
	}()
	return f, warning, nil
}

func binOr(b string) string {
	if b == "" {
		return "ssh"
	}
	return b
}

// waitReady polls until the local port accepts a TCP connection or ssh exits.
// ssh binds the local listener only after authentication and after
// ExitOnForwardFailure checks, so an accepted connection means the tunnel is
// up (the remote end may still refuse if nothing listens there).
func (m *Manager) waitReady(ctx context.Context, f *Forward) error {
	to := m.ReadyTimeout
	if to == 0 {
		to = 10 * time.Second
	}
	deadline := time.NewTimer(to)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.LocalPort))
	for {
		select {
		case <-f.done:
			if f.err != nil {
				return f.err
			}
			return errors.New("ssh exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("not ready after %s", to)
		case <-tick.C:
			c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				// Make sure the listener belongs to our ssh (not a race with
				// another process): ssh must still be alive.
				select {
				case <-f.done:
					return errors.New("ssh exited")
				default:
					return nil
				}
			}
		}
	}
}

func (f *Forward) kill() {
	_ = f.stdin.Close() // remote cat sees EOF; ssh exits
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		_ = f.cmd.Process.Kill()
		<-f.done
	}
}

// Close closes the forward on localPort. It reports whether one existed.
func (m *Manager) Close(localPort int) bool {
	m.mu.Lock()
	f := m.fws[localPort]
	delete(m.fws, localPort)
	m.mu.Unlock()
	if f == nil {
		return false
	}
	f.kill()
	return true
}

// CloseLease closes every forward of a lease and returns their local ports.
func (m *Manager) CloseLease(leaseID string) []int {
	var ports []int
	for _, f := range m.List(leaseID) {
		if m.Close(f.LocalPort) {
			ports = append(ports, f.LocalPort)
		}
	}
	return ports
}

// CloseAll closes every forward (on dol exit).
func (m *Manager) CloseAll() {
	for _, f := range m.List("") {
		m.Close(f.LocalPort)
	}
}

// List returns open forwards, optionally filtered by lease, sorted by port.
func (m *Manager) List(leaseID string) []v1.Forward {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []v1.Forward
	for _, f := range m.fws {
		if leaseID == "" || f.LeaseID == leaseID {
			out = append(out, f.Forward)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LocalPort < out[j].LocalPort })
	return out
}

type limitedWriter struct {
	mu sync.Mutex
	w  strings.Builder
	n  int
}

func (l *limitedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rem := l.n - l.w.Len(); rem > 0 {
		if len(p) > rem {
			l.w.Write(p[:rem])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}
