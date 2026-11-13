package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/nodefake"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	"github.com/fl0wo/docker-offload-lan/internal/testutil/fakessh"
	"github.com/fl0wo/docker-offload-lan/internal/tunnel"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	fakessh.Main()
	os.Exit(m.Run())
}

func withFakeSSH(t *testing.T) func(*Options) {
	bin := fakessh.Binary(t)
	return func(o *Options) {
		o.SSH = sshx.Options{Binary: bin}
		o.Tunnels = tunnel.NewManager(o.SSH)
	}
}

func structured[T any](t *testing.T, r *mcp.CallToolResult) T {
	t.Helper()
	if r.IsError {
		t.Fatalf("tool error: %s", textOf(r))
	}
	var out T
	b, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func textOf(r *mcp.CallToolResult) string {
	var s []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s = append(s, tc.Text)
		}
	}
	return strings.Join(s, "\n")
}

func echoListener(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func roundTrip(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hi"))
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
		t.Fatalf("tunnel round trip: %q %v", b, err)
	}
}

func acquireRemote(t *testing.T, h *harness, workload string) v1.AcquireResult {
	t.Helper()
	r := structured[v1.AcquireResult](t, h.call("dol_acquire", map[string]any{"workload": workload, "est_memory_gb": 4}))
	if r.Placement != v1.PlacementRemote {
		t.Fatalf("want remote, got %+v", r)
	}
	return r
}

func TestKindCreateTunnelKubeconfigAndRelease(t *testing.T) {
	h := newHarnessWith(t, 40, withFakeSSH(t))
	// The "node API server" is a local echo listener; since its port is busy
	// locally the client must pick another local port and rewrite the
	// kubeconfig to it.
	apiPort := echoListener(t)
	old := nodefake.KindAPIPort
	nodefake.KindAPIPort = apiPort
	t.Cleanup(func() { nodefake.KindAPIPort = old })

	acq := acquireRemote(t, h, "kind")
	kr := structured[KindCreateResult](t, h.call("dol_kind_create", map[string]any{"lease_id": acq.LeaseID, "name": "e2e"}))
	if kr.Cluster != "dol-"+acq.LeaseID+"-e2e" || kr.AccessMode != "tunnel" {
		t.Fatalf("%+v", kr)
	}
	if !strings.HasPrefix(kr.Commands.Kubectl, "kubectl --kubeconfig ") || !strings.Contains(kr.Commands.Kubectl, kr.Kubeconfig) {
		t.Fatalf("kubectl prefix: %q", kr.Commands.Kubectl)
	}
	st, err := os.Stat(kr.Kubeconfig)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig must exist with mode 0600: %v %v", st, err)
	}
	b, _ := os.ReadFile(kr.Kubeconfig)
	if strings.Contains(string(b), fmt.Sprintf(":%d\n", apiPort)) || !strings.Contains(string(b), "server: "+kr.APIServer) {
		t.Fatalf("server not rewritten to the tunnel port (%s):\n%s", kr.APIServer, b)
	}
	var localPort int
	fmt.Sscanf(kr.APIServer, "https://127.0.0.1:%d", &localPort)
	if localPort == 0 || localPort == apiPort || !strings.Contains(strings.Join(kr.Warnings, " "), "busy") {
		t.Fatalf("expected busy-port fallback, got %s warnings %v", kr.APIServer, kr.Warnings)
	}
	roundTrip(t, localPort)
	if len(kr.PortMappings) != 1 || !strings.Contains(strings.Join(kr.NextSteps, " "), "dol_forward remote_port 8080") {
		t.Fatalf("port mappings must be offered as forwards: %+v", kr.NextSteps)
	}

	if r := h.call("dol_kind_load", map[string]any{"lease_id": acq.LeaseID, "cluster": "e2e", "images": []string{"svc-a:dev", "svc-b:dev"}}); r.IsError {
		t.Fatal(textOf(r))
	}
	if r := h.call("dol_kind_load", map[string]any{"lease_id": acq.LeaseID, "cluster": "nope", "images": []string{"x"}}); !r.IsError {
		t.Fatal("unknown cluster must fail")
	}
	if r := h.call("dol_kind_create", map[string]any{"lease_id": acq.LeaseID, "name": "e2e"}); !r.IsError {
		t.Fatal("duplicate cluster must fail")
	}

	rel := structured[v1.ReleaseResult](t, h.call("dol_release", map[string]any{"lease_id": acq.LeaseID}))
	if len(rel.Forwards) != 1 || rel.Forwards[0] != localPort {
		t.Fatalf("release must close the API tunnel: %+v", rel)
	}
	if _, err := os.Stat(kr.Kubeconfig); !os.IsNotExist(err) {
		t.Fatal("kubeconfig must be deleted on release")
	}
	if !tunnel.PortFree(localPort) {
		time.Sleep(200 * time.Millisecond)
		if !tunnel.PortFree(localPort) {
			t.Fatal("tunnel still listening after release")
		}
	}
}

