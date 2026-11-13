// Package fakessh provides a stand-in for the ssh binary in tests: the test
// binary re-executes itself as "ssh". It runs the remote command locally
// (sh -c) and implements -L forwards by proxying locally, so tunnels, rsync
// and dial-stdio can be exercised end to end without an sshd.
//
// Usage: call fakessh.Main() first thing in TestMain, then Binary(t) gives a
// path usable as sshx.Options.Binary.
package fakessh

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const envVar = "DOL_FAKE_SSH"

// Main turns the process into a fake ssh when invoked through Binary. It
// returns normally otherwise.
func Main() {
	if os.Getenv(envVar) != "1" {
		return
	}
	os.Exit(run(os.Args[1:]))
}

// Binary writes a wrapper script that execs the current test binary as ssh.
func Binary(t testing.TB) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\n" + envVar + "=1 exec '" + exe + "' \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(args []string) int {
	if os.Getenv("DOL_FAKE_SSH_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "ssh: connect to host node port 2222: Connection refused")
		return 255
	}
	var forward string
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		switch a {
		case "-o", "-F", "-S", "-p", "-i", "-l", "-E":
			i++
		case "-L":
			i++
			forward = args[i]
		default:
			if !strings.HasPrefix(a, "-") {
				goto host
			}
		}
	}
host:
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "fakessh: no host")
		return 255
	}
	cmd := strings.Join(args[i+1:], " ")
	if forward != "" {
		p := strings.Split(forward, ":") // 127.0.0.1:L:host:R
		l, err := net.Listen("tcp", p[0]+":"+p[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bind [127.0.0.1]:"+p[1]+": Address already in use")
			return 255
		}
		go proxy(l, net.JoinHostPort(p[2], p[3]))
	}
	if cmd == "" {
		select {}
	}
	c := exec.Command("sh", "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 255
	}
	return 0
}

func proxy(l net.Listener, target string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			r, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				return
			}
			go func() { _, _ = io.Copy(r, c); _ = r.Close() }()
			_, _ = io.Copy(c, r)
			_ = c.Close()
		}()
	}
}
