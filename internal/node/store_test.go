package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func sampleLease(id string, state v1.LeaseState, mem int64) *v1.Lease {
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 123456789, time.UTC)
	return &v1.Lease{
		ID: id,
		LeaseRequest: v1.LeaseRequest{Owner: "alice@mac", Session: "s-aaaaaaaaaa", Project: "p", Workload: v1.WorkloadKind,
			EstMemoryBytes: mem, EstDiskBytes: 5, TTLSeconds: 60, Pinned: true, ClientVersion: "0.1.0"},
		State: state, CreatedAt: t0, LastHeartbeatAt: t0.Add(time.Second), ExpiresAt: t0.Add(time.Hour),
		SSHUser: "dol", Labels: v1.LeaseLabels(id, "alice@mac", "s-aaaaaaaaaa", "p"),
		Resources: v1.LeaseResources{
			KindClusters:    []v1.KindCluster{{Name: "e2e", ClusterName: "dol-" + id + "-e2e", APIServerPort: 16443, AccessMode: "tunnel"}},
			ComposeProjects: []string{"web"}, SyncDirs: []string{"/srv/dol/" + id + "/p"},
		},
		LastError: "boom",
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	a := sampleLease("l-aaaaaaaaaa", v1.StateActive, 3*G)
	b := sampleLease("l-bbbbbbbbbb", v1.StateReleased, 5*G)
	b.CreatedAt = b.CreatedAt.Add(time.Minute)
	for _, l := range []*v1.Lease{a, b} {
		if err := s.Insert(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddPort(ctx, 16443, a.ID, "dol-l-aaaaaaaaaa-e2e"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, metaDraining, "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(ctx); v != len(migrations) {
		t.Fatalf("schema version %d", v)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, a)
	}
	if _, err := s.Get(ctx, "l-cccccccccc"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	all, _ := s.List(ctx, "")
	act, _ := s.List(ctx, "alice@mac", v1.StateActive)
	none, _ := s.List(ctx, "bob")
	if len(all) != 2 || all[0].ID != a.ID || len(act) != 1 || len(none) != 0 {
		t.Fatalf("list: %d %d %d", len(all), len(act), len(none))
	}
	if c, n, _ := s.Committed(ctx, ""); c != 3*G || n != 1 {
		t.Fatalf("committed %d %d", c, n)
	}
	if c, n, _ := s.Committed(ctx, a.ID); c != 0 || n != 0 {
		t.Fatalf("committed excluding %d %d", c, n)
	}
	if used, _ := s.UsedPorts(ctx); !used[16443] || len(used) != 1 {
		t.Fatalf("ports %v", used)
	}
	if err := s.AddPort(ctx, 16443, b.ID, "other"); err == nil {
		t.Fatal("double allocation must fail")
	}
	if v, _ := s.GetMeta(ctx, metaDraining); v != "true" {
		t.Fatal("meta lost")
	}
	got.State = v1.StateReleased
	if err := s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, sampleLease("l-zzzzzzzzzz", v1.StateActive, 1)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.db.ExecContext(ctx, "PRAGMA user_version = 99")
	_ = s.Close()
	if _, err := OpenStore(ctx, dir); err == nil || !strings.Contains(err.Error(), "upgrade dol") {
		t.Fatalf("want upgrade hint, got %v", err)
	}
}

func TestSyncPathValidation(t *testing.T) {
	root := "/srv/dol"
	id := "l-aaaaaaaaaa"
	ok := map[string]string{
		"/srv/dol/l-aaaaaaaaaa":            "/srv/dol/l-aaaaaaaaaa",
		"/srv/dol/l-aaaaaaaaaa/proj":       "/srv/dol/l-aaaaaaaaaa/proj",
		"/srv/dol/l-aaaaaaaaaa//proj/./x/": "/srv/dol/l-aaaaaaaaaa/proj/x",
	}
	for in, want := range ok {
		got, err := ValidateSyncPath(root, id, in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v", in, got, err)
		}
	}
	for _, bad := range []string{
		"", "relative/path", "/srv/dol", "/srv/dol/l-bbbbbbbbbb/proj", "/srv/dol/l-aaaaaaaaaa/../l-bbbbbbbbbb",
		"/srv/dol/l-aaaaaaaaaa/proj/../../..", "/srv/dol/l-aaaaaaaaaab", "/etc", "/srv/dol/l-aaaaaaaaaa/..",
	} {
		if _, err := ValidateSyncPath(root, id, bad); v1.CodeOf(err) != v1.CodeBadRequest {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := ValidateSyncPath(root, "../etc", "/srv/dol/../etc"); err == nil {
		t.Fatal("invalid lease id accepted")
	}
}

func TestSyncDirSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "l-aaaaaaaaaa"
	// <root>/<lease> is a symlink pointing outside the sync root.
	if err := os.Symlink(outside, filepath.Join(root, id)); err != nil {
		t.Fatal(err)
	}
	if err := CheckNoSymlinks(root, filepath.Join(root, id, "proj")); v1.CodeOf(err) != v1.CodeBadRequest {
		t.Fatalf("symlinked lease dir accepted: %v", err)
	}
	if err := RemoveSyncDir(root, id, filepath.Join(root, id)); err == nil {
		t.Fatal("RemoveSyncDir followed a symlink")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside the sync root was deleted")
	}

	// A nested symlink deeper in the tree is refused too.
	id2 := "l-bbbbbbbbbb"
	if err := os.MkdirAll(filepath.Join(root, id2), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, id2, "proj")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSyncDir(root, id2, filepath.Join(root, id2, "proj", "sub")); err == nil {
		t.Fatal("nested symlink followed")
	}

	// A real directory is removed, and the empty lease dir with it.
	id3 := "l-cccccccccc"
	p := filepath.Join(root, id3, "proj")
	if err := os.MkdirAll(filepath.Join(p, "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSyncDir(root, id3, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id3)); !os.IsNotExist(err) {
		t.Fatal("empty lease dir left behind")
	}
	// Idempotent.
	if err := RemoveSyncDir(root, id3, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("victim deleted")
	}
}
