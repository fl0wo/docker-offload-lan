package node

import (
	"context"
	"errors"
	"strings"

	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// ErrBusy means another long operation holds the lease (reaper skips it).
var ErrBusy = errors.New("lease busy")

// Teardown removes everything a lease owns, in the order of brief §7.6:
// kind clusters → compose projects → labeled containers/networks/volumes →
// sync dirs. It then marks the lease released, or failed with last_error
// when anything could not be removed (the reaper retries failed leases).
//
// It is idempotent. block=false returns ErrBusy instead of waiting when
// another operation (kind create, a concurrent release) holds the lease.
func (s *Server) Teardown(ctx context.Context, id string, keepSyncDirs, block bool) (v1.Inventory, *v1.Lease, error) {
	inv := emptyInventory()
	lk := s.opLock(id)
	if block {
		lk.Lock()
	} else if !lk.TryLock() {
		return inv, nil, ErrBusy
	}
	defer lk.Unlock()

	l, err := s.store.Get(ctx, id)
	if err != nil {
		return inv, nil, err
	}
	var errs []string

	var keptClusters []v1.KindCluster
	for _, c := range l.Resources.KindClusters {
		if err := s.kind.Delete(ctx, c.ClusterName); err != nil {
			errs = append(errs, errText("deleting kind cluster %s: %v", c.ClusterName, err))
			keptClusters = append(keptClusters, c)
			continue
		}
		if err := s.store.FreePort(ctx, c.ClusterName); err != nil {
			s.log.Warn("freeing kind port", "cluster", c.ClusterName, "err", err)
		}
		inv.KindClusters = append(inv.KindClusters, c.ClusterName)
	}

	var keptProjects []string
	for _, p := range l.Resources.ComposeProjects {
		// Any client may register any project name; `compose down` must not
		// reach resources of other leases or of the node owner. The lease's
		// own share of a mixed project is removed by label below.
		if err := s.docker.RequireComposeOwned(ctx, p, id); err != nil {
			if v1.CodeOf(err) == v1.CodeNotOwned {
				s.log.Warn("not bringing down compose project with foreign resources", "lease", id, "project", p, "err", err)
				continue
			}
			errs = append(errs, errText("checking compose project %s: %v", p, err))
			keptProjects = append(keptProjects, p)
			continue
		}
		if _, err := s.docker.Run(ctx, nil, "compose", "-p", p, "down", "--volumes", "--remove-orphans", "--timeout", "10"); err != nil {
			errs = append(errs, errText("docker compose -p %s down: %v", p, err))
			keptProjects = append(keptProjects, p)
			continue
		}
		inv.ComposeProjects = append(inv.ComposeProjects, p)
	}

	d := s.docker.TeardownLease(ctx, id)
	inv.Containers = append(inv.Containers, d.Containers...)
	inv.Networks = append(inv.Networks, d.Networks...)
	inv.Volumes = append(inv.Volumes, d.Volumes...)
	errs = append(errs, d.Errors...)
	if len(d.Errors) > 0 {
		s.setDockerProblem(firstDockerErr(d.Errors))
	}

	keptDirs := l.Resources.SyncDirs
	if !keepSyncDirs {
		keptDirs = nil
		for _, p := range l.Resources.SyncDirs {
			if err := RemoveSyncDir(s.cfg.SyncRoot, id, p); err != nil {
				errs = append(errs, errText("removing sync dir %s: %v", p, err))
				keptDirs = append(keptDirs, p)
				continue
			}
			inv.SyncDirs = append(inv.SyncDirs, p)
		}
	}
	inv.Errors = errs

	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.store.Get(ctx, id)
	if err != nil {
		return inv, nil, err
	}
	cur.Resources.KindClusters = nonNilClusters(keptClusters)
	cur.Resources.ComposeProjects = nonNil(keptProjects)
	cur.Resources.SyncDirs = nonNil(keptDirs)
	if len(errs) == 0 {
		cur.State, cur.LastError = v1.StateReleased, ""
	} else {
		cur.State, cur.LastError = v1.StateFailed, strings.Join(errs, "; ")
	}
	if err := s.store.Update(ctx, cur); err != nil {
		return inv, nil, err
	}
	if len(errs) == 0 {
		s.log.Info("lease torn down", "lease", id, "containers", len(inv.Containers), "networks", len(inv.Networks),
			"volumes", len(inv.Volumes), "kind_clusters", len(inv.KindClusters), "compose_projects", len(inv.ComposeProjects),
			"sync_dirs", len(inv.SyncDirs))
	} else {
		s.log.Warn("lease teardown incomplete; will retry", "lease", id, "errors", cur.LastError)
	}
	return inv, cur, nil
}

func firstDockerErr(errs []string) error {
	return &dockerx.CmdError{Stderr: errs[0], Err: errors.New(errs[0])}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func nonNilClusters(xs []v1.KindCluster) []v1.KindCluster {
	if xs == nil {
		return []v1.KindCluster{}
	}
	return xs
}
