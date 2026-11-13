// Package sshx spawns the system ssh binary (ADR-0002): one-shot remote
// commands and stdio "connections" used to reach the node API through
// `dol node dial-stdio`.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options control how ssh is invoked.
type Options struct {
	// Binary is the ssh executable; default "ssh".
	Binary string
	// ConnectTimeout bounds TCP connect + key exchange (ssh -o ConnectTimeout).
	// Rounded up to whole seconds, minimum 1s. 0 = 5s.
	ConnectTimeout time.Duration
	// ExtraArgs are inserted before the destination (e.g. "-F", cfg in tests).
	ExtraArgs []string
}

func (o Options) bin() string {
	if o.Binary == "" {
		return "ssh"
	}
	return o.Binary
}

// Args returns the full argv (without the binary) for running remoteCmd on
// host. The destination is preceded by "--" so a hostile host string cannot
// be parsed as an option.
func (o Options) Args(host string, remoteCmd ...string) []string {
	ct := o.ConnectTimeout
	if ct <= 0 {
		ct = 5 * time.Second
	}
	secs := int((ct + time.Second - 1) / time.Second)
	args := []string{
		"-o", "BatchMode=yes", // never prompt: agents cannot type passwords
		"-o", "ConnectTimeout=" + strconv.Itoa(secs),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-T", // no tty: we carry binary-safe streams
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, "--", host)
	return append(args, remoteCmd...)
}

// Command builds an *exec.Cmd running remoteCmd on host.
func (o Options) Command(ctx context.Context, host string, remoteCmd ...string) *exec.Cmd {
	return exec.CommandContext(ctx, o.bin(), o.Args(host, remoteCmd...)...)
}

// Run executes remoteCmd on host and returns stdout. Errors include ssh's
// stderr and are classified with Classify.
func (o Options) Run(ctx context.Context, host string, remoteCmd ...string) ([]byte, error) {
	cmd := o.Command(ctx, host, remoteCmd...)
	// A ControlMaster forked by this ssh (ControlPersist) may inherit the
	// output pipes; don't wait for it to exit.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil // ssh exited 0; only a forked ControlMaster still holds the pipes
	}
	if err != nil {
		return stdout.Bytes(), &Error{Host: host, Err: err, Stderr: strings.TrimSpace(stderr.String()), Ctx: ctx.Err()}
	}
	return stdout.Bytes(), nil
}

// Error is an ssh failure with its stderr.
type Error struct {
	Host   string
	Err    error
	Stderr string
	Ctx    error // context error at failure time, if any
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("ssh %s: %v", e.Host, e.Err)
	if e.Ctx != nil {
		msg = fmt.Sprintf("ssh %s: %v", e.Host, e.Ctx)
	}
	if e.Stderr != "" {
		msg += ": " + lastLines(e.Stderr, 3)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns the process exit code or -1.
func (e *Error) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Hint returns an actionable suggestion for common ssh failures.
func Hint(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "permission denied"):
		return "ssh key not accepted: check `ssh <host>` works without a password and that your public key is in ~dol/.ssh/authorized_keys on the node"
	case strings.Contains(s, "could not resolve hostname"):
		return "unknown host: check the Host alias in ~/.ssh/config (run `dol init` to add it)"
	case strings.Contains(s, "connection refused"):
		return "nothing listens on the ssh port: is sshd running inside WSL on port 2222 (`sudo systemctl status ssh`)?"
	case strings.Contains(s, "timed out") || strings.Contains(s, "no route to host") || strings.Contains(s, "host is down"):
		return "node unreachable: is it powered on and on the same LAN? Is the Hyper-V firewall rule for port 2222 present?"
	case strings.Contains(s, "host key verification failed"):
		return "unknown host key: run `ssh <host>` once interactively to accept it"
	case strings.Contains(s, "command not found") || strings.Contains(s, "no such file"):
		return "dol is not installed on the node or not on PATH for non-interactive ssh: re-run deploy/linux/install.sh"
	case strings.Contains(s, "connect to node agent") || strings.Contains(s, "node.sock"):
		return "the node agent is not running: `sudo systemctl status dol-node` on the node"
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// StdioConn is a net.Conn backed by an ssh process's stdin/stdout.
type StdioConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *capBuffer
	host   string

	closeOnce sync.Once
	closed    atomic.Bool
	waitOnce  sync.Once
	waitErr   error
	done      chan struct{}
}

// DialStdio starts `ssh host remoteCmd...` and returns its stdio as a
// net.Conn. ctx only bounds process start; the connection lives until Close.
func (o Options) DialStdio(ctx context.Context, host string, remoteCmd ...string) (*StdioConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(o.bin(), o.Args(host, remoteCmd...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &StdioConn{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &capBuffer{max: 8 << 10}, host: host, done: make(chan struct{})}
	cmd.Stderr = c.stderr
	// Don't let a forked ControlMaster holding our pipes block Wait.
	cmd.WaitDelay = time.Second
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, &Error{Host: host, Err: err}
	}
	go func() { _ = c.wait(); close(c.done) }()
	return c, nil
}

func (c *StdioConn) wait() error {
	c.waitOnce.Do(func() { c.waitErr = c.cmd.Wait() })
	return c.waitErr
}

func (c *StdioConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err != nil && n == 0 && !c.closed.Load() {
		// Surface ssh's stderr when the remote side ended without data.
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
		if se := strings.TrimSpace(c.stderr.String()); se != "" {
			return 0, &Error{Host: c.host, Err: err, Stderr: se}
		}
	}
	return n, err
}

func (c *StdioConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close terminates the ssh process.
//
// It never blocks: a backgrounded ControlMaster forked by this ssh can
// inherit the stdout pipe and keep it open for ControlPersist (minutes), so
// we close our read end ourselves instead of waiting for EOF, and reap the
// process asynchronously.
func (c *StdioConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		go func() {
			select {
			case <-c.done:
			case <-time.After(500 * time.Millisecond):
				killProcess(c.cmd)
			}
		}()
	})
	return nil
}

// Stderr returns captured ssh stderr so far.
func (c *StdioConn) Stderr() string { return c.stderr.String() }

func (c *StdioConn) LocalAddr() net.Addr                { return stdioAddr{} }
func (c *StdioConn) RemoteAddr() net.Addr               { return stdioAddr{c.host} }
func (c *StdioConn) SetDeadline(t time.Time) error      { return nil }
func (c *StdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *StdioConn) SetWriteDeadline(t time.Time) error { return nil }

type stdioAddr struct{ host string }

func (stdioAddr) Network() string  { return "ssh-stdio" }
func (a stdioAddr) String() string { return a.host }

// capBuffer keeps the last max bytes written.
type capBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
