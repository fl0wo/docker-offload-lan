package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

// ClientConfig is ~/.config/docker-offload-lan/config.yaml (brief §7.1).
type ClientConfig struct {
	Version    int         `yaml:"version" json:"version" jsonschema:"config schema version; must be 1"`
	Identity   Identity    `yaml:"identity" json:"identity"`
	Nodes      []NodeRef   `yaml:"nodes" json:"nodes"`
	Local      LocalConfig `yaml:"local" json:"local"`
	Policy     Policy      `yaml:"policy" json:"policy"`
	Forwarding Forwarding  `yaml:"forwarding" json:"forwarding"`
	Sync       SyncConfig  `yaml:"sync" json:"sync"`
}

type Identity struct {
	Owner string `yaml:"owner" json:"owner" jsonschema:"owner label; default $USER@$(hostname -s)"`
}

type NodeRef struct {
	Name          string     `yaml:"name" json:"name" jsonschema:"node name (lowercase letters, digits, -)"`
	SSHHost       string     `yaml:"ssh_host" json:"ssh_host" jsonschema:"ssh destination, ideally an alias from ~/.ssh/config"`
	DockerContext string     `yaml:"docker_context,omitempty" json:"docker_context,omitempty" jsonschema:"docker context managed by dol; default dol-<name>"`
	Priority      int        `yaml:"priority,omitempty" json:"priority,omitempty" jsonschema:"higher is tried first; default 100"`
	WakeOnLAN     *WakeOnLAN `yaml:"wake_on_lan,omitempty" json:"wake_on_lan,omitempty"`
}

type WakeOnLAN struct {
	MAC       string `yaml:"mac" json:"mac"`
	Broadcast string `yaml:"broadcast,omitempty" json:"broadcast,omitempty"`
}

type LocalConfig struct {
	DockerContext string  `yaml:"docker_context" json:"docker_context" jsonschema:"auto | a docker context name"`
	MaxMemoryGB   float64 `yaml:"max_memory_gb" json:"max_memory_gb" jsonschema:"advisory: warn when a local fallback exceeds this"`
}

type Policy struct {
	Prefer                string `yaml:"prefer" json:"prefer" jsonschema:"remote | local"`
	Fallback              string `yaml:"fallback" json:"fallback" jsonschema:"always | if_capacity | never"`
	HealthTimeoutMS       int    `yaml:"health_timeout_ms" json:"health_timeout_ms"`
	LeaseRequestTimeoutMS int    `yaml:"lease_request_timeout_ms" json:"lease_request_timeout_ms"`
	HeartbeatIntervalS    int    `yaml:"heartbeat_interval_s" json:"heartbeat_interval_s"`
}

type Forwarding struct {
	AutoForward bool `yaml:"auto_forward" json:"auto_forward"`
}

type SyncConfig struct {
	RemoteRoot       string `yaml:"remote_root" json:"remote_root"`
	RespectGitignore bool   `yaml:"respect_gitignore" json:"respect_gitignore"`
}

// DefaultClient returns a client config with all defaults applied.
func DefaultClient() *ClientConfig {
	return &ClientConfig{
		Version:  1,
		Identity: Identity{Owner: DefaultOwner()},
		Local:    LocalConfig{DockerContext: "auto", MaxMemoryGB: 8},
		Policy: Policy{
			Prefer: "remote", Fallback: "always",
			HealthTimeoutMS: 2000, LeaseRequestTimeoutMS: 5000, HeartbeatIntervalS: 30,
		},
		Sync: SyncConfig{RemoteRoot: "/srv/dol", RespectGitignore: true},
	}
}

// DefaultOwner is $USER@$(hostname -s).
func DefaultOwner() string {
	u := os.Getenv("USER")
	if u == "" {
		if cu, err := user.Current(); err == nil {
			u = cu.Username
		}
	}
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(u + "@" + h)
}

// ClientConfigPath returns $XDG_CONFIG_HOME/docker-offload-lan/config.yaml.
func ClientConfigPath() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), AppDir, "config.yaml")
}

// StateDir returns $XDG_STATE_HOME/docker-offload-lan.
func StateDir() string { return filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), AppDir) }

// AppDir is the directory name used under XDG roots and /etc.
const AppDir = "docker-offload-lan"

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, fallback)
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// LoadClient reads and validates the client config. A missing file yields the
// defaults with no nodes (everything falls back to local), not an error.
func LoadClient(path string) (*ClientConfig, error) {
	c := DefaultClient()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading client config %s: %w", path, err)
	}
	if err := decodeStrict(b, c); err != nil {
		return nil, fmt.Errorf("client config %s: %w\n(hint: compare with schemas/client-config.schema.json or run `dol init`)", path, err)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("client config %s: %w", path, err)
	}
	return c, nil
}

func (c *ClientConfig) normalize() error {
	if c.Version != 1 {
		return fmt.Errorf("version: %d is not supported (expected 1)", c.Version)
	}
	if c.Identity.Owner == "" {
		c.Identity.Owner = DefaultOwner()
	}
	seen := map[string]bool{}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if !nameRe.MatchString(n.Name) {
			return fmt.Errorf("nodes[%d].name %q: use 1-32 lowercase letters, digits or '-'", i, n.Name)
		}
		if seen[n.Name] {
			return fmt.Errorf("nodes[%d].name %q is duplicated", i, n.Name)
		}
		seen[n.Name] = true
		if n.SSHHost == "" || strings.HasPrefix(n.SSHHost, "-") || strings.ContainsAny(n.SSHHost, " \t\n'\"`$;|&") {
			return fmt.Errorf("nodes[%d].ssh_host %q: must be a plain ssh destination such as an alias from ~/.ssh/config", i, n.SSHHost)
		}
		if n.DockerContext == "" {
			n.DockerContext = "dol-" + n.Name
		}
		if n.Priority == 0 {
			n.Priority = 100
		}
	}
	p := &c.Policy
	if p.Prefer != "remote" && p.Prefer != "local" {
		return fmt.Errorf("policy.prefer %q: use remote or local", p.Prefer)
	}
	if p.Fallback != "always" && p.Fallback != "if_capacity" && p.Fallback != "never" {
		return fmt.Errorf("policy.fallback %q: use always, if_capacity or never", p.Fallback)
	}
	if p.HealthTimeoutMS <= 0 || p.LeaseRequestTimeoutMS <= 0 || p.HeartbeatIntervalS <= 0 {
		return fmt.Errorf("policy timeouts and heartbeat_interval_s must be > 0")
	}
	if c.Local.DockerContext == "" {
		c.Local.DockerContext = "auto"
	}
	if !strings.HasPrefix(c.Sync.RemoteRoot, "/") {
		return fmt.Errorf("sync.remote_root %q must be an absolute path", c.Sync.RemoteRoot)
	}
	return nil
}
