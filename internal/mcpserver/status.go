package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// StatusOptions tune Status.
type StatusOptions struct {
	// AllSessions lists leases of every session (the CLI) instead of only
	// this session's (the MCP tool).
	AllSessions bool
}

// Status checks every node (fresh health + capacity, in parallel, bounded by
// health_timeout_ms) and local Docker, and lists leases.
func (c *Core) Status(ctx context.Context, o StatusOptions) *v1.StatusResult {
	res := &v1.StatusResult{
		Owner: c.owner, Session: c.session, Nodes: []v1.NodeStatus{}, Leases: []v1.ClientLease{},
		Policy: fmt.Sprintf("prefer=%s fallback=%s health_timeout_ms=%d", c.cfg.Policy.Prefer, c.cfg.Policy.Fallback, c.cfg.Policy.HealthTimeoutMS),
	}
	var wg sync.WaitGroup
	nodes := c.placer.SortedNodes()
	res.Nodes = make([]v1.NodeStatus, len(nodes))
	for i, n := range nodes {
		wg.Go(func() {
			start := time.Now()
			ns := v1.NodeStatus{Name: n.Name}
			h, err := c.placer.Health(ctx, n, true)
			if err == nil {
				ns.Healthy, ns.Health = true, h
				cctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.HealthTimeoutMS)*time.Millisecond)
				if capy, err := c.nodes[n.Name].api.Capacity(cctx); err == nil {
					ns.Capacity = capy
				} else {
					ns.Error = "capacity: " + errText(err)
				}
				cancel()
			} else {
				ns.Error = errText(err)
			}
			ns.Millis = time.Since(start).Milliseconds()
			res.Nodes[i] = ns
		})
	}
	wg.Go(func() {
		res.Local = v1.LocalStatus{DockerContext: c.localCtx}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		info, err := c.docker(c.localCtx).Info(lctx)
		if err != nil {
			res.Local.Error = errText(err)
			return
		}
		res.Local.Reachable, res.Local.MemTotalBytes, res.Local.Arch = true, info.MemTotal, info.GoArch()
	})
	wg.Wait()
	for _, l := range c.leases(o.AllSessions) {
		res.Leases = append(res.Leases, l)
	}
	return res
}

func errText(err error) string {
	if e, ok := v1.AsError(err); ok {
		s := e.Message
		if e.Hint != "" {
			s += " (" + e.Hint + ")"
		}
		return s
	}
	return err.Error()
}

// leases returns non-released leases of this owner (this session only unless
// all).
func (c *Core) leases(all bool) []v1.ClientLease {
	ls, _ := c.store.ListLeases()
	out := []v1.ClientLease{}
	for _, l := range ls {
		if l.Owner != c.owner || l.State == v1.StateReleased {
			continue
		}
		if !all && l.Session != c.session {
			continue
		}
		if l.Labels == nil {
			l.Labels = map[string]string{}
		}
		out = append(out, l)
	}
	return out
}

// ListInput is the input of dol_list.
type ListInput struct {
	LeaseID     string `json:"lease_id,omitempty" jsonschema:"only this lease; default: every lease of this session"`
	AllSessions bool   `json:"all_sessions,omitempty" jsonschema:"include leases of other Claude Code sessions of this user"`
}

// List returns leases and their label-scoped resources on each Docker host.
func (c *Core) List(ctx context.Context, in ListInput) (*v1.ListResult, error) {
	res := &v1.ListResult{Leases: []v1.ClientLease{}, Resources: []v1.Resource{}, Warnings: []string{}}
	var filters []string
	if id := strings.TrimSpace(in.LeaseID); id != "" {
		l, err := c.store.GetLease(id)
		if err != nil {
			return nil, err
		}
		if l.Labels == nil {
			l.Labels = map[string]string{}
		}
		res.Leases = append(res.Leases, *l)
		filters = []string{v1.LeaseFilter(l.ID)}
	} else {
		res.Leases = c.leases(in.AllSessions)
		filters = []string{v1.ManagedFilter, "label=" + v1.LabelOwner + "=" + c.owner}
		if !in.AllSessions {
			filters = append(filters, "label="+v1.LabelSession+"="+c.session)
		}
	}
	var contexts []string
	for _, l := range res.Leases {
		if l.State == v1.StateLost {
			res.Warnings = append(res.Warnings, lostError(&l).Error())
			continue
		}
		if !slices.Contains(contexts, l.DockerContext) {
			contexts = append(contexts, l.DockerContext)
		}
		for _, p := range l.Resources.ComposeProjects {
			res.Resources = append(res.Resources, v1.Resource{Kind: "compose_project", Name: p, LeaseID: l.ID})
		}
		for _, k := range l.Resources.KindClusters {
			res.Resources = append(res.Resources, v1.Resource{Kind: "kind_cluster", Name: k.ClusterName, LeaseID: l.ID})
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, dc := range contexts {
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			rs, err := listResources(lctx, c.docker(dc), filters)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not list resources on docker context %s: %v", dc, err))
			}
			res.Resources = append(res.Resources, rs...)
		})
	}
	wg.Wait()
	return res, nil
}

func listResources(ctx context.Context, d *dockerx.Docker, filters []string) ([]v1.Resource, error) {
	var out []v1.Resource
	cs, err := d.Containers(ctx, filters...)
	if err != nil {
		return nil, err
	}
	for _, ct := range cs {
		lb := dockerx.ParseLabels(ct.RawLabels)
		out = append(out, v1.Resource{Kind: "container", ID: short(ct.ID), Name: ct.Names, State: ct.State,
			Image: ct.Image, Ports: ct.Ports, LeaseID: lb[v1.LabelLease]})
	}
	ns, err := d.Networks(ctx, filters...)
	if err != nil {
		return out, err
	}
	for _, n := range ns {
		out = append(out, v1.Resource{Kind: "network", ID: short(n.ID), Name: n.Name, LeaseID: dockerx.ParseLabels(n.RawLabels)[v1.LabelLease]})
	}
	vs, err := d.Volumes(ctx, filters...)
	if err != nil {
		return out, err
	}
	for _, v := range vs {
		out = append(out, v1.Resource{Kind: "volume", Name: v.Name, LeaseID: dockerx.ParseLabels(v.RawLabels)[v1.LabelLease]})
	}
	return out, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
