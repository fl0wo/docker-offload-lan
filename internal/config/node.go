package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NodeConfigPath is the default node config location.
const NodeConfigPath = "/etc/docker-offload-lan/node.yaml"

// NodeConfig is /etc/docker-offload-lan/node.yaml (brief §7.1).
type NodeConfig struct {
	Version  int            `yaml:"version" json:"version" jsonschema:"config schema version; must be 1"`
	Name     string         `yaml:"name" json:"name"`
	Socket   string         `yaml:"socket" json:"socket" jsonschema:"unix socket of the node API; group-owned by dol"`
	DataDir  string         `yaml:"data_dir" json:"data_dir"`
	SyncRoot string         `yaml:"sync_root" json:"sync_root" jsonschema:"root of client workspace syncs; must match the client's sync.remote_root"`
	Capacity CapacityConfig `yaml:"capacity" json:"capacity"`
	Leases   LeasesConfig   `yaml:"leases" json:"leases"`
	Reaper   ReaperConfig   `yaml:"reaper" json:"reaper"`
	Kind     KindConfig     `yaml:"kind" json:"kind"`
}

type CapacityConfig struct {
	Provider           string  `yaml:"provider" json:"provider" jsonschema:"auto | linux | wsl2 | macos-vm | fake"`
	ReserveMemoryGB    float64 `yaml:"reserve_memory_gb" json:"reserve_memory_gb"`
	MinFreeDiskGB      float64 `yaml:"min_free_disk_gb" json:"min_free_disk_gb"`
	WindowsMemoryCheck bool    `yaml:"windows_memory_check" json:"windows_memory_check"`
	// Fake configures the fake provider (tests and demos only).
	Fake *FakeCapacity `yaml:"fake,omitempty" json:"fake,omitempty"`
}

// FakeCapacity is a static capacity snapshot for the fake provider.
type FakeCapacity struct {
	TotalMemoryGB     float64 `yaml:"total_memory_gb" json:"total_memory_gb"`
	AvailableMemoryGB float64 `yaml:"available_memory_gb" json:"available_memory_gb"`
	FreeDiskGB        float64 `yaml:"free_disk_gb" json:"free_disk_gb"`
	HostFreeMemoryGB  float64 `yaml:"host_free_memory_gb,omitempty" json:"host_free_memory_gb,omitempty"`
	HostFreeDiskGB    float64 `yaml:"host_free_disk_gb,omitempty" json:"host_free_disk_gb,omitempty"`
}

type LeasesConfig struct {
	HeartbeatGraceS int `yaml:"heartbeat_grace_s" json:"heartbeat_grace_s"`
	MaxTTLS         int `yaml:"max_ttl_s" json:"max_ttl_s"`
	MaxActive       int `yaml:"max_active" json:"max_active"`
}

type ReaperConfig struct {
	IntervalS           int `yaml:"interval_s" json:"interval_s"`
	ReapUnlabeledAfterS int `yaml:"reap_unlabeled_after_s" json:"reap_unlabeled_after_s" jsonschema:"0 = never delete unlabeled resources"`
}

type KindConfig struct {
	Binary     string `yaml:"binary" json:"binary"`
	PortRange  [2]int `yaml:"port_range" json:"port_range"`
	AccessMode string `yaml:"access_mode" json:"access_mode" jsonschema:"tunnel | lan"`
	// LANAddress overrides the address put in certSANs / kubeconfig in lan mode.
	LANAddress string `yaml:"lan_address,omitempty" json:"lan_address,omitempty"`
}

// DefaultNode returns a node config with all defaults applied.
func DefaultNode() *NodeConfig {
	h, _ := os.Hostname()
	return &NodeConfig{
		Version:  1,
		Name:     strings.ToLower(h),
		Socket:   "/run/docker-offload-lan/node.sock",
		DataDir:  "/var/lib/docker-offload-lan",
		SyncRoot: "/srv/dol",
		Capacity: CapacityConfig{
			Provider: "auto", ReserveMemoryGB: 4, MinFreeDiskGB: 30, WindowsMemoryCheck: true,
		},
		Leases: LeasesConfig{HeartbeatGraceS: 600, MaxTTLS: 86400, MaxActive: 32},
		Reaper: ReaperConfig{IntervalS: 60},
		Kind: KindConfig{
			Binary: "/usr/local/bin/kind", PortRange: [2]int{16443, 16999}, AccessMode: "tunnel",
		},
	}
}

// LoadNode reads and validates the node config. Unlike the client config, a
// missing file is an error: run `dol node init`.
func LoadNode(path string) (*NodeConfig, error) {
	c := DefaultNode()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading node config %s: %w\n(hint: run `sudo dol node init` to create it)", path, err)
	}
	if err := decodeStrict(b, c); err != nil {
		return nil, fmt.Errorf("node config %s: %w\n(hint: compare with schemas/node-config.schema.json)", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("node config %s: %w", path, err)
	}
	return c, nil
}

// Validate checks semantic constraints.
func (c *NodeConfig) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version: %d is not supported (expected 1)", c.Version)
	}
	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("name %q: use 1-32 lowercase letters, digits or '-'", c.Name)
	}
	if !filepath.IsAbs(c.Socket) || !filepath.IsAbs(c.DataDir) || !filepath.IsAbs(c.SyncRoot) || filepath.Clean(c.SyncRoot) == "/" {
		return fmt.Errorf("socket, data_dir and sync_root must be absolute paths (sync_root must not be /)")
	}
	switch c.Capacity.Provider {
	case "auto", "linux", "wsl2", "macos-vm":
	case "fake":
		if c.Capacity.Fake == nil {
			return fmt.Errorf("capacity.provider is fake but capacity.fake is not set")
		}
	default:
		return fmt.Errorf("capacity.provider %q: use auto, linux, wsl2, macos-vm or fake", c.Capacity.Provider)
	}
	if c.Capacity.ReserveMemoryGB < 0 || c.Capacity.MinFreeDiskGB < 0 {
		return fmt.Errorf("capacity reserve/min values must be >= 0")
	}
	if c.Leases.HeartbeatGraceS <= 0 || c.Leases.MaxTTLS <= 0 || c.Leases.MaxActive <= 0 {
		return fmt.Errorf("leases.heartbeat_grace_s, max_ttl_s and max_active must be > 0")
	}
	if c.Reaper.IntervalS <= 0 || c.Reaper.ReapUnlabeledAfterS < 0 {
		return fmt.Errorf("reaper.interval_s must be > 0 and reap_unlabeled_after_s >= 0")
	}
	if c.Kind.PortRange[0] < 1024 || c.Kind.PortRange[1] > 65535 || c.Kind.PortRange[0] > c.Kind.PortRange[1] {
		return fmt.Errorf("kind.port_range %v must be within [1024, 65535] and ordered", c.Kind.PortRange)
	}
	if c.Kind.AccessMode != "tunnel" && c.Kind.AccessMode != "lan" {
		return fmt.Errorf("kind.access_mode %q: use tunnel or lan", c.Kind.AccessMode)
	}
	return nil
}
