package mcpserver

// Guards confirmed during docs/security-review-2026-11.md. They passed
// before any fix and are kept as regression tests.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/config"
)

// The fake ssh runs the "remote" command with sh -c, exactly like sshd does,
// so a shell metacharacter that reached the remote command line (mkdir, or
// rsync's remote path) would execute here.
func TestSecuritySyncDirNamesCannotInjectRemoteCommands(t *testing.T) {
	remoteRoot := t.TempDir()
	canary := filepath.Join(t.TempDir(), "PWNED")
	base := t.TempDir()
	h := newHarnessWith(t, 40, func(o *Options) {
		withFakeSSH(t)(o)
		o.ProjectDir = base
	}, func(c *config.ClientConfig) { c.Sync.RemoteRoot = remoteRoot })
	acq := acquireRemote(t, h, "container")

	for _, name := range []string{
		"a;touch " + canary,
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		"x' ; touch '" + canary,
		"-e touch " + canary,
		"--rsync-path=touch " + strings.ReplaceAll(canary, "/", "_"),
		"line\ntouch " + canary,
		"..",
	} {
		dir := filepath.Join(base, name)
		if name != ".." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
		}
		r := h.call("dol_sync", map[string]any{"lease_id": acq.LeaseID, "path": dir})
		if r.IsError {
			t.Logf("%q refused: %s", name, textOf(r))
			continue // refusing is fine; executing is not
		}
		sr := structured[SyncResult](t, r)
		t.Logf("%q -> %s", name, sr.RemotePath)
		rel, err := filepath.Rel(filepath.Join(remoteRoot, acq.LeaseID), sr.RemotePath)
		if err != nil || strings.Contains(rel, "/") || strings.HasPrefix(rel, ".") || strings.ContainsAny(rel, " ;$`'\n") {
			t.Errorf("%q synced to unsafe remote path %q", name, sr.RemotePath)
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("a directory name executed a command on the node")
	}
}
