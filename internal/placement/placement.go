// Package placement decides where a workload runs (brief §7.5): try nodes by
// priority with short health checks and a bounded lease request, then apply
// the fallback policy. It never blocks the agent for longer than the
// configured timeouts.
//
// Health cache (ADR-0020, amended): placement always probes health fresh so a
// frozen node costs at most health_timeout; successful results are cached
// for CacheTTL (15 s) only for read-only views (dol_status/metadata).
// Failures are never cached.
package placement

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// NodeAPI is the subset of nodeclient.Client used for placement.
type NodeAPI interface {
	Health(ctx context.Context) (*v1.Health, error)
	CreateLease(ctx context.Context, req v1.LeaseRequest) (*v1.Lease, error)
}

// Node is a configured node.
type Node struct {
	Name          string
	SSHHost       string
	DockerContext string
	Priority      int
	API           NodeAPI
}

// Policy mirrors config.Policy plus local limits.
type Policy struct {
	Prefer              string // remote | local
	Fallback            string // always | if_capacity | never
	HealthTimeout       time.Duration
	LeaseRequestTimeout time.Duration
	LocalMaxMemoryBytes int64 // advisory; 0 = no warning
}

// LocalProbe reports the local Docker VM's total memory (docker info
// MemTotal) and the memory already promised to active local leases.
type LocalProbe interface {
	MemTotal(ctx context.Context) (int64, error)
	Committed() int64
}

// Placer places workloads. It is safe for concurrent use.
type Placer struct {
	Nodes        []Node
	Policy       Policy
	LocalContext string
	Local        LocalProbe
	ClientArch   string
	CacheTTL     time.Duration // default 15s
	Now          func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	h  *v1.Health
	at time.Time
}

// Request is what to place.
type Request struct {
	Lease  v1.LeaseRequest
	Prefer string // overrides Policy.Prefer when set
}

// Decision is the outcome of a successful placement.
type Decision struct {
	Placement v1.Placement
	Node      *Node     // nil for local
	Lease     *v1.Lease // remote lease; nil for local
	NodeArch  string
	Reason    string
	Attempts  []v1.Attempt
	Warnings  []string
}

func (p *Placer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Placer) ttl() time.Duration {
	if p.CacheTTL > 0 {
		return p.CacheTTL
	}
	return 15 * time.Second
}

// SortedNodes returns nodes by priority desc, then name.
func (p *Placer) SortedNodes() []Node {
	ns := slices.Clone(p.Nodes)
	slices.SortStableFunc(ns, func(a, b Node) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return ns
}

// Health checks a node within Policy.HealthTimeout. With fresh=false a
// success younger than CacheTTL is reused.
func (p *Placer) Health(ctx context.Context, n Node, fresh bool) (*v1.Health, error) {
	if !fresh {
		p.mu.Lock()
		c, ok := p.cache[n.Name]
		p.mu.Unlock()
		if ok && p.now().Sub(c.at) < p.ttl() {
			return c.h, nil
		}
	}
	hctx, cancel := context.WithTimeout(ctx, p.Policy.HealthTimeout)
	defer cancel()
	h, err := n.API.Health(hctx)
	if err == nil && hctx.Err() != nil {
		err = v1.Errorf(v1.CodeTimeout, "run `dol doctor`", "node %s did not answer within %s", n.Name, p.Policy.HealthTimeout)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]cached{}
	}
	if err != nil {
		delete(p.cache, n.Name)
		return nil, err
	}
	p.cache[n.Name] = cached{h: h, at: p.now()}
	return h, nil
}

// Invalidate drops a node's cached health.
func (p *Placer) Invalidate(name string) {
	p.mu.Lock()
	delete(p.cache, name)
	p.mu.Unlock()
}

