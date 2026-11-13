package node

import (
	"fmt"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// WindowsHeadroom is kept free on the Windows host of a WSL2 node: Windows
// itself is under pressure too (brief §7.3).
const WindowsHeadroom = 2 * v1.GiB

// Limits are the admission parameters from the node config.
type Limits struct {
	ReserveMemory      int64
	MinFreeDisk        int64
	MaxActive          int
	WindowsMemoryCheck bool
}

// LimitsFromConfig converts the node config.
func LimitsFromConfig(c *config.NodeConfig) Limits {
	return Limits{
		ReserveMemory:      v1.GBToBytes(c.Capacity.ReserveMemoryGB),
		MinFreeDisk:        v1.GBToBytes(c.Capacity.MinFreeDiskGB),
		MaxActive:          c.Leases.MaxActive,
		WindowsMemoryCheck: c.Capacity.WindowsMemoryCheck,
	}
}

// Budget is the memory arithmetic of brief §7.3 for one snapshot.
type Budget struct {
	Total         int64
	Reserved      int64
	Schedulable   int64 // total - reserve
	Committed     int64 // sum of est_memory of active leases
	ObservedUsed  int64 // total - available
	EffectiveUsed int64 // max(committed, observed)
	// FreeForNew is what a new lease may reserve. May be negative.
	FreeForNew int64
	// HostFreeForNew is windows_free - headroom when the host check applies.
	HostFreeForNew int64
	HostChecked    bool
	// HostLimited is true when the host check is the binding constraint.
	HostLimited bool
}

// ComputeBudget applies the admission formula.
func ComputeBudget(s capacity.Snapshot, lim Limits, committed int64) Budget {
	b := Budget{Total: s.MemTotal, Reserved: lim.ReserveMemory, Committed: committed}
	b.Schedulable = s.MemTotal - lim.ReserveMemory
	b.ObservedUsed = s.MemTotal - s.MemAvailable
	b.EffectiveUsed = max(b.Committed, b.ObservedUsed)
	b.FreeForNew = b.Schedulable - b.EffectiveUsed
	if lim.WindowsMemoryCheck && s.HostMemKnown {
		b.HostChecked = true
		b.HostFreeForNew = s.HostMemFree - WindowsHeadroom
		if b.HostFreeForNew < b.FreeForNew {
			b.FreeForNew, b.HostLimited = b.HostFreeForNew, true
		}
	}
	return b
}

// Admission is the outcome of Admit: nil error means accepted.
type Admission struct {
	Budget Budget
	Err    *v1.Error
}

// Request is what admission needs from a lease request.
type Request struct {
	EstMemory int64
	EstDisk   int64
}

func nonNeg(v int64) int64 { return max(v, 0) }

// Admit decides whether a request fits (brief §7.3). It is pure; callers
// hold the admission mutex around Admit and the lease insert.
//
// Refusal precedence: draining, max_leases, memory, disk, host disk.
func Admit(node string, s capacity.Snapshot, lim Limits, committed int64, active int, draining bool, r Request) Admission {
	b := ComputeBudget(s, lim, committed)
	a := Admission{Budget: b}
	if draining {
		a.Err = v1.Errorf(v1.CodeDraining,
			"use another node or local Docker; the node admin re-enables it with POST /v1/admin/drain {\"draining\": false}",
			"node %s is draining and does not accept new leases", node)
		return a
	}
	if active >= lim.MaxActive {
		a.Err = v1.Errorf(v1.CodeMaxLeases,
			"release leases you no longer need (dol_release) or raise leases.max_active in node.yaml",
			"node %s has %d active leases, the maximum is %d", node, active, lim.MaxActive).
			WithDetails(map[string]any{"leases_active": active, "max_active": lim.MaxActive})
		return a
	}
	if err := memoryRefusal(node, s, b, active, r.EstMemory); err != nil {
		a.Err = err
		return a
	}
	if s.DiskFree-r.EstDisk < lim.MinFreeDisk {
		a.Err = (&v1.Error{Code: v1.CodeInsufficientDisk,
			Message: fmt.Sprintf("%s free on %s, %s requested, %s must stay free",
				v1.FormatBytes(nonNeg(s.DiskFree)), s.DiskPath, v1.FormatBytes(r.EstDisk), v1.FormatBytes(lim.MinFreeDisk)),
			Hint: "free disk on the node (`docker system df`, remove unused images) or lower capacity.min_free_disk_gb; the reaper prunes build cache and dangling images automatically"}).
			WithDetails(map[string]any{"disk_path": s.DiskPath, "disk_free_bytes": s.DiskFree,
				"requested_bytes": r.EstDisk, "min_free_bytes": lim.MinFreeDisk})
		return a
	}
	if s.HostDiskKnown && s.HostDiskFree < lim.MinFreeDisk {
		a.Err = (&v1.Error{Code: v1.CodeInsufficientDisk,
			Message: fmt.Sprintf("the Windows drive behind the WSL disk (%s) has %s free, below the %s minimum",
				s.HostDiskPath, v1.FormatBytes(nonNeg(s.HostDiskFree)), v1.FormatBytes(lim.MinFreeDisk)),
			Hint: "free space on the Windows C: drive (the WSL virtual disk grows into it), then retry"}).
			WithDetails(map[string]any{"host_disk_path": s.HostDiskPath, "host_disk_free_bytes": s.HostDiskFree,
				"min_free_bytes": lim.MinFreeDisk})
		return a
	}
	return a
}

// memoryRefusal returns an insufficient_memory error when est does not fit
// in the budget, nil otherwise.
func memoryRefusal(node string, s capacity.Snapshot, b Budget, active int, est int64) *v1.Error {
	if est <= b.FreeForNew {
		return nil
	}
	details := map[string]any{
		"requested_bytes":        est,
		"schedulable_free_bytes": b.FreeForNew,
		"total_bytes":            b.Total,
		"reserved_bytes":         b.Reserved,
		"schedulable_bytes":      b.Schedulable,
		"committed_bytes":        b.Committed,
		"observed_used_bytes":    b.ObservedUsed,
		"leases_active":          active,
	}
	msg := fmt.Sprintf("%s schedulable, %s requested (node %s: %s total, %s reserved, %s committed to %d active leases, %s in use)",
		v1.FormatBytes(nonNeg(b.FreeForNew)), v1.FormatBytes(est), node,
		v1.FormatBytes(b.Total), v1.FormatBytes(b.Reserved), v1.FormatBytes(b.Committed), active, v1.FormatBytes(b.ObservedUsed))
	if b.HostChecked {
		details["host_free_bytes"] = s.HostMemFree
		details["host_headroom_bytes"] = WindowsHeadroom
	}
	if b.HostLimited {
		msg = fmt.Sprintf("%s schedulable, %s requested (the Windows host has only %s free RAM and keeps %s for itself)",
			v1.FormatBytes(nonNeg(b.FreeForNew)), v1.FormatBytes(est), v1.FormatBytes(s.HostMemFree), v1.FormatBytes(WindowsHeadroom))
	}
	return (&v1.Error{Code: v1.CodeInsufficientMemory, Message: msg,
		Hint: "request less memory, release leases you no longer need (dol_release), or let dol fall back to local Docker"}).
		WithDetails(details)
}

// ExpiryPolicy computes lease deadlines (brief §7.6).
type ExpiryPolicy struct {
	Grace  time.Duration // leases.heartbeat_grace_s
	MaxTTL time.Duration // leases.max_ttl_s
}

// TTL returns the effective time-to-live of an unpinned lease: ttl_s if set,
// capped at max_ttl_s.
func (p ExpiryPolicy) TTL(ttlSeconds int64) time.Duration {
	ttl := p.MaxTTL
	if ttlSeconds > 0 {
		if d := time.Duration(ttlSeconds) * time.Second; d < ttl {
			ttl = d
		}
	}
	return ttl
}

// Deadline returns when the lease expires; ok=false means never (pinned
// without explicit expiry). Pinned leases keep their explicit expiry in
// ExpiresAt.
func (p ExpiryPolicy) Deadline(l *v1.Lease) (t time.Time, ok bool) {
	if l.Pinned {
		return l.ExpiresAt, !l.ExpiresAt.IsZero()
	}
	hb := l.LastHeartbeatAt.Add(p.Grace)
	hard := l.CreatedAt.Add(p.TTL(l.TTLSeconds))
	if hard.Before(hb) {
		return hard, true
	}
	return hb, true
}

// Refresh recomputes ExpiresAt of an unpinned lease after its heartbeat or
// TTL changed. Pinned leases keep their explicit expiry.
func (p ExpiryPolicy) Refresh(l *v1.Lease) {
	if !l.Pinned {
		l.ExpiresAt, _ = p.Deadline(l)
	}
}

// Expired reports whether the lease is past its deadline at now.
func (p ExpiryPolicy) Expired(l *v1.Lease, now time.Time) bool {
	d, ok := p.Deadline(l)
	return ok && now.After(d)
}
