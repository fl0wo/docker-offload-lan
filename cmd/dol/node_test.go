package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func okProbes() initProbes {
	return initProbes{
		dockerInfo: func(context.Context) (*dockerx.Info, error) {
			return &dockerx.Info{ServerVersion: "28.4.0", Architecture: "x86_64", DockerRootDir: "/var/lib/docker"}, nil
		},
		kindPath:   func(string) (string, error) { return "/usr/local/bin/kind", nil },
		chownGroup: func(string) error { return nil },
	}
}

func initFlagsFor(dir string) nodeInitFlags {
	d := config.DefaultNode()
	return nodeInitFlags{
		path: filepath.Join(dir, "etc", "node.yaml"), name: "tower", socket: filepath.Join(dir, "run", "node.sock"),
		dataDir: filepath.Join(dir, "data"), syncRoot: filepath.Join(dir, "srv"), provider: "auto",
		reserveGB: 4, minDiskGB: 30, accessMode: "tunnel", kindBinary: d.Kind.Binary,
	}
}

func noneChanged(string) bool { return false }

func TestNodeInitDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	f := initFlagsFor(dir)
	f.dryRun = true
	var out bytes.Buffer
	if err := runNodeInit(context.Background(), f, noneChanged, &out, okProbes()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would write") || !strings.Contains(out.String(), "name: tower") {
		t.Fatal(out.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("dry run created %v", entries)
	}
}

func TestNodeInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	f := initFlagsFor(dir)
	var out bytes.Buffer
	if err := runNodeInit(context.Background(), f, noneChanged, &out, okProbes()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadNode(f.path)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if cfg.Name != "tower" || cfg.DataDir != f.dataDir || cfg.Leases.HeartbeatGraceS != 600 {
		t.Fatalf("config %+v", cfg)
	}
	for _, d := range []string{f.dataDir, f.syncRoot} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("%s not created", d)
		}
	}
	first, _ := os.ReadFile(f.path)
	st1, _ := os.Stat(f.path)

	// Second run: nothing changes, flags are reported as ignored.
	out.Reset()
	f.name = "other"
	changed := func(n string) bool { return n == "name" }
	if err := runNodeInit(context.Background(), f, changed, &out, okProbes()); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(f.path)
	st2, _ := os.Stat(f.path)
	if !bytes.Equal(first, second) || !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("second run modified the config")
	}
	if !strings.Contains(out.String(), "kept unchanged") || !strings.Contains(out.String(), "--name ignored") {
		t.Fatal(out.String())
	}

	// --force rewrites with the flags.
	f.force = true
	if err := runNodeInit(context.Background(), f, changed, io.Discard, okProbes()); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.LoadNode(f.path); cfg.Name != "other" {
		t.Fatal("force did not apply flags")
	}
}

func TestNodeInitReportsDockerAndKind(t *testing.T) {
	dir := t.TempDir()
	p := okProbes()
	p.dockerInfo = func(context.Context) (*dockerx.Info, error) {
		return nil, errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	}
	p.kindPath = func(string) (string, error) { return "", errors.New("not on PATH") }
	var out bytes.Buffer
	err := runNodeInit(context.Background(), initFlagsFor(dir), noneChanged, &out, p)
	if err == nil || !strings.Contains(err.Error(), "Docker is not reachable") || !strings.Contains(err.Error(), "systemctl start docker") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(out.String(), "kind: not found") {
		t.Fatal(out.String())
	}
	// The config was still written so the operator only has to fix Docker.
	if _, err := config.LoadNode(initFlagsFor(dir).path); err != nil {
		t.Fatal(err)
	}
	// An invalid existing config is reported, not overwritten.
	_ = os.WriteFile(initFlagsFor(dir).path, []byte("version: 1\nbogus: true\n"), 0o600)
	err = runNodeInit(context.Background(), initFlagsFor(dir), noneChanged, io.Discard, okProbes())
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err %v", err)
	}
}

// noDocker is a docker runner that fails like a missing daemon. serve tests
// must never reach the developer's real Docker: the reaper would act on it.
type noDocker struct{}

func (noDocker) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	return nil, &dockerx.CmdError{Args: args, Err: errors.New("exit 1"), Stderr: "Cannot connect to the Docker daemon (test)"}
}

type noKind struct{}

func (noKind) Run(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("kind disabled in test")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNodeServeStartsServesAndStops(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolserve")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "run", "n.sock")
	cfgPath := filepath.Join(dir, "node.yaml")
	cfg := fmt.Sprintf(`version: 1
name: ci
socket: %s
data_dir: %s
sync_root: %s
capacity:
  provider: fake
  fake: {total_memory_gb: 16, available_memory_gb: 16, free_disk_gb: 100}
`, sock, filepath.Join(dir, "data"), filepath.Join(dir, "sync"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- runNodeServe(ctx, cfgPath, logs, serveDeps{docker: &dockerx.Docker{R: noDocker{}}, kind: noKind{}})
	}()

	c := nodeclient.NewUnix("ci", sock)
	defer c.Close()
	var h *v1.Health
	for i := 0; i < 100; i++ {
		if h, err = c.Health(ctx); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("agent never came up: %v\nlogs:\n%s", err, logs.String())
	}
	if h.Node != "ci" || h.Status != "degraded" { // docker is "down" in this test
		t.Fatalf("health %+v", h)
	}
	l, err := c.CreateLease(ctx, v1.LeaseRequest{Owner: "o", Session: "s", Workload: v1.WorkloadBuild, EstMemoryBytes: v1.GiB})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	cancel() // SIGTERM in production (main uses signal.NotifyContext)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket not removed on shutdown")
	}
	// Every log line is JSON; the lease creation was logged.
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	sawLease := false
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("non-JSON log line %q", sc.Text())
		}
		if m["msg"] == "lease created" && m["lease"] == l.ID {
			sawLease = true
		}
	}
	if !sawLease {
		t.Fatalf("lease creation not logged:\n%s", logs.String())
	}
}

func TestDialStdioCommandMissingSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"node", "dial-stdio", "--socket", sock})
	cmd.SetIn(strings.NewReader(""))
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot connect to node agent") || !strings.Contains(err.Error(), sock) {
		t.Fatalf("err %v", err)
	}
}
