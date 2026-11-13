package node

import (
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const G = v1.GiB

func TestAdmitTable(t *testing.T) {
	base := capacity.Snapshot{MemTotal: 32 * G, MemAvailable: 30 * G, DiskPath: "/var/lib/docker", DiskFree: 100 * G}
	wsl := base
	wsl.HostMemKnown, wsl.HostMemFree = true, 5*G
	wsl.HostDiskKnown, wsl.HostDiskPath, wsl.HostDiskFree = true, "/mnt/c", 100*G
	lowC := wsl
	lowC.HostMemFree = 40 * G
	lowC.HostDiskFree = 12 * G
	busy := base
	busy.MemAvailable = 12 * G // 20 GiB observed in use by things outside leases
	lim := Limits{ReserveMemory: 4 * G, MinFreeDisk: 30 * G, MaxActive: 3, WindowsMemoryCheck: true}
	noWin := lim
	noWin.WindowsMemoryCheck = false

	cases := []struct {
		name      string
		snap      capacity.Snapshot
		lim       Limits
		committed int64
		active    int
		draining  bool
		req       Request
		code      v1.ErrorCode
		msg       []string       // substrings of the message
		details   map[string]any // exact detail values
	}{
		{name: "accept", snap: base, lim: lim, committed: 10 * G, active: 1, req: Request{EstMemory: 10 * G}},
		{name: "accept exactly all schedulable", snap: base, lim: lim, req: Request{EstMemory: 26 * G}},
		{name: "one byte too many", snap: base, lim: lim, req: Request{EstMemory: 26*G + 1}, code: v1.CodeInsufficientMemory, msg: []string{"26.0 GiB schedulable"}},
		{name: "draining wins over everything", snap: base, lim: lim, active: 5, draining: true, req: Request{EstMemory: 100 * G},
			code: v1.CodeDraining, msg: []string{"node test is draining"}},
		{name: "max leases", snap: base, lim: lim, active: 3, req: Request{EstMemory: G},
			code: v1.CodeMaxLeases, msg: []string{"3 active leases, the maximum is 3"},
			details: map[string]any{"leases_active": 3, "max_active": 3}},
		{name: "committed memory", snap: base, lim: lim, committed: 20 * G, active: 2, req: Request{EstMemory: 10 * G},
			code: v1.CodeInsufficientMemory, msg: []string{"8.0 GiB schedulable, 10.0 GiB requested", "20.0 GiB committed to 2 active leases", "4.0 GiB reserved"}},
		{name: "6.1 GiB schedulable example", snap: base, lim: lim, committed: 28*G - v1.GBToBytes(6.1), active: 2, req: Request{EstMemory: 10 * G},
			code: v1.CodeInsufficientMemory, msg: []string{"6.1 GiB schedulable, 10.0 GiB requested"},
			details: map[string]any{"requested_bytes": 10 * G, "schedulable_free_bytes": v1.GBToBytes(6.1), "committed_bytes": 28*G - v1.GBToBytes(6.1)}},
		{name: "observed usage beats smaller commitments", snap: busy, lim: lim, committed: 2 * G, active: 1, req: Request{EstMemory: 9 * G},
			code: v1.CodeInsufficientMemory, msg: []string{"8.0 GiB schedulable, 9.0 GiB requested", "20.0 GiB in use"},
			details: map[string]any{"observed_used_bytes": 20 * G, "schedulable_free_bytes": 8 * G}},
		{name: "windows host memory limits", snap: wsl, lim: lim, req: Request{EstMemory: 4 * G},
			code: v1.CodeInsufficientMemory, msg: []string{"3.0 GiB schedulable, 4.0 GiB requested", "Windows host has only 5.0 GiB free RAM"},
			details: map[string]any{"host_free_bytes": 5 * G, "host_headroom_bytes": 2 * G, "schedulable_free_bytes": 3 * G}},
		{name: "windows check disabled", snap: wsl, lim: noWin, req: Request{EstMemory: 4 * G}},
		{name: "windows fits", snap: wsl, lim: lim, req: Request{EstMemory: 3 * G}},
		{name: "docker disk", snap: base, lim: lim, req: Request{EstMemory: G, EstDisk: 75 * G},
			code: v1.CodeInsufficientDisk, msg: []string{"100.0 GiB free on /var/lib/docker, 75.0 GiB requested, 30.0 GiB must stay free"},
			details: map[string]any{"disk_free_bytes": 100 * G, "requested_bytes": 75 * G, "min_free_bytes": 30 * G}},
		{name: "windows C: drive", snap: lowC, lim: lim, req: Request{EstMemory: G},
			code: v1.CodeInsufficientDisk, msg: []string{"(/mnt/c) has 12.0 GiB free, below the 30.0 GiB minimum"},
			details: map[string]any{"host_disk_free_bytes": 12 * G}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Admit("test", c.snap, c.lim, c.committed, c.active, c.draining, c.req)
			if c.code == "" {
				if a.Err != nil {
					t.Fatalf("refused: %v", a.Err)
				}
				return
			}
			if a.Err == nil || a.Err.Code != c.code {
				t.Fatalf("want %s, got %v", c.code, a.Err)
			}
			if a.Err.Code.HTTPStatus() != 409 || !a.Err.Code.IsRefusal() || a.Err.Hint == "" {
				t.Fatalf("refusals must be 409 with a hint: %+v", a.Err)
			}
			for _, m := range c.msg {
				if !strings.Contains(a.Err.Message, m) {
					t.Errorf("message %q lacks %q", a.Err.Message, m)
				}
			}
			for k, want := range c.details {
				if got := a.Err.Details[k]; got != want {
					t.Errorf("details[%s] = %v (%T), want %v (%T)", k, got, got, want, want)
				}
			}
		})
	}
}

func TestExpiryPolicy(t *testing.T) {
	p := ExpiryPolicy{Grace: 10 * time.Minute, MaxTTL: 24 * time.Hour}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := &v1.Lease{CreatedAt: t0, LastHeartbeatAt: t0}
	if d, ok := p.Deadline(l); !ok || !d.Equal(t0.Add(10*time.Minute)) {
		t.Fatal(d)
	}
	l.LastHeartbeatAt = t0.Add(23*time.Hour + 55*time.Minute)
	if d, _ := p.Deadline(l); !d.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("max ttl must cap: %v", d)
	}
	l.TTLSeconds = 3600
	if d, _ := p.Deadline(l); !d.Equal(t0.Add(time.Hour)) {
		t.Fatalf("ttl_s must cap: %v", d)
	}
	l.TTLSeconds = 7 * 86400 // above max_ttl: capped
	if p.TTL(l.TTLSeconds) != 24*time.Hour {
		t.Fatal("ttl above max_ttl must be capped")
	}
	pinned := &v1.Lease{LeaseRequest: v1.LeaseRequest{Pinned: true}, CreatedAt: t0, LastHeartbeatAt: t0}
	if _, ok := p.Deadline(pinned); ok || p.Expired(pinned, t0.Add(1000*time.Hour)) {
		t.Fatal("pinned without expiry never expires")
	}
	pinned.ExpiresAt = t0.Add(48 * time.Hour)
	if p.Expired(pinned, t0.Add(47*time.Hour)) || !p.Expired(pinned, t0.Add(49*time.Hour)) {
		t.Fatal("pinned explicit expiry")
	}
}
