// Package kindx generates kind cluster configs and runs the kind binary on the
// node (brief §7.9, ADR-0007). Everything except Kind.* is a pure function.
//
// Verified kind facts live in docs/compat.md §1.
package kindx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"go.yaml.in/yaml/v3"
)

// kind's own cluster name rule (hard error) and our length cap (kind only
// warns above 50, but longer names break node hostnames).
var clusterNameRe = regexp.MustCompile(`^[a-z0-9.-]+$`)

// MaxClusterNameLen is kind's recommended maximum cluster name length.
const MaxClusterNameLen = 50

// ClusterName validates the user-facing name and returns the real kind
// cluster name "dol-<lease>-<name>".
func ClusterName(leaseID, name string) (string, error) {
	if !v1.ValidLeaseID(leaseID) {
		return "", v1.Errorf(v1.CodeBadRequest, "use the lease id returned by dol_acquire", "invalid lease id %q", leaseID)
	}
	if err := v1.ValidateKindName(name); err != nil {
		return "", err
	}
	c := v1.KindClusterName(leaseID, name)
	if !clusterNameRe.MatchString(c) || len(c) > MaxClusterNameLen {
		return "", v1.Errorf(v1.CodeBadRequest, "use a shorter name with lowercase letters, digits and '-'",
			"kind cluster name %q is invalid (must match %s and be at most %d characters)", c, clusterNameRe, MaxClusterNameLen)
	}
	return c, nil
}

var managedClusterRe = regexp.MustCompile(`^dol-(l-[a-z2-7]{10})-[a-z0-9-]+$`)

