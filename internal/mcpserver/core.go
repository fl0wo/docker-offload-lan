// Package mcpserver implements `dol mcp`, the stdio MCP server agents use to
// acquire capacity (brief §8), and the Core it shares with the human CLI
// (`dol status`, `dol leases`, `dol release`).
package mcpserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	"github.com/fl0wo/docker-offload-lan/internal/placement"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	"github.com/fl0wo/docker-offload-lan/internal/state"
	"github.com/fl0wo/docker-offload-lan/internal/tunnel"
	"github.com/fl0wo/docker-offload-lan/internal/version"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// NodeAPI is the subset of *nodeclient.Client the client side uses.
type NodeAPI interface {
	placement.NodeAPI
	Capacity(ctx context.Context) (*v1.Capacity, error)
	GetLease(ctx context.Context, id string) (*v1.Lease, error)
	Heartbeat(ctx context.Context, id string) (*v1.HeartbeatResponse, error)
	PatchLease(ctx context.Context, id string, p v1.LeasePatch) (*v1.Lease, error)
	ReleaseLease(ctx context.Context, id string, keepSyncDir bool) (*v1.ReleaseResponse, error)
}

// Forwarder is the hook for SSH port forwards (internal/tunnel, milestone
// M4). Core calls it on release and on shutdown.
type Forwarder interface {
	// CloseLease closes every forward of the lease and returns the local
	// ports that were closed.
	CloseLease(ctx context.Context, l v1.ClientLease) ([]int, error)
	// CloseAll closes every forward owned by this process.
	CloseAll()
}

// Options configure a Core. Zero values pick production defaults.
type Options struct {
	Config *config.ClientConfig
	Store  *state.Store
	// Docker runs the docker CLI; default dockerx.ExecRunner.
	Docker dockerx.Runner
	// NodeAPI builds the node API client; default nodeclient.NewSSH.
	NodeAPI func(n config.NodeRef) NodeAPI
	// ProjectDir defaults to $CLAUDE_PROJECT_DIR, then the working directory.
	ProjectDir string
	// Session defaults to a new v1.NewSessionID().
	Session string
	// LocalContext overrides local.docker_context / auto-detection.
	LocalContext string
	ClientArch   string // default runtime.GOARCH
	Forwarder    Forwarder
	// SSH configures every ssh invocation (node API, tunnels, rsync). Its
	// ConnectTimeout defaults to policy.health_timeout_ms.
	SSH sshx.Options
	// Tunnels manages ssh -L forwards; default tunnel.NewManager(SSH).
	Tunnels *tunnel.Manager
	// KindBinary is the local kind binary for local kind fallback; default "kind".
	KindBinary string
	// Rsync overrides rsync binary detection.
	Rsync  string
	Logger *slog.Logger
	Now    func() time.Time
}

// Core is the client logic behind every MCP tool and CLI command.
type Core struct {
	cfg        *config.ClientConfig
	store      *state.Store
	runner     dockerx.Runner
	nodes      map[string]nodeEntry
	placer     *placement.Placer
	session    string
	owner      string
	project    string
	projectDir string
	ssh        sshx.Options
	tunnels    *tunnel.Manager
	kindBin    string
	rsync      string
	localCtx   string
	localWarn  string
	arch       string
	fwd        Forwarder
	log        *slog.Logger
	now        func() time.Time

	mu      sync.Mutex
	ensured map[string]bool                // docker contexts verified this process
	proxies map[string]*dockerproxy.Server // lease id -> Docker API proxy (ADR-0040)

	bg       sync.WaitGroup
	cancelBG context.CancelFunc
	started  bool
}

type nodeEntry struct {
	ref config.NodeRef
	api NodeAPI
}

