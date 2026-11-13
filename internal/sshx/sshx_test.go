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
