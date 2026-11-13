package mcpserver

// Regression tests from docs/security-review-2026-11.md. Tool arguments are
// treated as attacker-controlled (prompt injection).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// localComposeHarness returns a harness with a project dir containing a
// compose file, and a local lease.
func localComposeHarness(t *testing.T, project string) (*harness, v1.AcquireResult) {
	t.Helper()
	proj := filepath.Join(t.TempDir(), project)
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "compose.yaml"), []byte("services: {}\n"), 0o644)
	h := newHarnessWith(t, 40, func(o *Options) { o.ProjectDir = proj })
	h.docker.composeConfig = `{"name":"` + project + `","services":{"db":{"image":"postgres"}}}`
	r := structured[v1.AcquireResult](t, h.call("dol_acquire", map[string]any{"workload": "compose", "est_memory_gb": 1, "prefer": "local"}))
	if r.Placement != v1.PlacementLocal {
		t.Fatalf("want local placement, got %+v", r)
	}
	return h, r
}

// SR-01: a compose project name chosen by the agent (project_name, or the
// compose file's `name:`, or simply the directory name) that equals an
// existing project the lease does not own must not be started: `compose up`
// would recreate the foreign containers with this lease's labels, and the
// lease's teardown (`compose down --volumes`) would delete them and their
// volumes.
func TestSecurityComposeUpRefusesForeignProject(t *testing.T) {
	h, acq := localComposeHarness(t, "shop")
	l, _ := h.store.GetLease(acq.LeaseID)
	// The user's own, unrelated `docker compose up` in ~/code/shop.
	h.docker.add(l.DockerContext, "shop-db-1", map[string]string{dockerx.ComposeLabelProject: "shop"})

	r := h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID})
	if !r.IsError {
		t.Fatalf("compose up over a foreign project must be refused, got %s", textOf(r))
	}
	if !strings.Contains(textOf(r), "project_name") {
		t.Fatalf("error must tell the agent to pick another project_name: %s", textOf(r))
	}
	if h.docker.called("--context "+l.DockerContext+" compose -p shop -f") || h.docker.called("--context "+l.DockerContext+" compose -p shop down") {
		t.Fatalf("compose must not run on the foreign project: %v", h.docker.calls)
	}
	if l2, _ := h.store.GetLease(acq.LeaseID); len(l2.Resources.ComposeProjects) != 0 {
		t.Fatalf("foreign project must not be registered on the lease: %v", l2.Resources.ComposeProjects)
	}
}

// SR-01 (teardown side): a project that was ours at `up` time but now also
// contains foreign containers must not be brought down by name.
func TestSecurityComposeDownSkipsForeignResources(t *testing.T) {
	h, acq := localComposeHarness(t, "shop")
	if r := h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID}); r.IsError {
		t.Fatal(textOf(r))
	}
	l, _ := h.store.GetLease(acq.LeaseID)
	h.docker.add(l.DockerContext, "shop-db-1", map[string]string{dockerx.ComposeLabelProject: "shop"})

	for tool, args := range map[string]map[string]any{
		"dol_compose_down": {"lease_id": acq.LeaseID, "project_name": "shop", "volumes": true},
		"dol_stop":         {"lease_id": acq.LeaseID, "target": "shop"},
		"dol_remove":       {"lease_id": acq.LeaseID, "target": "shop", "volumes": true},
	} {
		r := h.call(tool, args)
		if !r.IsError || !strings.Contains(textOf(r), "shop-db-1") {
			t.Fatalf("%s on a project with foreign containers must be refused naming them, got %s", tool, textOf(r))
		}
	}
	h.call("dol_release", map[string]any{"lease_id": acq.LeaseID})
	for _, c := range h.docker.calls {
		if strings.Contains(c, "compose -p shop down") || strings.Contains(c, "compose -p shop stop") {
			t.Fatalf("foreign compose project touched: %q", c)
		}
	}
	if _, ok := h.docker.get(l.DockerContext, "shop-db-1"); !ok {
		t.Fatal("foreign container removed")
	}
}