// Place runs the §7.5 algorithm. Errors are *v1.Error with code
// CodeNoPlacement or CodeLocalDockerDown and list every attempt.
func (p *Placer) Place(ctx context.Context, req Request) (*Decision, error) {
	prefer := cmp.Or(req.Prefer, p.Policy.Prefer, "remote")
	est := req.Lease.EstMemoryBytes
	var attempts []v1.Attempt

	if prefer == "local" {
		start := p.now()
		ok, why := p.localFits(ctx, est)
		if ok {
			return p.local(est, "prefer=local and local Docker has room: "+why, attempts), nil
		}
		attempts = append(attempts, v1.Attempt{Node: "local", Outcome: "skipped", Message: why, Millis: ms(p.now().Sub(start))})
	}

	for _, n := range p.SortedNodes() {
		a, lease, h := p.tryNode(ctx, n, req.Lease)
		attempts = append(attempts, a)
		if lease == nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		node := n
		d := &Decision{Placement: v1.PlacementRemote, Node: &node, Lease: lease, NodeArch: h.Arch, Attempts: attempts}
		d.Reason = fmt.Sprintf("node %s accepted the lease (%s)", n.Name, v1.FormatBytes(est))
		if prior := failures(attempts[:len(attempts)-1]); prior != "" {
			d.Reason += "; skipped " + prior
		}
		if h.Arch != "" && p.ClientArch != "" && h.Arch != p.ClientArch {
			d.Warnings = append(d.Warnings, fmt.Sprintf(
				"node %s is %s but this machine is %s: build where you run — build images with the returned docker prefix (on the node); never `docker save | docker load` images across machines",
				n.Name, h.Arch, p.ClientArch))
		}
		return d, nil
	}

	why := failures(attempts)
	if len(p.Nodes) == 0 {
		why = "no nodes are configured (run `dol init` to add one)"
	}
	switch p.Policy.Fallback {
	case "never":
		if prefer == "local" { // local was the preference and did not fit
			return nil, p.noPlacement(why, attempts, "policy.fallback is never")
		}
		return nil, p.noPlacement(why, attempts, "policy.fallback is never, so dol did not fall back to local Docker")
	case "if_capacity":
		if prefer == "local" { // already checked above and it did not fit
			return nil, p.noPlacement(why, attempts, "local Docker has no room either (policy.fallback=if_capacity)")
		}
		start := p.now()
		ok, lwhy := p.localFits(ctx, est)
		attempts = append(attempts, v1.Attempt{Node: "local", Outcome: map[bool]string{true: "placed", false: "refused"}[ok], Message: lwhy, Millis: ms(p.now().Sub(start))})
		if !ok {
			err := p.noPlacement(why+"; local: "+lwhy, attempts, "policy.fallback=if_capacity and local Docker has no room")
			if strings.Contains(lwhy, "unreachable") {
				err.Code = v1.CodeLocalDockerDown
				err.Hint = "start local Docker (Docker Desktop / OrbStack / Colima) or fix the node with `dol doctor`"
			}
			return nil, err
		}
		return p.local(est, "fell back to local Docker ("+lwhy+"): "+why, attempts), nil
	default: // always
		return p.local(est, "fell back to local Docker: "+why, attempts), nil
	}
}

func (p *Placer) local(est int64, reason string, attempts []v1.Attempt) *Decision {
	d := &Decision{Placement: v1.PlacementLocal, Reason: reason, Attempts: attempts}
	if lim := p.Policy.LocalMaxMemoryBytes; lim > 0 && est > lim {
		d.Warnings = append(d.Warnings, fmt.Sprintf(
			"running %s locally exceeds local.max_memory_gb (%s): this laptop may slow down; keep the footprint small or retry dol_acquire when a node is available",
			v1.FormatBytes(est), v1.FormatBytes(lim)))
	}
	return d
}

func (p *Placer) noPlacement(why string, attempts []v1.Attempt, policy string) *v1.Error {
	return v1.Errorf(v1.CodeNoPlacement,
		"run `dol doctor` to fix the node, lower est_memory_gb, release unused leases (dol_list / dol_release), or change policy.fallback",
		"no placement for this workload: %s; %s", why, policy).
		WithDetails(map[string]any{"attempts": attempts})
}

