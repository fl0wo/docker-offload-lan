package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
)

// TestMain lets the test binary act as a fake `ssh` that implements -L by
// proxying locally and exits when stdin closes, like the real thing with
// `cat >/dev/null` as remote command.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_SSH") == "1" {
		fakeSSH()
		return
	}
	os.Exit(m.Run())
}

func fakeSSH() {
	var spec string
	for i, a := range os.Args {
		if a == "-L" {
			spec = os.Args[i+1]
		}
	}
	if os.Getenv("FAKE_SSH_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "ssh: connect to host x port 2222: Connection refused")
		os.Exit(255)
	}
	// 127.0.0.1:LPORT:RHOST:RPORT
	p := strings.Split(spec, ":")
	l, err := net.Listen("tcp", p[0]+":"+p[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bind:", err)
		os.Exit(255)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				r, err := net.Dial("tcp", p[2]+":"+p[3])
				if err != nil {
					c.Close()
					return
				}
				go io.Copy(r, c)
				io.Copy(c, r)
				c.Close()
				r.Close()
			}()
		}
	}()
	io.Copy(io.Discard, os.Stdin) // exit when parent goes away
}

func newMgr(t *testing.T) *Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := t.TempDir() + "/ssh"
	os.WriteFile(wrapper, []byte("#!/bin/sh\nFAKE_SSH=1 exec "+exe+" \"$@\"\n"), 0o755)
	return &Manager{SSH: sshx.Options{Binary: wrapper}, fws: map[int]*Forward{}}
}

func echoServer(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestForwardRoundTripAndClose(t *testing.T) {
	m := newMgr(t)
	rport := echoServer(t)
	f, warn, err := m.Open(context.Background(), Spec{LeaseID: "l-aaaaaaaaaa", SSHHost: "node", RemotePort: rport})
	if err != nil || warn != "" {
		t.Fatal(err, warn)
	}
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", f.LocalPort))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	io.ReadFull(c, buf)
	c.Close()
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}
	if got := m.List("l-aaaaaaaaaa"); len(got) != 1 {
		t.Fatal(got)
	}
	ports := m.CloseLease("l-aaaaaaaaaa")
	if len(ports) != 1 || !waitFree(f.LocalPort) {
		t.Fatal("forward not closed")
	}
}

func waitFree(p int) bool {
	for range 50 {
		if PortFree(p) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestBusyPortPicksAnother(t *testing.T) {
	m := newMgr(t)
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	bp := busy.Addr().(*net.TCPAddr).Port
	f, warn, err := m.Open(context.Background(), Spec{SSHHost: "n", LocalPort: bp, RemotePort: echoServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	if f.LocalPort == bp || !strings.Contains(warn, "busy") {
		t.Fatalf("port %d warn %q", f.LocalPort, warn)
	}
	if _, _, err := m.Open(context.Background(), Spec{SSHHost: "n", LocalPort: bp, RemotePort: 1, StrictLocalPort: true}); err == nil {
		t.Fatal("strict must fail")
	}
}

func TestSSHFailureIsActionable(t *testing.T) {
	m := newMgr(t)
	t.Setenv("FAKE_SSH_FAIL", "1")
	_, _, err := m.Open(context.Background(), Spec{SSHHost: "n", RemotePort: 80})
	if err == nil || !strings.Contains(err.Error(), "Connection refused") || !strings.Contains(err.Error(), "sshd") {
		t.Fatalf("want actionable error, got %v", err)
	}
}

func TestParentDeathKillsTunnel(t *testing.T) {
	// Simulates dol being SIGKILLed: the stdin pipe closes without Close().
	m := newMgr(t)
	f, _, err := m.Open(context.Background(), Spec{SSHHost: "n", RemotePort: echoServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	f.stdin.Close()
	select {
	case <-f.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("ssh did not exit when its stdin closed")
	}
}
