// Package clientsetup implements the file and docker-context changes made by
// `dol init` on the client. Every operation is idempotent and reports whether
// it changed anything (ADR-0034).
package clientsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/doctor"
	"go.yaml.in/yaml/v3"
)

// Change describes the outcome of one step.
type Change struct {
	Changed bool
	What    string
	Path    string
	Backup  string // backup file written before modifying, if any
}

func (c Change) String() string {
	state := "unchanged"
	if c.Changed {
		state = "changed"
	}
	s := fmt.Sprintf("[%s] %s", state, c.What)
	if c.Path != "" {
		s += " (" + c.Path + ")"
	}
	if c.Backup != "" {
		s += ", backup: " + c.Backup
	}
	return s
}

// HostSpec describes the ssh Host block for one node.
type HostSpec struct {
	Alias        string // Host alias, also the node's ssh_host
	HostName     string // IP or DNS name
	User         string
	Port         int
	IdentityFile string // optional
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:_-]*$`)

// Validate rejects values that could inject ssh config directives.
func (h HostSpec) Validate() error {
	if !aliasRe.MatchString(h.Alias) {
		return fmt.Errorf("ssh alias %q: use letters, digits, '.', '_' or '-'", h.Alias)
	}
	if !hostNameRe.MatchString(h.HostName) {
		return fmt.Errorf("host %q: use an IP address or DNS name", h.HostName)
	}
	if !aliasRe.MatchString(h.User) {
		return fmt.Errorf("user %q is not a valid user name", h.User)
	}
	if h.Port < 1 || h.Port > 65535 {
		return fmt.Errorf("port %d is out of range", h.Port)
	}
	if strings.ContainsAny(h.IdentityFile, "\n\r\"") {
		return fmt.Errorf("identity file %q contains invalid characters", h.IdentityFile)
	}
	return nil
}

func beginMarker(alias string) string { return "# BEGIN docker-offload-lan node " + alias }
func endMarker(alias string) string   { return "# END docker-offload-lan node " + alias }

// RenderHostBlock renders the managed Host block (with markers).
func RenderHostBlock(h HostSpec) string {
	var b strings.Builder
	fmt.Fprintln(&b, beginMarker(h.Alias))
	fmt.Fprintf(&b, "Host %s\n", h.Alias)
	fmt.Fprintf(&b, "    HostName %s\n", h.HostName)
	fmt.Fprintf(&b, "    Port %d\n", h.Port)
	fmt.Fprintf(&b, "    User %s\n", h.User)
	if h.IdentityFile != "" {
		fmt.Fprintf(&b, "    IdentityFile \"%s\"\n", h.IdentityFile)
		fmt.Fprintln(&b, "    IdentitiesOnly yes")
	}
	// Multiplex docker/dol calls over one connection (Docker recommends it).
	fmt.Fprintln(&b, "    ControlMaster auto")
	fmt.Fprintln(&b, "    ControlPath ~/.ssh/cm-%C")
	fmt.Fprintln(&b, "    ControlPersist 10m")
	fmt.Fprintln(&b, "    ServerAliveInterval 15")
	fmt.Fprintln(&b, "    ServerAliveCountMax 3")
	fmt.Fprintln(&b, endMarker(h.Alias))
	return b.String()
}

// SSHPaths are the files dol init manages under ~/.ssh.
type SSHPaths struct {
	Dir      string // ~/.ssh
	Config   string // ~/.ssh/config
	Fragment string // ~/.ssh/config.d/docker-offload-lan
}

// DefaultSSHPaths derives the paths from $HOME.
func DefaultSSHPaths() (SSHPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return SSHPaths{}, err
	}
	d := filepath.Join(home, ".ssh")
	return SSHPaths{Dir: d, Config: filepath.Join(d, "config"), Fragment: filepath.Join(d, "config.d", "docker-offload-lan")}, nil
}

// UpsertHostBlock writes or replaces the node's block in the fragment file.
func UpsertHostBlock(p SSHPaths, h HostSpec) (Change, error) {
	if err := h.Validate(); err != nil {
		return Change{}, err
	}
	c := Change{What: "ssh Host " + h.Alias, Path: p.Fragment}
	if err := os.MkdirAll(filepath.Dir(p.Fragment), 0o700); err != nil {
		return c, err
	}
	cur, err := os.ReadFile(p.Fragment)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	block := RenderHostBlock(h)
	next, err := replaceBlock(string(cur), h.Alias, block)
	if err != nil {
		return c, fmt.Errorf("%s: %w", p.Fragment, err)
	}
	if next == string(cur) {
		return c, nil
	}
	if len(cur) == 0 {
		next = "# Managed by `dol init` (docker-offload-lan). Blocks between BEGIN/END markers are rewritten.\n\n" + next
	}
	c.Changed = true
	return c, writeAtomic(p.Fragment, []byte(next), 0o600)
}

func replaceBlock(cur, alias, block string) (string, error) {
	b, e := beginMarker(alias), endMarker(alias)
	i := strings.Index(cur, b+"\n")
	if i < 0 {
		if hasUnmanagedHost(cur, alias) {
			return "", fmt.Errorf("a Host %s block exists outside dol's markers; remove it or pick another --name", alias)
		}
		sep := ""
		if cur != "" && !strings.HasSuffix(cur, "\n\n") {
			sep = "\n"
			if !strings.HasSuffix(cur, "\n") {
				sep = "\n\n"
			}
		}
		return cur + sep + block, nil
	}
	j := strings.Index(cur[i:], e+"\n")
	if j < 0 {
		return "", fmt.Errorf("found %q without matching %q; fix the file by hand", b, e)
	}
	return cur[:i] + block + cur[i+j+len(e)+1:], nil
}

func hasUnmanagedHost(cur, alias string) bool {
	for _, l := range strings.Split(cur, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.EqualFold(f[0], "Host") {
			for _, a := range f[1:] {
				if a == alias {
					return true
				}
			}
		}
	}
	return false
}

// IncludeLine is placed at the top of ~/.ssh/config. Relative Include paths
// are resolved against ~/.ssh.
const IncludeLine = "Include config.d/*"

var includeRe = regexp.MustCompile(`(?i)^\s*Include\s+(.*)$`)

// EnsureInclude adds IncludeLine at the top of ~/.ssh/config if no Include
// already covers the fragment. It must be at the top: an Include placed after
// a Host line would only apply inside that Host block.
func EnsureInclude(p SSHPaths, now time.Time) (Change, error) {
	c := Change{What: "ssh config includes config.d/*", Path: p.Config}
	cur, err := os.ReadFile(p.Config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	for _, l := range strings.Split(string(cur), "\n") {
		if m := includeRe.FindStringSubmatch(l); m != nil {
			for _, pat := range strings.Fields(m[1]) {
				if includeCovers(p, strings.Trim(pat, `"`)) {
					return c, nil
				}
			}
		}
		// Only the global section (before the first Host/Match) counts.
		f := strings.Fields(l)
		if len(f) > 0 && (strings.EqualFold(f[0], "Host") || strings.EqualFold(f[0], "Match")) {
			break
		}
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return c, err
	}
	if len(cur) > 0 {
		c.Backup = p.Config + ".bak-dol-" + now.Format("20060102-150405")
		if err := os.WriteFile(c.Backup, cur, 0o600); err != nil {
			return c, fmt.Errorf("backing up %s: %w", p.Config, err)
		}
	}
	next := "# Added by `dol init` (docker-offload-lan); must stay above any Host block.\n" + IncludeLine + "\n\n" + string(cur)
	c.Changed = true
	return c, writeAtomic(p.Config, []byte(next), 0o600)
}

