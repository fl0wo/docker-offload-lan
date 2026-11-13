package placement

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/nodefake"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

type fakeLocal struct {
	total     int64
	err       error
	committed int64
}

func (f fakeLocal) MemTotal(context.Context) (int64, error) { return f.total, f.err }
func (f fakeLocal) Committed() int64                        { return f.committed }

func policy(fallback string) Policy {
	return Policy{Prefer: "remote", Fallback: fallback, HealthTimeout: 300 * time.Millisecond,
		LeaseRequestTimeout: time.Second, LocalMaxMemoryBytes: 8 * v1.GiB}
}

func req(gb float64) Request {
	return Request{Lease: v1.LeaseRequest{Owner: "o@h", Session: "s-aaaaaaaaaa", Project: "p",
		Workload: v1.WorkloadContainer, EstMemoryBytes: v1.GBToBytes(gb)}}
}

func node(t *testing.T, name string, prio int, free float64) (Node, *nodefake.Server) {
	s := nodefake.Serve(t, nodefake.New(name, v1.GBToBytes(free)))
	return Node{Name: name, SSHHost: name, DockerContext: "dol-" + name, Priority: prio, API: s.Client()}, s
}

func TestRemoteByPriorityWithArchWarning(t *testing.T) {
	lo, _ := node(t, "lo", 10, 100)
	hi, _ := node(t, "hi", 200, 100)
	p := &Placer{Nodes: []Node{lo, hi}, Policy: policy("always"), ClientArch: "arm64", Local: fakeLocal{total: 16 * v1.GiB}}
	d, err := p.Place(context.Background(), req(4))
	if err != nil {
		t.Fatal(err)
	}
	if d.Placement != v1.PlacementRemote || d.Node.Name != "hi" || d.Lease == nil || !v1.ValidLeaseID(d.Lease.ID) {
		t.Fatalf("%+v", d)
	}
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "build where you run") {
		t.Fatalf("want arch warning, got %v", d.Warnings)
	}
}

