// Package node is the node agent (`dol node serve`): an HTTP/1.1 JSON API on
// a unix socket (brief §7.2), node-side admission control (§7.3, ADR-0003),
// a SQLite lease store, heartbeats, the reaper (§7.6, ADR-0004) and kind
// operations (§7.9, ADR-0007).
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/kindx"
	"github.com/fl0wo/docker-offload-lan/internal/version"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Clock is injectable so tests can move time (reaper, expiry).
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options configure a Server. Config, Store, Provider and Docker are required.
type Options struct {
	Config   *config.NodeConfig
	Store    *Store
	Provider capacity.Provider
	Docker   *dockerx.Docker
	// Kind runs the kind binary; nil uses kindx.ExecRunner{Bin: Config.Kind.Binary}.
	Kind   kindx.Runner
	Clock  Clock
	Logger *slog.Logger
	// PortFree probes whether a TCP port can be bound; nil probes for real.
	PortFree func(port int) bool
	// LANAddrs returns certSANs for lan-mode kind clusters (LAN IP,
	// hostname); nil detects them.
	LANAddrs func() []string
	// KindWait is passed to `kind create cluster --wait`; default 180s.
	KindWait time.Duration
}

// Server is the node agent.
type Server struct {
	cfg      *config.NodeConfig
	store    *Store
	prov     capacity.Provider
	docker   *dockerx.Docker
	kind     *kindx.Kind
	clock    Clock
	log      *slog.Logger
	lim      Limits
	exp      ExpiryPolicy
	portFree func(int) bool
	lanAddrs func() []string
	kindWait time.Duration

	// mu is the admission mutex (ADR-0003). It also serializes every
	// read-modify-write of a lease so heartbeats, the reaper and handlers
	// never lose each other's updates. Never held across slow work.
	mu       sync.Mutex
	draining atomic.Bool

	// ops holds one mutex per lease for long operations (kind create/load/
	// delete, teardown) so they never run concurrently on the same lease.
	opsMu sync.Mutex
	ops   map[string]*sync.Mutex

	// orphanScope, when set, limits orphan cleanup to matching lease ids.
	// Tests against a shared real Docker daemon use it so they never touch
	// resources of other processes.
	orphanScope func(leaseID string) bool

	statMu        sync.Mutex
	unlabeled     v1.UnlabeledCounts
	unlabeledAt   time.Time
	dockerProblem string
	dockerProbeAt time.Time // last docker liveness probe made by health
}

const metaDraining = "draining"

// New builds a Server and restores persisted state (drain flag).
func New(ctx context.Context, o Options) (*Server, error) {
	if o.Config == nil || o.Store == nil || o.Provider == nil || o.Docker == nil {
		return nil, errors.New("node.New: Config, Store, Provider and Docker are required")
	}
	s := &Server{
		cfg: o.Config, store: o.Store, prov: o.Provider, docker: o.Docker,
		clock: o.Clock, log: o.Logger, portFree: o.PortFree, lanAddrs: o.LANAddrs, kindWait: o.KindWait,
		lim: LimitsFromConfig(o.Config),
		exp: ExpiryPolicy{
			Grace:  time.Duration(o.Config.Leases.HeartbeatGraceS) * time.Second,
			MaxTTL: time.Duration(o.Config.Leases.MaxTTLS) * time.Second,
		},
		ops: map[string]*sync.Mutex{},
	}
	kr := o.Kind
	if kr == nil {
		kr = kindx.ExecRunner{Bin: kindBinary(o.Config.Kind.Binary)}
	}
	s.kind = &kindx.Kind{R: kr}
	if s.clock == nil {
		s.clock = realClock{}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.portFree == nil {
		s.portFree = probePort
	}
	if s.lanAddrs == nil {
		s.lanAddrs = func() []string { return DetectLANAddrs(o.Config.Kind.LANAddress) }
	}
	if s.kindWait <= 0 {
		s.kindWait = 180 * time.Second
	}
	d, err := s.store.GetMeta(ctx, metaDraining)
	if err != nil {
		return nil, err
	}
	s.draining.Store(d == "true")
	return s, nil
}

// kindBinary prefers the configured path and falls back to PATH.
func kindBinary(configured string) string {
	if configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	return "kind"
}

func probePort(p int) bool {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// DetectLANAddrs returns [LAN IPv4, hostname] for certSANs; override wins
// over the detected address.
func DetectLANAddrs(override string) []string {
	var out []string
	if override != "" {
		out = append(out, override)
	} else if ip := firstLANIPv4(); ip != "" {
		out = append(out, ip)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		out = append(out, h)
	}
	return out
}

func firstLANIPv4() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				return n.IP.String()
			}
		}
	}
	return ""
}

