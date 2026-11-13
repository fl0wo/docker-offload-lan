package v1

import (
	"fmt"
	"time"
)

// Health is the body of GET /v1/healthz.
type Health struct {
	Status     string    `json:"status"` // "ok" | "degraded"
	Version    string    `json:"version"`
	APIVersion string    `json:"api_version"`
	Node       string    `json:"node"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	Provider   string    `json:"provider"`
	Time       time.Time `json:"time"` // node clock, for skew detection
	Accepting  bool      `json:"accepting"`
	// Problems lists reasons for "degraded" (e.g. docker daemon unreachable).
	Problems []string `json:"problems,omitempty"`
}

// MemoryCapacity describes memory in bytes. See brief §7.3 for semantics.
type MemoryCapacity struct {
	TotalBytes           int64 `json:"total_bytes"`
	AvailableBytes       int64 `json:"available_bytes"`
	ReservedBytes        int64 `json:"reserved_bytes"`
	CommittedBytes       int64 `json:"committed_bytes"`
	SchedulableFreeBytes int64 `json:"schedulable_free_bytes"`
	// HostFreeBytes is free RAM on the host OS (Windows for WSL2); 0 if unknown.
	HostFreeBytes int64 `json:"host_free_bytes"`
}

// DiskCapacity describes the disk Docker writes to.
type DiskCapacity struct {
	Path      string `json:"path"`
	FreeBytes int64  `json:"free_bytes"`
	// HostFreeBytes is free space on the host drive backing the VM disk
	// (Windows C: for WSL2); 0 if unknown / not applicable.
	HostFreeBytes int64 `json:"host_free_bytes"`
	MinFreeBytes  int64 `json:"min_free_bytes"`
}

// CPUCapacity is informational only; admission does not use CPU.
type CPUCapacity struct {
	LogicalCores int     `json:"logical_cores"`
	Load1        float64 `json:"load1"`
}

// Capacity is the body of GET /v1/capacity.
type Capacity struct {
	Node         string         `json:"node"`
	Arch         string         `json:"arch"`
	Provider     string         `json:"provider"`
	Memory       MemoryCapacity `json:"memory"`
	Disk         DiskCapacity   `json:"disk"`
	CPU          CPUCapacity    `json:"cpu"`
	LeasesActive int            `json:"leases_active"`
	Accepting    bool           `json:"accepting"`
	Reason       string         `json:"reason"`
	// Unlabeled counts Docker resources without our managed label; they are
	// never deleted by default but are reported (brief §7.6 step 4).
	Unlabeled UnlabeledCounts `json:"unlabeled"`
	// Warnings lists degraded measurements (e.g. Windows interop unavailable).
	Warnings []string `json:"warnings,omitempty"`
}

// UnlabeledCounts counts resources on the node that dol does not own.
type UnlabeledCounts struct {
	Containers int `json:"containers"`
	Volumes    int `json:"volumes"`
	Networks   int `json:"networks"`
}

// GiB is 1<<30 bytes.
const GiB = int64(1) << 30

// FormatBytes renders bytes as GiB with one decimal, e.g. "6.1 GiB".
func FormatBytes(b int64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/float64(GiB))
}

// GBToBytes converts a (possibly fractional) GiB amount to bytes.
func GBToBytes(gb float64) int64 { return int64(gb * float64(GiB)) }
