// Package nodefake is a small in-memory implementation of the node API
// (pkg/api/v1) for client tests. It is NOT the real node agent: admission is
// simply "est_memory <= free", there is no Docker and no reaper. Failure
// modes (hang, HTTP 500, lease gone) are switchable at runtime.
package nodefake

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Mode selects how every request is answered.
type Mode int

const (
	ModeOK       Mode = iota // normal behavior
	ModeHang                 // block until the client gives up or Unhang
	ModeError500             // answer every request with 500 internal
)

// Fake is an in-memory node. Zero value is not usable; call New.
type Fake struct {
	mu        sync.Mutex
	name      string
	arch      string
	freeBytes int64 // schedulable memory before committed leases
	maxActive int
	draining  bool
	mode      Mode
	unhang    chan struct{}
	leases    map[string]*v1.Lease
	calls     map[string]int
}

// New returns a healthy node with freeBytes of schedulable memory.
func New(name string, freeBytes int64) *Fake {
	return &Fake{name: name, arch: "amd64", freeBytes: freeBytes, maxActive: 32,
		unhang: make(chan struct{}), leases: map[string]*v1.Lease{}, calls: map[string]int{}}
}

// SetMode switches the failure mode. Leaving ModeHang releases hung requests.
func (f *Fake) SetMode(m Mode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == ModeHang && m != ModeHang {
		close(f.unhang)
		f.unhang = make(chan struct{})
	}
	f.mode = m
}

// SetFree sets schedulable memory (before committed leases).
func (f *Fake) SetFree(b int64) { f.mu.Lock(); f.freeBytes = b; f.mu.Unlock() }

// SetArch sets the reported architecture.
func (f *Fake) SetArch(a string) { f.mu.Lock(); f.arch = a; f.mu.Unlock() }

// SetDraining makes POST /leases answer 409 draining.
func (f *Fake) SetDraining(d bool) { f.mu.Lock(); f.draining = d; f.mu.Unlock() }

// Expire marks a lease expired, as the reaper would after missed heartbeats.
// Further heartbeats get 410.
func (f *Fake) Expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.leases[id]; l != nil {
		l.State = v1.StateExpired
	}
}

// Forget deletes a lease entirely (heartbeats then get 404).
func (f *Fake) Forget(id string) { f.mu.Lock(); delete(f.leases, id); f.mu.Unlock() }

// Lease returns a copy of a lease.
func (f *Fake) Lease(id string) (v1.Lease, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[id]
	if !ok {
		return v1.Lease{}, false
	}
	return *l, true
}

// Calls returns how often a route ("POST /v1/leases" style) was hit.
func (f *Fake) Calls(route string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[route] }

func (f *Fake) committedLocked() int64 {
	var c int64
	for _, l := range f.leases {
		if l.State == v1.StateActive {
			c += l.EstMemoryBytes
		}
	}
	return c
}

func (f *Fake) activeLocked() int {
	n := 0
	for _, l := range f.leases {
		if l.State == v1.StateActive {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *v1.Error) {
	writeJSON(w, e.Code.HTTPStatus(), v1.ErrorResponse{Error: e})
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	mode, unhang := f.mode, f.unhang
	f.mu.Unlock()
	switch mode {
	case ModeHang:
		select {
		case <-r.Context().Done():
			return
		case <-unhang:
		}
	case ModeError500:
		writeErr(w, v1.Errorf(v1.CodeInternal, "fake node", "injected failure"))
		return
	}
	f.mux().ServeHTTP(w, r)
}

func (f *Fake) mux() *http.ServeMux {
	m := http.NewServeMux()
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request)) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.calls[pattern]++
			f.mu.Unlock()
			fn(w, r)
		})
	}
	h("GET /v1/healthz", f.health)
	h("GET /v1/capacity", f.capacity)
	h("POST /v1/leases", f.createLease)
	h("GET /v1/leases", f.listLeases)
	h("GET /v1/leases/{id}", f.getLease)
	h("POST /v1/leases/{id}/heartbeat", f.heartbeat)
	h("PATCH /v1/leases/{id}", f.patchLease)
	h("DELETE /v1/leases/{id}", f.releaseLease)
	h("GET /v1/leases/{id}/resources", f.resources)
	h("POST /v1/leases/{id}/compose/{project}", f.registerCompose)
	h("POST /v1/leases/{id}/sync", f.registerSync)
	h("POST /v1/leases/{id}/kind", f.kindCreate)
	h("POST /v1/leases/{id}/kind/{name}/load", f.kindLoad)
	h("DELETE /v1/leases/{id}/kind/{name}", f.kindDelete)
	return m
}