// NewCore builds a Core. It resolves the local docker context (running
// `docker context ls/show` when local.docker_context is auto) but does not
// register a session or start background work; see Start.
func NewCore(ctx context.Context, o Options) (*Core, error) {
	if o.Config == nil || o.Store == nil {
		return nil, errors.New("mcpserver: Config and Store are required")
	}
	c := &Core{
		cfg: o.Config, store: o.Store, runner: o.Docker, session: o.Session,
		owner: o.Config.Identity.Owner, arch: cmp.Or(o.ClientArch, runtime.GOARCH),
		fwd: o.Forwarder, log: o.Logger, now: o.Now, nodes: map[string]nodeEntry{}, ensured: map[string]bool{},
		proxies: map[string]*dockerproxy.Server{},
	}
	if c.runner == nil {
		c.runner = dockerx.ExecRunner{}
	}
	if c.session == "" {
		c.session = v1.NewSessionID()
	}
	if c.owner == "" {
		c.owner = config.DefaultOwner()
	}
	c.ssh = o.SSH
	if c.ssh.ConnectTimeout == 0 {
		c.ssh.ConnectTimeout = time.Duration(c.cfg.Policy.HealthTimeoutMS) * time.Millisecond
	}
	c.tunnels = o.Tunnels
	if c.tunnels == nil {
		c.tunnels = tunnel.NewManager(c.ssh)
	}
	c.kindBin, c.rsync = o.KindBinary, o.Rsync
	if c.fwd == nil {
		c.fwd = tunnelForwarder{c.tunnels}
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.now == nil {
		c.now = time.Now
	}
	dir := o.ProjectDir
	if dir == "" {
		dir = ProjectDir()
	}
	c.projectDir = dir
	c.project = ProjectName(dir)

	c.localCtx = cmp.Or(o.LocalContext, c.cfg.Local.DockerContext)
	if c.localCtx == "" || c.localCtx == "auto" {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		name, err := (&dockerx.Docker{R: c.runner}).DetectLocalContext(dctx)
		cancel()
		if err != nil {
			c.localCtx = "default"
			c.localWarn = fmt.Sprintf("could not auto-detect the local docker context (%v); using %q", err, c.localCtx)
			c.log.Warn("local docker context detection failed", "err", err)
		} else {
			c.localCtx = name
		}
	}

	newAPI := o.NodeAPI
	if newAPI == nil {
		opts := c.ssh
		newAPI = func(n config.NodeRef) NodeAPI {
			return nodeclient.NewSSH(n.Name, n.SSHHost, opts)
		}
	}
	pol := c.cfg.Policy
	c.placer = &placement.Placer{
		Policy: placement.Policy{
			Prefer: pol.Prefer, Fallback: pol.Fallback,
			HealthTimeout:       time.Duration(pol.HealthTimeoutMS) * time.Millisecond,
			LeaseRequestTimeout: time.Duration(pol.LeaseRequestTimeoutMS) * time.Millisecond,
			LocalMaxMemoryBytes: v1.GBToBytes(c.cfg.Local.MaxMemoryGB),
		},
		LocalContext: c.localCtx,
		Local:        localProbe{c},
		ClientArch:   c.arch,
		Now:          o.Now,
	}
	for _, ref := range c.cfg.Nodes {
		if ref.DockerContext == "" {
			ref.DockerContext = "dol-" + ref.Name
		}
		if ref.Priority == 0 {
			ref.Priority = 100
		}
		api := newAPI(ref)
		c.nodes[ref.Name] = nodeEntry{ref: ref, api: api}
		c.placer.Nodes = append(c.placer.Nodes, placement.Node{
			Name: ref.Name, SSHHost: ref.SSHHost, DockerContext: ref.DockerContext, Priority: ref.Priority, API: api,
		})
	}
	return c, nil
}

// Session returns this process's session id.
func (c *Core) Session() string { return c.session }

// Owner returns the owner label value.
func (c *Core) Owner() string { return c.owner }

// LocalContext returns the docker context used for local placement.
func (c *Core) LocalContext() string { return c.localCtx }

// ProjectDir returns $CLAUDE_PROJECT_DIR (set by Claude Code for MCP
// servers, docs/compat.md §6) or the working directory.
func ProjectDir() string {
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	d, _ := os.Getwd()
	return d
}

// ProjectName is the label-safe basename of dir.
func ProjectName(dir string) string {
	b := filepath.Base(filepath.Clean(dir))
	if b == "." || b == "/" || b == "" {
		return "default"
	}
	return sanitizeLabel(b)
}

func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == ',' {
			return '_'
		}
		return r
	}, s)
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

func (c *Core) docker(contextName string) *dockerx.Docker {
	return &dockerx.Docker{Context: contextName, R: c.runner}
}

// localProbe adapts Core to placement.LocalProbe.
type localProbe struct{ c *Core }

func (p localProbe) MemTotal(ctx context.Context) (int64, error) {
	info, err := p.c.docker(p.c.localCtx).Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.MemTotal, nil
}