func (s *Server) opLock(id string) *sync.Mutex {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	m, ok := s.ops[id]
	if !ok {
		m = &sync.Mutex{}
		s.ops[id] = m
	}
	return m
}

func (s *Server) now() time.Time { return s.clock.Now().UTC().Round(0) }

// Draining reports the drain flag.
func (s *Server) Draining() bool { return s.draining.Load() }

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	p := v1.PathPrefix
	mux.HandleFunc("GET "+p+"/healthz", s.handleHealth)
	mux.HandleFunc("GET "+p+"/capacity", s.handleCapacity)
	mux.HandleFunc("POST "+p+"/leases", s.handleCreate)
	mux.HandleFunc("GET "+p+"/leases", s.handleList)
	mux.HandleFunc("GET "+p+"/leases/{id}", s.handleGet)
	mux.HandleFunc("POST "+p+"/leases/{id}/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("PATCH "+p+"/leases/{id}", s.handlePatch)
	mux.HandleFunc("DELETE "+p+"/leases/{id}", s.handleRelease)
	mux.HandleFunc("GET "+p+"/leases/{id}/resources", s.handleResources)
	mux.HandleFunc("POST "+p+"/leases/{id}/compose/{project}", s.handleRegisterCompose)
	mux.HandleFunc("POST "+p+"/leases/{id}/sync", s.handleRegisterSync)
	mux.HandleFunc("POST "+p+"/leases/{id}/kind", s.handleKindCreate)
	mux.HandleFunc("POST "+p+"/leases/{id}/kind/{name}/load", s.handleKindLoad)
	mux.HandleFunc("DELETE "+p+"/leases/{id}/kind/{name}", s.handleKindDelete)
	mux.HandleFunc("POST "+p+"/admin/drain", s.handleDrain)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, v1.Errorf(v1.CodeNotFound, "check that client and node run the same dol version (`dol --version`)",
			"no route for %s %s on this node agent", r.Method, r.URL.Path))
	})
	return s.logRequests(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// A client that half-closes its side after sending the request (as
		// `printf … | dol node dial-stdio` does) makes net/http cancel the
		// request context. Node operations are short and must not be torn
		// in half, so handlers run detached with their own deadline; kind
		// operations set longer ones.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestTimeout)
		defer cancel()
		h.ServeHTTP(rec, r.WithContext(ctx))
		lvl := slog.LevelDebug
		if r.Method != http.MethodGet && r.URL.Path != v1.PathPrefix+"/healthz" {
			lvl = slog.LevelInfo
		}
		if rec.status >= 500 {
			lvl = slog.LevelWarn
		}
		s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds(), "ssh_user", PeerUser(r.Context()))
	})
}

// Serve serves the API on ln until ctx is cancelled, then shuts down
// gracefully (in-flight requests get up to 30 s).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:           s.Handler(),
		ConnContext:       connContext,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := hs.Shutdown(sctx)
		if serr := <-errc; err == nil && !errors.Is(serr, http.ErrServerClosed) {
			err = serr
		}
		return err
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	e, ok := v1.AsError(err)
	if !ok {
		e = v1.Errorf(v1.CodeInternal, "check the node agent logs: `journalctl -u dol-node` on the node", "%v", err)
	}
	writeJSON(w, e.Code.HTTPStatus(), v1.ErrorResponse{Error: e})
}

const maxBody = 1 << 20

// requestTimeout bounds every request except kind operations and release.
const requestTimeout = 2 * time.Minute

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return v1.Errorf(v1.CodeBadRequest, "retry the request", "reading request body: %v", err)
	}
	if len(b) > maxBody {
		return v1.Errorf(v1.CodeBadRequest, "send a smaller request", "request body larger than %d bytes", maxBody)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return v1.Errorf(v1.CodeBadRequest, "send a JSON body matching pkg/api/v1", "invalid JSON body: %v", err)
	}
	return nil
}

