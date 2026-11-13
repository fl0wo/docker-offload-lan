package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func TestReaperExpiresTearsDownAndSparesOthers(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	c := n.client

	abandoned := n.lease(t, 2)
	pinned := n.lease(t, 2, func(r *v1.LeaseRequest) { r.Pinned = true })
	alive := n.lease(t, 2)

	// Resources of each lease, an orphan (lease unknown to the store), a
	// resource with a malformed lease label, and things dol does not own.
	n.docker.add(dockerx.KindContainer, "abandoned-app", abandoned.Labels)
	n.docker.add(dockerx.KindNetwork, "abandoned-net", abandoned.Labels)
	n.docker.add(dockerx.KindVolume, "abandoned-vol", abandoned.Labels)
	n.docker.add(dockerx.KindContainer, "pinned-db", pinned.Labels)
	n.docker.add(dockerx.KindContainer, "alive-web", alive.Labels)
	orphan := v1.LeaseLabels("l-orphanabcd", "bob", "s-x", "old")
	n.docker.add(dockerx.KindContainer, "orphan-app", orphan)
	n.docker.add(dockerx.KindVolume, "orphan-vol", orphan)
	n.docker.add(dockerx.KindContainer, "weird", map[string]string{v1.LabelManaged: "true", v1.LabelLease: "not-a-lease"})
	n.docker.add(dockerx.KindContainer, "users-postgres", map[string]string{"com.example.app": "pg"})
	n.docker.add(dockerx.KindVolume, "users-data", nil)
	n.docker.add(dockerx.KindNetwork, "users-net", nil)
	n.docker.add(dockerx.KindNetwork, "bridge", nil)
	if err := c.RegisterCompose(ctx, abandoned.ID, "abandoned-stack"); err != nil {
		t.Fatal(err)
	}

	// 5 minutes in: everyone is within grace.
	n.clock.Advance(5 * time.Minute)
	if _, err := c.Heartbeat(ctx, alive.ID); err != nil {
		t.Fatal(err)
	}
	r := n.srv.ReapOnce(ctx)
	if len(r.Expired) != 0 || len(r.TornDown) != 0 {
		t.Fatalf("nothing should expire yet: %+v", r)
	}
	// The orphan goes on the first pass already.
	if !slices.Equal(r.Orphans, []string{"l-orphanabcd"}) {
		t.Fatalf("orphans %v", r.Orphans)
	}

	// Grace (600 s) after the last heartbeat of "abandoned" has passed;
	// "alive" heartbeated 5 minutes ago.
	n.clock.Advance(5*time.Minute + time.Second)
	r = n.srv.ReapOnce(ctx)
	if !slices.Equal(r.Expired, []string{abandoned.ID}) || !slices.Equal(r.TornDown, []string{abandoned.ID}) {
		t.Fatalf("report %+v", r)
	}
	if r.Unlabeled != (v1.UnlabeledCounts{Containers: 1, Volumes: 1, Networks: 1}) {
		t.Fatalf("unlabeled %+v", r.Unlabeled)
	}
	want := []string{"alive-web", "pinned-db", "users-postgres", "weird"}
	if got := n.docker.names(dockerx.KindContainer); !slices.Equal(got, want) {
		t.Fatalf("containers %v, want %v", got, want)
	}
	if got := n.docker.names(dockerx.KindVolume); !slices.Equal(got, []string{"users-data"}) {
		t.Fatalf("volumes %v", got)
	}
	if got := n.docker.names(dockerx.KindNetwork); !slices.Equal(got, []string{"bridge", "users-net"}) {
		t.Fatalf("networks %v", got)
	}
	if len(n.docker.callsWith("compose -p abandoned-stack down")) != 1 {
		t.Fatal("compose project of the expired lease not taken down")
	}
	// Unlabeled objects were never inspected, stopped or removed.
	for _, name := range []string{"users-postgres", "users-data", "users-net", "weird"} {
		for _, call := range n.docker.callsWith("") {
			if strings.HasSuffix(call, "-- "+name) {
				t.Fatalf("touched unowned resource: %s", call)
			}
		}
	}
	got, _ := c.GetLease(ctx, abandoned.ID)
	if got.State != v1.StateReleased {
		t.Fatalf("state %s", got.State)
	}
	_, err := c.Heartbeat(ctx, abandoned.ID)
	wantCode(t, err, v1.CodeLeaseGone)
	cp, _ := c.Capacity(ctx)
	if cp.Unlabeled.Containers != 1 || cp.Memory.CommittedBytes != 4*G {
		t.Fatalf("capacity %+v", cp)
	}

	// A month later: the pinned lease survives, the silent one is gone.
	n.clock.Advance(30 * 24 * time.Hour)
	r = n.srv.ReapOnce(ctx)
	if !slices.Equal(r.Expired, []string{alive.ID}) {
		t.Fatalf("report %+v", r)
	}
	p, _ := c.GetLease(ctx, pinned.ID)
	if p.State != v1.StateActive || !slices.Contains(n.docker.names(dockerx.KindContainer), "pinned-db") {
		t.Fatal("pinned lease must survive without heartbeats")
	}
}

