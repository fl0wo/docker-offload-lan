package node

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// TestRealDockerTeardown runs against the local Docker daemon. Opt in with
// DOL_DOCKER_TEST=1. It only creates and removes resources it names itself
// (prefix dol-test-<random>) and only lets the reaper act on its own lease
// ids, so it is safe on a developer machine shared with other workloads.
func TestRealDockerTeardown(t *testing.T) {
	if os.Getenv("DOL_DOCKER_TEST") != "1" {
		t.Skip("set DOL_DOCKER_TEST=1 to run against the local Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Output(); err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	image := "busybox:latest"
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() != nil {
		docker("pull", image)
	}

	dir := sockDir(t)
	cfg := testConfig(dir)
	st, err := OpenStore(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	clock := newClock()
	orphanID := v1.NewLeaseID()
	srv, err := New(ctx, Options{Config: cfg, Store: st, Provider: capacity.FakeFromConfig(cfg.Capacity.Fake),
		Docker: dockerx.New(""), Kind: newFakeKind(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ListenUnix(cfg.Socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	c := nodeclient.NewUnix("test", cfg.Socket)
	defer c.Close()

	l, err := c.CreateLease(ctx, v1.LeaseRequest{Owner: "dol-test", Session: "s-dockertest", Project: "itest",
		Workload: v1.WorkloadContainer, EstMemoryBytes: v1.GiB})
	if err != nil {
		t.Fatal(err)
	}
	srv.orphanScope = func(id string) bool { return id == orphanID || id == l.ID }

	tag := "dol-test-" + strings.TrimPrefix(l.ID, "l-")
	labelArgs := func(labels map[string]string) []string {
		var a []string
		for k, v := range labels {
			a = append(a, "--label", k+"="+v)
		}
		return a
	}
	// Cleanup of everything we created, by exact name, in case of failure.
	created := map[string][]string{}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		for _, ref := range created["container"] {
			_ = exec.CommandContext(cctx, "docker", "rm", "-f", ref).Run()
		}
		for _, ref := range created["network"] {
			_ = exec.CommandContext(cctx, "docker", "network", "rm", ref).Run()
		}
		for _, ref := range created["volume"] {
			_ = exec.CommandContext(cctx, "docker", "volume", "rm", ref).Run()
		}
	})
	sleeper := []string{image, "sh", "-c", "trap 'exit 0' TERM; while true; do sleep 1; done"}

	// Resources of the lease.
	net := tag + "-net"
	vol := tag + "-vol"
	app := tag + "-app"
	created["network"] = append(created["network"], net, tag+"-orphan-net")
	created["volume"] = append(created["volume"], vol, tag+"-orphan-vol")
	created["container"] = append(created["container"], app, tag+"-orphan", tag+"-unlabeled")
	docker(append(append([]string{"network", "create"}, labelArgs(l.Labels)...), net)...)
	docker(append(append([]string{"volume", "create"}, labelArgs(l.Labels)...), vol)...)
	docker(append(append([]string{"run", "-d", "--name", app, "--network", net, "-v", vol + ":/data"}, labelArgs(l.Labels)...), sleeper...)...)

	// An orphan: labeled with a lease the node has never seen.
	orphan := v1.LeaseLabels(orphanID, "dol-test", "s-gone", "itest")
	docker(append(append([]string{"network", "create"}, labelArgs(orphan)...), tag+"-orphan-net")...)
	docker(append(append([]string{"volume", "create"}, labelArgs(orphan)...), tag+"-orphan-vol")...)
	docker(append(append([]string{"run", "-d", "--name", tag + "-orphan"}, labelArgs(orphan)...), sleeper...)...)

	// Not ours: no dol labels at all. Must survive everything.
	unlabeledID := docker(append([]string{"run", "-d", "--name", tag + "-unlabeled", "--label", "dol-test-unowned=" + tag}, sleeper...)...)

	inv, err := c.Resources(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inv.Containers, []string{app}) || !slices.Equal(inv.Networks, []string{net}) || !slices.Equal(inv.Volumes, []string{vol}) {
		t.Fatalf("inventory %+v", inv)
	}
	cp, err := c.Capacity(ctx)
	if err != nil || cp.Unlabeled.Containers < 1 {
		t.Fatalf("capacity must count the unlabeled container: %+v %v", cp.Unlabeled, err)
	}

	start := time.Now()
	rel, err := c.ReleaseLease(ctx, l.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("release took %v, removed %+v", time.Since(start).Round(time.Millisecond), rel.Removed)
	if rel.Lease.State != v1.StateReleased || len(rel.Removed.Errors) != 0 ||
		!slices.Equal(rel.Removed.Containers, []string{app}) || !slices.Equal(rel.Removed.Networks, []string{net}) ||
		!slices.Equal(rel.Removed.Volumes, []string{vol}) {
		t.Fatalf("release %+v", rel)
	}
	if out := docker("ps", "-a", "-q", "--filter", v1.LeaseFilter(l.ID)); out != "" {
		t.Fatalf("lease containers left: %s", out)
	}
	if out := docker("volume", "ls", "-q", "--filter", v1.LeaseFilter(l.ID)); out != "" {
		t.Fatalf("lease volumes left: %s", out)
	}

	// The reaper removes the orphan (scoped to this test's lease ids).
	rep := srv.ReapOnce(ctx)
	if !slices.Contains(rep.Orphans, orphanID) {
		t.Fatalf("orphan not reaped: %+v", rep)
	}
	for _, kind := range []string{"ps -a -q", "volume ls -q", "network ls -q"} {
		args := append(strings.Fields(kind), "--filter", v1.LeaseFilter(orphanID))
		if out := docker(args...); out != "" {
			t.Fatalf("orphan %s left: %s", kind, out)
		}
	}
	// The unlabeled container is untouched and still running.
	if state := docker("inspect", "--format", "{{.State.Running}}", unlabeledID); state != "true" {
		t.Fatalf("unlabeled container state %q", state)
	}
	t.Logf("unlabeled container %s survived release and reaper; reaper reported %+v unlabeled", unlabeledID[:12], rep.Unlabeled)
}
