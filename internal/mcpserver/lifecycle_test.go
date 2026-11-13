package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/state"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func (h *harness) putLease(id string, placement v1.Placement, session string, pinned bool) v1.ClientLease {
	h.t.Helper()
	ctxName := "desktop-linux"
	node := ""
	if placement == v1.PlacementRemote {
		ctxName, node = "dol-tower", "tower"
	}
	l := v1.ClientLease{ID: id, Placement: placement, Node: node, DockerContext: ctxName, State: v1.StateActive,
		Owner: "alice@mac", Session: session, Project: "p", Workload: v1.WorkloadContainer, EstMemoryBytes: v1.GiB,
		Pinned: pinned, CreatedAt: time.Now().UTC().Format(time.RFC3339), Labels: v1.LeaseLabels(id, "alice@mac", session, "p")}
	if err := h.store.PutLease(l); err != nil {
		h.t.Fatal(err)
	}
	return l
}

func TestDeadSessionCleanup(t *testing.T) {
	h := newHarness(t, 40)
	ctx := context.Background()

	// A dead session: a real process that has exited.
	p := exec.Command("true")
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	dead := state.Session{ID: "s-deaddeadde", PID: p.Process.Pid, StartTime: "1.000000"}
	if err := h.store.PutSession(dead); err != nil {
		t.Fatal(err)
	}
	// A live foreign session (same process, other id): its leases are kept.
	live, err := state.NewSelfSession("s-livelivell", "alice@mac", "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutSession(live); err != nil {
		t.Fatal(err)
	}

	l1 := h.putLease("l-aaaaaaaaaa", v1.PlacementLocal, dead.ID, false)
	l2 := h.putLease("l-bbbbbbbbbb", v1.PlacementLocal, dead.ID, true)          // pinned: kept
	l3 := h.putLease("l-cccccccccc", v1.PlacementLocal, "s-norecordxx", false) // no session record: dead
	l4 := h.putLease("l-dddddddddd", v1.PlacementLocal, live.ID, false)        // live: kept
	for _, l := range []v1.ClientLease{l1, l2, l3, l4} {
		h.docker.add("desktop-linux", "c-"+l.ID, l.Labels)
	}
	h.docker.add("desktop-linux", "users-own", nil)
	kube := h.store.Path(state.KubeDir, l1.ID+"-x.yaml")
	_ = state.WriteFileAtomic(kube, []byte("x"), 0o600)

	// Remote leases of the dead session: forgotten only if the node says gone.
	gone := h.putLease("l-eeeeeeeeee", v1.PlacementRemote, dead.ID, false)
	nl, err := h.node.Client().CreateLease(ctx, v1.LeaseRequest{Owner: "alice@mac", Session: dead.ID, Workload: v1.WorkloadContainer, EstMemoryBytes: v1.GiB})
	if err != nil {
		t.Fatal(err)
	}
	stillActive := h.putLease(nl.ID, v1.PlacementRemote, dead.ID, false)

	rep := h.core.CleanupDeadSessions(ctx)
	t.Logf("%s", rep)
	if len(rep.DeadSessions) != 1 || rep.DeadSessions[0] != dead.ID {
		t.Fatalf("dead sessions %v", rep.DeadSessions)
	}
	if _, ok := rep.TornDown[l1.ID]; !ok || len(rep.TornDown) != 2 {
		t.Fatalf("torn down %v", rep.TornDown)
	}
	if _, ok := rep.TornDown[l3.ID]; !ok {
		t.Fatalf("lease without session record must be cleaned: %v", rep.TornDown)
	}
	for id, want := range map[string]bool{l1.ID: false, l2.ID: true, l3.ID: false, l4.ID: true} {
		_, exists := h.docker.get("desktop-linux", "c-"+id)
		_, err := h.store.GetLease(id)
		if exists != want || (err == nil) != want {
			t.Errorf("%s: container exists=%v record err=%v, want kept=%v", id, exists, err, want)
		}
	}
	if _, ok := h.docker.get("desktop-linux", "users-own"); !ok {
		t.Fatal("unlabeled container must never be touched")
	}
	if _, err := os.Stat(kube); !os.IsNotExist(err) {
		t.Fatal("kubeconfig of cleaned lease must be deleted")
	}
	if _, err := h.store.GetLease(gone.ID); v1.CodeOf(err) != v1.CodeNotFound {
		t.Fatal("remote record unknown to the node must be forgotten")
	}
	if _, err := h.store.GetLease(stillActive.ID); err != nil {
		t.Fatal("remote record still active on the node must be kept (the reaper owns it)")
	}
	if _, ok := h.store.GetSession(dead.ID); ok {
		t.Fatal("dead session record must be pruned")
	}
}

func TestStartRegistersAndCloseUnregistersSession(t *testing.T) {
	h := newHarness(t, 40)
	r := ok[v1.AcquireResult](h, h.acquire(1))
	if err := h.core.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.store.SessionAlive(h.core.Session()) {
		t.Fatal("session not registered")
	}
	h.core.Close()
	if _, ok := h.store.GetSession(h.core.Session()); ok {
		t.Fatal("session not unregistered on Close")
	}
	// Remote leases survive Close: the node reaper owns them (ADR-0021).
	if l, ok := h.node.Fake.Lease(r.LeaseID); !ok || l.State != v1.StateActive {
		t.Fatalf("remote lease must not be released on exit: %+v", l)
	}
}