func (s *Server) internal(err error, what string) *v1.Error {
	s.log.Error(what, "err", err)
	return v1.Errorf(v1.CodeInternal, "check the node agent logs: `journalctl -u dol-node` on the node", "%s: %v", what, err)
}

func (s *Server) notFound(id string) *v1.Error {
	return v1.Errorf(v1.CodeNotFound,
		"the lease may belong to another node or was never created; call dol_acquire for a new lease",
		"lease %s not found on node %s", id, s.cfg.Name)
}

func (s *Server) gone(l *v1.Lease) *v1.Error {
	return v1.Errorf(v1.CodeLeaseGone,
		"the lease and its resources are gone or being removed; mark it lost and call dol_acquire for a new lease",
		"lease %s is %s on node %s", l.ID, l.State, s.cfg.Name).
		WithDetails(map[string]any{"state": string(l.State)})
}

// getLease loads a lease; callers that modify it must hold s.mu.
func (s *Server) getLease(ctx context.Context, id string) (*v1.Lease, *v1.Error) {
	if !v1.ValidLeaseID(id) {
		return nil, s.notFound(id)
	}
	l, err := s.store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, s.notFound(id)
	}
	if err != nil {
		return nil, s.internal(err, "reading lease")
	}
	return l, nil
}

// activeLease loads a lease that must be active; callers hold s.mu.
func (s *Server) activeLease(ctx context.Context, id string) (*v1.Lease, *v1.Error) {
	l, e := s.getLease(ctx, id)
	if e != nil {
		return nil, e
	}
	if l.State != v1.StateActive {
		return nil, s.gone(l)
	}
	if s.exp.Expired(l, s.now()) {
		l.State = v1.StateExpired
		if err := s.store.Update(ctx, l); err != nil {
			return nil, s.internal(err, "expiring lease")
		}
		s.log.Info("lease expired", "lease", l.ID, "reason", "deadline passed before reaper ran")
		return nil, s.gone(l)
	}
	return l, nil
}

// ---- health & capacity -----------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.probeDocker(r.Context())
	h := v1.Health{
		Status: "ok", Version: version.Version, APIVersion: v1.APIVersion, Node: s.cfg.Name,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Provider: s.prov.Name(), Time: s.now(),
		Accepting: !s.Draining(),
	}
	s.statMu.Lock()
	if s.dockerProblem != "" {
		h.Status = "degraded"
		h.Problems = append(h.Problems, s.dockerProblem)
	}
	s.statMu.Unlock()
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) snapshot(ctx context.Context) (capacity.Snapshot, *v1.Error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snap, err := s.prov.Snapshot(cctx)
	if err != nil {
		s.log.Error("capacity snapshot failed", "provider", s.prov.Name(), "err", err)
		return snap, v1.Errorf(v1.CodeUnavailable,
			"check the node agent logs (`journalctl -u dol-node`) and capacity.provider in node.yaml",
			"node %s cannot measure its capacity: %v", s.cfg.Name, err)
	}
	return snap, nil
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	snap, e := s.snapshot(r.Context())
	if e != nil {
		writeErr(w, e)
		return
	}
	committed, active, err := s.store.Committed(r.Context(), "")
	if err != nil {
		writeErr(w, s.internal(err, "summing leases"))
		return
	}
	a := Admit(s.cfg.Name, snap, s.lim, committed, active, s.Draining(), Request{EstMemory: 1})
	b := a.Budget
	c := v1.Capacity{
		Node: s.cfg.Name, Arch: runtime.GOARCH, Provider: s.prov.Name(),
		Memory: v1.MemoryCapacity{
			TotalBytes: snap.MemTotal, AvailableBytes: snap.MemAvailable, ReservedBytes: b.Reserved,
			CommittedBytes: b.Committed, SchedulableFreeBytes: nonNeg(b.FreeForNew), HostFreeBytes: snap.HostMemFree,
		},
		Disk: v1.DiskCapacity{
			Path: snap.DiskPath, FreeBytes: snap.DiskFree, HostFreeBytes: snap.HostDiskFree, MinFreeBytes: s.lim.MinFreeDisk,
		},
		CPU:          v1.CPUCapacity{LogicalCores: snap.CPUCores, Load1: snap.Load1},
		LeasesActive: active,
		Accepting:    a.Err == nil,
		Warnings:     snap.Warnings,
	}
	if a.Err != nil {
		c.Reason = string(a.Err.Code) + ": " + a.Err.Message
	}
	c.Unlabeled = s.unlabeledCounts(r.Context())
	s.statMu.Lock()
	if s.dockerProblem != "" {
		c.Warnings = append(c.Warnings, s.dockerProblem)
	}
	s.statMu.Unlock()
	writeJSON(w, http.StatusOK, c)
}

