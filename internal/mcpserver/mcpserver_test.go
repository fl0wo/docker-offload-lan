package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/nodefake"
	"github.com/fl0wo/docker-offload-lan/internal/state"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// harness runs the real MCP server against a fake node and a fake docker
// CLI, and talks to it through the SDK client like an agent would.
type harness struct {
	t      *testing.T
	core   *Core
	cs     *mcp.ClientSession
	docker *fakeDocker
	node   *nodefake.Server
	store  *state.Store
	cfg    *config.ClientConfig
}

type hopt func(*config.ClientConfig)

func newHarness(t *testing.T, freeGB float64, opts ...hopt) *harness {
	t.Helper()
	cfg := config.DefaultClient()
	cfg.Identity.Owner = "alice@mac"
	cfg.Nodes = []config.NodeRef{{Name: "tower", SSHHost: "tower", DockerContext: "dol-tower", Priority: 100}}
	cfg.Policy.HealthTimeoutMS = 500
	cfg.Policy.LeaseRequestTimeoutMS = 1000
	for _, o := range opts {
		o(cfg)
	}
	h := &harness{t: t, docker: newFakeDocker(), cfg: cfg}
	h.node = nodefake.Serve(t, nodefake.New("tower", v1.GBToBytes(freeGB)))
	var err error
	h.store, err = state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.core, err = NewCore(context.Background(), Options{
		Config: cfg, Store: h.store, Docker: h.docker, ProjectDir: "/work/payments-api", ClientArch: "arm64",
		NodeAPI: func(n config.NodeRef) NodeAPI { return h.node.Client() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.core.Close)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := NewServer(h.core).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	h.cs, err = mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.cs.Close() })
	return h
}

// call invokes a tool. It fails the test on protocol errors, not on tool
// errors (isError results are returned).
func (h *harness) call(name string, args map[string]any) *mcp.CallToolResult {
	h.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ok asserts success, a 2–4 line summary, and decodes structured content.
func ok[T any](h *harness, r *mcp.CallToolResult) T {
	h.t.Helper()
	if r.IsError {
		h.t.Fatalf("unexpected tool error: %s", text(r))
	}
	if len(r.Content) != 1 {
		h.t.Fatalf("want exactly one text summary, got %d content items", len(r.Content))
	}
	if n := len(strings.Split(text(r), "\n")); n < 1 || n > 4 {
		h.t.Fatalf("summary must be short (≤4 lines), got %d:\n%s", n, text(r))
	}
	if r.StructuredContent == nil {
		h.t.Fatal("missing structured content")
	}
	b, _ := json.Marshal(r.StructuredContent)
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("decoding structured content: %v\n%s", err, b)
	}
	return out
}

func toolErr(h *harness, r *mcp.CallToolResult, wantCode v1.ErrorCode) string {
	h.t.Helper()
	if !r.IsError {
		h.t.Fatalf("want isError, got success: %s", text(r))
	}
	s := text(r)
	if !strings.Contains(s, "["+string(wantCode)+"]") || !strings.Contains(s, "Next:") {
		h.t.Fatalf("error text must carry code %s and a Next: hint, got:\n%s", wantCode, s)
	}
	return s
}

func (h *harness) acquire(gb float64, extra ...map[string]any) *mcp.CallToolResult {
	args := map[string]any{"workload": "container", "est_memory_gb": gb}
	for _, e := range extra {
		for k, v := range e {
			args[k] = v
		}
	}
	return h.call("dol_acquire", args)
}

