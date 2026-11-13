package v1

// Structured output of MCP tools served by `dol mcp` (brief §8). These are a
// public contract for agents: tool descriptions reference these field names.

// Placement says where work runs.
type Placement string

const (
	PlacementRemote Placement = "remote"
	PlacementLocal  Placement = "local"
)

// Commands are ready-to-paste command prefixes. Environment variables do not
// persist across agent bash calls, so every command must carry its target.
type Commands struct {
	Docker     string `json:"docker"`
	Compose    string `json:"compose"`
	Kubectl    string `json:"kubectl,omitempty"`
	LabelFlags string `json:"label_flags"`
}

// PlacementInfo is embedded by every tool result that can affect where things
// run.
type PlacementInfo struct {
	Placement Placement `json:"placement"`
	Node      string    `json:"node,omitempty"` // empty for local
	Reason    string    `json:"reason"`
	LeaseID   string    `json:"lease_id"`
	Commands  Commands  `json:"commands"`
	Warnings  []string  `json:"warnings"`
	NextSteps []string  `json:"next_steps"`
}

// Attempt records one node tried during placement.
type Attempt struct {
	Node    string    `json:"node"`
	Outcome string    `json:"outcome"` // "placed" | "unreachable" | "refused" | "error" | "skipped"
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
	Millis  int64     `json:"ms"`
}

// AcquireResult is the output of dol_acquire.
type AcquireResult struct {
	PlacementInfo
	Labels         map[string]string `json:"labels"`
	DockerContext  string            `json:"docker_context"`
	NodeArch       string            `json:"node_arch,omitempty"`
	ClientArch     string            `json:"client_arch"`
	EstMemoryBytes int64             `json:"est_memory_bytes"`
	ExpiresAt      string            `json:"expires_at,omitempty"`
	Pinned         bool              `json:"pinned"`
	Attempts       []Attempt         `json:"attempts"`
}

// ClientLease is the client's view of a lease (remote or local), as listed by
// dol_status / dol_list and stored in client state.
type ClientLease struct {
	ID             string            `json:"id"`
	Placement      Placement         `json:"placement"`
	Node           string            `json:"node,omitempty"`
	DockerContext  string            `json:"docker_context"`
	State          LeaseState        `json:"state"`
	Owner          string            `json:"owner"`
	Session        string            `json:"session"`
	Project        string            `json:"project"`
	Workload       Workload          `json:"workload"`
	EstMemoryBytes int64             `json:"est_memory_bytes"`
	Pinned         bool              `json:"pinned"`
	CreatedAt      string            `json:"created_at"`
	ExpiresAt      string            `json:"expires_at,omitempty"`
	Labels         map[string]string `json:"labels"`
	Reason         string            `json:"reason,omitempty"`
	Resources      LeaseResources    `json:"resources"`
	Forwards       []Forward         `json:"forwards"`
	Kubeconfigs    map[string]string `json:"kubeconfigs,omitempty"` // cluster -> path
}

// Forward is an SSH local port forward managed by dol mcp.
type Forward struct {
	LeaseID    string `json:"lease_id"`
	LocalPort  int    `json:"local_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	Purpose    string `json:"purpose,omitempty"` // "user" | "kind-api:<cluster>"
	PID        int    `json:"pid,omitempty"`
}

// NodeStatus summarizes one configured node for dol_status.
type NodeStatus struct {
	Name     string    `json:"name"`
	Healthy  bool      `json:"healthy"`
	Error    string    `json:"error,omitempty"`
	Health   *Health   `json:"health,omitempty"`
	Capacity *Capacity `json:"capacity,omitempty"`
	Millis   int64     `json:"ms"`
}

// LocalStatus describes the local Docker fallback.
type LocalStatus struct {
	DockerContext string `json:"docker_context"`
	Reachable     bool   `json:"reachable"`
	Error         string `json:"error,omitempty"`
	MemTotalBytes int64  `json:"mem_total_bytes,omitempty"`
	Arch          string `json:"arch,omitempty"`
}

// StatusResult is the output of dol_status.
type StatusResult struct {
	Owner   string        `json:"owner"`
	Session string        `json:"session"`
	Nodes   []NodeStatus  `json:"nodes"`
	Local   LocalStatus   `json:"local"`
	Leases  []ClientLease `json:"leases"`
	Policy  string        `json:"policy"`
}

// Resource is a concrete Docker resource listed by dol_list.
type Resource struct {
	Kind    string            `json:"kind"` // container | network | volume | kind_cluster | compose_project
	ID      string            `json:"id,omitempty"`
	Name    string            `json:"name"`
	State   string            `json:"state,omitempty"`
	Image   string            `json:"image,omitempty"`
	Ports   string            `json:"ports,omitempty"`
	LeaseID string            `json:"lease_id"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// ListResult is the output of dol_list.
type ListResult struct {
	Leases    []ClientLease `json:"leases"`
	Resources []Resource    `json:"resources"`
	Warnings  []string      `json:"warnings"`
}

// ReleaseResult is the output of dol_release.
type ReleaseResult struct {
	LeaseID   string    `json:"lease_id"`
	Placement Placement `json:"placement"`
	Node      string    `json:"node,omitempty"`
	Removed   Inventory `json:"removed"`
	Forwards  []int     `json:"closed_forwards"`
	Warnings  []string  `json:"warnings"`
}

// ActionResult is the output of simple actions (stop/remove/resize/pin/forward…).
type ActionResult struct {
	PlacementInfo
	Done    []string `json:"done"`
	Details any      `json:"details,omitempty"`
}
