package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// ---- fake clock -------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---- fake docker CLI ----------------------------------------------------------

type fobj struct {
	ID, Name, State string
	Labels          map[string]string
}

// row is what --format templates see.
type row struct {
	ID, Names, Name, Image, State, Status, Ports string
	labels                                       map[string]string
}

func (r row) Label(k string) string { return r.labels[k] }

// fakeDocker simulates the subset of the docker CLI used by dockerx and the
// node: ps / network ls / volume ls with label filters and Go-template
// formats, inspect, stop, rm, compose down and prune.
type fakeDocker struct {
	mu      sync.Mutex
	objs    map[dockerx.ObjectKind][]*fobj
	calls   []string
	fail    map[string]string // args prefix -> stderr of an injected failure
	counter int
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{objs: map[dockerx.ObjectKind][]*fobj{}, fail: map[string]string{}}
}

func (f *fakeDocker) add(kind dockerx.ObjectKind, name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.objs[kind] = append(f.objs[kind], &fobj{ID: fmt.Sprintf("id%04d", f.counter), Name: name, State: "running", Labels: labels})
}

func (f *fakeDocker) names(kind dockerx.ObjectKind) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, o := range f.objs[kind] {
		out = append(out, o.Name)
	}
	sort.Strings(out)
	return out
}

func (f *fakeDocker) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func matches(o *fobj, filters []string) bool {
	for _, fl := range filters {
		kv, ok := strings.CutPrefix(fl, "label=")
		if !ok {
			continue
		}
		k, v, hasV := strings.Cut(kv, "=")
		got, present := o.Labels[k]
		if !present || (hasV && got != v) {
			return false
		}
	}
	return true
}

func (f *fakeDocker) find(kind dockerx.ObjectKind, ref string) (int, *fobj) {
	for i, o := range f.objs[kind] {
		if o.ID == ref || o.Name == ref {
			return i, o
		}
	}
	return -1, nil
}

func notFound(args []string, kind dockerx.ObjectKind, ref string) error {
	return &dockerx.CmdError{Args: args, Err: errors.New("exit status 1"), Stderr: fmt.Sprintf("Error: No such %s: %s", kind, ref)}
}

func (f *fakeDocker) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for p, stderr := range f.fail {
		if strings.HasPrefix(joined, p) {
			return nil, &dockerx.CmdError{Args: args, Err: errors.New("exit status 1"), Stderr: stderr}
		}
	}
	var filters []string
	var format, ref string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--filter":
			filters = append(filters, args[i+1])
			i++
		case "--format":
			format = args[i+1]
			i++
		case "--":
			if i+1 < len(args) {
				ref = args[i+1]
			}
			i = len(args)
		default:
			pos = append(pos, args[i])
		}
	}
	listKind := map[string]dockerx.ObjectKind{"ps": dockerx.KindContainer, "network ls": dockerx.KindNetwork, "volume ls": dockerx.KindVolume}
	for prefix, kind := range listKind {
		if joined == prefix || strings.HasPrefix(joined, prefix+" ") {
			return f.list(kind, filters, format)
		}
	}
	switch {
	case strings.HasPrefix(joined, "info"):
		return []byte(`{"ServerVersion":"28.4.0","OSType":"linux","Architecture":"x86_64","DockerRootDir":"/var/lib/docker"}`), nil
	case len(pos) >= 2 && pos[1] == "inspect":
		kind := dockerx.ObjectKind(pos[0])
		_, o := f.find(kind, ref)
		if o == nil {
			return nil, notFound(args, kind, ref)
		}
		b, _ := json.Marshal(o.Labels)
		return b, nil
	case pos[0] == "stop":
		_, o := f.find(dockerx.KindContainer, ref)
		if o == nil {
			return nil, notFound(args, dockerx.KindContainer, ref)
		}
		o.State = "exited"
		return nil, nil
	case pos[0] == "rm":
		return nil, f.rm(args, dockerx.KindContainer, ref)
	case len(pos) >= 2 && pos[1] == "rm":
		return nil, f.rm(args, dockerx.ObjectKind(pos[0]), ref)
	case pos[0] == "compose", pos[0] == "builder", pos[0] == "image":
		return nil, nil
	}
	return nil, fmt.Errorf("fakeDocker: unsupported command %q", joined)
}

func (f *fakeDocker) rm(args []string, kind dockerx.ObjectKind, ref string) error {
	i, o := f.find(kind, ref)
	if o == nil {
		return notFound(args, kind, ref)
	}
	f.objs[kind] = append(f.objs[kind][:i], f.objs[kind][i+1:]...)
	return nil
}