// unlabeledCounts returns the reaper's last count, refreshing it when stale.
func (s *Server) unlabeledCounts(ctx context.Context) v1.UnlabeledCounts {
	s.statMu.Lock()
	fresh := !s.unlabeledAt.IsZero() && time.Since(s.unlabeledAt) < 2*time.Duration(s.cfg.Reaper.IntervalS)*time.Second
	c := s.unlabeled
	s.statMu.Unlock()
	if fresh {
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nc, err := s.docker.CountUnlabeled(cctx)
	if err != nil {
		s.setDockerProblem(err)
		return c
	}
	s.statMu.Lock()
	s.unlabeled, s.unlabeledAt = nc, time.Now()
	s.statMu.Unlock()
	return nc
}

// probeDocker refreshes dockerProblem at most every 10 s (bounded to 1 s) so
// health never reports "ok" for a node whose daemon is down: otherwise a
// freshly started agent looked healthy until the first reaper pass and
// placement would send work to a node that cannot run it.
func (s *Server) probeDocker(ctx context.Context) {
	s.statMu.Lock()
	fresh := !s.dockerProbeAt.IsZero() && time.Since(s.dockerProbeAt) < 10*time.Second
	if !fresh {
		s.dockerProbeAt = time.Now()
	}
	s.statMu.Unlock()
	if fresh {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := s.docker.Info(pctx)
	if err != nil && pctx.Err() != nil {
		return // slow, not down: keep the last known state
	}
	s.setDockerProblem(err)
}

func (s *Server) setDockerProblem(err error) {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if err == nil {
		s.dockerProblem = ""
		return
	}
	if dockerx.IsDaemonDown(err) {
		s.dockerProblem = "docker daemon unreachable on the node: " + err.Error() + " (check `sudo systemctl status docker`)"
	}
}

// ---- leases ---------------------------------------------------------------

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req v1.LeaseRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := req.Validate(); err != nil {
		writeErr(w, err)
		return
	}
	// The snapshot is taken outside the mutex (it may be slow); committed
	// memory, which changes with every admission, is read inside it.
	snap, e := s.snapshot(ctx)
	if e != nil {
		writeErr(w, e)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	committed, active, err := s.store.Committed(ctx, "")
	if err != nil {
		writeErr(w, s.internal(err, "summing leases"))
		return
	}
	a := Admit(s.cfg.Name, snap, s.lim, committed, active, s.Draining(),
		Request{EstMemory: req.EstMemoryBytes, EstDisk: req.EstDiskBytes})
	if a.Err != nil {
		s.log.Info("lease refused", "code", a.Err.Code, "owner", req.Owner, "requested_bytes", req.EstMemoryBytes, "reason", a.Err.Message)
		writeErr(w, a.Err)
		return
	}
	now := s.now()
	l := &v1.Lease{
		LeaseRequest: req, State: v1.StateActive, CreatedAt: now, LastHeartbeatAt: now,
		SSHUser:   PeerUser(ctx),
		Resources: v1.LeaseResources{KindClusters: []v1.KindCluster{}, ComposeProjects: []string{}, SyncDirs: []string{}},
	}
	if req.Pinned {
		if req.TTLSeconds > 0 {
			l.ExpiresAt = now.Add(time.Duration(req.TTLSeconds) * time.Second)
		}
	} else {
		s.exp.Refresh(l)
	}
	for attempt := 0; ; attempt++ {
		l.ID = v1.NewLeaseID()
		l.Labels = v1.LeaseLabels(l.ID, req.Owner, req.Session, req.Project)
		err = s.store.Insert(ctx, l)
		if err == nil || attempt == 3 {
			break
		}
	}
	if err != nil {
		writeErr(w, s.internal(err, "storing lease"))
		return
	}
	s.log.Info("lease created", "lease", l.ID, "owner", l.Owner, "project", l.Project, "workload", l.Workload,
		"est_memory_bytes", l.EstMemoryBytes, "pinned", l.Pinned, "ssh_user", l.SSHUser)
	writeJSON(w, http.StatusCreated, l)
}

var validStates = map[v1.LeaseState]bool{v1.StateActive: true, v1.StateReleasing: true, v1.StateExpired: true,
	v1.StateReleased: true, v1.StateFailed: true}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var states []v1.LeaseState
	if st := q.Get("state"); st != "" {
		if !validStates[v1.LeaseState(st)] {
			writeErr(w, v1.Errorf(v1.CodeBadRequest, "use active, releasing, expired, released or failed", "unknown state %q", st))
			return
		}
		states = append(states, v1.LeaseState(st))
	}
	ls, err := s.store.List(r.Context(), q.Get("owner"), states...)
	if err != nil {
		writeErr(w, s.internal(err, "listing leases"))
		return
	}
	writeJSON(w, http.StatusOK, v1.LeaseList{Leases: ls})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	l, e := s.getLease(r.Context(), r.PathValue("id"))
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.activeLease(ctx, r.PathValue("id"))
	if e != nil {
		writeErr(w, e)
		return
	}
	l.LastHeartbeatAt = s.now()
	s.exp.Refresh(l)
	if err := s.store.Update(ctx, l); err != nil {
		writeErr(w, s.internal(err, "recording heartbeat"))
		return
	}
	writeJSON(w, http.StatusOK, v1.HeartbeatResponse{ExpiresAt: l.ExpiresAt})
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var p v1.LeasePatch
	if err := decode(r, &p); err != nil {
		writeErr(w, err)
		return
	}
	if p.EstMemoryBytes != nil && *p.EstMemoryBytes <= 0 {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "pass a positive est_memory_bytes", "est_memory_bytes must be > 0 (got %d)", *p.EstMemoryBytes))
		return
	}
	if p.TTLSeconds != nil && *p.TTLSeconds < 0 {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "pass ttl_s >= 0 (0 = no explicit expiry)", "ttl_s must be >= 0"))
		return
	}
	var snap capacity.Snapshot
	if p.EstMemoryBytes != nil {
		var e *v1.Error
		if snap, e = s.snapshot(ctx); e != nil {
			writeErr(w, e)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.activeLease(ctx, r.PathValue("id"))
	if e != nil {
		writeErr(w, e)
		return
	}
	now := s.now()
	if p.EstMemoryBytes != nil && *p.EstMemoryBytes > l.EstMemoryBytes {
		// Growing re-runs memory admission without the lease's own
		// reservation; shrinking is always allowed.
		committed, active, err := s.store.Committed(ctx, l.ID)
		if err != nil {
			writeErr(w, s.internal(err, "summing leases"))
			return
		}
		b := ComputeBudget(snap, s.lim, committed)
		if err := memoryRefusal(s.cfg.Name, snap, b, active, *p.EstMemoryBytes); err != nil {
			err.Message = "cannot resize lease " + l.ID + ": " + err.Message
			err.Hint = "keep the current size, release other leases, or acquire a new lease that may fall back to local Docker"
			writeErr(w, err)
			return
		}
	}
	if p.EstMemoryBytes != nil {
		l.EstMemoryBytes = *p.EstMemoryBytes
	}
	if p.Pinned != nil && *p.Pinned != l.Pinned {
		l.Pinned = *p.Pinned
		l.ExpiresAt = time.Time{}
	}
	if p.TTLSeconds != nil {
		l.TTLSeconds = *p.TTLSeconds
		if l.Pinned {
			l.ExpiresAt = time.Time{}
			if *p.TTLSeconds > 0 {
				l.ExpiresAt = now.Add(time.Duration(*p.TTLSeconds) * time.Second)
			}
		}
	}
	l.LastHeartbeatAt = now // a PATCH proves the client is alive
	s.exp.Refresh(l)
	if err := s.store.Update(ctx, l); err != nil {
		writeErr(w, s.internal(err, "updating lease"))
		return
	}
	s.log.Info("lease updated", "lease", l.ID, "est_memory_bytes", l.EstMemoryBytes, "pinned", l.Pinned)
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	keep, _ := strconv.ParseBool(r.URL.Query().Get("keep_sync_dir"))
	// Teardown must finish even if the client disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	s.mu.Lock()
	l, e := s.getLease(ctx, id)
	if e != nil {
		s.mu.Unlock()
		writeErr(w, e)
		return
	}
	if l.State == v1.StateActive {
		l.State = v1.StateReleasing
		if err := s.store.Update(ctx, l); err != nil {
			s.mu.Unlock()
			writeErr(w, s.internal(err, "releasing lease"))
			return
		}
	}
	s.mu.Unlock()
	s.log.Info("lease release requested", "lease", id, "keep_sync_dir", keep, "ssh_user", PeerUser(r.Context()))
	inv, final, err := s.Teardown(ctx, id, keep, true)
	if err != nil {
		writeErr(w, s.internal(err, "tearing down lease"))
		return
	}
	writeJSON(w, http.StatusOK, v1.ReleaseResponse{Lease: *final, Removed: inv})
}

func emptyInventory() v1.Inventory {
	return v1.Inventory{Containers: []string{}, Networks: []string{}, Volumes: []string{},
		KindClusters: []string{}, ComposeProjects: []string{}, SyncDirs: []string{}}
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, e := s.getLease(ctx, r.PathValue("id"))
	if e != nil {
		writeErr(w, e)
		return
	}
	inv := emptyInventory()
	for _, c := range l.Resources.KindClusters {
		inv.KindClusters = append(inv.KindClusters, c.ClusterName)
	}
	inv.ComposeProjects = append(inv.ComposeProjects, l.Resources.ComposeProjects...)
	if l.State != v1.StateReleased {
		inv.SyncDirs = append(inv.SyncDirs, l.Resources.SyncDirs...)
	}
	dinv, err := s.docker.LeaseInventory(ctx, l.ID)
	if err != nil {
		s.setDockerProblem(err)
		writeErr(w, v1.Errorf(v1.CodeUnavailable, "check Docker on the node: `sudo systemctl status docker`",
			"cannot list Docker resources of lease %s: %v", l.ID, err))
		return
	}
	inv.Containers = append(inv.Containers, dinv.Containers...)
	inv.Networks = append(inv.Networks, dinv.Networks...)
	inv.Volumes = append(inv.Volumes, dinv.Volumes...)
	writeJSON(w, http.StatusOK, inv)
}

// composeProjectRe is docker compose's project name rule.
var composeProjectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (s *Server) handleRegisterCompose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proj := r.PathValue("project")
	if !composeProjectRe.MatchString(proj) || len(proj) > 128 {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "compose project names use lowercase letters, digits, '_' and '-', starting with a letter or digit",
			"invalid compose project name %q", proj))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.activeLease(ctx, r.PathValue("id"))
	if e != nil {
		writeErr(w, e)
		return
	}
	if !contains(l.Resources.ComposeProjects, proj) {
		l.Resources.ComposeProjects = append(l.Resources.ComposeProjects, proj)
		if err := s.store.Update(ctx, l); err != nil {
			writeErr(w, s.internal(err, "registering compose project"))
			return
		}
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleRegisterSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req v1.SyncRegisterRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	p, err := ValidateSyncPath(s.cfg.SyncRoot, id, req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := CheckNoSymlinks(s.cfg.SyncRoot, p); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.activeLease(ctx, id)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !contains(l.Resources.SyncDirs, p) {
		l.Resources.SyncDirs = append(l.Resources.SyncDirs, p)
		if err := s.store.Update(ctx, l); err != nil {
			writeErr(w, s.internal(err, "registering sync dir"))
			return
		}
	}
	writeJSON(w, http.StatusOK, l)
}

// DrainResponse is the body of POST /v1/admin/drain.
type DrainResponse struct {
	Draining     bool `json:"draining"`
	LeasesActive int  `json:"leases_active"`
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := v1.DrainRequest{Draining: true}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.SetMeta(ctx, metaDraining, strconv.FormatBool(req.Draining)); err != nil {
		writeErr(w, s.internal(err, "persisting drain flag"))
		return
	}
	s.draining.Store(req.Draining)
	_, active, _ := s.store.Committed(ctx, "")
	s.log.Warn("drain flag changed", "draining", req.Draining, "leases_active", active, "ssh_user", PeerUser(ctx))
	writeJSON(w, http.StatusOK, DrainResponse{Draining: req.Draining, LeasesActive: active})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// errText renders an error for last_error without newlines.
func errText(format string, a ...any) string { return fmt.Sprintf(format, a...) }