func includeCovers(p SSHPaths, pat string) bool {
	if strings.HasPrefix(pat, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pat = filepath.Join(home, pat[2:])
		}
	}
	if !filepath.IsAbs(pat) {
		pat = filepath.Join(p.Dir, pat)
	}
	ok, _ := filepath.Match(pat, p.Fragment)
	return ok
}

// UpsertClientNode adds or updates a node in the client config, preserving the
// rest of the file (comments included). The result must load cleanly.
func UpsertClientNode(path string, n config.NodeRef) (Change, error) {
	c := Change{What: "client config node " + n.Name, Path: path}
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(cur)) == 0 {
		cur = nil
		if err := yaml.Unmarshal([]byte(defaultClientYAML()), &doc); err != nil {
			return c, err
		}
	} else if err := yaml.Unmarshal(cur, &doc); err != nil {
		return c, fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return c, fmt.Errorf("%s: top level must be a mapping", path)
	}
	root := doc.Content[0]
	var nodes *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "nodes" {
			nodes = root.Content[i+1]
		}
	}
	if nodes == nil || (nodes.Kind == yaml.ScalarNode && (nodes.Tag == "!!null" || nodes.Value == "")) {
		if nodes == nil {
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "nodes"}, &yaml.Node{Kind: yaml.SequenceNode})
			nodes = root.Content[len(root.Content)-1]
		} else {
			*nodes = yaml.Node{Kind: yaml.SequenceNode}
		}
	}
	if nodes.Kind != yaml.SequenceNode {
		return c, fmt.Errorf("%s: nodes must be a list", path)
	}
	var item yaml.Node
	if err := item.Encode(n); err != nil {
		return c, err
	}
	replaced := false
	for i, el := range nodes.Content {
		if el.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(el.Content); j += 2 {
			if el.Content[j].Value == "name" && el.Content[j+1].Value == n.Name {
				var existing config.NodeRef
				if err := el.Decode(&existing); err == nil && existing == n && cur != nil {
					return c, nil
				}
				// Keep keys we do not manage (e.g. wake_on_lan, priority).
				mergeMapping(el, &item)
				nodes.Content[i] = el
				replaced = true
			}
		}
	}
	if !replaced {
		nodes.Style = 0
		nodes.Content = append(nodes.Content, &item)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return c, err
	}
	if bytes.Equal(out.Bytes(), cur) {
		return c, nil
	}
	if err := validateClientYAML(out.Bytes()); err != nil {
		return c, fmt.Errorf("refusing to write an invalid config: %w", err)
	}
	if cur != nil {
		c.Backup = path + ".bak-dol"
		if err := os.WriteFile(c.Backup, cur, 0o600); err != nil {
			return c, err
		}
	}
	c.Changed = true
	return c, writeAtomic(path, out.Bytes(), 0o600)
}