func (p *Placer) tryNode(ctx context.Context, n Node, req v1.LeaseRequest) (v1.Attempt, *v1.Lease, *v1.Health) {
	start := p.now()
	a := v1.Attempt{Node: n.Name}
	done := func(outcome string, err error) (v1.Attempt, *v1.Lease, *v1.Health) {
		a.Outcome = outcome
		if err != nil {
			a.Code = v1.CodeOf(err)
			if e, ok := v1.AsError(err); ok {
				a.Message = e.Message
			} else {
				a.Message = err.Error()
			}
		}
		a.Millis = ms(p.now().Sub(start))
		return a, nil, nil
	}
	// Always probe fresh before asking for a lease (ADR-0020, amended): a
	// cached success would let a node that froze in the last CacheTTL cost
	// lease_request_timeout instead of health_timeout. A probe over an
	// existing ssh ControlMaster costs milliseconds.
	h, err := p.Health(ctx, n, true)
	if err != nil {
		return done(outcomeFor(err), err)
	}
	if !h.Accepting {
		return done("refused", v1.Errorf(v1.CodeDraining, "", "node %s is not accepting new leases (draining)", n.Name))
	}
	lctx, cancel := context.WithTimeout(ctx, p.Policy.LeaseRequestTimeout)
	defer cancel()
	lease, err := n.API.CreateLease(lctx, req)
	if err == nil && lctx.Err() != nil {
		err = v1.Errorf(v1.CodeTimeout, "", "node %s did not answer the lease request within %s", n.Name, p.Policy.LeaseRequestTimeout)
	}
	if err != nil {
		o := outcomeFor(err)
		if o == "unreachable" {
			p.Invalidate(n.Name)
		}
		return done(o, err)
	}
	a.Outcome = "placed"
	a.Millis = ms(p.now().Sub(start))
	return a, lease, h
}

func outcomeFor(err error) string {
	switch c := v1.CodeOf(err); {
	case c == v1.CodeUnreachable || c == v1.CodeTimeout:
		return "unreachable"
	case c.IsRefusal():
		return "refused"
	default:
		return "error"
	}
}

// localFits checks local Docker's MemTotal minus memory promised to active
// local leases against est.
func (p *Placer) localFits(ctx context.Context, est int64) (bool, string) {
	if p.Local == nil {
		return false, "local Docker probe not configured"
	}
	lctx, cancel := context.WithTimeout(ctx, max(p.Policy.LeaseRequestTimeout, 3*time.Second))
	defer cancel()
	total, err := p.Local.MemTotal(lctx)
	if err != nil {
		return false, fmt.Sprintf("local Docker (context %s) is unreachable: %v", p.LocalContext, err)
	}
	committed := p.Local.Committed()
	free := total - committed
	if est > free {
		return false, fmt.Sprintf("local Docker has %s free (%s total, %s promised to other local leases), %s requested",
			v1.FormatBytes(max(free, 0)), v1.FormatBytes(total), v1.FormatBytes(committed), v1.FormatBytes(est))
	}
	return true, fmt.Sprintf("local Docker has %s free of %s", v1.FormatBytes(free), v1.FormatBytes(total))
}

// failures renders failed attempts as "tower unreachable: …; b refused: …".
func failures(as []v1.Attempt) string {
	var parts []string
	for _, a := range as {
		if a.Outcome == "placed" {
			continue
		}
		msg := a.Message
		if a.Code != "" && !strings.Contains(msg, string(a.Code)) {
			msg = string(a.Code) + ": " + msg
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", a.Node, a.Outcome, msg))
	}
	return strings.Join(parts, "; ")
}

func ms(d time.Duration) int64 { return d.Milliseconds() }
