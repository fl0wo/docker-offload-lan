package node

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func wantCode(t *testing.T, err error, code v1.ErrorCode) *v1.Error {
	t.Helper()
	e, ok := v1.AsError(err)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if e.Message == "" || e.Hint == "" {
		t.Fatalf("errors need a message and a hint: %+v", e)
	}
	return e
}

func TestLeaseLifecycleEndToEnd(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	c := n.client

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Node != "test" || h.Provider != "fake" || !h.Accepting || h.APIVersion != "v1" || h.Time.IsZero() {
		t.Fatalf("health %+v", h)
	}

	l := n.lease(t, 10)
	if !v1.ValidLeaseID(l.ID) || l.State != v1.StateActive || l.Owner != "alice@mac" || l.EstMemoryBytes != 10*G {
		t.Fatalf("lease %+v", l)
	}
	if want := v1.LeaseLabels(l.ID, "alice@mac", "s-aaaaaaaaaa", "demo"); len(l.Labels) != 5 || l.Labels[v1.LabelLease] != l.ID || l.Labels[v1.LabelManaged] != "true" || l.Labels[v1.LabelProject] != want[v1.LabelProject] {
		t.Fatalf("labels %v", l.Labels)
	}
	if !l.ExpiresAt.Equal(n.clock.Now().Add(600 * time.Second)) {
		t.Fatalf("expires_at %v", l.ExpiresAt)
	}
	t.Logf("ssh_user recorded from peer credentials: %q", l.SSHUser)
	// ssh_user comes from the peer credentials of the socket connection.
	if me, err := user.Current(); err == nil && l.SSHUser != me.Username && l.SSHUser != "uid:"+me.Uid {
		t.Fatalf("ssh_user %q, want %q", l.SSHUser, me.Username)
	}

	cp, err := c.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Memory.CommittedBytes != 10*G || cp.Memory.SchedulableFreeBytes != 18*G || cp.LeasesActive != 1 || !cp.Accepting ||
		cp.Memory.ReservedBytes != 4*G || cp.Disk.MinFreeBytes != 30*G {
		t.Fatalf("capacity %+v", cp)
	}

	n.clock.Advance(5 * time.Minute)
	hb, err := c.Heartbeat(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hb.ExpiresAt.Equal(n.clock.Now().Add(600 * time.Second)) {
		t.Fatalf("heartbeat expires_at %v", hb.ExpiresAt)
	}

	// Resize: growing beyond capacity is refused with numbers, shrinking and
	// growing within capacity succeed.
	big := 30 * G
	_, err = c.PatchLease(ctx, l.ID, v1.LeasePatch{EstMemoryBytes: &big})
	e := wantCode(t, err, v1.CodeInsufficientMemory)
	if !strings.Contains(e.Message, "28.0 GiB schedulable, 30.0 GiB requested") {
		t.Fatalf("resize must exclude the lease's own reservation: %s", e.Message)
	}
	fit := 28 * G
	pl, err := c.PatchLease(ctx, l.ID, v1.LeasePatch{EstMemoryBytes: &fit})
	if err != nil || pl.EstMemoryBytes != fit {
		t.Fatal(pl, err)
	}
	// Pin with a 2h explicit expiry, then unpin.
	yes, ttl := true, int64(7200)
	pl, err = c.PatchLease(ctx, l.ID, v1.LeasePatch{Pinned: &yes, TTLSeconds: &ttl})
	if err != nil || !pl.Pinned || !pl.ExpiresAt.Equal(n.clock.Now().Add(2*time.Hour)) {
		t.Fatal(pl, err)
	}
	no := false
	pl, err = c.PatchLease(ctx, l.ID, v1.LeasePatch{Pinned: &no})
	if err != nil || pl.Pinned || !pl.ExpiresAt.Equal(n.clock.Now().Add(600*time.Second)) {
		t.Fatal(pl, err)
	}

	// Track compose + sync; create labeled docker resources plus a stranger.
	if err := c.RegisterCompose(ctx, l.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterCompose(ctx, l.ID, "Bad Name"); v1.CodeOf(err) != v1.CodeBadRequest {
		t.Fatal(err)
	}
	syncDir := filepath.Join(n.cfg.SyncRoot, l.ID, "demo")
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterSyncDir(ctx, l.ID, syncDir); err != nil {
		t.Fatal(err)
	}
	err = c.RegisterSyncDir(ctx, l.ID, filepath.Join(n.cfg.SyncRoot, "l-bbbbbbbbbb"))
	wantCode(t, err, v1.CodeBadRequest)
	n.docker.add(dockerx.KindContainer, "app", l.Labels)
	n.docker.add(dockerx.KindNetwork, "appnet", l.Labels)
	n.docker.add(dockerx.KindVolume, "appdata", l.Labels)
	n.docker.add(dockerx.KindContainer, "users-own-postgres", map[string]string{"com.example": "x"})

	inv, err := c.Resources(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(inv.Containers, ",") != "app" || len(inv.Networks) != 1 || len(inv.Volumes) != 1 ||
		inv.ComposeProjects[0] != "web" || inv.SyncDirs[0] != syncDir {
		t.Fatalf("inventory %+v", inv)
	}
	got, err := c.GetLease(ctx, l.ID)
	if err != nil || len(got.Resources.ComposeProjects) != 1 || len(got.Resources.SyncDirs) != 1 {
		t.Fatal(got, err)
	}
	ls, err := c.ListLeases(ctx, "alice@mac", v1.StateActive)
	if err != nil || len(ls) != 1 {
		t.Fatal(ls, err)
	}

	rel, err := c.ReleaseLease(ctx, l.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Lease.State != v1.StateReleased || len(rel.Removed.Errors) != 0 ||
		strings.Join(rel.Removed.Containers, ",") != "app" || len(rel.Removed.Networks) != 1 || len(rel.Removed.Volumes) != 1 ||
		len(rel.Removed.ComposeProjects) != 1 || len(rel.Removed.SyncDirs) != 1 {
		t.Fatalf("release %+v", rel)
	}
	if got := n.docker.names(dockerx.KindContainer); strings.Join(got, ",") != "users-own-postgres" {
		t.Fatalf("containers left: %v", got)
	}
	if len(n.docker.callsWith("compose -p web down --volumes --remove-orphans")) != 1 {
		t.Fatal("compose project not taken down")
	}
	if _, err := os.Stat(syncDir); !os.IsNotExist(err) {
		t.Fatal("sync dir not removed")
	}
	inv, err = c.Resources(ctx, l.ID)
	if err != nil || !inv.Empty() {
		t.Fatalf("resources after release: %+v %v", inv, err)
	}
	// Released lease: heartbeat says gone, release is idempotent.
	_, err = c.Heartbeat(ctx, l.ID)
	wantCode(t, err, v1.CodeLeaseGone)
	if _, err := c.ReleaseLease(ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	cp, _ = c.Capacity(ctx)
	if cp.Memory.CommittedBytes != 0 || cp.LeasesActive != 0 {
		t.Fatalf("capacity after release %+v", cp.Memory)
	}
}

func TestErrorsAndRoutes(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	c := n.client

	_, err := c.Heartbeat(ctx, "l-zzzzzzzzzz")
	wantCode(t, err, v1.CodeNotFound)
	_, err = c.GetLease(ctx, "not-a-lease")
	wantCode(t, err, v1.CodeNotFound)
	_, err = c.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: "vm", EstMemoryBytes: 1})
	wantCode(t, err, v1.CodeBadRequest)
	_, err = c.ListLeases(ctx, "", "bogus")
	wantCode(t, err, v1.CodeBadRequest)

	// Raw HTTP: envelope shape, unknown routes, malformed JSON.
	for _, tc := range []struct {
		method, path, body string
		status             int
		code               v1.ErrorCode
	}{
		{"GET", "/v1/nope", "", 404, v1.CodeNotFound},
		{"PUT", "/v1/healthz", "", 404, v1.CodeNotFound},
		{"POST", "/v1/leases", "{not json", 400, v1.CodeBadRequest},
		{"GET", "/v1/leases/l-zzzzzzzzzz/resources", "", 404, v1.CodeNotFound},
	} {
		status, body := n.rawRequest(t, tc.method, tc.path, tc.body)
		var er v1.ErrorResponse
		if err := json.Unmarshal(body, &er); err != nil || status != tc.status || er.Error == nil || er.Error.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, status, body)
		}
	}

	// Expired lease → heartbeat answers 410 even before the reaper ran.
	l := n.lease(t, 1)
	n.clock.Advance(601 * time.Second)
	_, err = c.Heartbeat(ctx, l.ID)
	wantCode(t, err, v1.CodeLeaseGone)
	got, _ := c.GetLease(ctx, l.ID)
	if got.State != v1.StateExpired {
		t.Fatalf("state %s", got.State)
	}
	err = c.RegisterCompose(ctx, l.ID, "web")
	wantCode(t, err, v1.CodeLeaseGone)
}

func TestDrainAndRefusalsOverHTTP(t *testing.T) {
	n := newTestNode(t, func(c *config.NodeConfig) {})
	ctx := context.Background()
	c := n.client
	_, err := c.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: 100 * G})
	e := wantCode(t, err, v1.CodeInsufficientMemory)
	if !strings.Contains(e.Message, "28.0 GiB schedulable, 100.0 GiB requested") || e.Details["requested_bytes"].(float64) != float64(100*G) {
		t.Fatalf("refusal %+v", e)
	}
	n.prov.Update(func(s *capacity.Snapshot) { s.DiskFree = 10 * G })
	_, err = c.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: G})
	wantCode(t, err, v1.CodeInsufficientDisk)
	cp, _ := c.Capacity(ctx)
	if cp.Accepting || !strings.HasPrefix(cp.Reason, "insufficient_disk") {
		t.Fatalf("capacity %+v", cp)
	}
	n.prov.Update(func(s *capacity.Snapshot) { s.DiskFree = 200 * G })

	if err := c.Drain(ctx, true); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: G})
	wantCode(t, err, v1.CodeDraining)
	h, _ := c.Health(ctx)
	if h.Accepting {
		t.Fatal("healthz must report not accepting while draining")
	}

	// Drain survives a restart.
	n.stop()
	n2 := startNode(t, n.cfg, n)
	if !n2.srv.Draining() {
		t.Fatal("drain flag lost on restart")
	}
	if err := n2.client.Drain(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := n2.client.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: G}); err != nil {
		t.Fatal(err)
	}

	// Provider failure → 503 unavailable, not a refusal.
	n2.prov.SetErr(os.ErrPermission)
	_, err = n2.client.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: G})
	wantCode(t, err, v1.CodeUnavailable)
}