// Committed sums estimates of active local leases of every session.
func (p localProbe) Committed() int64 {
	ls, _ := p.c.store.ListLeases()
	var sum int64
	for _, l := range ls {
		if l.Placement == v1.PlacementLocal && l.State == v1.StateActive {
			sum += l.EstMemoryBytes
		}
	}
	return sum
}

// ---- lifecycle ----

// Start registers the session, cleans up local leases of dead sessions (in
// the background) and starts the heartbeat loop. Call Close when done.
func (c *Core) Start(ctx context.Context) error {
	sess, err := state.NewSelfSession(c.session, c.owner, c.project)
	if err != nil {
		return err
	}
	if err := c.store.PutSession(sess); err != nil {
		return fmt.Errorf("registering session: %w", err)
	}
	bgctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancelBG = cancel
	c.started = true
	c.bg.Go(func() {
		rep := c.CleanupDeadSessions(bgctx)
		if !rep.empty() {
			c.log.Info("cleaned up after dead sessions", "report", rep.String())
		}
	})
	c.bg.Go(func() { c.heartbeatLoop(bgctx) })
	return nil
}

// Close stops background work and child processes and unregisters the
// session. Remote leases are NOT released: the node's reaper removes them
// after heartbeat_grace_s unless they are pinned (ADR-0021). Local leases of
// this session are torn down by the next `dol mcp` that starts.
func (c *Core) Close() {
	if c.cancelBG != nil {
		c.cancelBG()
	}
	c.bg.Wait()
	c.fwd.CloseAll()
	c.stopAllProxies()
	for _, n := range c.nodes {
		if cl, ok := n.api.(interface{ Close() }); ok {
			cl.Close()
		}
	}
	if c.started {
		_ = c.store.RemoveSession(c.session)
	}
}

// ---- helpers ----

// commandsFor renders the ready-to-paste prefixes for a docker context.
func commandsFor(contextName string, labels map[string]string) v1.Commands {
	d := "docker --context " + v1.ShellQuote(contextName)
	return v1.Commands{Docker: d, Compose: d + " compose", LabelFlags: v1.LabelFlags(labels)}
}

func (c *Core) placementInfo(l v1.ClientLease) v1.PlacementInfo {
	cmds := commandsFor(l.DockerContext, l.Labels)
	if host := c.proxyHost(l.ID); host != "" {
		cmds.DockerProxied = "docker -H " + v1.ShellQuote(host)
		cmds.ComposeProxied = cmds.DockerProxied + " compose"
	}
	return v1.PlacementInfo{
		Placement: l.Placement, Node: l.Node, Reason: l.Reason, LeaseID: l.ID,
		Commands: cmds, Warnings: []string{}, NextSteps: []string{},
	}
}