func TestFallbacks(t *testing.T) {
	ctx := context.Background()

	t.Run("stopped node falls back with reason", func(t *testing.T) {
		n, s := node(t, "tower", 100, 100)
		s.Stop()
		p := &Placer{Nodes: []Node{n}, Policy: policy("always")}
		d, err := p.Place(ctx, req(2))
		if err != nil || d.Placement != v1.PlacementLocal {
			t.Fatalf("%+v %v", d, err)
		}
		if !strings.Contains(d.Reason, "tower unreachable") || d.Attempts[0].Code != v1.CodeUnreachable {
			t.Fatalf("reason %q attempts %+v", d.Reason, d.Attempts)
		}
	})

	t.Run("hung node falls back within health timeout", func(t *testing.T) {
		n, s := node(t, "tower", 100, 100)
		s.Fake.SetMode(nodefake.ModeHang)
		p := &Placer{Nodes: []Node{n}, Policy: policy("always")}
		start := time.Now()
		d, err := p.Place(ctx, req(2))
		took := time.Since(start)
		if err != nil || d.Placement != v1.PlacementLocal || d.Attempts[0].Code != v1.CodeTimeout {
			t.Fatalf("%+v %v", d, err)
		}
		if took > p.Policy.HealthTimeout+300*time.Millisecond {
			t.Fatalf("took %s", took)
		}
		t.Logf("hung node: local fallback after %s (health_timeout %s)", took, p.Policy.HealthTimeout)
	})

	t.Run("full node 409 falls back with numbers", func(t *testing.T) {
		n, _ := node(t, "tower", 100, 6)
		p := &Placer{Nodes: []Node{n}, Policy: policy("always")}
		d, err := p.Place(ctx, req(10))
		if err != nil || d.Placement != v1.PlacementLocal {
			t.Fatalf("%+v %v", d, err)
		}
		if !strings.Contains(d.Reason, "6.0 GiB schedulable, 10.0 GiB requested") || d.Attempts[0].Outcome != "refused" {
			t.Fatalf("reason %q", d.Reason)
		}
		if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "max_memory_gb") {
			t.Fatalf("want local max memory warning: %v", d.Warnings)
		}
	})

	t.Run("never lists every node", func(t *testing.T) {
		a, sa := node(t, "a", 100, 100)
		sa.Stop()
		b, _ := node(t, "b", 50, 1)
		p := &Placer{Nodes: []Node{a, b}, Policy: policy("never")}
		_, err := p.Place(ctx, req(4))
		if v1.CodeOf(err) != v1.CodeNoPlacement {
			t.Fatalf("want no_placement, got %v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "a unreachable") || !strings.Contains(msg, "b refused") || !strings.Contains(msg, "never") {
			t.Fatalf("message must list every node: %s", msg)
		}
	})

	t.Run("if_capacity", func(t *testing.T) {
		n, _ := node(t, "tower", 100, 1)
		p := &Placer{Nodes: []Node{n}, Policy: policy("if_capacity"), Local: fakeLocal{total: 8 * v1.GiB, committed: 3 * v1.GiB}}
		if d, err := p.Place(ctx, req(4)); err != nil || d.Placement != v1.PlacementLocal {
			t.Fatalf("4 GiB should fit locally: %+v %v", d, err)
		}
		_, err := p.Place(ctx, req(6))
		if v1.CodeOf(err) != v1.CodeNoPlacement || !strings.Contains(err.Error(), "5.0 GiB free") {
			t.Fatalf("want refusal with numbers, got %v", err)
		}
		p.Local = fakeLocal{err: errors.New("daemon down")}
		if _, err := p.Place(ctx, req(2)); v1.CodeOf(err) != v1.CodeLocalDockerDown || !strings.Contains(fmtErr(err), "local Docker (context ) is unreachable") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no nodes", func(t *testing.T) {
		p := &Placer{Policy: policy("always")}
		d, err := p.Place(ctx, req(1))
		if err != nil || !strings.Contains(d.Reason, "no nodes are configured") {
			t.Fatalf("%+v %v", d, err)
		}
	})
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestPreferLocal(t *testing.T) {
	n, s := node(t, "tower", 100, 100)
	p := &Placer{Nodes: []Node{n}, Policy: policy("always"), Local: fakeLocal{total: 8 * v1.GiB}}
	r := req(2)
	r.Prefer = "local"
	d, err := p.Place(context.Background(), r)
	if err != nil || d.Placement != v1.PlacementLocal || s.Fake.Calls("POST /v1/leases") != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	r = req(12) // does not fit locally → node
	r.Prefer = "local"
	d, err = p.Place(context.Background(), r)
	if err != nil || d.Placement != v1.PlacementRemote || d.Attempts[0].Node != "local" || d.Attempts[0].Outcome != "skipped" {
		t.Fatalf("%+v %v", d, err)
	}
}

// countingAPI counts health calls.
type countingAPI struct {
	NodeAPI
	health atomic.Int32
}

func (c *countingAPI) Health(ctx context.Context) (*v1.Health, error) {
	c.health.Add(1)
	return c.NodeAPI.Health(ctx)
}

func TestPlacementAlwaysProbesHealth(t *testing.T) {
	n, s := node(t, "tower", 100, 100)
	api := &countingAPI{NodeAPI: n.API}
	n.API = api
	now := time.Unix(1000, 0)
	p := &Placer{Nodes: []Node{n}, Policy: policy("always"), Now: func() time.Time { return now }}
	ctx := context.Background()
	for range 3 {
		if _, err := p.Place(ctx, req(1)); err != nil {
			t.Fatal(err)
		}
	}
	// A node that froze since the last success must cost health_timeout,
	// not lease_request_timeout: placement never trusts a cached success.
	if got := api.health.Load(); got != 3 {
		t.Fatalf("placement must probe health every time: %d health calls", got)
	}
	if _, err := p.Health(ctx, n, false); err != nil || api.health.Load() != 3 {
		t.Fatal("read-only callers may use the cached success")
	}
	// Failures are not cached: once the node is back it is used immediately.
	s.Fake.SetMode(nodefake.ModeError500)
	p.Invalidate("tower")
	if d, _ := p.Place(ctx, req(1)); d.Placement != v1.PlacementLocal {
		t.Fatal("want local while node fails")
	}
	s.Fake.SetMode(nodefake.ModeOK)
	if d, _ := p.Place(ctx, req(1)); d.Placement != v1.PlacementRemote {
		t.Fatal("recovered node must be used at once")
	}
}
