package sshx

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArgsGuardDestination(t *testing.T) {
	a := Options{ConnectTimeout: 1500 * time.Millisecond}.Args("-oProxyCommand=x", "dol", "node", "dial-stdio")
	s := strings.Join(a, " ")
	if !strings.Contains(s, "ConnectTimeout=2") || !strings.Contains(s, "BatchMode=yes") {
		t.Fatal(s)
	}
	i := indexOf(a, "--")
	if i < 0 || a[i+1] != "-oProxyCommand=x" {
		t.Fatalf("destination must follow --: %v", a)
	}
}

func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}

// fakeSSH writes a script that ignores ssh options and runs the remote command
// locally, so DialStdio can be tested without sshd.
func fakeSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
while [ "$1" != "--" ]; do shift; done
shift; shift
exec "$@"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDialStdioEcho(t *testing.T) {
	o := Options{Binary: fakeSSH(t)}
	c, err := o.DialStdio(context.Background(), "host", "cat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("%q %v", buf, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDialStdioSurfacesStderr(t *testing.T) {
	o := Options{Binary: fakeSSH(t)}
	c, err := o.DialStdio(context.Background(), "host", "sh", "-c", "echo 'Permission denied (publickey).' >&2; exit 255")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("want stderr in error, got %v", err)
	}
	if Hint(err.Error()) == "" {
		t.Fatal("expected hint")
	}
}

func TestCloseKillsHungProcess(t *testing.T) {
	o := Options{Binary: fakeSSH(t)}
	c, err := o.DialStdio(context.Background(), "host", "sh", "-c", "trap '' TERM; sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.Close()
	if time.Since(start) > 3*time.Second {
		t.Fatal("close took too long")
	}
}

// A backgrounded ControlMaster forked by ssh inherits the stdout pipe and
// lives for ControlPersist. Close must unblock a pending Read immediately,
// and Run must not wait for the forked process (regression: a frozen node
// delayed fallback by ~ControlPersist instead of health_timeout_ms).
func TestForkedMasterHoldingPipesDoesNotBlock(t *testing.T) {
	o := Options{Binary: fakeSSH(t)}
	c, err := o.DialStdio(context.Background(), "host", "sh", "-c", "sleep 30 & sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); readDone <- err }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	_ = c.Close()
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Read still blocked 3s after Close")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close+Read took %s", d)
	}

	start = time.Now()
	out, err := o.Run(context.Background(), "host", "sh", "-c", "sleep 30 & echo hi")
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("%q %v", out, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run waited for the forked process: %s", d)
	}
}