// lease loads a lease for an operation. Lost leases yield CodeLeaseLost.
func (c *Core) lease(id string) (*v1.ClientLease, error) {
	l, err := c.store.GetLease(strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if l.State == v1.StateLost {
		return l, lostError(l)
	}
	return l, nil
}

func lostError(l *v1.ClientLease) error {
	return v1.Errorf(v1.CodeLeaseLost,
		"call dol_acquire for a new lease and recreate the workload with the new prefixes; dol_release "+l.ID+" clears the stale record",
		"lease %s on node %s is lost: %s. Its containers have been or will be removed by the node", l.ID, l.Node, cmp.Or(l.Reason, "the node no longer knows it"))
}

func (c *Core) node(name string) (nodeEntry, error) {
	n, ok := c.nodes[name]
	if !ok {
		return n, v1.Errorf(v1.CodeNotFound, "add the node back to ~/.config/docker-offload-lan/config.yaml or run `dol init`",
			"node %q is not in the dol config any more", name)
	}
	return n, nil
}

// ensureContext makes sure a node's docker context exists (once per process).
func (c *Core) ensureContext(ctx context.Context, ref config.NodeRef) []string {
	c.mu.Lock()
	done := c.ensured[ref.DockerContext]
	c.mu.Unlock()
	if done {
		return nil
	}
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	warn, err := c.docker("").EnsureContext(ectx, ref.DockerContext, ref.SSHHost)
	if err != nil {
		return []string{fmt.Sprintf("could not create docker context %s (%v): run `docker context create %s --docker host=ssh://%s` or `dol doctor`",
			ref.DockerContext, err, ref.DockerContext, ref.SSHHost)}
	}
	c.mu.Lock()
	c.ensured[ref.DockerContext] = true
	c.mu.Unlock()
	if warn != "" {
		return []string{warn}
	}
	return nil
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func badInput(format string, a ...any) error {
	return v1.Errorf(v1.CodeBadRequest, "fix the arguments and call the tool again", format, a...)
}

// ---- acquire ----

// AcquireInput is the input of dol_acquire (brief §8).
type AcquireInput struct {
	Workload    string  `json:"workload" jsonschema:"what will run: container, compose, kind or build"`
	EstMemoryGB float64 `json:"est_memory_gb" jsonschema:"total memory the workload will need in GB; estimate generously"`
	EstDiskGB   float64 `json:"est_disk_gb,omitempty" jsonschema:"disk the workload will need in GB (images, volumes); optional"`
	Project     string  `json:"project,omitempty" jsonschema:"project label; defaults to the project directory name"`
	Prefer      string  `json:"prefer,omitempty" jsonschema:"remote or local; overrides the configured preference"`
	Pinned      bool    `json:"pinned,omitempty" jsonschema:"keep the lease after this session ends; only if the user asks"`
	TTLMinutes  int     `json:"ttl_minutes,omitempty" jsonschema:"hard lifetime in minutes; 0 = node default"`
	AllowWake   bool    `json:"allow_wake,omitempty" jsonschema:"allow Wake-on-LAN of a sleeping node (not implemented yet)"`
}

// Acquire places a workload and records the lease.
func (c *Core) Acquire(ctx context.Context, in AcquireInput) (*v1.AcquireResult, error) {
	w := v1.Workload(strings.ToLower(strings.TrimSpace(in.Workload)))
	if !w.Valid() {
		return nil, badInput("workload %q is invalid: use container, compose, kind or build", in.Workload)
	}
	if !(in.EstMemoryGB > 0) || in.EstMemoryGB > 4096 {
		return nil, badInput("est_memory_gb must be between 0 and 4096 (got %v); e.g. 0.5 for a small container, 2 + 0.5 per service for kind", in.EstMemoryGB)
	}
	if in.EstDiskGB < 0 || in.TTLMinutes < 0 {
		return nil, badInput("est_disk_gb and ttl_minutes must be >= 0")
	}
	if in.Prefer != "" && in.Prefer != "remote" && in.Prefer != "local" {
		return nil, badInput("prefer %q is invalid: use remote or local", in.Prefer)
	}
	project := c.project
	if p := strings.TrimSpace(in.Project); p != "" {
		project = sanitizeLabel(p)
	}
	req := v1.LeaseRequest{
		Owner: c.owner, Session: c.session, Project: project, Workload: w,
		EstMemoryBytes: v1.GBToBytes(in.EstMemoryGB), EstDiskBytes: v1.GBToBytes(in.EstDiskGB),
		TTLSeconds: int64(in.TTLMinutes) * 60, Pinned: in.Pinned, ClientVersion: version.Version,
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	dec, err := c.placer.Place(ctx, placement.Request{Lease: req, Prefer: in.Prefer})
	if err != nil {
		return nil, err
	}

	now := c.now().UTC()
	res := &v1.AcquireResult{ClientArch: c.arch, EstMemoryBytes: req.EstMemoryBytes, Pinned: in.Pinned, Attempts: dec.Attempts}
	if res.Attempts == nil {
		res.Attempts = []v1.Attempt{}
	}
	warnings := slices.Clone(dec.Warnings)
	var cl v1.ClientLease
	switch dec.Placement {
	case v1.PlacementRemote:
		n, err := c.node(dec.Node.Name)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, c.ensureContext(ctx, n.ref)...)
		l := dec.Lease
		labels := l.Labels
		if len(labels) == 0 {
			labels = v1.LeaseLabels(l.ID, c.owner, c.session, project)
		}
		created := l.CreatedAt
		if created.IsZero() {
			created = now
		}
		cl = v1.ClientLease{
			ID: l.ID, Placement: v1.PlacementRemote, Node: n.ref.Name, DockerContext: n.ref.DockerContext,
			State: v1.StateActive, Owner: c.owner, Session: c.session, Project: project, Workload: w,
			EstMemoryBytes: req.EstMemoryBytes, Pinned: l.Pinned || in.Pinned,
			CreatedAt: rfc3339(created), ExpiresAt: rfc3339(l.ExpiresAt),
			Labels: labels, Reason: dec.Reason, Resources: l.Resources,
		}
		res.NodeArch = dec.NodeArch
	default:
		id := v1.NewLeaseID()
		cl = v1.ClientLease{
			ID: id, Placement: v1.PlacementLocal, DockerContext: c.localCtx, State: v1.StateActive,
			Owner: c.owner, Session: c.session, Project: project, Workload: w,
			EstMemoryBytes: req.EstMemoryBytes, Pinned: in.Pinned, CreatedAt: rfc3339(now),
			Labels: v1.LeaseLabels(id, c.owner, c.session, project), Reason: dec.Reason,
		}
		if in.TTLMinutes > 0 {
			cl.ExpiresAt = rfc3339(now.Add(time.Duration(in.TTLMinutes) * time.Minute))
		}
		if c.localWarn != "" {
			warnings = append(warnings, c.localWarn)
		}
	}
	if in.AllowWake {
		warnings = append(warnings, "allow_wake is accepted but Wake-on-LAN is not implemented yet (planned for v1.1)")
	}
	if err := c.store.PutLease(cl); err != nil {
		if cl.Placement == v1.PlacementRemote {
			n, _ := c.node(cl.Node)
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = n.api.ReleaseLease(rctx, cl.ID, false)
			cancel()
		}
		return nil, fmt.Errorf("saving lease %s to %s: %w", cl.ID, c.store.Dir(), err)
	}
	if c.cfg.Proxy.Enabled {
		host, err := c.startProxy(ctx, cl)
		if err != nil {
			warnings = append(warnings, "docker API proxy (proxy.enabled) could not be started; use the docker context prefix: "+err.Error())
		}
		res.DockerHost = host
	}
	res.PlacementInfo = c.placementInfo(cl)
	res.Warnings = append(res.Warnings, warnings...)
	res.NextSteps = nextSteps(cl, res.Commands)
	res.Labels = cl.Labels
	res.DockerContext = cl.DockerContext
	res.ExpiresAt = cl.ExpiresAt
	c.log.Info("lease acquired", "lease", cl.ID, "placement", cl.Placement, "node", cl.Node, "reason", cl.Reason)
	return res, nil
}

func nextSteps(l v1.ClientLease, cmd v1.Commands) []string {
	steps := []string{
		fmt.Sprintf("Prefix EVERY docker command with `%s` (and compose with `%s`); environment variables do not persist between commands.", cmd.Docker, cmd.Compose),
	}
	switch l.Workload {
	case v1.WorkloadCompose:
		steps = append(steps, fmt.Sprintf("Add the `labels` to every service, network and volume in the compose file, then run `%s -p <project> up -d`.", cmd.Compose))
	case v1.WorkloadKind:
		steps = append(steps, "Create the cluster with dol_kind_create (lease_id "+l.ID+"); it returns the kubectl prefix to use.")
	case v1.WorkloadBuild:
		steps = append(steps, fmt.Sprintf("Build with `%s build %s -t <tag> .` so the image is built natively where it will run and removed on release.", cmd.Docker, cmd.LabelFlags))
	default:
		steps = append(steps, fmt.Sprintf("Run containers with `%s run -d %s -p 127.0.0.1:<port>:<port> <image>`.", cmd.Docker, cmd.LabelFlags))
	}
	if cmd.DockerProxied != "" {
		steps = append(steps, fmt.Sprintf("Alternatively use `%s` (and `%s`): it adds the lease labels to containers, networks and volumes and rewrites bind mounts under the project directory for you (call dol_sync first on a remote lease). Images built with BuildKit are not labeled.",
			cmd.DockerProxied, cmd.ComposeProxied))
	}
	steps = append(steps,
		"Add the label_flags to every `docker run`, `docker build`, `docker network create` and `docker volume create`; dol only stops, removes and cleans up labeled resources.",
		"When finished, call dol_release with lease_id "+l.ID+".")
	return steps
}

// ---- release ----

// Release tears down a lease (remote via the node, local via label-scoped
// docker commands), closes its forwards and deletes its kubeconfigs.
func (c *Core) Release(ctx context.Context, id string, keepSyncDir bool) (*v1.ReleaseResult, error) {
	l, err := c.store.GetLease(strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	res := &v1.ReleaseResult{LeaseID: l.ID, Placement: l.Placement, Node: l.Node, Forwards: []int{}, Warnings: []string{}}
	c.stopProxy(l.ID)
	closed, err := c.fwd.CloseLease(ctx, *l)
	if err != nil {
		res.Warnings = append(res.Warnings, "closing forwards: "+err.Error())
	}
	res.Forwards = append(res.Forwards, closed...)

	keepRecord := false
	switch {
	case l.State == v1.StateLost:
		res.Warnings = append(res.Warnings, "the lease was already lost on the node; only the local record was cleared")
	case l.Placement == v1.PlacementRemote:
		n, err := c.node(l.Node)
		if err != nil {
			return nil, err
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // teardown of kind clusters takes a while
		rr, err := n.api.ReleaseLease(rctx, l.ID, keepSyncDir)
		cancel()
		switch code := v1.CodeOf(err); {
		case err == nil:
			res.Removed = rr.Removed
			if len(rr.Removed.Errors) > 0 {
				res.Warnings = append(res.Warnings, "the node could not remove everything yet; its reaper retries: "+strings.Join(rr.Removed.Errors, "; "))
			}
		case code == v1.CodeNotFound || code == v1.CodeLeaseGone:
			res.Warnings = append(res.Warnings, "the node no longer had this lease (already released or expired)")
		default:
			// Stop heartbeating it; a later dol_release retries.
			_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error { x.State = v1.StateReleasing; return nil })
			e, _ := v1.AsError(err)
			hint := "retry dol_release later; without heartbeats the node's reaper removes everything after its grace period"
			msg := err.Error()
			if e != nil {
				msg = e.Message
			}
			return nil, v1.Errorf(cmp.Or(code, v1.CodeInternal), hint, "could not release lease %s on node %s: %s", l.ID, l.Node, msg)
		}
	default:
		res.Removed = c.teardownLocal(ctx, *l)
		if len(res.Removed.Errors) > 0 {
			keepRecord = true
			res.Warnings = append(res.Warnings, "some local resources could not be removed; call dol_release again: "+strings.Join(res.Removed.Errors, "; "))
		}
	}
	c.removeLeaseFiles(l, res)
	if keepRecord {
		_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error { x.State = v1.StateFailed; return nil })
	} else if err := c.store.DeleteLease(l.ID); err != nil {
		res.Warnings = append(res.Warnings, "deleting lease record: "+err.Error())
	}
	c.log.Info("lease released", "lease", l.ID, "placement", l.Placement)
	return res, nil
}

func (c *Core) removeLeaseFiles(l *v1.ClientLease, res *v1.ReleaseResult) {
	if _, err := c.store.RemoveLeaseFiles(l.ID); err != nil && res != nil {
		res.Warnings = append(res.Warnings, "deleting kubeconfigs/compose files: "+err.Error())
	}
	for _, p := range l.Kubeconfigs {
		if c.store.Contains(p) { // never delete outside the state dir
			_ = os.Remove(p)
		}
	}
}

// teardownLocal removes the local resources of a lease: tracked compose
// projects first, then every container/network/volume carrying the lease
// label (brief §7.6 order).
func (c *Core) teardownLocal(ctx context.Context, l v1.ClientLease) v1.Inventory {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	d := c.docker(l.DockerContext)
	var compose, kinds []string
	var errs []string
	for _, k := range l.Resources.KindClusters {
		if _, err := c.kindCmd(tctx, l.DockerContext, "delete", "cluster", "--name", k.ClusterName); err != nil {
			errs = append(errs, fmt.Sprintf("kind cluster %s: %v", k.ClusterName, err))
			continue
		}
		kinds = append(kinds, k.ClusterName)
	}
	for _, p := range l.Resources.ComposeProjects {
		if err := d.RequireComposeOwned(tctx, p, l.ID); err != nil {
			if v1.CodeOf(err) != v1.CodeNotOwned {
				errs = append(errs, fmt.Sprintf("compose project %s: %v", p, err))
			} else {
				// The lease's own share is removed by label below.
				c.log.Warn("not bringing down compose project with foreign resources", "lease", l.ID, "project", p, "err", err)
			}
			continue
		}
		if _, err := d.ComposeProject(tctx, p, "down", "--volumes", "--remove-orphans", "--timeout", "10"); err != nil {
			errs = append(errs, fmt.Sprintf("compose project %s: %v", p, err))
			continue
		}
		compose = append(compose, p)
	}
	inv := d.TeardownLease(tctx, l.ID)
	inv.ComposeProjects = compose
	inv.KindClusters = kinds
	inv.Errors = append(errs, inv.Errors...)
	return inv
}

// ---- stop / remove ----

// TargetInput selects a resource of a lease.
type TargetInput struct {
	LeaseID string `json:"lease_id" jsonschema:"lease id from dol_acquire, e.g. l-abcdefghij"`
	Target  string `json:"target" jsonschema:"container name or id, or a compose project registered on the lease"`
}

// RemoveInput is the input of dol_remove.
type RemoveInput struct {
	TargetInput
	Volumes bool `json:"volumes,omitempty" jsonschema:"also remove the container's anonymous volumes / the compose project's volumes"`
}

func (c *Core) hasCompose(l *v1.ClientLease, target string) bool {
	return slices.Contains(l.Resources.ComposeProjects, target)
}

// Stop stops an owned container or registered compose project.
func (c *Core) Stop(ctx context.Context, in TargetInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(in.Target)
	if t == "" || strings.HasPrefix(t, "-") {
		return nil, badInput("target %q is invalid: pass a container name/id or compose project (see dol_list)", in.Target)
	}
	d := c.docker(l.DockerContext)
	res := &v1.ActionResult{PlacementInfo: c.placementInfo(*l), Done: []string{}}
	if c.hasCompose(l, t) {
		if err := d.RequireComposeOwned(ctx, t, l.ID); err != nil {
			return nil, err
		}
		if _, err := d.ComposeProject(ctx, t, "stop"); err != nil {
			return nil, fmt.Errorf("stopping compose project %s: %w", t, err)
		}
		res.Done = append(res.Done, "stopped compose project "+t)
		return res, nil
	}
	if err := d.StopOwned(ctx, t, l.ID); err != nil {
		return nil, err
	}
	res.Done = append(res.Done, "stopped container "+t)
	return res, nil
}

// Remove removes an owned container/network/volume or brings down a
// registered compose project.
func (c *Core) Remove(ctx context.Context, in RemoveInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(in.Target)
	if t == "" || strings.HasPrefix(t, "-") {
		return nil, badInput("target %q is invalid: pass a container/network/volume name or id, or a compose project (see dol_list)", in.Target)
	}
	d := c.docker(l.DockerContext)
	res := &v1.ActionResult{PlacementInfo: c.placementInfo(*l), Done: []string{}}
	if c.hasCompose(l, t) {
		if err := d.RequireComposeOwned(ctx, t, l.ID); err != nil {
			return nil, err
		}
		args := []string{"down", "--remove-orphans"}
		if in.Volumes {
			args = append(args, "--volumes")
		}
		if _, err := d.ComposeProject(ctx, t, args...); err != nil {
			return nil, fmt.Errorf("removing compose project %s: %w", t, err)
		}
		_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
			x.Resources.ComposeProjects = slices.DeleteFunc(x.Resources.ComposeProjects, func(p string) bool { return p == t })
			return nil
		})
		res.Done = append(res.Done, "removed compose project "+t)
		return res, nil
	}
	var lastErr error
	for _, k := range []dockerx.ObjectKind{dockerx.KindContainer, dockerx.KindNetwork, dockerx.KindVolume} {
		err := d.RemoveOwned(ctx, k, t, l.ID, in.Volumes)
		if err == nil {
			res.Done = append(res.Done, fmt.Sprintf("removed %s %s", k, t))
			return res, nil
		}
		if v1.CodeOf(err) != v1.CodeNotFound {
			return nil, err
		}
		lastErr = err
	}
	return nil, v1.Errorf(v1.CodeNotFound, "list the lease's resources with dol_list",
		"no container, network or volume %q on %s (%v)", t, l.DockerContext, lastErr)
}

