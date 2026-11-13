package node

import (
	"context"
	"errors"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/kindx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// ReapReport summarizes one reaper pass.
type ReapReport struct {
	Expired        []string           // leases moved to expired this pass
	TornDown       []string           // leases fully released this pass
	Failed         []string           // leases whose teardown failed (retried next pass)
	Skipped        []string           // leases busy with another operation
	Orphans        []string           // lease ids whose labeled resources were removed
	OrphanClusters []string           // dol kind clusters without a live lease, deleted
	Unlabeled      v1.UnlabeledCounts // reported only (never deleted, see ADR-0012)
	Pruned         bool               // disk pressure prune ran
	Errors         []string
}

// RunReaper runs a reconcile pass immediately (startup reconciliation) and
// then every reaper.interval_s until ctx is cancelled.
func (s *Server) RunReaper(ctx context.Context) {
	interval := time.Duration(s.cfg.Reaper.IntervalS) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r := s.ReapOnce(ctx)
		if len(r.Expired)+len(r.TornDown)+len(r.Failed)+len(r.Orphans)+len(r.OrphanClusters)+len(r.Errors) > 0 || r.Pruned {
			s.log.Info("reaper pass", "expired", r.Expired, "torn_down", r.TornDown, "failed", r.Failed,
				"orphans", r.Orphans, "orphan_clusters", r.OrphanClusters, "pruned", r.Pruned, "errors", r.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReapOnce performs one pass of brief §7.6 steps 1–5. Safe to call
// concurrently with request handling.
func (s *Server) ReapOnce(ctx context.Context) ReapReport {
	var rep ReapReport
	fail := func(format string, a ...any) { rep.Errors = append(rep.Errors, errText(format, a...)) }

	// Steps 1+2: expire active leases past their deadline (unpinned:
	// heartbeat grace or max TTL; pinned: explicit expires_at).
	now := s.now()
	s.mu.Lock()
	active, err := s.store.List(ctx, "", v1.StateActive)
	if err != nil {
		fail("listing leases: %v", err)
	}
	for i := range active {
		l := &active[i]
		if !s.exp.Expired(l, now) {
			continue
		}
		l.State = v1.StateExpired
		if err := s.store.Update(ctx, l); err != nil {
			fail("expiring %s: %v", l.ID, err)
			continue
		}
		dl, _ := s.exp.Deadline(l)
		s.log.Info("lease expired", "lease", l.ID, "owner", l.Owner, "pinned", l.Pinned,
			"last_heartbeat_at", l.LastHeartbeatAt, "deadline", dl)
		rep.Expired = append(rep.Expired, l.ID)
	}
	s.mu.Unlock()

	// Tear down everything not live: newly expired, failed earlier, and
	// releasing leases interrupted by a restart.
	pending, err := s.store.List(ctx, "", v1.StateExpired, v1.StateFailed, v1.StateReleasing)
	if err != nil {
		fail("listing leases to tear down: %v", err)
	}
	for _, l := range pending {
		_, final, err := s.Teardown(ctx, l.ID, false, false)
		switch {
		case errors.Is(err, ErrBusy):
			rep.Skipped = append(rep.Skipped, l.ID)
		case err != nil:
			fail("tearing down %s: %v", l.ID, err)
		case final.State == v1.StateReleased:
			rep.TornDown = append(rep.TornDown, l.ID)
		default:
			rep.Failed = append(rep.Failed, l.ID)
		}
	}

	// Step 3: orphans: managed resources whose lease is unknown or dead.
	s.reapOrphans(ctx, &rep)

	// Step 4: unlabeled resources are counted, never touched by default.
	if c, err := s.docker.CountUnlabeled(ctx); err != nil {
		s.setDockerProblem(err)
		fail("counting unlabeled resources: %v", err)
	} else {
		s.setDockerProblem(nil)
		rep.Unlabeled = c
		s.statMu.Lock()
		s.unlabeled, s.unlabeledAt = c, time.Now()
		s.statMu.Unlock()
		if s.cfg.Reaper.ReapUnlabeledAfterS > 0 && c.Containers+c.Volumes+c.Networks > 0 {
			s.log.Warn("reaper.reap_unlabeled_after_s is set, but deleting unlabeled resources is not implemented "+
				"(ADR-0012): they are only reported", "containers", c.Containers, "volumes", c.Volumes, "networks", c.Networks)
		}
	}

	// Step 5: disk pressure: prune build cache and dangling images only.
	if snap, err := s.prov.Snapshot(ctx); err == nil {
		low := snap.DiskFree < s.lim.MinFreeDisk || (snap.HostDiskKnown && snap.HostDiskFree < s.lim.MinFreeDisk)
		if low {
			s.log.Warn("disk below minimum; pruning build cache and dangling images",
				"disk_free_bytes", snap.DiskFree, "host_disk_free_bytes", snap.HostDiskFree, "min_free_bytes", s.lim.MinFreeDisk)
			rep.Pruned = true
			for _, args := range [][]string{{"builder", "prune", "-f"}, {"image", "prune", "-f"}} {
				if _, err := s.docker.Run(ctx, nil, args...); err != nil {
					fail("docker %v: %v", args, err)
				}
			}
		}
	}
	return rep
}

// orphanable reports whether resources labeled with a lease in this state
// are orphans. Expired and failed leases are torn down in full (including
// their labeled resources) by the pending-teardown step of the same pass.
func orphanable(st v1.LeaseState) bool { return st == v1.StateReleased }

func (s *Server) reapOrphans(ctx context.Context, rep *ReapReport) {
	ids, err := s.docker.ManagedLeaseIDs(ctx)
	if err != nil {
		s.setDockerProblem(err)
		rep.Errors = append(rep.Errors, errText("listing managed resources: %v", err))
	}
	for _, id := range ids {
		if !v1.ValidLeaseID(id) {
			s.log.Warn("managed resource with an invalid lease label; leaving it alone", "lease_label", id)
			continue
		}
		if s.orphanScope != nil && !s.orphanScope(id) {
			continue
		}
		l, err := s.store.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			rep.Errors = append(rep.Errors, errText("reading lease %s: %v", id, err))
			continue
		case !orphanable(l.State):
			continue
		}
		lk := s.opLock(id)
		if !lk.TryLock() {
			continue
		}
		inv := s.docker.TeardownLease(ctx, id)
		lk.Unlock()
		s.log.Info("removed orphaned resources", "lease", id, "known", err == nil,
			"containers", inv.Containers, "networks", inv.Networks, "volumes", inv.Volumes, "errors", inv.Errors)
		rep.Orphans = append(rep.Orphans, id)
		rep.Errors = append(rep.Errors, inv.Errors...)
	}

	// kind clusters named dol-<lease>-* whose lease is unknown or released.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	clusters, err := s.kind.Clusters(cctx)
	cancel()
	if err != nil {
		s.log.Debug("listing kind clusters failed (is kind installed?)", "err", err)
		return
	}
	for _, c := range clusters {
		id, ok := kindx.LeaseOfCluster(c)
		if !ok || (s.orphanScope != nil && !s.orphanScope(id)) {
			continue
		}
		l, err := s.store.Get(ctx, id)
		if err == nil && (l.State != v1.StateReleased || trackedCluster(l, c)) {
			continue // live, or still tracked (Teardown handles it)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			continue
		}
		lk := s.opLock(id)
		if !lk.TryLock() {
			continue
		}
		if err := s.kind.Delete(ctx, c); err != nil {
			rep.Errors = append(rep.Errors, errText("deleting orphan kind cluster %s: %v", c, err))
		} else {
			_ = s.store.FreePort(ctx, c)
			rep.OrphanClusters = append(rep.OrphanClusters, c)
			s.log.Info("deleted orphan kind cluster", "cluster", c)
		}
		lk.Unlock()
	}
}

func trackedCluster(l *v1.Lease, cluster string) bool {
	for _, c := range l.Resources.KindClusters {
		if c.ClusterName == cluster {
			return true
		}
	}
	return false
}