func TestMaxLeases(t *testing.T) {
	n := newTestNode(t, func(c *config.NodeConfig) { c.Leases.MaxActive = 2 })
	n.lease(t, 1)
	n.lease(t, 1)
	_, err := n.client.CreateLease(context.Background(), v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadKind, EstMemoryBytes: G})
	e := wantCode(t, err, v1.CodeMaxLeases)
	if !strings.Contains(e.Message, "2 active leases, the maximum is 2") {
		t.Fatal(e.Message)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	n := newTestNode(t)
	l := n.lease(t, 3)
	n.stop()
	n2 := startNode(t, n.cfg, n)
	got, err := n2.client.GetLease(context.Background(), l.ID)
	if err != nil || got.State != v1.StateActive || got.EstMemoryBytes != 3*G || !got.CreatedAt.Equal(l.CreatedAt) {
		t.Fatal(got, err)
	}
	cp, _ := n2.client.Capacity(context.Background())
	if cp.Memory.CommittedBytes != 3*G {
		t.Fatalf("committed after restart %d", cp.Memory.CommittedBytes)
	}
}

func TestPinnedAtCreate(t *testing.T) {
	n := newTestNode(t)
	l := n.lease(t, 1, func(r *v1.LeaseRequest) { r.Pinned = true })
	if !l.ExpiresAt.IsZero() {
		t.Fatalf("pinned without ttl must have no expiry: %v", l.ExpiresAt)
	}
	l2 := n.lease(t, 1, func(r *v1.LeaseRequest) { r.Pinned, r.TTLSeconds = true, 3600 })
	if !l2.ExpiresAt.Equal(n.clock.Now().Add(time.Hour)) {
		t.Fatal(l2.ExpiresAt)
	}
	l3 := n.lease(t, 1, func(r *v1.LeaseRequest) { r.TTLSeconds = 60 })
	if !l3.ExpiresAt.Equal(n.clock.Now().Add(time.Minute)) {
		t.Fatalf("unpinned ttl shorter than grace: %v", l3.ExpiresAt)
	}
}