// ---- resize / pin ----

// ResizeInput is the input of dol_resize.
type ResizeInput struct {
	LeaseID     string  `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	EstMemoryGB float64 `json:"est_memory_gb" jsonschema:"new total memory estimate in GB"`
}

// Resize changes a lease's memory estimate.
func (c *Core) Resize(ctx context.Context, in ResizeInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	if !(in.EstMemoryGB > 0) || in.EstMemoryGB > 4096 {
		return nil, badInput("est_memory_gb must be between 0 and 4096 (got %v)", in.EstMemoryGB)
	}
	b := v1.GBToBytes(in.EstMemoryGB)
	var warn []string
	if l.Placement == v1.PlacementRemote {
		n, err := c.node(l.Node)
		if err != nil {
			return nil, err
		}
		pctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS)*time.Millisecond)
		_, err = n.api.PatchLease(pctx, l.ID, v1.LeasePatch{EstMemoryBytes: &b})
		cancel()
		if err != nil {
			if e, ok := v1.AsError(err); ok && e.Code.IsRefusal() {
				return nil, v1.Errorf(e.Code,
					"keep the current size ("+v1.FormatBytes(l.EstMemoryBytes)+"), free memory with dol_release on other leases, or release this lease and dol_acquire again (may fall back to local)",
					"node %s refused to resize lease %s to %s: %s", l.Node, l.ID, v1.FormatBytes(b), e.Message)
			}
			return nil, c.markIfGone(ctx, l, err)
		}
	} else if lim := v1.GBToBytes(c.cfg.Local.MaxMemoryGB); lim > 0 && b > lim {
		warn = append(warn, fmt.Sprintf("%s locally exceeds local.max_memory_gb (%s); keep the footprint small", v1.FormatBytes(b), v1.FormatBytes(lim)))
	}
	nl, err := c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error { x.EstMemoryBytes = b; return nil })
	if err != nil {
		return nil, err
	}
	res := &v1.ActionResult{PlacementInfo: c.placementInfo(*nl), Done: []string{fmt.Sprintf("resized lease %s from %s to %s", l.ID, v1.FormatBytes(l.EstMemoryBytes), v1.FormatBytes(b))}}
	res.Warnings = append(res.Warnings, warn...)
	return res, nil
}

// PinInput is the input of dol_pin.
type PinInput struct {
	LeaseID    string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	Pinned     bool   `json:"pinned" jsonschema:"true keeps the lease after this session ends; false makes it session-bound again"`
	TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"optional hard expiry in minutes for a pinned lease"`
}

