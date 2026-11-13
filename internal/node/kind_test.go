package node

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func TestKindLifecycle(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	c := n.client
	l := n.lease(t, 4, func(r *v1.LeaseRequest) { r.Workload = v1.WorkloadKind })

	user := "nodes:\n- role: control-plane\n  extraPortMappings:\n  - containerPort: 30080\n    hostPort: 8080\n    listenAddress: 127.0.0.1\n"
	resp, err := c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "e2e", ConfigYAML: user})
	if err != nil {
		t.Fatal(err)
	}
	cluster := "dol-" + l.ID + "-e2e"
	if resp.Cluster != cluster || resp.AccessMode != v1.AccessTunnel || resp.APIServer != (v1.HostPort{Host: "127.0.0.1", Port: 20000}) ||
		!strings.Contains(resp.Kubeconfig, "kind: Config") || len(resp.PortMappings) != 1 || resp.PortMappings[0].HostPort != 8080 {
		t.Fatalf("response %+v", resp)
	}
	cfg := n.kind.configs[cluster]
	if !strings.Contains(cfg, "apiServerAddress: 127.0.0.1") || !strings.Contains(cfg, "apiServerPort: 20000") || strings.Contains(cfg, "certSANs") {
		t.Fatalf("kind config:\n%s", cfg)
	}
	if !calledWith(n.kind.calls, "create cluster --name "+cluster) {
		t.Fatalf("calls %v", n.kind.calls)
	}
	got, _ := c.GetLease(ctx, l.ID)
	if len(got.Resources.KindClusters) != 1 || got.Resources.KindClusters[0] != (v1.KindCluster{Name: "e2e", ClusterName: cluster, APIServerPort: 20000, AccessMode: "tunnel"}) {
		t.Fatalf("resources %+v", got.Resources)
	}

	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "e2e"})
	wantCode(t, err, v1.CodeConflict)

	// lan mode: second port, certSANs, LAN host.
	lan, err := c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "lan", AccessMode: v1.AccessLAN})
	if err != nil {
		t.Fatal(err)
	}
	if lan.APIServer != (v1.HostPort{Host: "192.168.1.50", Port: 20001}) {
		t.Fatalf("lan %+v", lan.APIServer)
	}
	lcfg := n.kind.configs["dol-"+l.ID+"-lan"]
	if !strings.Contains(lcfg, "apiServerAddress: 0.0.0.0") || !strings.Contains(lcfg, `- "192.168.1.50"`) || !strings.Contains(lcfg, `- "tower"`) {
		t.Fatalf("lan config:\n%s", lcfg)
	}

	if r, err := c.KindLoad(ctx, l.ID, "e2e", []string{"app:dev", "api:dev"}); err != nil || len(r.Loaded) != 2 {
		t.Fatal(r, err)
	}
	if !calledWith(n.kind.calls, "load docker-image --name "+cluster+" app:dev api:dev") {
		t.Fatalf("calls %v", n.kind.calls)
	}
	_, err = c.KindLoad(ctx, l.ID, "nope", []string{"x"})
	wantCode(t, err, v1.CodeNotFound)
	_, err = c.KindLoad(ctx, l.ID, "e2e", []string{"--help"})
	wantCode(t, err, v1.CodeBadRequest)

	if err := c.KindDelete(ctx, l.ID, "e2e"); err != nil {
		t.Fatal(err)
	}
	if n.kind.has(cluster) {
		t.Fatal("cluster not deleted")
	}
	used, _ := n.store.UsedPorts(ctx)
	if used[20000] || !used[20001] {
		t.Fatalf("ports %v", used)
	}
	// The freed port is reused.
	again, err := c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "e2e"})
	if err != nil || again.APIServer.Port != 20000 {
		t.Fatal(again, err)
	}

	// Release deletes remaining clusters and frees their ports.
	rel, err := c.ReleaseLease(ctx, l.ID, false)
	if err != nil || len(rel.Removed.KindClusters) != 2 {
		t.Fatal(rel, err)
	}
	if used, _ := n.store.UsedPorts(ctx); len(used) != 0 {
		t.Fatalf("ports after release %v", used)
	}
}

func calledWith(calls []string, want string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, want) {
			return true
		}
	}
	return false
}

func TestKindErrors(t *testing.T) {
	n := newTestNode(t, func(c *config.NodeConfig) { c.Kind.PortRange = [2]int{20000, 20000} })
	ctx := context.Background()
	c := n.client
	l := n.lease(t, 4)

	_, err := c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "Bad_Name"})
	wantCode(t, err, v1.CodeBadRequest)
	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "x", ConfigYAML: "kind: Pod\n"})
	wantCode(t, err, v1.CodeBadRequest)
	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "x", AccessMode: "public"})
	wantCode(t, err, v1.CodeBadRequest)
	if used, _ := n.store.UsedPorts(ctx); len(used) != 0 {
		t.Fatal("bad requests must not allocate ports")
	}

	// kind failure: 500 kind_failed, rolled back.
	n.kind.fail["create"] = errors.New("ERROR: failed to create cluster: node(s) already exist")
	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "x"})
	e := codeErr(t, err, v1.CodeKindFailed)
	if !strings.Contains(e.Message, "already exist") {
		t.Fatal(e.Message)
	}
	got, _ := c.GetLease(ctx, l.ID)
	if used, _ := n.store.UsedPorts(ctx); len(used) != 0 || len(got.Resources.KindClusters) != 0 {
		t.Fatalf("failed create not rolled back: %v %+v", used, got.Resources)
	}
	// kind missing: hint says how to install it.
	n.kind.fail["create"] = &exec.Error{Name: "kind", Err: exec.ErrNotFound}
	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "x"})
	if e := codeErr(t, err, v1.CodeKindFailed); !strings.Contains(e.Hint, "install.sh") {
		t.Fatal(e.Hint)
	}
	delete(n.kind.fail, "create")

	// Port range exhausted.
	if _, err := c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err = c.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "b"})
	if e := codeErr(t, err, v1.CodeConflict); !strings.Contains(e.Hint, "port_range") {
		t.Fatal(e.Hint)
	}

	// Image missing on the node.
	n.kind.fail["load"] = errors.New(`ERROR: image: "app:dev" not present locally`)
	_, err = c.KindLoad(ctx, l.ID, "a", []string{"app:dev"})
	if e := codeErr(t, err, v1.CodeKindFailed); !strings.Contains(e.Hint, "build the image on the node") {
		t.Fatal(e.Hint)
	}
}