// LeaseOfCluster returns the lease id encoded in a dol-managed cluster name.
func LeaseOfCluster(cluster string) (string, bool) {
	m := managedClusterRe.FindStringSubmatch(cluster)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ConfigOptions are the node-controlled parts of a kind config.
type ConfigOptions struct {
	AccessMode string // v1.AccessTunnel | v1.AccessLAN
	Port       int    // API server port allocated from kind.port_range
	// CertSANs are added to the API server certificate in lan mode (LAN IP,
	// hostname). Ignored in tunnel mode.
	CertSANs []string
}

const (
	kindAPIVersion = "kind.x-k8s.io/v1alpha4"
	kindKind       = "Cluster"
)

// GenerateConfig merges an optional user kind config with the networking
// settings the node controls. It returns the YAML to pass to
// `kind create cluster --config` and warnings about overridden user values.
//
// tunnel: apiServerAddress 127.0.0.1 (reached through ssh -L).
// lan: apiServerAddress 0.0.0.0 plus a kubeadm ClusterConfiguration patch
// adding CertSANs; the patch has no apiVersion so it applies to kubeadm
// v1beta3 and v1beta4 alike, and kind appends (not replaces) certSANs.
func GenerateConfig(userYAML string, o ConfigOptions) ([]byte, []string, error) {
	bad := func(format string, a ...any) error {
		return v1.Errorf(v1.CodeBadRequest, "pass a valid kind Cluster config (kind.x-k8s.io/v1alpha4) or none at all", format, a...)
	}
	if o.Port <= 0 || o.Port > 65535 {
		return nil, nil, fmt.Errorf("internal: invalid api server port %d", o.Port)
	}
	doc := map[string]any{}
	if strings.TrimSpace(userYAML) != "" {
		dec := yaml.NewDecoder(strings.NewReader(userYAML))
		if err := dec.Decode(&doc); err != nil {
			return nil, nil, bad("cannot parse config_yaml: %v", err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, nil, bad("config_yaml must contain a single YAML document")
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	if k, ok := doc["kind"]; ok && k != kindKind {
		return nil, nil, bad("config_yaml has kind %v, expected %s", k, kindKind)
	}
	if av, ok := doc["apiVersion"]; ok && av != kindAPIVersion {
		return nil, nil, bad("config_yaml has apiVersion %v, expected %s", av, kindAPIVersion)
	}
	doc["kind"], doc["apiVersion"] = kindKind, kindAPIVersion

	var warnings []string
	net := map[string]any{}
	if n, ok := doc["networking"]; ok && n != nil {
		m, ok := n.(map[string]any)
		if !ok {
			return nil, nil, bad("networking must be a mapping")
		}
		net = m
	}
	addr := "127.0.0.1"
	switch o.AccessMode {
	case v1.AccessTunnel, "":
	case v1.AccessLAN:
		addr = "0.0.0.0"
	default:
		return nil, nil, bad("access_mode %q is invalid: use tunnel or lan", o.AccessMode)
	}
	if v, ok := net["apiServerAddress"]; ok && fmt.Sprint(v) != addr {
		warnings = append(warnings, fmt.Sprintf("networking.apiServerAddress %v replaced by %s (set by access_mode)", v, addr))
	}
	if v, ok := net["apiServerPort"]; ok && fmt.Sprint(v) != strconv.Itoa(o.Port) {
		warnings = append(warnings, fmt.Sprintf("networking.apiServerPort %v replaced by %d (allocated by the node)", v, o.Port))
	}
	net["apiServerAddress"] = addr
	net["apiServerPort"] = o.Port
	doc["networking"] = net

	var patches []any
	if p, ok := doc["kubeadmConfigPatches"]; ok && p != nil {
		l, ok := p.([]any)
		if !ok {
			return nil, nil, bad("kubeadmConfigPatches must be a list of strings")
		}
		for i, e := range l {
			if _, ok := e.(string); !ok {
				return nil, nil, bad("kubeadmConfigPatches[%d] must be a string (a YAML patch, usually written with |)", i)
			}
		}
		patches = l
	}
	if o.AccessMode == v1.AccessLAN && len(o.CertSANs) > 0 {
		doc["kubeadmConfigPatches"] = append(patches, CertSANsPatch(o.CertSANs))
	}
	if _, err := PortMappingsFromDoc(doc); err != nil {
		return nil, nil, bad("%v", err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), warnings, nil
}

// CertSANsPatch returns a kubeadm ClusterConfiguration patch adding sans to
// the API server certificate. Values are double-quoted (JSON quoting is valid
// YAML) so IPs stay strings.
func CertSANsPatch(sans []string) string {
	var b strings.Builder
	b.WriteString("kind: ClusterConfiguration\napiServer:\n  certSANs:\n")
	for _, s := range sans {
		b.WriteString("  - " + strconv.Quote(s) + "\n")
	}
	return b.String()
}

// PortMappings extracts nodes[].extraPortMappings from a kind config.
func PortMappings(cfg []byte) ([]v1.PortMapping, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		return nil, err
	}
	return PortMappingsFromDoc(doc)
}

// PortMappingsFromDoc extracts extraPortMappings from a decoded config.
func PortMappingsFromDoc(doc map[string]any) ([]v1.PortMapping, error) {
	nodes, _ := doc["nodes"].([]any)
	var out []v1.PortMapping
	for i, n := range nodes {
		nm, ok := n.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("nodes[%d] must be a mapping", i)
		}
		pms, ok := nm["extraPortMappings"]
		if !ok || pms == nil {
			continue
		}
		list, ok := pms.([]any)
		if !ok {
			return nil, fmt.Errorf("nodes[%d].extraPortMappings must be a list", i)
		}
		for j, pm := range list {
			m, ok := pm.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("nodes[%d].extraPortMappings[%d] must be a mapping", i, j)
			}
			cp, okc := m["containerPort"].(int)
			hp, okh := m["hostPort"].(int)
			if !okc || !okh || cp <= 0 || cp > 65535 || hp < 0 || hp > 65535 {
				return nil, fmt.Errorf("nodes[%d].extraPortMappings[%d] needs integer containerPort and hostPort", i, j)
			}
			p := v1.PortMapping{ContainerPort: cp, HostPort: hp}
			p.ListenAddress, _ = m["listenAddress"].(string)
			p.Protocol, _ = m["protocol"].(string)
			out = append(out, p)
		}
	}
	return out, nil
}

// ErrNoPort means the whole port range is in use.
var ErrNoPort = errors.New("no free port")

// AllocatePort returns the lowest port in [lo, hi] that is not in used and
// for which free (if non-nil) reports true.
func AllocatePort(lo, hi int, used map[int]bool, free func(int) bool) (int, error) {
	for p := lo; p <= hi; p++ {
		if used[p] {
			continue
		}
		if free != nil && !free(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w in kind.port_range [%d, %d]: %d allocated", ErrNoPort, lo, hi, len(used))
}

// Runner executes the kind binary. Tests substitute a fake.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs a real kind binary.
type ExecRunner struct{ Bin string }

// Run implements Runner. Errors carry the last lines of stderr (kind logs
// progress there); kubeconfigs are written to files, never to stderr.
func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "kind"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CmdError{Args: args, Err: err, Stderr: lastLines(stderr.String(), 6)}
	}
	return stdout.Bytes(), nil
}

// CmdError is a failed kind invocation.
type CmdError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *CmdError) Error() string {
	a := strings.Join(e.Args, " ")
	if e.Stderr != "" {
		return fmt.Sprintf("kind %s: %s", a, e.Stderr)
	}
	return fmt.Sprintf("kind %s: %v", a, e.Err)
}

func (e *CmdError) Unwrap() error { return e.Err }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Kind wraps kind subcommands.
type Kind struct{ R Runner }

// Create runs `kind create cluster`. The kubeconfig is written to
// kubeconfigPath (callers read it and must never log it).
func (k *Kind) Create(ctx context.Context, cluster, configPath, kubeconfigPath string, wait time.Duration) error {
	_, err := k.R.Run(ctx, "create", "cluster", "--name", cluster, "--config", configPath,
		"--kubeconfig", kubeconfigPath, "--wait", fmt.Sprintf("%ds", int(wait.Seconds())))
	return err
}

// Load runs `kind load docker-image` for images already present on the node.
func (k *Kind) Load(ctx context.Context, cluster string, images []string) error {
	_, err := k.R.Run(ctx, append([]string{"load", "docker-image", "--name", cluster}, images...)...)
	return err
}

// Delete runs `kind delete cluster` (a no-op for unknown clusters).
func (k *Kind) Delete(ctx context.Context, cluster string) error {
	_, err := k.R.Run(ctx, "delete", "cluster", "--name", cluster)
	return err
}

// Clusters runs `kind get clusters`.
func (k *Kind) Clusters(ctx context.Context) ([]string, error) {
	out, err := k.R.Run(ctx, "get", "clusters")
	if err != nil {
		return nil, err
	}
	var cs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "No kind clusters") {
			cs = append(cs, l)
		}
	}
	sort.Strings(cs)
	return cs, nil
}
