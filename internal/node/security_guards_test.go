package node

// Guards confirmed during docs/security-review-2026-11.md. They passed
// before any fix and are kept as regression tests.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func TestSecurityNodeAPIGuards(t *testing.T) {
	n := newTestNode(t)
	a := n.lease(t, 1)
	b := n.lease(t, 1)

	// Path parameters with encoded slashes or traversal never reach a lease.
	for _, p := range []string{
		"/v1/leases/" + a.ID + "%2F..%2F" + b.ID + "/heartbeat",
		"/v1/leases/..%2F" + b.ID + "/heartbeat",
		"/v1/leases/" + a.ID + "%00/heartbeat",
	} {
		if st, body := n.rawRequest(t, "POST", p, ""); st != 404 && st != 400 {
			t.Errorf("POST %s: %d %s", p, st, body)
		}
	}
	// Compose project names that look like flags or paths are refused.
	for _, p := range []string{"-p", "--volumes", "..", "a%2Fb", "A", "a%20b", "_x"} {
		if st, _ := n.rawRequest(t, "POST", "/v1/leases/"+a.ID+"/compose/"+p, ""); st != 400 && st != 404 {
			t.Errorf("compose project %q: status %d", p, st)
		}
	}
	// Lease A cannot register a sync dir of lease B, or anything outside its tree.
	for _, p := range []string{
		filepath.Join(n.cfg.SyncRoot, b.ID, "proj"),
		n.cfg.SyncRoot + "/" + a.ID + "/../" + b.ID,
		n.cfg.SyncRoot, "/", "relative",
	} {
		body, _ := json.Marshal(v1.SyncRegisterRequest{Path: p})
		if st, _ := n.rawRequest(t, "POST", "/v1/leases/"+a.ID+"/sync", string(body)); st != 400 {
			t.Errorf("sync path %q: status %d", p, st)
		}
	}
	// Oversized bodies are refused before parsing.
	big := `{"owner":"` + strings.Repeat("x", maxBody) + `"}`
	if st, _ := n.rawRequest(t, "POST", "/v1/leases", big); st != 400 {
		t.Errorf("oversized body: status %d", st)
	}
	// Kind names are scoped to the lease: B's cluster is unreachable via A.
	if _, err := n.client.KindCreate(context.Background(), b.ID, v1.KindCreateRequest{Name: "e2e"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"e2e", v1.KindClusterName(b.ID, "e2e")} {
		if st, _ := n.rawRequest(t, "DELETE", "/v1/leases/"+a.ID+"/kind/"+name, ""); st != 404 {
			t.Errorf("kind %q via lease A: status %d", name, st)
		}
	}
	if !n.kind.has(v1.KindClusterName(b.ID, "e2e")) {
		t.Fatal("lease B's cluster was deleted through lease A")
	}
}

// Symlinks inside a synced tree (rsync -a copies them from the repo) are
// removed as links; their targets survive teardown.
func TestSecuritySyncTeardownDoesNotFollowInnerSymlinks(t *testing.T) {
	n := newTestNode(t)
	l := n.lease(t, 1)
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(n.cfg.SyncRoot, l.ID, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := n.client.RegisterSyncDir(context.Background(), l.ID, proj); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(proj, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(proj, "sub", "file-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.client.ReleaseLease(context.Background(), l.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(proj); !os.IsNotExist(err) {
		t.Fatal("sync dir not removed")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("symlink target outside the sync dir was deleted")
	}
}
