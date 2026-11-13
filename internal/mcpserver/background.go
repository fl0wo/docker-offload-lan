package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// ---- heartbeats ----

func (c *Core) heartbeatLoop(ctx context.Context) {
	iv := time.Duration(c.cfg.Policy.HeartbeatIntervalS) * time.Second
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.HeartbeatOnce(ctx)
		}
	}
}

// HeartbeatReport says what one heartbeat round did.
type HeartbeatReport struct {
	OK, Lost, Failed []string
}

// HeartbeatOnce heartbeats every active remote lease of this session, in
// parallel. 404/410 marks the lease lost; transport errors are logged and
// retried next round (a node that is briefly unreachable must not lose
// leases on the client side; the node's grace period decides).
func (c *Core) HeartbeatOnce(ctx context.Context) HeartbeatReport {
	ls, _ := c.store.ListLeases()
	var (
		mu  sync.Mutex
		rep HeartbeatReport
		wg  sync.WaitGroup
	)
	timeout := time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS) * time.Millisecond
	for _, l := range ls {
		if l.Session != c.session || l.Placement != v1.PlacementRemote || l.State != v1.StateActive {
			continue
		}
		n, ok := c.nodes[l.Node]
		if !ok {
			continue
		}
		wg.Go(func() {
			hctx, cancel := context.WithTimeout(ctx, timeout)
			hb, err := n.api.Heartbeat(hctx, l.ID)
			cancel()
			mu.Lock()
			defer mu.Unlock()
			switch code := v1.CodeOf(err); {
			case err == nil:
				rep.OK = append(rep.OK, l.ID)
				if exp := rfc3339(hb.ExpiresAt); exp != "" && exp != l.ExpiresAt {
					_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
						if x.State == v1.StateActive {
							x.ExpiresAt = exp
						}
						return nil
					})
				}
			case code == v1.CodeNotFound || code == v1.CodeLeaseGone:
				rep.Lost = append(rep.Lost, l.ID)
				_, _ = c.markLost(ctx, l.ID, err)
				c.log.Warn("lease lost", "lease", l.ID, "node", l.Node, "err", err)
			default:
				rep.Failed = append(rep.Failed, l.ID)
				c.log.Warn("heartbeat failed; will retry", "lease", l.ID, "node", l.Node, "err", err)
			}
		})
	}
	wg.Wait()
	return rep
}

// ---- dead-session cleanup ----

// CleanupReport says what CleanupDeadSessions did.
type CleanupReport struct {
	DeadSessions []string
	TornDown     map[string]v1.Inventory // local lease → removed
	Forgotten    []string                // remote lease records the node no longer has
	Errors       []string
}

func (r CleanupReport) empty() bool {
	return len(r.DeadSessions)+len(r.TornDown)+len(r.Forgotten)+len(r.Errors) == 0
}

func (r CleanupReport) String() string {
	var ids []string
	for id := range r.TornDown {
		ids = append(ids, id)
	}
	return fmt.Sprintf("dead sessions %v, local leases torn down %v, remote records forgotten %v, errors %v",
		r.DeadSessions, ids, r.Forgotten, r.Errors)
}

// CleanupDeadSessions tears down LOCAL leases whose owning session is dead
// (PID gone or start time changed, or no session record), except pinned
// leases that have not reached their expiry. Remote lease records of dead
// sessions are only forgotten once the node confirms the lease is gone; the
// node's reaper owns remote cleanup.
func (c *Core) CleanupDeadSessions(ctx context.Context) CleanupReport {
	rep := CleanupReport{TornDown: map[string]v1.Inventory{}}
	dead, err := c.store.PruneDeadSessions()
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	for _, s := range dead {
		rep.DeadSessions = append(rep.DeadSessions, s.ID)
	}
	ls, _ := c.store.ListLeases()
	alive := map[string]bool{c.session: true}
	isAlive := func(s string) bool {
		if v, ok := alive[s]; ok {
			return v
		}
		alive[s] = c.store.SessionAlive(s)
		return alive[s]
	}
	now := c.now()
	for _, l := range ls {
		if isAlive(l.Session) {
			continue
		}
		switch l.Placement {
		case v1.PlacementLocal:
			if l.Pinned && !expired(l.ExpiresAt, now) {
				continue
			}
			inv := c.teardownLocal(ctx, l)
			if len(inv.Errors) > 0 {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %s", l.ID, strings.Join(inv.Errors, "; ")))
				continue // retried by the next dol mcp start
			}
			c.removeLeaseFiles(&l, nil)
			_ = c.store.DeleteLease(l.ID)
			rep.TornDown[l.ID] = inv
		case v1.PlacementRemote:
			if l.State == v1.StateLost {
				c.removeLeaseFiles(&l, nil)
				_ = c.store.DeleteLease(l.ID)
				rep.Forgotten = append(rep.Forgotten, l.ID)
				continue
			}
			n, ok := c.nodes[l.Node]
			if !ok {
				continue
			}
			gctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS)*time.Millisecond)
			nl, err := n.api.GetLease(gctx, l.ID)
			cancel()
			code := v1.CodeOf(err)
			if code == v1.CodeNotFound || code == v1.CodeLeaseGone || (err == nil && nl.State != v1.StateActive && nl.State != v1.StateReleasing) {
				c.removeLeaseFiles(&l, nil)
				_ = c.store.DeleteLease(l.ID)
				rep.Forgotten = append(rep.Forgotten, l.ID)
			}
		}
	}
	return rep
}

func expired(rfc string, now time.Time) bool {
	if rfc == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, rfc)
	return err == nil && now.After(t)
}