func TestKindDeleteClosesTunnel(t *testing.T) {
	h := newHarnessWith(t, 40, withFakeSSH(t))
	nodefake.KindAPIPort = 0 // random: never busy
	free, _ := tunnel.FreePort()
	nodefake.KindAPIPort = free
	t.Cleanup(func() { nodefake.KindAPIPort = 16443 })
	acq := acquireRemote(t, h, "kind")
	kr := structured[KindCreateResult](t, h.call("dol_kind_create", map[string]any{"lease_id": acq.LeaseID, "name": "a"}))
	if kr.APIServer != fmt.Sprintf("https://127.0.0.1:%d", free) {
		t.Fatalf("free port must be kept: %s", kr.APIServer)
	}
	if r := h.call("dol_kind_delete", map[string]any{"lease_id": acq.LeaseID, "cluster": "a"}); r.IsError {
		t.Fatal(textOf(r))
	}
	if len(h.core.tunnels.List(acq.LeaseID)) != 0 {
		t.Fatal("tunnel not closed")
	}
	l, _ := h.store.GetLease(acq.LeaseID)
	if len(l.Resources.KindClusters) != 0 || len(l.Kubeconfigs) != 0 {
		t.Fatalf("lease still tracks cluster: %+v", l)
	}
}

func TestLANModeUsesNodeAddress(t *testing.T) {
	h := newHarnessWith(t, 40, withFakeSSH(t))
	acq := acquireRemote(t, h, "kind")
	kr := structured[KindCreateResult](t, h.call("dol_kind_create", map[string]any{"lease_id": acq.LeaseID, "name": "lan", "access_mode": "lan"}))
	b, _ := os.ReadFile(kr.Kubeconfig)
	if !strings.Contains(string(b), "server: https://192.0.2.10:") || strings.Contains(string(b), "0.0.0.0") {
		t.Fatalf("lan kubeconfig must point at the node address:\n%s", b)
	}
	if len(h.core.tunnels.List(acq.LeaseID)) != 0 {
		t.Fatal("lan mode must not open a tunnel")
	}
}

func TestForwardAndUnforward(t *testing.T) {
	h := newHarnessWith(t, 40, withFakeSSH(t))
	acq := acquireRemote(t, h, "container")
	svc := echoListener(t) // "service on the node" (busy locally → another port)
	fr := structured[ForwardResult](t, h.call("dol_forward", map[string]any{"lease_id": acq.LeaseID, "remote_port": svc}))
	if fr.LocalPort == svc || fr.LocalPort == 0 {
		t.Fatalf("%+v", fr)
	}
	roundTrip(t, fr.LocalPort)
	l, _ := h.store.GetLease(acq.LeaseID)
	if len(l.Forwards) != 1 || l.Forwards[0].LocalPort != fr.LocalPort {
		t.Fatalf("forward not recorded: %+v", l.Forwards)
	}
	if r := h.call("dol_unforward", map[string]any{"lease_id": acq.LeaseID, "local_port": fr.LocalPort}); r.IsError {
		t.Fatal(textOf(r))
	}
	if r := h.call("dol_unforward", map[string]any{"lease_id": acq.LeaseID, "local_port": fr.LocalPort}); !r.IsError {
		t.Fatal("second unforward must fail")
	}
}

func TestForwardSSHFailureIsActionable(t *testing.T) {
	h := newHarnessWith(t, 40, withFakeSSH(t))
	acq := acquireRemote(t, h, "container")
	t.Setenv("DOL_FAKE_SSH_FAIL", "1")
	r := h.call("dol_forward", map[string]any{"lease_id": acq.LeaseID, "remote_port": 8080})
	if !r.IsError || !strings.Contains(textOf(r), "Next:") || !strings.Contains(textOf(r), "Connection refused") {
		t.Fatalf("want actionable error, got %s", textOf(r))
	}
}