func (f *fakeDocker) list(kind dockerx.ObjectKind, filters []string, format string) ([]byte, error) {
	var buf bytes.Buffer
	var tmpl *template.Template
	if format != "{{json .}}" {
		var err error
		if tmpl, err = template.New("f").Parse(format); err != nil {
			return nil, err
		}
	}
	for _, o := range f.objs[kind] {
		if !matches(o, filters) {
			continue
		}
		r := row{ID: o.ID, Names: o.Name, Name: o.Name, Image: "busybox", State: o.State, labels: o.Labels}
		if tmpl == nil {
			var v any = map[string]string{"ID": r.ID, "Names": r.Names, "Image": r.Image, "State": r.State}
			if kind != dockerx.KindContainer {
				v = map[string]string{"ID": r.ID, "Name": r.Name}
			}
			b, _ := json.Marshal(v)
			buf.Write(b)
		} else if err := tmpl.Execute(&buf, r); err != nil {
			return nil, err
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ---- fake kind -------------------------------------------------------------

type fakeKind struct {
	mu       sync.Mutex
	clusters map[string]bool
	calls    []string
	fail     map[string]error // subcommand ("create", "delete", "load") -> error
	configs  map[string]string
}

func newFakeKind() *fakeKind {
	return &fakeKind{clusters: map[string]bool{}, fail: map[string]error{}, configs: map[string]string{}}
}

func flagValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (k *fakeKind) Run(_ context.Context, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, strings.Join(args, " "))
	if err := k.fail[args[0]]; err != nil {
		return nil, err
	}
	name := flagValue(args, "--name")
	switch args[0] {
	case "create":
		cfg, err := os.ReadFile(flagValue(args, "--config"))
		if err != nil {
			return nil, err
		}
		k.configs[name] = string(cfg)
		k.clusters[name] = true
		kc := "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://127.0.0.1:1\n  name: kind-" + name + "\n"
		return nil, os.WriteFile(flagValue(args, "--kubeconfig"), []byte(kc), 0o600)
	case "delete":
		delete(k.clusters, name)
	case "load":
		if !k.clusters[name] {
			return nil, fmt.Errorf("unknown cluster %q", name)
		}
	case "get":
		var out []string
		for c := range k.clusters {
			out = append(out, c)
		}
		sort.Strings(out)
		return []byte(strings.Join(out, "\n") + "\n"), nil
	}
	return nil, nil
}

func (k *fakeKind) has(c string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.clusters[c]
}

// ---- test node -------------------------------------------------------------

type testNode struct {
	t      *testing.T
	cfg    *config.NodeConfig
	srv    *Server
	store  *Store
	prov   *capacity.Fake
	docker *fakeDocker
	kind   *fakeKind
	clock  *fakeClock
	socket string
	client *nodeclient.Client
	cancel context.CancelFunc
	done   chan error
}

func testConfig(dir string) *config.NodeConfig {
	c := config.DefaultNode()
	c.Name = "test"
	c.DataDir = filepath.Join(dir, "data")
	c.SyncRoot = filepath.Join(dir, "sync")
	c.Socket = filepath.Join(dir, "n.sock")
	c.Capacity.Provider = "fake"
	c.Capacity.ReserveMemoryGB = 4
	c.Capacity.MinFreeDiskGB = 30
	c.Capacity.Fake = &config.FakeCapacity{TotalMemoryGB: 32, AvailableMemoryGB: 32, FreeDiskGB: 200}
	c.Leases = config.LeasesConfig{HeartbeatGraceS: 600, MaxTTLS: 86400, MaxActive: 32}
	c.Kind.PortRange = [2]int{20000, 20009}
	return c
}

// sockDir returns a short temp dir: unix socket paths are limited to ~104
// bytes on macOS.
func sockDir(t *testing.T) string {
	d := t.TempDir()
	if len(d) > 80 {
		var err error
		if d, err = os.MkdirTemp("", "dol"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
	}
	return d
}

func newTestNode(t *testing.T, mutate ...func(*config.NodeConfig)) *testNode {
	t.Helper()
	dir := sockDir(t)
	cfg := testConfig(dir)
	for _, m := range mutate {
		m(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return startNode(t, cfg, nil)
}

// startNode starts a server; store may be reused across restarts.
func startNode(t *testing.T, cfg *config.NodeConfig, prev *testNode) *testNode {
	t.Helper()
	ctx := context.Background()
	st, err := OpenStore(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	n := &testNode{t: t, cfg: cfg, store: st, prov: capacity.FakeFromConfig(cfg.Capacity.Fake),
		docker: newFakeDocker(), kind: newFakeKind(), clock: newClock(), socket: cfg.Socket}
	if prev != nil {
		n.docker, n.kind, n.clock, n.prov = prev.docker, prev.kind, prev.clock, prev.prov
	}
	n.srv, err = New(ctx, Options{
		Config: cfg, Store: st, Provider: n.prov, Docker: &dockerx.Docker{R: n.docker}, Kind: n.kind,
		Clock: n.clock, PortFree: func(int) bool { return true },
		LANAddrs: func() []string { return []string{"192.168.1.50", "tower"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ListenUnix(cfg.Socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	n.cancel, n.done = cancel, make(chan error, 1)
	go func() { n.done <- n.srv.Serve(sctx, ln) }()
	n.client = nodeclient.NewUnix(cfg.Name, cfg.Socket)
	t.Cleanup(n.stop)
	return n
}

func (n *testNode) stop() {
	if n.cancel == nil {
		return
	}
	n.client.Close()
	n.cancel()
	if err := <-n.done; err != nil {
		n.t.Errorf("serve: %v", err)
	}
	_ = n.store.Close()
	n.cancel = nil
}

func (n *testNode) lease(t *testing.T, memGiB float64, mutate ...func(*v1.LeaseRequest)) *v1.Lease {
	t.Helper()
	req := v1.LeaseRequest{Owner: "alice@mac", Session: "s-aaaaaaaaaa", Project: "demo", Workload: v1.WorkloadCompose,
		EstMemoryBytes: v1.GBToBytes(memGiB), ClientVersion: "test"}
	for _, m := range mutate {
		m(&req)
	}
	l, err := n.client.CreateLease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// rawRequest performs a request on the socket and returns status and body.
func (n *testNode) rawRequest(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", n.socket)
	}}}
	defer hc.CloseIdleConnections()
	req, _ := http.NewRequest(method, "http://node"+path, strings.NewReader(body))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