func (f *Fake) health(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, 200, v1.Health{Status: "ok", Version: "fake", APIVersion: v1.APIVersion, Node: f.name,
		OS: "linux", Arch: f.arch, Provider: "fake", Time: time.Now().UTC(), Accepting: !f.draining})
}

func (f *Fake) capacity(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	free := f.freeBytes - f.committedLocked()
	writeJSON(w, 200, v1.Capacity{Node: f.name, Arch: f.arch, Provider: "fake",
		Memory: v1.MemoryCapacity{TotalBytes: f.freeBytes, AvailableBytes: free, CommittedBytes: f.committedLocked(), SchedulableFreeBytes: free},
		Disk:   v1.DiskCapacity{Path: "/var/lib/docker", FreeBytes: 100 * v1.GiB},
		CPU:    v1.CPUCapacity{LogicalCores: 12}, LeasesActive: f.activeLocked(), Accepting: !f.draining})
}

func (f *Fake) createLease(w http.ResponseWriter, r *http.Request) {
	var req v1.LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "send JSON", "bad body: %v", err))
		return
	}
	if err := req.Validate(); err != nil {
		e, _ := v1.AsError(err)
		writeErr(w, e)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	free := f.freeBytes - f.committedLocked()
	switch {
	case f.draining:
		writeErr(w, v1.Errorf(v1.CodeDraining, "try again later or use another node", "node %s is draining and accepts no new leases", f.name))
		return
	case f.activeLocked() >= f.maxActive:
		writeErr(w, v1.Errorf(v1.CodeMaxLeases, "release unused leases", "node %s already has %d active leases", f.name, f.maxActive))
		return
	case req.EstMemoryBytes > free:
		writeErr(w, v1.Errorf(v1.CodeInsufficientMemory, "request less memory or release other leases",
			"insufficient_memory: %s schedulable, %s requested", v1.FormatBytes(free), v1.FormatBytes(req.EstMemoryBytes)).
			WithDetails(map[string]any{"schedulable_free_bytes": free, "requested_bytes": req.EstMemoryBytes}))
		return
	}
	now := time.Now().UTC()
	id := v1.NewLeaseID()
	l := &v1.Lease{ID: id, LeaseRequest: req, State: v1.StateActive, CreatedAt: now, LastHeartbeatAt: now,
		ExpiresAt: now.Add(10 * time.Minute), SSHUser: "dol",
		Labels: v1.LeaseLabels(id, req.Owner, req.Session, req.Project)}
	f.leases[id] = l
	writeJSON(w, 201, l)
}

func (f *Fake) listLeases(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := v1.LeaseList{Leases: []v1.Lease{}}
	for _, l := range f.leases {
		if o := r.URL.Query().Get("owner"); o != "" && l.Owner != o {
			continue
		}
		out.Leases = append(out.Leases, *l)
	}
	writeJSON(w, 200, out)
}

// lookup returns the lease or writes 404/410.
func (f *Fake) lookupLocked(w http.ResponseWriter, r *http.Request, allowDead bool) *v1.Lease {
	id := r.PathValue("id")
	l, ok := f.leases[id]
	if !ok {
		writeErr(w, v1.Errorf(v1.CodeNotFound, "list leases", "lease %s not found", id))
		return nil
	}
	if !allowDead && l.State != v1.StateActive {
		writeErr(w, v1.Errorf(v1.CodeLeaseGone, "acquire a new lease", "lease %s is %s", id, l.State))
		return nil
	}
	return l
}