func TestToolsAreDescribedForAgents(t *testing.T) {
	h := newHarness(t, 40)
	if ins := h.cs.InitializeResult().Instructions; ins == "" || strings.Count(ins, "\n") < 4 || strings.Count(ins, "\n") > 7 {
		t.Fatalf("instructions must be 5-8 lines, got:\n%s", ins)
	}
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"dol_status": true, "dol_acquire": true, "dol_release": true, "dol_list": true,
		"dol_stop": true, "dol_remove": true, "dol_resize": true, "dol_pin": true}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %s", tool.Name)
			continue
		}
		delete(want, tool.Name)
		words := len(strings.Fields(tool.Description))
		t.Logf("%-12s %2d words", tool.Name, words)
		if words > 85 || words < 20 {
			t.Errorf("%s: description has %d words (want 20-85)", tool.Name, words)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s: missing input/output schema", tool.Name)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing tools: %v", want)
	}
	// Inputs per brief §8.
	var acq *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "dol_acquire" {
			acq = tool
		}
	}
	b, _ := json.Marshal(acq.InputSchema)
	for _, k := range []string{"workload", "est_memory_gb", "est_disk_gb", "project", "prefer", "pinned", "ttl_minutes", "allow_wake", `"enum"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("dol_acquire schema lacks %s: %s", k, b)
		}
	}
	var sch struct{ Required []string }
	_ = json.Unmarshal(b, &sch)
	if strings.Join(sch.Required, ",") != "workload,est_memory_gb" {
		t.Errorf("required = %v", sch.Required)
	}
}

func TestAcquireRemote(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(4, map[string]any{"workload": "kind"}))
	if r.Placement != v1.PlacementRemote || r.Node != "tower" || r.DockerContext != "dol-tower" {
		t.Fatalf("%+v", r)
	}
	if r.Commands.Docker != "docker --context dol-tower" || r.Commands.Compose != "docker --context dol-tower compose" {
		t.Fatalf("commands %+v", r.Commands)
	}
	if !strings.Contains(r.Commands.LabelFlags, v1.LabelLease+"="+r.LeaseID) || r.Labels[v1.LabelProject] != "payments-api" ||
		r.Labels[v1.LabelOwner] != "alice@mac" || r.Labels[v1.LabelSession] != h.core.Session() {
		t.Fatalf("labels %+v / %s", r.Labels, r.Commands.LabelFlags)
	}
	if len(r.Attempts) != 1 || r.Attempts[0].Outcome != "placed" || r.NodeArch != "amd64" || r.ClientArch != "arm64" {
		t.Fatalf("attempts %+v arch %s/%s", r.Attempts, r.NodeArch, r.ClientArch)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "build where you run") {
		t.Fatalf("missing arch warning: %v", r.Warnings)
	}
	if !strings.Contains(strings.Join(r.NextSteps, " "), "dol_kind_create") || !strings.Contains(strings.Join(r.NextSteps, " "), "dol_release") {
		t.Fatalf("next steps: %v", r.NextSteps)
	}
	if !h.docker.called("context create dol-tower --description managed by docker-offload-lan --docker host=ssh://tower") {
		t.Fatalf("docker context not ensured: %v", h.docker.calls)
	}
	if h.docker.called("context use") {
		t.Fatal("must never switch the current context")
	}
	if _, ok := h.node.Fake.Lease(r.LeaseID); !ok {
		t.Fatal("lease not on node")
	}
	l, err := h.store.GetLease(r.LeaseID)
	if err != nil || l.State != v1.StateActive || l.Placement != v1.PlacementRemote {
		t.Fatalf("state %+v %v", l, err)
	}
	// Second acquire reuses the ensured context (no second create).
	ok[v1.AcquireResult](h, h.acquire(1))
	n := 0
	for _, c := range h.docker.calls {
		if strings.HasPrefix(c, "context ls") {
			n++
		}
	}
	if n != 2 { // one for local auto-detection, one for EnsureContext
		t.Fatalf("context ls ran %d times", n)
	}
}

func TestAcquireFallsBackWhenNodeStopped(t *testing.T) {
	h := newHarness(t, 40)
	h.node.Stop()
	r := ok[v1.AcquireResult](h, h.acquire(2))
	if r.Placement != v1.PlacementLocal || r.Commands.Docker != "docker --context desktop-linux" || r.Node != "" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Reason, "tower unreachable") || r.Attempts[0].Code != v1.CodeUnreachable {
		t.Fatalf("reason %q attempts %+v", r.Reason, r.Attempts)
	}
	if !v1.ValidLeaseID(r.LeaseID) || r.Labels[v1.LabelLease] != r.LeaseID {
		t.Fatalf("local lease %+v", r)
	}
	if l, err := h.store.GetLease(r.LeaseID); err != nil || l.Placement != v1.PlacementLocal {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestAcquireFallsBackFastWhenNodeHangs(t *testing.T) {
	h := newHarness(t, 40)
	h.node.Fake.SetMode(nodefake.ModeHang)
	budget := time.Duration(h.cfg.Policy.HealthTimeoutMS)*time.Millisecond + 300*time.Millisecond
	start := time.Now()
	res := h.acquire(2)
	took := time.Since(start)
	r := ok[v1.AcquireResult](h, res)
	if r.Placement != v1.PlacementLocal || r.Attempts[0].Code != v1.CodeTimeout {
		t.Fatalf("%+v", r)
	}
	t.Logf("hung node → local fallback in %s (health_timeout_ms=%d, budget %s); reason: %s", took, h.cfg.Policy.HealthTimeoutMS, budget, r.Reason)
	if took > budget {
		t.Fatalf("fallback took %s, budget %s", took, budget)
	}
}

func TestAcquireFallsBackWhenNodeFull(t *testing.T) {
	h := newHarness(t, 6)
	r := ok[v1.AcquireResult](h, h.acquire(10))
	if r.Placement != v1.PlacementLocal {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Reason, "6.0 GiB schedulable, 10.0 GiB requested") || r.Attempts[0].Outcome != "refused" ||
		r.Attempts[0].Code != v1.CodeInsufficientMemory {
		t.Fatalf("reason %q attempts %+v", r.Reason, r.Attempts)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "max_memory_gb") {
		t.Fatalf("10 GiB local > 8 GiB max must warn: %v", r.Warnings)
	}
	if h.node.Fake.Calls("POST /v1/leases") != 1 {
		t.Fatal("lease request not sent")
	}
}

func TestFallbackNeverIsAnActionableError(t *testing.T) {
	h := newHarness(t, 2, func(c *config.ClientConfig) {
		c.Policy.Fallback = "never"
		c.Nodes = append(c.Nodes, config.NodeRef{Name: "spare", SSHHost: "spare", DockerContext: "dol-spare", Priority: 10})
	})
	s := toolErr(h, h.acquire(4), v1.CodeNoPlacement)
	for _, want := range []string{"tower refused", "spare refused", "policy.fallback is never", "2.0 GiB schedulable, 4.0 GiB requested"} {
		if !strings.Contains(s, want) {
			t.Errorf("error text lacks %q:\n%s", want, s)
		}
	}
	ls, _ := h.store.ListLeases()
	if len(ls) != 0 {
		t.Fatalf("no lease must be recorded: %+v", ls)
	}
}

func TestHeartbeatGoneMarksLeaseLost(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(2))
	rep := h.core.HeartbeatOnce(context.Background())
	if len(rep.OK) != 1 {
		t.Fatalf("%+v", rep)
	}
	h.node.Fake.Expire(r.LeaseID) // node reaper expired it → 410
	rep = h.core.HeartbeatOnce(context.Background())
	if len(rep.Lost) != 1 || rep.Lost[0] != r.LeaseID {
		t.Fatalf("%+v", rep)
	}
	l, _ := h.store.GetLease(r.LeaseID)
	if l.State != v1.StateLost {
		t.Fatalf("state %s", l.State)
	}
	s := toolErr(h, h.call("dol_resize", map[string]any{"lease_id": r.LeaseID, "est_memory_gb": 3}), v1.CodeLeaseLost)
	if !strings.Contains(s, r.LeaseID) || !strings.Contains(s, "dol_acquire") || !strings.Contains(s, "lease_gone") {
		t.Fatalf("lost message must say which lease, why and what next:\n%s", s)
	}
	toolErr(h, h.call("dol_stop", map[string]any{"lease_id": r.LeaseID, "target": "web"}), v1.CodeLeaseLost)
	// Lost leases are not heartbeated again.
	if rep := h.core.HeartbeatOnce(context.Background()); len(rep.OK)+len(rep.Lost)+len(rep.Failed) != 0 {
		t.Fatalf("%+v", rep)
	}
	// dol_release clears the stale record.
	rel := ok[v1.ReleaseResult](h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}))
	if len(rel.Warnings) == 0 {
		t.Fatal("want warning about lost lease")
	}
	if _, err := h.store.GetLease(r.LeaseID); v1.CodeOf(err) != v1.CodeNotFound {
		t.Fatal("record must be gone")
	}
}

func TestHeartbeatUnreachableKeepsLease(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(2))
	h.node.Stop()
	rep := h.core.HeartbeatOnce(context.Background())
	if len(rep.Failed) != 1 {
		t.Fatalf("%+v", rep)
	}
	if l, _ := h.store.GetLease(r.LeaseID); l.State != v1.StateActive {
		t.Fatalf("unreachable must not mark lost: %s", l.State)
	}
}

func TestStopAndRemoveRefuseUnlabeled(t *testing.T) {
	h := newHarness(t, 40)
	h.node.Stop()
	r := ok[v1.AcquireResult](h, h.acquire(1))
	h.docker.add("desktop-linux", "foreign", map[string]string{"com.example": "x"})
	h.docker.add("desktop-linux", "bare", nil)
	h.docker.add("desktop-linux", "mine", r.Labels)
	other := v1.LeaseLabels("l-zzzzzzzzzz", "alice@mac", "s-zzzzzzzzzz", "p")
	h.docker.add("desktop-linux", "theirs", other)

	for _, target := range []string{"foreign", "bare", "theirs"} {
		s := toolErr(h, h.call("dol_stop", map[string]any{"lease_id": r.LeaseID, "target": target}), v1.CodeNotOwned)
		if !strings.Contains(s, "refusing") {
			t.Fatalf("%s: %s", target, s)
		}
		toolErr(h, h.call("dol_remove", map[string]any{"lease_id": r.LeaseID, "target": target}), v1.CodeNotOwned)
		if c, ok := h.docker.get("desktop-linux", target); !ok || c.state != "running" {
			t.Fatalf("%s was touched", target)
		}
	}
	a := ok[v1.ActionResult](h, h.call("dol_stop", map[string]any{"lease_id": r.LeaseID, "target": "mine"}))
	if c, _ := h.docker.get("desktop-linux", "mine"); c.state != "exited" || a.Done[0] != "stopped container mine" {
		t.Fatalf("mine not stopped: %+v", a)
	}
	ok[v1.ActionResult](h, h.call("dol_remove", map[string]any{"lease_id": r.LeaseID, "target": "mine", "volumes": true}))
	if _, exists := h.docker.get("desktop-linux", "mine"); exists {
		t.Fatal("mine not removed")
	}
	toolErr(h, h.call("dol_remove", map[string]any{"lease_id": r.LeaseID, "target": "nothing"}), v1.CodeNotFound)
}

func TestListAndReleaseLocal(t *testing.T) {
	h := newHarness(t, 40)
	h.node.Stop()
	r := ok[v1.AcquireResult](h, h.acquire(1))
	h.docker.add("desktop-linux", "web", r.Labels)
	h.docker.add("desktop-linux", "db", r.Labels)
	h.docker.add("desktop-linux", "users-own", nil)
	kube := h.store.Path(state.KubeDir, r.LeaseID+"-e2e.yaml")
	if err := state.WriteFileAtomic(kube, []byte("apiVersion: v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	lr := ok[v1.ListResult](h, h.call("dol_list", nil))
	if len(lr.Leases) != 1 || len(lr.Resources) != 2 {
		t.Fatalf("list %+v", lr)
	}
	for _, res := range lr.Resources {
		if res.LeaseID != r.LeaseID || res.Name == "users-own" {
			t.Fatalf("list must be label-scoped: %+v", res)
		}
	}
	if lr1 := ok[v1.ListResult](h, h.call("dol_list", map[string]any{"lease_id": r.LeaseID})); len(lr1.Resources) != 2 {
		t.Fatalf("%+v", lr1)
	}

	rel := ok[v1.ReleaseResult](h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}))
	if rel.Placement != v1.PlacementLocal || len(rel.Removed.Containers) != 2 || len(rel.Removed.Errors) != 0 {
		t.Fatalf("%+v", rel)
	}
	if _, ok := h.docker.get("desktop-linux", "users-own"); !ok {
		t.Fatal("unlabeled container must survive release")
	}
	if _, err := os.Stat(kube); !os.IsNotExist(err) {
		t.Fatal("kubeconfig not deleted")
	}
	if _, err := h.store.GetLease(r.LeaseID); v1.CodeOf(err) != v1.CodeNotFound {
		t.Fatal("lease record not deleted")
	}
	toolErr(h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}), v1.CodeNotFound)
}

func TestReleaseRemote(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(2))
	rel := ok[v1.ReleaseResult](h, h.call("dol_release", map[string]any{"lease_id": r.LeaseID}))
	if rel.Placement != v1.PlacementRemote || rel.Node != "tower" {
		t.Fatalf("%+v", rel)
	}
	if l, _ := h.node.Fake.Lease(r.LeaseID); l.State != v1.StateReleased {
		t.Fatalf("node lease state %s", l.State)
	}
	// Unreachable node: error, record kept (state releasing), not heartbeated.
	r2 := ok[v1.AcquireResult](h, h.acquire(2))
	h.node.Stop()
	s := toolErr(h, h.call("dol_release", map[string]any{"lease_id": r2.LeaseID}), v1.CodeUnreachable)
	if !strings.Contains(s, "reaper") {
		t.Fatalf("%s", s)
	}
	if l, _ := h.store.GetLease(r2.LeaseID); l.State != v1.StateReleasing {
		t.Fatalf("%+v", l)
	}
}

func TestResizeAndPin(t *testing.T) {
	h := newHarness(t, 8)
	r := ok[v1.AcquireResult](h, h.acquire(4))
	if r.Placement != v1.PlacementRemote {
		t.Fatalf("%+v", r)
	}
	s := toolErr(h, h.call("dol_resize", map[string]any{"lease_id": r.LeaseID, "est_memory_gb": 20}), v1.CodeInsufficientMemory)
	if !strings.Contains(s, "8.0 GiB schedulable, 20.0 GiB requested") || !strings.Contains(s, "keep the current size (4.0 GiB)") {
		t.Fatalf("%s", s)
	}
	a := ok[v1.ActionResult](h, h.call("dol_resize", map[string]any{"lease_id": r.LeaseID, "est_memory_gb": 6}))
	if l, _ := h.node.Fake.Lease(r.LeaseID); l.EstMemoryBytes != 6*v1.GiB || a.Commands.Docker != "docker --context dol-tower" {
		t.Fatalf("node not patched: %+v", l)
	}
	ok[v1.ActionResult](h, h.call("dol_pin", map[string]any{"lease_id": r.LeaseID, "pinned": true, "ttl_minutes": 120}))
	nl, _ := h.node.Fake.Lease(r.LeaseID)
	cl, _ := h.store.GetLease(r.LeaseID)
	if !nl.Pinned || !cl.Pinned || cl.ExpiresAt == "" || cl.EstMemoryBytes != 6*v1.GiB {
		t.Fatalf("node %+v client %+v", nl, cl)
	}
	toolErr(h, h.call("dol_resize", map[string]any{"lease_id": "l-nonexisten", "est_memory_gb": 1}), v1.CodeNotFound)
	toolErr(h, h.call("dol_resize", map[string]any{"lease_id": "../../etc", "est_memory_gb": 1}), v1.CodeBadRequest)
	// Schema validation by the SDK rejects bad enums before our handler runs.
	if bad := h.call("dol_acquire", map[string]any{"workload": "vm", "est_memory_gb": 1}); !bad.IsError || !strings.Contains(text(bad), "container compose kind build") {
		t.Fatalf("%s", text(bad))
	}
	toolErr(h, h.call("dol_acquire", map[string]any{"workload": "build", "est_memory_gb": 5000}), v1.CodeBadRequest)
}

func TestStatus(t *testing.T) {
	h := newHarness(t, 40, func(c *config.ClientConfig) {
		c.Nodes = append(c.Nodes, config.NodeRef{Name: "spare", SSHHost: "spare", DockerContext: "dol-spare", Priority: 10})
	})
	ok[v1.AcquireResult](h, h.acquire(1))
	res := h.call("dol_status", nil)
	st := ok[v1.StatusResult](h, res)
	if len(st.Nodes) != 2 || !st.Nodes[0].Healthy || st.Nodes[0].Capacity == nil || st.Nodes[0].Health.Arch != "amd64" {
		t.Fatalf("%+v", st.Nodes)
	}
	if !st.Local.Reachable || st.Local.DockerContext != "desktop-linux" || st.Local.MemTotalBytes != 16*v1.GiB {
		t.Fatalf("%+v", st.Local)
	}
	if len(st.Leases) != 1 || st.Session != h.core.Session() || st.Owner != "alice@mac" {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(text(res), "tower: healthy amd64") {
		t.Fatalf("summary: %s", text(res))
	}
}
