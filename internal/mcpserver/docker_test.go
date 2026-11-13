package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/state"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// TestRealDockerLocalLease runs against the real local Docker. It only
// touches resources it creates (names prefixed dol-m3test-) and is skipped
// unless DOL_DOCKER_TEST=1.
func TestRealDockerLocalLease(t *testing.T) {
	if os.Getenv("DOL_DOCKER_TEST") != "1" {
		t.Skip("set DOL_DOCKER_TEST=1 to run against the local Docker daemon")
	}
	image := orDefault(os.Getenv("DOL_DOCKER_TEST_IMAGE"), "busybox:latest")
	ctx := context.Background()
	cfg := config.DefaultClient()
	cfg.Identity.Owner = "m3test@local"
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	core, err := NewCore(ctx, Options{Config: cfg, Store: st, ProjectDir: "/work/m3test"}) // real docker CLI, auto context
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	if strings.HasPrefix(core.LocalContext(), "dol-") {
		t.Fatalf("auto-detected a dol context: %s", core.LocalContext())
	}
	t.Logf("local context: %s", core.LocalContext())

	sT, cT := mcp.NewInMemoryTransports()
	ss, err := NewServer(core).Connect(ctx, sT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, cT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	h := &harness{t: t, cs: cs, core: core, store: st}

	r := ok[v1.AcquireResult](h, h.acquire(0.25))
	if r.Placement != v1.PlacementLocal {
		t.Fatalf("%+v", r)
	}
	suffix := strings.TrimPrefix(r.LeaseID, "l-")
	labeled, unlabeled := "dol-m3test-labeled-"+suffix, "dol-m3test-unlabeled-"+suffix
	netName, volName := "dol-m3test-net-"+suffix, "dol-m3test-vol-"+suffix
	sh := func(cmd string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { // only our own names
		_ = exec.Command("sh", "-c", r.Commands.Docker+" rm -f "+labeled+" "+unlabeled+" >/dev/null 2>&1; "+
			r.Commands.Docker+" network rm "+netName+" >/dev/null 2>&1; "+r.Commands.Docker+" volume rm "+volName+" >/dev/null 2>&1").Run()
	})
	// Exactly what an agent would paste: prefix + label_flags.
	sh(r.Commands.Docker + " network create " + r.Commands.LabelFlags + " " + netName)
	sh(r.Commands.Docker + " volume create " + r.Commands.LabelFlags + " " + volName)
	sh(r.Commands.Docker + " run -d --init --name " + labeled + " " + r.Commands.LabelFlags + " --network " + netName + " -v " + volName + ":/data " + image + " sleep 300")
	sh(r.Commands.Docker + " run -d --name " + unlabeled + " " + image + " sleep 300")

	lr := ok[v1.ListResult](h, h.call("dol_list", map[string]any{"lease_id": r.LeaseID}))
	kinds := map[string]int{}
	for _, x := range lr.Resources {
		if x.Name == unlabeled {
			t.Fatal("unlabeled container listed")
		}
		kinds[x.Kind]++
	}
	if kinds["container"] != 1 || kinds["network"] != 1 || kinds["volume"] != 1 {
		t.Fatalf("list: %+v", lr.Resources)
	}
	toolErr(h, h.call("dol_stop", map[string]any{"lease_id": r.LeaseID, "target": unlabeled}), v1.CodeNotOwned)
	toolErr(h, h.call("dol_remove", map[string]any{"lease_id": r.LeaseID, "target": unlabeled}), v1.CodeNotOwned)

	start := time.Now()
	rel := ok[v1.ReleaseResult](h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}))
	t.Logf("release took %s: %+v", time.Since(start).Round(time.Millisecond), rel.Removed)
	if len(rel.Removed.Containers) != 1 || len(rel.Removed.Networks) != 1 || len(rel.Removed.Volumes) != 1 || len(rel.Removed.Errors) != 0 {
		t.Fatalf("removed %+v", rel.Removed)
	}
	if out := sh(r.Commands.Docker + " ps -a --filter name=^" + labeled + "$ --format '{{.Names}}'"); out != "" {
		t.Fatalf("labeled container still exists: %s", out)
	}
	if out := sh(r.Commands.Docker + " ps --filter name=^" + unlabeled + "$ --format '{{.State}}'"); out != "running" {
		t.Fatalf("unlabeled container must be untouched and running, got %q", out)
	}
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