func (f *Fake) getLease(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.lookupLocked(w, r, true); l != nil {
		writeJSON(w, 200, l)
	}
}

func (f *Fake) heartbeat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.lookupLocked(w, r, false); l != nil {
		l.LastHeartbeatAt = time.Now().UTC()
		l.ExpiresAt = l.LastHeartbeatAt.Add(10 * time.Minute)
		writeJSON(w, 200, v1.HeartbeatResponse{ExpiresAt: l.ExpiresAt})
	}
}

func (f *Fake) patchLease(w http.ResponseWriter, r *http.Request) {
	var p v1.LeasePatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "send JSON", "bad body: %v", err))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lookupLocked(w, r, false)
	if l == nil {
		return
	}
	if p.EstMemoryBytes != nil {
		free := f.freeBytes - f.committedLocked() + l.EstMemoryBytes
		if *p.EstMemoryBytes > free {
			writeErr(w, v1.Errorf(v1.CodeInsufficientMemory, "keep the current size or release other leases",
				"insufficient_memory: %s schedulable, %s requested", v1.FormatBytes(free), v1.FormatBytes(*p.EstMemoryBytes)))
			return
		}
		l.EstMemoryBytes = *p.EstMemoryBytes
	}
	if p.Pinned != nil {
		l.Pinned = *p.Pinned
	}
	if p.TTLSeconds != nil && *p.TTLSeconds > 0 {
		l.ExpiresAt = time.Now().UTC().Add(time.Duration(*p.TTLSeconds) * time.Second)
	}
	writeJSON(w, 200, l)
}

func (f *Fake) releaseLease(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lookupLocked(w, r, true)
	if l == nil {
		return
	}
	if l.State == v1.StateReleased {
		writeErr(w, v1.Errorf(v1.CodeLeaseGone, "nothing to do", "lease %s already released", l.ID))
		return
	}
	l.State = v1.StateReleased
	inv := v1.Inventory{Containers: []string{}, Networks: []string{}, Volumes: []string{},
		KindClusters: []string{}, ComposeProjects: l.Resources.ComposeProjects, SyncDirs: []string{}}
	writeJSON(w, 200, v1.ReleaseResponse{Lease: *l, Removed: inv})
}

func (f *Fake) resources(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.lookupLocked(w, r, true); l != nil {
		writeJSON(w, 200, v1.Inventory{ComposeProjects: l.Resources.ComposeProjects})
	}
}

func (f *Fake) registerCompose(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.lookupLocked(w, r, false); l != nil {
		l.Resources.ComposeProjects = append(l.Resources.ComposeProjects, r.PathValue("project"))
		writeJSON(w, 200, map[string]string{})
	}
}

func (f *Fake) registerSync(w http.ResponseWriter, r *http.Request) {
	var req v1.SyncRegisterRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.lookupLocked(w, r, false); l != nil {
		l.Resources.SyncDirs = append(l.Resources.SyncDirs, req.Path)
		writeJSON(w, 200, map[string]string{})
	}
}

// KindAPIPort is the API server port the fake reports for new clusters; tests
// may point it at a local listener to emulate the node's API server.
var KindAPIPort = 16443

// FakeKubeconfig is what the fake returns for kind clusters.
const FakeKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Zm9v
    server: https://127.0.0.1:%d
  name: kind-%s
contexts:
- context:
    cluster: kind-%s
    user: kind-%s
  name: kind-%s
current-context: kind-%s
kind: Config
users:
- name: kind-%s
  user:
    client-certificate-data: YmFy