// Pin pins or unpins a lease.
func (c *Core) Pin(ctx context.Context, in PinInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	if in.TTLMinutes < 0 {
		return nil, badInput("ttl_minutes must be >= 0")
	}
	ttl := int64(in.TTLMinutes) * 60
	expires := ""
	if ttl > 0 {
		expires = rfc3339(c.now().Add(time.Duration(ttl) * time.Second))
	}
	if l.Placement == v1.PlacementRemote {
		n, err := c.node(l.Node)
		if err != nil {
			return nil, err
		}
		p := v1.LeasePatch{Pinned: &in.Pinned}
		if ttl > 0 {
			p.TTLSeconds = &ttl
		}
		pctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS)*time.Millisecond)
		nl, err := n.api.PatchLease(pctx, l.ID, p)
		cancel()
		if err != nil {
			return nil, c.markIfGone(ctx, l, err)
		}
		if !nl.ExpiresAt.IsZero() {
			expires = rfc3339(nl.ExpiresAt)
		}
	}
	nl, err := c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		x.Pinned = in.Pinned
		if expires != "" || !in.Pinned {
			x.ExpiresAt = expires
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	verb := "pinned"
	if !in.Pinned {
		verb = "unpinned"
	}
	done := fmt.Sprintf("%s lease %s", verb, l.ID)
	if expires != "" {
		done += " until " + expires
	}
	res := &v1.ActionResult{PlacementInfo: c.placementInfo(*nl), Done: []string{done}}
	if in.Pinned {
		res.NextSteps = append(res.NextSteps, "The lease survives the end of this session; release it with dol_release or `dol release "+l.ID+"` when no longer needed.")
	}
	return res, nil
}

// markIfGone converts 404/410 from the node into a lost lease.
func (c *Core) markIfGone(ctx context.Context, l *v1.ClientLease, err error) error {
	code := v1.CodeOf(err)
	if code != v1.CodeNotFound && code != v1.CodeLeaseGone {
		return err
	}
	nl, uerr := c.markLost(ctx, l.ID, err)
	if uerr != nil {
		return err
	}
	return lostError(nl)
}

func (c *Core) markLost(ctx context.Context, id string, cause error) (*v1.ClientLease, error) {
	msg := cause.Error()
	if e, ok := v1.AsError(cause); ok {
		msg = e.Message
	}
	return c.store.UpdateLease(ctx, id, func(x *v1.ClientLease) error {
		x.State = v1.StateLost
		x.Reason = fmt.Sprintf("the node answered %s at %s (%s)", v1.CodeOf(cause), c.now().UTC().Format(time.RFC3339), msg)
		return nil
	})
}
