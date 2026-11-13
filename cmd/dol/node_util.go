package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/fl0wo/docker-offload-lan/internal/node"
	"go.yaml.in/yaml/v3"
)

func yamlEncoder(w io.Writer) *yaml.Encoder {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	return enc
}

// writeFileAtomic writes via a temp file and rename so readers never see a
// partial config.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // /etc/docker-offload-lan must be readable
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// findKind returns the kind binary the agent will use: the configured path,
// else kind on PATH.
func findKind(configured string) (string, error) {
	if configured != "" {
		if fi, err := os.Stat(configured); err == nil && !fi.IsDir() {
			return configured, nil
		}
	}
	p, err := exec.LookPath("kind")
	if err != nil {
		return "", fmt.Errorf("not at %s and not on PATH", configured)
	}
	return p, nil
}

// chownToDolGroup gives a directory to group dol (setgid keeps new files in
// the group) so SSH users in that group can rsync into it.
func chownToDolGroup(path string) error {
	g, err := user.LookupGroup(node.SocketGroup)
	if err != nil {
		return fmt.Errorf("group %q not found: %s stays owned by its creator; create the group (deploy/linux/install.sh does) and re-run", node.SocketGroup, path)
	}
	gid, _ := strconv.Atoi(g.Gid)
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("chown %s to group %s: %w", path, node.SocketGroup, err)
	}
	return os.Chmod(path, syncRootMode)
}

// syncRootMode lets group dol rsync into sync_root (setgid keeps new dirs in
// the group) and keeps synced workspaces, which may contain secrets, away
// from other local users.
const syncRootMode = os.ModeSetgid | 0o770