`

func (f *Fake) kindCreate(w http.ResponseWriter, r *http.Request) {
	var req v1.KindCreateRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := v1.ValidateKindName(req.Name); err != nil {
		e, _ := v1.AsError(err)
		writeErr(w, e)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lookupLocked(w, r, false)
	if l == nil {
		return
	}
	cn := v1.KindClusterName(l.ID, req.Name)
	mode := req.AccessMode
	if mode == "" {
		mode = v1.AccessTunnel
	}
	port := KindAPIPort
	l.Resources.KindClusters = append(l.Resources.KindClusters, v1.KindCluster{Name: req.Name, ClusterName: cn, APIServerPort: port, AccessMode: mode})
	host := "127.0.0.1"
	if mode == v1.AccessLAN {
		host = "192.0.2.10"
	}
	kc := fmt.Sprintf(FakeKubeconfig, port, cn, cn, cn, cn, cn, cn)
	if mode == v1.AccessLAN {
		kc = fmt.Sprintf(strings.Replace(FakeKubeconfig, "127.0.0.1", "0.0.0.0", 1), port, cn, cn, cn, cn, cn, cn)
	}
	writeJSON(w, 201, v1.KindCreateResponse{Cluster: cn, Kubeconfig: kc, APIServer: v1.HostPort{Host: host, Port: port}, AccessMode: mode,
		PortMappings: []v1.PortMapping{{ContainerPort: 30080, HostPort: 8080, ListenAddress: "127.0.0.1"}}})
}

func (f *Fake) findCluster(l *v1.Lease, name string) int {
	for i, c := range l.Resources.KindClusters {
		if c.Name == name || c.ClusterName == name {
			return i
		}
	}
	return -1
}

func (f *Fake) kindLoad(w http.ResponseWriter, r *http.Request) {
	var req v1.KindLoadRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lookupLocked(w, r, false)
	if l == nil {
		return
	}
	if f.findCluster(l, r.PathValue("name")) < 0 {
		writeErr(w, v1.Errorf(v1.CodeNotFound, "create it first", "no kind cluster %q on lease %s", r.PathValue("name"), l.ID))
		return
	}
	writeJSON(w, 200, v1.KindLoadResponse{Loaded: req.Images})
}

func (f *Fake) kindDelete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lookupLocked(w, r, false)
	if l == nil {
		return
	}
	i := f.findCluster(l, r.PathValue("name"))
	if i < 0 {
		writeErr(w, v1.Errorf(v1.CodeNotFound, "list clusters with dol_list", "no kind cluster %q on lease %s", r.PathValue("name"), l.ID))
		return
	}
	l.Resources.KindClusters = append(l.Resources.KindClusters[:i], l.Resources.KindClusters[i+1:]...)
	writeJSON(w, 200, map[string]string{})
}

// Server is a Fake served on a unix socket.
type Server struct {
	Fake   *Fake
	Socket string
	srv    *http.Server
	ln     net.Listener
	once   sync.Once
}

// Serve starts f on a unix socket in a short temp dir (macOS limits socket
// paths to 104 bytes, and t.TempDir() paths can be longer). It is stopped at
// test cleanup.
func Serve(t testing.TB, f *Fake) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "dolfake")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Fake: f, Socket: sock, ln: ln, srv: &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}}
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() { s.Stop(); _ = os.RemoveAll(dir) })
	return s
}

// Client returns a nodeclient for this server.
func (s *Server) Client() *nodeclient.Client { return nodeclient.NewUnix(s.Fake.name, s.Socket) }

// Stop closes the listener and all connections; afterwards dials fail like a
// stopped node agent.
func (s *Server) Stop() {
	s.once.Do(func() {
		s.Fake.SetMode(ModeOK) // release hung handlers
		_ = s.srv.Close()
		_ = os.Remove(s.Socket)
	})
}

// String implements fmt.Stringer.
func (s *Server) String() string { return fmt.Sprintf("nodefake(%s at %s)", s.Fake.name, s.Socket) }
