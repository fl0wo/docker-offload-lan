package integration

// Golden path (brief §15) against the containerized fake node: the real
// `dol mcp` binary is driven over stdio by the official MCP Go SDK client,
// exactly like Claude Code would, and reaches the node over real ssh.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"github.com/fl0wo/docker-offload-lan/test/integration/harness"
)

const (
	nodeAlias  = "tower" // client-side node name → docker context dol-tower
	httpdImage = "busybox:1.37"
)

type agent struct {
	t       *testing.T
	n       *harness.Node
	cs      *mcp.ClientSession
	cmd     *exec.Cmd
	env     []string
	project string
}

// writeClientConfig writes an XDG config/state layout for dol mcp and returns
// the env to run it with.
func clientEnv(t *testing.T, n *harness.Node, policy string) ([]string, string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dolgp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cfgDir := filepath.Join(root, "config", "docker-offload-lan")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 1
identity:
  owner: it@golden
nodes:
  - name: %s
    ssh_host: %s
local:
  docker_context: dol-local-it
  max_memory_gb: 8
policy:
  prefer: remote
  fallback: always
  health_timeout_ms: 2000
  lease_request_timeout_ms: 5000
  heartbeat_interval_s: 2
%s`, nodeAlias, harness.Alias, policy)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "shop")
	if err := os.MkdirAll(filepath.Join(project, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, n.Env...),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"CLAUDE_PROJECT_DIR="+project,
		"DOL_DEBUG=1",
	)
	return env, project
}

// ensureLocalContext points a test-only context at this machine's Docker
// engine (the local fallback target) inside the test DOCKER_CONFIG.
func ensureLocalContext(t *testing.T, n *harness.Node) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		host = strings.TrimSpace(string(out))
	}
	_, _ = n.Docker(context.Background(), "context", "rm", "-f", "dol-local-it")
	if _, err := n.Docker(context.Background(), "context", "create", "dol-local-it", "--docker", "host="+host); err != nil {
		t.Fatal(err)
	}
}

func startAgent(t *testing.T, n *harness.Node, env []string, project string) *agent {
	t.Helper()
	cmd := exec.Command(n.DolBinary, "mcp")
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = project
	logf, _ := os.Create(filepath.Join(t.TempDir(), "dol-mcp.log"))
	cmd.Stderr = logf
	t.Cleanup(func() {
		if t.Failed() {
			_ = logf.Sync()
			b, _ := os.ReadFile(logf.Name())
			t.Logf("dol mcp stderr:\n%s", tailStr(string(b), 4000))
		}
	})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "golden-path-agent", Version: "1"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{t: t, n: n, cs: cs, cmd: cmd, env: env, project: project}
	t.Cleanup(func() { _ = cs.Close() })
	return a
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (a *agent) call(name string, args map[string]any) *mcp.CallToolResult {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	r, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: protocol error: %v", name, err)
	}
	return r
}

func text(r *mcp.CallToolResult) string {
	var s []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s = append(s, tc.Text)
		}
	}
	return strings.Join(s, "\n")
}

func ok[T any](t *testing.T, r *mcp.CallToolResult) T {
	t.Helper()
	if r.IsError {
		t.Fatalf("tool error: %s", text(r))
	}
	var out T
	b, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sh runs a shell command line (an agent's bash call) with the test env.
func (a *agent) sh(cmdline string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmdline)
	c.Env = append(os.Environ(), a.env...)
	c.Dir = a.project
	out, err := c.CombinedOutput()
	return string(out), err
}

func (a *agent) mustSh(cmdline string) string {
	a.t.Helper()
	out, err := a.sh(cmdline)
	if err != nil {
		a.t.Fatalf("%s: %v\n%s", cmdline, err, out)
	}
	return out
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	var last error
	for i := 0; i < 40; i++ {
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return string(b)
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", url, last)
	return ""
}

// nodeManaged lists managed resources on the node's docker.
func nodeManaged(t *testing.T, n *harness.Node, what string) string {
	out, err := n.Exec(context.Background(), "sh", "-c", "docker "+what+" --filter label="+v1.LabelManaged+"=true -q")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestGoldenPath(t *testing.T) {
	n := harness.Start(t)
	ensureLocalContext(t, n)
	env, project := clientEnv(t, n, "")
	a := startAgent(t, n, env, project)

	// 2. status: the node is healthy.
	st := ok[v1.StatusResult](t, a.call("dol_status", map[string]any{}))
	if len(st.Nodes) != 1 || !st.Nodes[0].Healthy {
		t.Fatalf("node not healthy: %+v", st.Nodes)
	}
	t.Logf("status: %s", text(a.call("dol_status", map[string]any{})))

	// 3. acquire → remote, with a docker context dol mcp created itself.
	acq := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 0.5, "project": "golden"}))
	if acq.Placement != v1.PlacementRemote || acq.Commands.Docker != "docker --context dol-"+nodeAlias {
		t.Fatalf("want remote placement via dol-%s, got %+v", nodeAlias, acq.PlacementInfo)
	}
	lease := acq.LeaseID

	// Agent runs a labeled web server on the node, published on 127.0.0.1.
	a.mustSh(fmt.Sprintf(`%s run -d --name web-%s %s -p 127.0.0.1:18080:80 %s sh -c 'mkdir -p /www && echo hello-from-node > /www/index.html && httpd -f -p 80 -h /www'`,
		acq.Commands.Docker, lease, acq.Commands.LabelFlags, httpdImage))
	// And an unlabeled container dol must never touch.
	a.mustSh(fmt.Sprintf(`%s run -d --name stranger-%s %s sleep 3600`, acq.Commands.Docker, lease, httpdImage))

	// 5. forward → curl localhost.
	fr := ok[map[string]any](t, a.call("dol_forward", map[string]any{"lease_id": lease, "remote_port": 18080}))
	port := int(fr["local_port"].(float64))
	if got := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)); !strings.Contains(got, "hello-from-node") {
		t.Fatalf("unexpected body %q", got)
	}

	// Ownership: the unlabeled container is refused.
	if r := a.call("dol_stop", map[string]any{"lease_id": lease, "target": "stranger-" + lease}); !r.IsError || !strings.Contains(text(r), "not_owned") {
		t.Fatalf("stopping an unlabeled container must be refused: %s", text(r))
	}

	// Compose with a relative bind mount, synced to the node.
	if err := os.WriteFile(filepath.Join(project, "www", "index.html"), []byte("hello-from-compose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := fmt.Sprintf(`services:
  web:
    image: %s
    command: ["httpd", "-f", "-p", "80", "-h", "/www"]
    ports: ["127.0.0.1:18081:80"]
    volumes: ["./www:/www:ro"]
`, httpdImage)
	if err := os.WriteFile(filepath.Join(project, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cu := ok[map[string]any](t, a.call("dol_compose_up", map[string]any{"lease_id": lease, "project_name": "shop"}))
	t.Logf("compose up: remote_dir=%v ports=%v", cu["remote_dir"], cu["ports"])
	fr2 := ok[map[string]any](t, a.call("dol_forward", map[string]any{"lease_id": lease, "remote_port": 18081}))
	if got := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", int(fr2["local_port"].(float64)))); !strings.Contains(got, "hello-from-compose") {
		t.Fatalf("compose service did not serve the synced file: %q", got)
	}

	list := ok[v1.ListResult](t, a.call("dol_list", map[string]any{"lease_id": lease}))
	t.Logf("list: %s", text(a.call("dol_list", map[string]any{"lease_id": lease})))
	if len(list.Resources) < 2 {
		t.Fatalf("expected the web container and compose resources, got %+v", list.Resources)
	}

	// 7. release → nothing managed left on the node, stranger untouched,
	// tunnels closed, sync dir gone.
	rel := ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": lease}))
	t.Logf("release: %s", text(a.call("dol_status", map[string]any{})))
	if len(rel.Forwards) != 2 {
		t.Errorf("release must close both tunnels, closed %v", rel.Forwards)
	}
	for _, what := range []string{"ps -a", "network ls", "volume ls"} {
		if left := nodeManaged(t, n, what); left != "" {
			t.Errorf("managed %s left on node after release: %s", what, left)
		}
	}
	if out, _ := n.Exec(context.Background(), "docker", "inspect", "-f", "{{.State.Running}}", "stranger-"+lease); strings.TrimSpace(out) != "true" {
		t.Errorf("unlabeled container must still be running, got %q", out)
	}
	if out, _ := n.Exec(context.Background(), "sh", "-c", "ls /srv/dol/"+lease+" 2>&1 || true"); !strings.Contains(out, "No such file") {
		t.Errorf("sync dir must be removed on release: %s", out)
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)); err == nil {
		t.Errorf("tunnel on %d still open after release", port)
	}
	_, _ = n.Exec(context.Background(), "docker", "rm", "-f", "stranger-"+lease)

	// 9. A request larger than the node → 409 → local fallback with numbers.
	big := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 500}))
	if big.Placement != v1.PlacementLocal || !strings.Contains(big.Reason, "GiB") || !strings.Contains(big.Reason, "insufficient_memory") {
		t.Fatalf("want local fallback with numbers, got %s / %s", big.Placement, big.Reason)
	}
	t.Logf("refusal reason: %s", big.Reason)
	ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": big.LeaseID}))

	// 8. Node frozen (paused container: TCP accepted nowhere, ssh hangs) →
	// local within health_timeout + overhead.
	if _, err := exec.Command("docker", "pause", n.Container).CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	down := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 0.5}))
	took := time.Since(start)
	_, _ = exec.Command("docker", "unpause", n.Container).CombinedOutput()
	t.Logf("frozen node → %s in %s: %s", down.Placement, took.Round(time.Millisecond), down.Reason)
	if down.Placement != v1.PlacementLocal {
		t.Fatalf("frozen node must fall back to local, got %+v", down.PlacementInfo)
	}
	if took > 2000*time.Millisecond+1500*time.Millisecond {
		t.Errorf("fallback took %s, want <= health_timeout (2s) + 1.5s", took)
	}
	ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": down.LeaseID}))
}

// TestAbandonedSessionIsReaped: 10. acquire remotely, kill the agent's dol
// mcp (SIGKILL, no goodbye), and the node's reaper removes everything after
// heartbeat_grace + interval.
func TestAbandonedSessionIsReaped(t *testing.T) {
	n := harness.Start(t)
	ctx := context.Background()
	const grace, interval = 6, 2
	if _, err := n.Exec(ctx, "sh", "-c", fmt.Sprintf(
		`sed -i -e 's/^\(\s*heartbeat_grace_s:\).*/\1 %d/' -e 's/^\(\s*interval_s:\).*/\1 %d/' /etc/docker-offload-lan/node.yaml && pkill -f '^/usr/local/bin/dol node serve'`, grace, interval)); err != nil {
		t.Fatal(err)
	}
	ensureLocalContext(t, n)
	env, project := clientEnv(t, n, "")
	// Wait for the agent loop to restart with the new config (it sleeps 2s).
	time.Sleep(2500 * time.Millisecond)
	for i := 0; i < 40; i++ {
		if _, err := n.Exec(ctx, "test", "-S", "/run/docker-offload-lan/node.sock"); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	a := startAgent(t, n, env, project)
	var acq v1.AcquireResult
	for i := 0; i < 20; i++ { // the agent may still be restarting
		acq = ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 0.25}))
		if acq.Placement == v1.PlacementRemote {
			break
		}
		ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": acq.LeaseID}))
		time.Sleep(time.Second)
	}
	if acq.Placement != v1.PlacementRemote {
		t.Fatalf("could not get a remote lease: %s", acq.Reason)
	}
	a.mustSh(fmt.Sprintf(`%s run -d %s %s sleep 3600`, acq.Commands.Docker, acq.Commands.LabelFlags, httpdImage))
	if nodeManaged(t, n, "ps -a") == "" {
		t.Fatal("container not created")
	}
	// Heartbeats keep it alive well past the grace period.
	time.Sleep(time.Duration(grace+interval+2) * time.Second)
	if nodeManaged(t, n, "ps -a") == "" {
		t.Fatal("lease was reaped although the session was alive and heartbeating")
	}
	// Session dies hard.
	_ = a.cmd.Process.Kill()
	killed := time.Now()
	for time.Since(killed) < time.Duration(grace+interval+20)*time.Second {
		if nodeManaged(t, n, "ps -a") == "" {
			t.Logf("abandoned lease reaped %s after the session died (grace %ds + interval %ds)", time.Since(killed).Round(time.Second), grace, interval)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("abandoned lease not reaped within grace+interval+20s; node log:\n%s", tailStr(n.NodeLog(ctx), 3000))
}
