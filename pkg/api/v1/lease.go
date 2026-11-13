package v1

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"time"
)

// Workload is the kind of work a lease is for.
type Workload string

const (
	WorkloadContainer Workload = "container"
	WorkloadCompose   Workload = "compose"
	WorkloadKind      Workload = "kind"
	WorkloadBuild     Workload = "build"
)

// Valid reports whether w is a known workload.
func (w Workload) Valid() bool {
	switch w {
	case WorkloadContainer, WorkloadCompose, WorkloadKind, WorkloadBuild:
		return true
	}
	return false
}

// LeaseState is the lifecycle state of a lease.
//
//	active ──release──► releasing ──teardown ok──► released
//	   │                     └──teardown error──► failed (retried by reaper)
//	   └──heartbeat/ttl timeout──► expired ──teardown──► released
//
// "lost" is a client-only state: the node answered 404/410 to a heartbeat.
type LeaseState string

const (
	StateActive    LeaseState = "active"
	StateReleasing LeaseState = "releasing"
	StateExpired   LeaseState = "expired"
	StateReleased  LeaseState = "released"
	StateFailed    LeaseState = "failed"
	StateLost      LeaseState = "lost" // client-side only
)

// Live reports whether the lease still holds (or may still hold) resources
// and capacity.
func (s LeaseState) Live() bool { return s == StateActive }

// Terminal reports whether no further transitions happen.
func (s LeaseState) Terminal() bool { return s == StateReleased || s == StateLost }

const leaseAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

var leaseIDRe = regexp.MustCompile(`^l-[a-z2-7]{10}$`)

// NewLeaseID returns a random lease ID: "l-" + 10 lowercase base32 chars.
func NewLeaseID() string {
	return "l-" + randomBase32(10)
}

// NewSessionID returns a random session ID: "s-" + 10 lowercase base32 chars.
func NewSessionID() string { return "s-" + randomBase32(10) }

func randomBase32(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // unrecoverable
	}
	out := make([]byte, n)
	for i := range b {
		out[i] = leaseAlphabet[int(b[i])%32]
	}
	return string(out)
}

// ValidLeaseID reports whether id has the lease ID format. Local leases use
// the same format; the placement tells them apart.
func ValidLeaseID(id string) bool { return leaseIDRe.MatchString(id) }

// LeaseRequest is the body of POST /v1/leases.
type LeaseRequest struct {
	Owner          string   `json:"owner"`
	Session        string   `json:"session"`
	Project        string   `json:"project"`
	Workload       Workload `json:"workload"`
	EstMemoryBytes int64    `json:"est_memory_bytes"`
	EstDiskBytes   int64    `json:"est_disk_bytes"`
	TTLSeconds     int64    `json:"ttl_s"` // 0 = node default (max_ttl_s)
	Pinned         bool     `json:"pinned"`
	ClientVersion  string   `json:"client_version"`
}

// Validate checks a request for obvious mistakes, returning a CodeBadRequest
// error with a hint.
func (r *LeaseRequest) Validate() error {
	bad := func(format string, args ...any) error {
		return Errorf(CodeBadRequest, "fix the request body and retry", format, args...)
	}
	if r.Owner == "" {
		return bad("owner is required")
	}
	if r.Session == "" {
		return bad("session is required")
	}
	if !r.Workload.Valid() {
		return bad("workload %q is invalid: use container, compose, kind or build", r.Workload)
	}
	if r.EstMemoryBytes <= 0 {
		return bad("est_memory_bytes must be > 0 (got %d)", r.EstMemoryBytes)
	}
	if r.EstDiskBytes < 0 || r.TTLSeconds < 0 {
		return bad("est_disk_bytes and ttl_s must be >= 0")
	}
	for f, v := range map[string]string{"owner": r.Owner, "session": r.Session, "project": r.Project} {
		if err := ValidateLabelValue(f, v); err != nil {
			return bad("%v", err)
		}
	}
	return nil
}

// KindCluster is a kind cluster tracked on a lease.
type KindCluster struct {
	Name          string `json:"name"`         // user-facing short name, e.g. "e2e"
	ClusterName   string `json:"cluster_name"` // real kind name: dol-<lease>-<name>
	APIServerPort int    `json:"api_server_port"`
	AccessMode    string `json:"access_mode"` // tunnel | lan
}

// LeaseResources lists what a lease tracks explicitly. Containers, networks
// and volumes are discovered through labels instead.
type LeaseResources struct {
	KindClusters    []KindCluster `json:"kind_clusters"`
	ComposeProjects []string      `json:"compose_projects"`
	SyncDirs        []string      `json:"sync_dirs"`
}

// Lease is a reservation of capacity on a node (or locally).
type Lease struct {
	ID string `json:"id"`
	LeaseRequest
	State           LeaseState        `json:"state"`
	CreatedAt       time.Time         `json:"created_at"`
	LastHeartbeatAt time.Time         `json:"last_heartbeat_at"`
	ExpiresAt       time.Time         `json:"expires_at"`
	SSHUser         string            `json:"ssh_user"`
	Labels          map[string]string `json:"labels"`
	Resources       LeaseResources    `json:"resources"`
	// LastError records the most recent teardown failure, if any.
	LastError string `json:"last_error,omitempty"`
}

// LeasePatch is the body of PATCH /v1/leases/{id}. Nil fields are unchanged.
type LeasePatch struct {
	EstMemoryBytes *int64 `json:"est_memory_bytes,omitempty"`
	Pinned         *bool  `json:"pinned,omitempty"`
	// TTLSeconds sets expires_at = now + ttl for pinned leases. 0 = no explicit expiry.
	TTLSeconds *int64 `json:"ttl_s,omitempty"`
}

// LeaseList is the body of GET /v1/leases.
type LeaseList struct {
	Leases []Lease `json:"leases"`
}

// HeartbeatResponse is the body of POST /v1/leases/{id}/heartbeat.
type HeartbeatResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// Inventory lists concrete resources, either owned (GET …/resources) or
// removed (DELETE …).
type Inventory struct {
	Containers []string `json:"containers"`
	Networks   []string `json:"networks"`
	Volumes    []string `json:"volumes"`
	// Images built with the lease's label (ADR-0037). Optional for old nodes.
	Images          []string `json:"images,omitempty"`
	KindClusters    []string `json:"kind_clusters"`
	ComposeProjects []string `json:"compose_projects"`
	SyncDirs        []string `json:"sync_dirs"`
	// Errors lists partial failures; the reaper retries them.
	Errors []string `json:"errors,omitempty"`
}

// Empty reports whether the inventory has no resources.
func (i Inventory) Empty() bool {
	return len(i.Containers)+len(i.Networks)+len(i.Volumes)+len(i.Images)+len(i.KindClusters)+
		len(i.ComposeProjects)+len(i.SyncDirs) == 0
}

// ReleaseResponse is the body of DELETE /v1/leases/{id}.
type ReleaseResponse struct {
	Lease   Lease     `json:"lease"`
	Removed Inventory `json:"removed"`
}

// DrainRequest is the body of POST /v1/admin/drain.
type DrainRequest struct {
	Draining bool `json:"draining"`
}

// SyncRegisterRequest is the body of POST /v1/leases/{id}/sync (ADR-0011).
type SyncRegisterRequest struct {
	Path string `json:"path"`
}