// The client reaches the agent through `dol node dial-stdio` over ssh; this
// test replaces ssh with an in-memory pipe into DialStdio.
func TestNodeClientThroughDialStdio(t *testing.T) {
	n := newTestNode(t)
	c := nodeclient.New("test", func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			_ = DialStdio(context.Background(), n.socket, b, b)
			_ = b.Close()
		}()
		return a, nil
	})
	defer c.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Health(ctx); err != nil {
			t.Fatal(err)
		}
	}
	l, err := c.CreateLease(ctx, v1.LeaseRequest{Owner: "a", Session: "s", Workload: v1.WorkloadBuild, EstMemoryBytes: G})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReleaseLease(ctx, l.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestDialStdioMissingSocket(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.sock")
	err := DialStdio(context.Background(), p, strings.NewReader(""), new(strings.Builder))
	if err == nil || !strings.Contains(err.Error(), "cannot connect to node agent") || !strings.Contains(err.Error(), p) {
		t.Fatalf("got %v", err)
	}
}

func TestDialStdioHalfClose(t *testing.T) {
	n := newTestNode(t)
	// The request is followed by EOF on stdin: the bridge half-closes and
	// still relays the full response before exiting.
	in := strings.NewReader("GET /v1/healthz HTTP/1.1\r\nHost: node\r\n\r\n")
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- DialStdio(context.Background(), n.socket, in, &out) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dial-stdio did not exit after the agent closed the connection")
	}
	if !strings.HasPrefix(out.String(), "HTTP/1.1 200 OK") || !strings.Contains(out.String(), `"api_version":"v1"`) {
		t.Fatalf("output %q", out.String())
	}
}

func TestListenUnixStaleAndBusy(t *testing.T) {
	dir := sockDir(t)
	p := filepath.Join(dir, "sub", "n.sock")
	ln, err := ListenUnix(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o660 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := ListenUnix(p, nil); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("second agent: %v", err)
	}
	// Simulate a crash: the socket file stays but nobody listens.
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	_ = ln.Close()
	ln2, err := ListenUnix(p, nil)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = ln2.Close()
	// A regular file is never removed.
	f := filepath.Join(dir, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if _, err := ListenUnix(f, nil); err == nil {
		t.Fatal("regular file replaced")
	}
}
