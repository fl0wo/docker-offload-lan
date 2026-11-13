// Package sync mirrors a local project directory to the node with rsync over
// ssh (brief §7.10).
package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Candidate rsync binaries, best first. Homebrew's GNU rsync is preferred
// over macOS's openrsync (docs/compat.md); both support the flags we use.
var Candidates = []string{"/opt/homebrew/bin/rsync", "/usr/local/bin/rsync", "/usr/bin/rsync"}

// FindRsync returns the best rsync binary and its version line.
func FindRsync() (string, string, error) {
	for _, c := range Candidates {
		if _, err := os.Stat(c); err == nil {
			out, _ := exec.Command(c, "--version").CombinedOutput()
			first, _, _ := strings.Cut(string(out), "\n")
			return c, strings.TrimSpace(first), nil
		}
	}
	if p, err := exec.LookPath("rsync"); err == nil {
		return p, "", nil
	}
	return "", "", fmt.Errorf("rsync not found: install it (`brew install rsync`)")
}

var projRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// SanitizeProject turns a project name into a safe single path component.
func SanitizeProject(p string) string {
	if projRe.MatchString(p) && p != "." && p != ".." {
		return p
	}
	var b strings.Builder
	for _, r := range p {
		if r < 128 && (r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), ".-")
	if s == "" {
		s = "project"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// RemoteDir is <remoteRoot>/<lease>/<project>.
func RemoteDir(remoteRoot, leaseID, project string) string {
	return path.Join(remoteRoot, leaseID, SanitizeProject(project))
}

// Options for one sync.
type Options struct {
	SSH              sshx.Options
	SSHHost          string
	LocalDir         string
	RemoteDir        string
	RespectGitignore bool
	IncludeGit       bool
	Rsync            string // binary; default FindRsync
}

// Args returns the rsync argv (without the binary).
func Args(o Options) []string {
	// -e takes a single shell-parsed string: quote each ssh arg.
	sshArgs := o.SSH.Args(o.SSHHost)
	// Args ends with "--", host; rsync appends host + remote command itself,
	// so drop the trailing "-- host".
	sshArgs = sshArgs[:len(sshArgs)-2]
	e := []string{shq(binOr(o.SSH.Binary))}
	for _, a := range sshArgs {
		e = append(e, shq(a))
	}
	args := []string{"-a", "-z", "--delete", "-e", strings.Join(e, " ")}
	if !o.IncludeGit {
		args = append(args, "--exclude=.git")
	}
	if o.RespectGitignore {
		args = append(args, "--filter=:- .gitignore")
	}
	src := strings.TrimSuffix(o.LocalDir, "/") + "/"
	dst := o.SSHHost + ":" + strings.TrimSuffix(o.RemoteDir, "/") + "/"
	return append(args, "--", src, dst)
}

func binOr(b string) string {
	if b == "" {
		return "ssh"
	}
	return b
}

func shq(s string) string { return v1.ShellQuote(s) }

// Run creates the remote dir and syncs. It never passes --checksum
// (openrsync protocol-29 bug, docs/compat.md).
func Run(ctx context.Context, o Options) error {
	if !filepath.IsAbs(o.LocalDir) {
		return fmt.Errorf("local dir %q must be absolute", o.LocalDir)
	}
	st, err := os.Stat(o.LocalDir)
	if err != nil || !st.IsDir() {
		return v1.Errorf(v1.CodeBadRequest, "pass an existing project directory", "cannot sync %s: not a directory", o.LocalDir)
	}
	if o.Rsync == "" {
		bin, _, err := FindRsync()
		if err != nil {
			return err
		}
		o.Rsync = bin
	}
	if _, err := o.SSH.Run(ctx, o.SSHHost, "mkdir", "-p", "--", shq(o.RemoteDir)); err != nil {
		return v1.Errorf(v1.CodeUnreachable, "check that the node's sync root exists and is owned by the dol user (`dol doctor`)",
			"creating %s on the node failed: %v", o.RemoteDir, err)
	}
	cmd := exec.CommandContext(ctx, o.Rsync, Args(o)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return v1.Errorf(v1.CodeInternal, "is rsync installed on the node? (`dol doctor`)", "rsync to %s:%s failed: %v: %s",
			o.SSHHost, o.RemoteDir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