// validateClientYAML runs the real loader (strict keys + semantic checks).
func validateClientYAML(b []byte) error {
	dir, err := os.MkdirTemp("", "dol-config-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	_, err = config.LoadClient(p)
	return err
}

// mergeMapping sets every key of src on dst (adding or replacing values).
func mergeMapping(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		found := false
		for j := 0; j+1 < len(dst.Content); j += 2 {
			if dst.Content[j].Value == k.Value {
				dst.Content[j+1] = v
				found = true
			}
		}
		if !found {
			dst.Content = append(dst.Content, k, v)
		}
	}
}

func defaultClientYAML() string {
	return `# docker-offload-lan client config. Schema: schemas/client-config.schema.json
version: 1
identity:
  owner: "" # default: $USER@$(hostname -s)
nodes: []
local:
  docker_context: auto # auto-detects orbstack / desktop-linux / colima
  max_memory_gb: 8
policy:
  prefer: remote # remote | local
  fallback: always # always | if_capacity | never
  health_timeout_ms: 2000
  lease_request_timeout_ms: 5000
  heartbeat_interval_s: 30
sync:
  remote_root: /srv/dol
  respect_gitignore: true
`
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// EnsureDockerContext creates context name → ssh://sshHost, or fixes the host
// of a context dol created earlier. It never touches contexts without dol's
// description and never changes the current context.
func EnsureDockerContext(ctx context.Context, r doctor.Runner, name, sshHost string) (Change, error) {
	c := Change{What: "docker context " + name + " → ssh://" + sshHost}
	want := "host=ssh://" + sshHost
	out, se, err := r.Run(ctx, "docker", "context", "inspect", name, "--format", "{{json .}}")
	if err != nil {
		if !strings.Contains(strings.ToLower(se), "not found") && !strings.Contains(strings.ToLower(se), "does not exist") {
			return c, fmt.Errorf("docker context inspect %s: %s", name, strings.TrimSpace(se+" "+err.Error()))
		}
		if _, se, err := r.Run(ctx, "docker", "context", "create", name,
			"--description", doctor.ManagedContextDescription, "--docker", want); err != nil {
			return c, fmt.Errorf("docker context create %s: %s", name, strings.TrimSpace(se+" "+err.Error()))
		}
		c.Changed = true
		return c, nil
	}
	var ci struct {
		Metadata struct {
			Description string `json:"Description"`
		} `json:"Metadata"`
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ci); err != nil {
		return c, fmt.Errorf("parsing docker context inspect %s: %w", name, err)
	}
	if ci.Endpoints["docker"].Host == "ssh://"+sshHost {
		return c, nil
	}
	if ci.Metadata.Description != doctor.ManagedContextDescription {
		return c, fmt.Errorf("docker context %s exists, points to %s and was not created by dol; remove it (`docker context rm %s`) or choose another --name",
			name, ci.Endpoints["docker"].Host, name)
	}
	if _, se, err := r.Run(ctx, "docker", "context", "update", name, "--docker", want); err != nil {
		return c, fmt.Errorf("docker context update %s: %s", name, strings.TrimSpace(se+" "+err.Error()))
	}
	c.Changed = true
	return c, nil
}

// LocalDockerMemory returns the local Docker VM's memory in bytes (0 if
// unknown) for the "lower your local VM memory" suggestion.
func LocalDockerMemory(ctx context.Context, r doctor.Runner, contextName string) int64 {
	out, _, err := r.Run(ctx, "docker", "--context", contextName, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}

// ClaudeMCPAddArgs is the canonical registration (docs/compat.md §6).
var ClaudeMCPAddArgs = []string{"mcp", "add", "-s", "user", "docker-offload-lan", "--", "dol", "mcp"}

// ClaudeMCPRegistered reports whether `claude mcp get docker-offload-lan` succeeds.
func ClaudeMCPRegistered(ctx context.Context, r doctor.Runner) bool {
	_, _, err := r.Run(ctx, "claude", "mcp", "get", "docker-offload-lan")
	return err == nil
}