// TestSyncAndComposeUpRemote syncs with the real rsync binary through the
// fake ssh (remote command runs locally), then renders/rewrites/labels a
// compose project and checks the exact `compose up` invocation.
func TestSyncAndComposeUpRemote(t *testing.T) {
	remoteRoot := t.TempDir()
	proj := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(filepath.Join(proj, "data"), 0o755)
	os.WriteFile(filepath.Join(proj, "data", "seed.sql"), []byte("select 1;"), 0o644)
	os.WriteFile(filepath.Join(proj, ".gitignore"), []byte("*.log\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "debug.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(proj, "compose.yaml"), []byte("services: {}\n"), 0o644)

	h := newHarnessWith(t, 40, func(o *Options) {
		withFakeSSH(t)(o)
		o.ProjectDir = proj
	}, func(c *config.ClientConfig) { c.Sync.RemoteRoot = remoteRoot })
	h.docker.composeConfig = fmt.Sprintf(`{"name":"shop","services":{"db":{"image":"postgres","ports":[{"host_ip":"127.0.0.1","target":5432,"published":"5432"}],
	  "volumes":[{"type":"bind","source":%q,"target":"/docker-entrypoint-initdb.d"}]}},
	  "networks":{"default":{"name":"shop_default"}}}`, filepath.Join(proj, "data"))

	acq := acquireRemote(t, h, "compose")
	sr := structured[SyncResult](t, h.call("dol_sync", map[string]any{"lease_id": acq.LeaseID}))
	want := filepath.Join(remoteRoot, acq.LeaseID, "shop")
	if sr.RemotePath != want {
		t.Fatalf("remote path %s, want %s", sr.RemotePath, want)
	}
	if _, err := os.Stat(filepath.Join(want, "data", "seed.sql")); err != nil {
		t.Fatal("file not synced")
	}
	if _, err := os.Stat(filepath.Join(want, "debug.log")); err == nil {
		t.Fatal(".gitignore not honored")
	}
	if fl, _ := h.node.Fake.Lease(acq.LeaseID); len(fl.Resources.SyncDirs) != 1 || fl.Resources.SyncDirs[0] != want {
		t.Fatalf("sync dir not registered on node: %+v", fl.Resources)
	}

	cu := structured[ComposeUpResult](t, h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID, "extra_args": []string{"--wait"}}))
	if cu.Project != "shop" || cu.RemoteDir != want || len(cu.Ports) != 1 {
		t.Fatalf("%+v", cu)
	}
	rendered, _ := os.ReadFile(cu.RenderedFile)
	for _, s := range []string{`"source": "` + filepath.Join(want, "data") + `"`, `"` + v1.LabelLease + `": "` + acq.LeaseID + `"`} {
		if !strings.Contains(string(rendered), s) {
			t.Fatalf("rendered file missing %s:\n%s", s, rendered)
		}
	}
	if !h.docker.called("--context dol-tower compose -p shop -f " + cu.RenderedFile + " up -d --wait") {
		t.Fatalf("compose up not called as expected: %v", h.docker.calls)
	}
	if fl, _ := h.node.Fake.Lease(acq.LeaseID); len(fl.Resources.ComposeProjects) != 1 {
		t.Fatal("compose project not registered on node")
	}
	if !strings.Contains(strings.Join(cu.NextSteps, " "), "dol_forward with remote_port 5432") {
		t.Fatalf("next steps: %v", cu.NextSteps)
	}
	if r := h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID, "extra_args": []string{"-f", "x.yml"}}); !r.IsError {
		t.Fatal("extra_args with -f must be refused")
	}
	if r := h.call("dol_compose_down", map[string]any{"lease_id": acq.LeaseID, "project_name": "other"}); !r.IsError {
		t.Fatal("unregistered project must be refused")
	}
	if r := h.call("dol_compose_down", map[string]any{"lease_id": acq.LeaseID, "project_name": "shop"}); r.IsError {
		t.Fatal(textOf(r))
	}
}

func TestRewriteKubeconfigServer(t *testing.T) {
	in := fmt.Sprintf(nodefake.FakeKubeconfig, 1, "c", "c", "c", "c", "c", "c")
	out, err := RewriteKubeconfigServer([]byte(in), "https://127.0.0.1:9999")
	if err != nil || !strings.Contains(string(out), "server: https://127.0.0.1:9999") || !strings.Contains(string(out), "client-certificate-data: YmFy") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := RewriteKubeconfigServer([]byte("kind: Config\n"), "x"); err == nil {
		t.Fatal("missing server must error")
	}
}