func TestReaperMaxTTLAndPinnedExpiry(t *testing.T) {
	n := newTestNode(t, func(c *config.NodeConfig) { c.Leases.MaxTTLS = 3600 })
	ctx := context.Background()
	l := n.lease(t, 1)
	pinned := n.lease(t, 1, func(r *v1.LeaseRequest) { r.Pinned, r.TTLSeconds = true, 7200 })
	for i := 0; i < 6; i++ { // heartbeats every 9 minutes for 54 minutes
		n.clock.Advance(9 * time.Minute)
		if _, err := n.client.Heartbeat(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
	}
	n.clock.Advance(7 * time.Minute) // 61 minutes: max TTL passed despite heartbeats
	r := n.srv.ReapOnce(ctx)
	if !slices.Equal(r.Expired, []string{l.ID}) {
		t.Fatalf("max_ttl not enforced: %+v", r)
	}
	n.clock.Advance(60 * time.Minute) // 121 minutes: pinned explicit expiry passed
	r = n.srv.ReapOnce(ctx)
	if !slices.Equal(r.Expired, []string{pinned.ID}) {
		t.Fatalf("pinned explicit expiry not enforced: %+v", r)
	}
}

func TestReaperRetriesFailedTeardown(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	l := n.lease(t, 1)
	n.docker.add(dockerx.KindContainer, "stuck", l.Labels)
	n.docker.fail["rm -f"] = "Error response from daemon: device or resource busy"

	rel, err := n.client.ReleaseLease(ctx, l.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Lease.State != v1.StateFailed || !strings.Contains(rel.Lease.LastError, "device or resource busy") || len(rel.Removed.Errors) != 1 {
		t.Fatalf("release %+v", rel)
	}
	r := n.srv.ReapOnce(ctx)
	if !slices.Equal(r.Failed, []string{l.ID}) {
		t.Fatalf("report %+v", r)
	}
	delete(n.docker.fail, "rm -f")
	r = n.srv.ReapOnce(ctx)
	if !slices.Equal(r.TornDown, []string{l.ID}) {
		t.Fatalf("report %+v", r)
	}
	got, _ := n.client.GetLease(ctx, l.ID)
	if got.State != v1.StateReleased || got.LastError != "" || len(n.docker.names(dockerx.KindContainer)) != 0 {
		t.Fatalf("lease %+v", got)
	}
}

func TestReaperReconcilesAfterRestart(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	l := n.lease(t, 1)
	n.docker.add(dockerx.KindContainer, "app", l.Labels)
	// Simulate a crash in the middle of a release.
	got, _ := n.store.Get(ctx, l.ID)
	got.State = v1.StateReleasing
	if err := n.store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	n.stop()
	n2 := startNode(t, n.cfg, n)
	r := n2.srv.ReapOnce(ctx) // what RunReaper does first on startup
	if !slices.Equal(r.TornDown, []string{l.ID}) || len(n2.docker.names(dockerx.KindContainer)) != 0 {
		t.Fatalf("report %+v", r)
	}
}

func TestReaperKindClusters(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	l := n.lease(t, 4, func(r *v1.LeaseRequest) { r.Workload = v1.WorkloadKind })
	if _, err := n.client.KindCreate(ctx, l.ID, v1.KindCreateRequest{Name: "e2e"}); err != nil {
		t.Fatal(err)
	}
	cluster := "dol-" + l.ID + "-e2e"
	n.kind.clusters["dol-l-zzzzzzzzzz-old"] = true // orphan from an unknown lease
	n.kind.clusters["kind"] = true                 // the user's own cluster
	n.kind.clusters["dol-hand-made"] = true        // looks like ours but is not a lease name

	n.clock.Advance(11 * time.Minute)
	r := n.srv.ReapOnce(ctx)
	if !slices.Equal(r.TornDown, []string{l.ID}) || !slices.Equal(r.OrphanClusters, []string{"dol-l-zzzzzzzzzz-old"}) {
		t.Fatalf("report %+v", r)
	}
	if n.kind.has(cluster) || !n.kind.has("kind") || !n.kind.has("dol-hand-made") {
		t.Fatalf("clusters %v", n.kind.clusters)
	}
	if used, _ := n.store.UsedPorts(ctx); len(used) != 0 {
		t.Fatalf("port not freed: %v", used)
	}
}

func TestReaperDiskPressurePrunesSafely(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	n.prov.Update(func(s *capacity.Snapshot) { s.DiskFree = 10 * G })
	r := n.srv.ReapOnce(ctx)
	if !r.Pruned || len(n.docker.callsWith("builder prune -f")) != 1 || len(n.docker.callsWith("image prune -f")) != 1 {
		t.Fatalf("report %+v calls %v", r, n.docker.calls)
	}
	for _, c := range n.docker.callsWith("") {
		if strings.Contains(c, "volume prune") || strings.Contains(c, "system prune") || strings.Contains(c, "prune -a") || strings.Contains(c, "--all") {
			t.Fatalf("unsafe prune: %s", c)
		}
	}
	n.prov.Update(func(s *capacity.Snapshot) { s.DiskFree = 200 * G })
	if r := n.srv.ReapOnce(ctx); r.Pruned {
		t.Fatal("pruned without pressure")
	}
}

func TestReaperDockerDownDegradesHealth(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	n.docker.fail[""] = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
	r := n.srv.ReapOnce(ctx)
	if len(r.Errors) == 0 {
		t.Fatal("expected errors")
	}
	h, _ := n.client.Health(ctx)
	if h.Status != "degraded" || len(h.Problems) != 1 || !strings.Contains(h.Problems[0], "systemctl status docker") {
		t.Fatalf("health %+v", h)
	}
	delete(n.docker.fail, "")
	n.srv.ReapOnce(ctx)
	if h, _ := n.client.Health(ctx); h.Status != "ok" {
		t.Fatalf("health did not recover: %+v", h)
	}
}

func TestTeardownBusyLeaseIsSkipped(t *testing.T) {
	n := newTestNode(t)
	l := n.lease(t, 1)
	lk := n.srv.opLock(l.ID)
	lk.Lock()
	_, _, err := n.srv.Teardown(context.Background(), l.ID, false, false)
	lk.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
