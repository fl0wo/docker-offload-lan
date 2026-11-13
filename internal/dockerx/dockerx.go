// Package dockerx wraps the docker CLI (ADR-0009) with label-scoped
// operations. Every destructive method re-verifies ownership labels on the
// inspected object before acting (ADR-0006).
package dockerx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Runner executes the docker binary. Tests substitute a fake.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout []byte, err error)
}

// ExecRunner runs a real binary.
type ExecRunner struct {
	Bin string // default "docker"
	// Env is appended to the current environment (e.g. DOCKER_CONFIG).
	Env []string
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(r.Env) > 0 {
		cmd.Env = append(cmd.Environ(), r.Env...)
	}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CmdError{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// CmdError is a failed docker invocation.
type CmdError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *CmdError) Error() string {
	a := strings.Join(e.Args, " ")
	if len(a) > 120 {
		a = a[:120] + "…"
	}
	if e.Stderr != "" {
		return fmt.Sprintf("docker %s: %s", a, e.Stderr)
	}
	return fmt.Sprintf("docker %s: %v", a, e.Err)
}

func (e *CmdError) Unwrap() error { return e.Err }

// IsNotFound reports whether err looks like "No such container/network/volume".
func IsNotFound(err error) bool {
	var ce *CmdError
	if errors.As(err, &ce) {
		s := strings.ToLower(ce.Stderr)
		return strings.Contains(s, "no such") || strings.Contains(s, "not found")
	}
	return false
}

// IsDaemonDown reports whether err means the daemon is unreachable.
func IsDaemonDown(err error) bool {
	var ce *CmdError
	if errors.As(err, &ce) {
		s := strings.ToLower(ce.Stderr)
		return strings.Contains(s, "cannot connect to the docker daemon") ||
			strings.Contains(s, "is the docker daemon running") ||
			strings.Contains(s, "error during connect")
	}
	var ee *exec.Error
	return errors.As(err, &ee) // docker binary missing
}

// Docker is a docker CLI bound to one context ("" = the CLI's default).
type Docker struct {
	Context string
	R       Runner
}

// New returns a Docker using the real CLI.
func New(contextName string) *Docker { return &Docker{Context: contextName, R: ExecRunner{}} }

func (d *Docker) run(ctx context.Context, args ...string) ([]byte, error) {
	return d.runIn(ctx, nil, args...)
}

func (d *Docker) runIn(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if d.Context != "" {
		args = append([]string{"--context", d.Context}, args...)
	}
	return d.R.Run(ctx, stdin, args...)
}

// Run runs arbitrary docker args with this context (for compose etc.).
func (d *Docker) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return d.runIn(ctx, stdin, args...)
}

// Info is a subset of `docker info`.
type Info struct {
	ServerVersion   string   `json:"ServerVersion"`
	OSType          string   `json:"OSType"`
	Architecture    string   `json:"Architecture"`
	MemTotal        int64    `json:"MemTotal"`
	NCPU            int      `json:"NCPU"`
	DockerRootDir   string   `json:"DockerRootDir"`
	Name            string   `json:"Name"`
	OperatingSystem string   `json:"OperatingSystem"`
	ServerErrors    []string `json:"ServerErrors"`
}

// GoArch maps docker's Architecture (x86_64, aarch64) to GOARCH names.
func (i *Info) GoArch() string {
	switch i.Architecture {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return i.Architecture
}

// Info runs `docker info`.
func (d *Docker) Info(ctx context.Context) (*Info, error) {
	out, err := d.run(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var i Info
	if err := json.Unmarshal(bytes.TrimSpace(out), &i); err != nil {
		return nil, fmt.Errorf("parsing docker info: %w", err)
	}
	if len(i.ServerErrors) > 0 {
		return &i, fmt.Errorf("docker daemon: %s", strings.Join(i.ServerErrors, "; "))
	}
	return &i, nil
}

// Container is a row of `docker ps`.
type Container struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
	// Labels is filled by Inspect-based helpers, not by ps.
	Labels map[string]string `json:"-"`
}

// Named is a row of `docker network ls` / `docker volume ls`.
type Named struct {
	ID   string `json:"ID"`
	Name string `json:"Name"`
}

func jsonLines[T any](out []byte) ([]T, error) {
	var res []T
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("parsing docker output %q: %w", truncate(string(line), 80), err)
		}
		res = append(res, v)
	}
	return res, sc.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func filterArgs(filters []string) []string {
	var a []string
	for _, f := range filters {
		a = append(a, "--filter", f)
	}
	return a
}

// Containers lists containers (all states) matching filters.
func (d *Docker) Containers(ctx context.Context, filters ...string) ([]Container, error) {
	args := append([]string{"ps", "-a", "--no-trunc", "--format", "{{json .}}"}, filterArgs(filters)...)
	out, err := d.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return jsonLines[Container](out)
}

// Networks lists networks matching filters.
func (d *Docker) Networks(ctx context.Context, filters ...string) ([]Named, error) {
	args := append([]string{"network", "ls", "--no-trunc", "--format", "{{json .}}"}, filterArgs(filters)...)
	out, err := d.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return jsonLines[Named](out)
}

// Volumes lists volumes matching filters.
func (d *Docker) Volumes(ctx context.Context, filters ...string) ([]Named, error) {
	args := append([]string{"volume", "ls", "--format", "{{json .}}"}, filterArgs(filters)...)
	out, err := d.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	vs, err := jsonLines[Named](out)
	for i := range vs {
		if vs[i].ID == "" {
			vs[i].ID = vs[i].Name
		}
	}
	return vs, err
}

// ObjectKind selects what Labels inspects.
type ObjectKind string

const (
	KindContainer ObjectKind = "container"
	KindNetwork   ObjectKind = "network"
	KindVolume    ObjectKind = "volume"
)

// Labels returns the labels of one object.
func (d *Docker) Labels(ctx context.Context, kind ObjectKind, ref string) (map[string]string, error) {
	format := "{{json .Labels}}"
	if kind == KindContainer {
		format = "{{json .Config.Labels}}"
	}
	out, err := d.run(ctx, string(kind), "inspect", "--format", format, "--", ref)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if t := bytes.TrimSpace(out); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &m); err != nil {
			return nil, fmt.Errorf("parsing labels of %s %s: %w", kind, ref, err)
		}
	}
	return m, nil
}

// RequireOwned returns a CodeNotOwned error unless the object carries the
// managed label and leaseID.
func (d *Docker) RequireOwned(ctx context.Context, kind ObjectKind, ref, leaseID string) (map[string]string, error) {
	labels, err := d.Labels(ctx, kind, ref)
	if err != nil {
		if IsNotFound(err) {
			return nil, v1.Errorf(v1.CodeNotFound, "list your resources with dol_list", "%s %q not found", kind, ref)
		}
		return nil, err
	}
	if !v1.IsOwnedBy(labels, leaseID) {
		owner := labels[v1.LabelLease]
		why := "it has no docker-offload-lan labels"
		if owner != "" {
			why = "it belongs to lease " + owner
		}
		return labels, v1.Errorf(v1.CodeNotOwned,
			"dol only stops/removes resources it created; add the label_flags from dol_acquire when creating resources",
			"refusing to touch %s %q: %s", kind, ref, why)
	}
	return labels, nil
}

// StopOwned stops a container after verifying ownership.
func (d *Docker) StopOwned(ctx context.Context, ref, leaseID string) error {
	if _, err := d.RequireOwned(ctx, KindContainer, ref, leaseID); err != nil {
		return err
	}
	_, err := d.run(ctx, "stop", "--time", "10", "--", ref)
	return err
}

// RemoveOwned removes a container/network/volume after verifying ownership.
func (d *Docker) RemoveOwned(ctx context.Context, kind ObjectKind, ref, leaseID string, volumes bool) error {
	if _, err := d.RequireOwned(ctx, kind, ref, leaseID); err != nil {
		return err
	}
	var err error
	switch kind {
	case KindContainer:
		args := []string{"rm", "-f"}
		if volumes {
			args = append(args, "-v")
		}
		_, err = d.run(ctx, append(args, "--", ref)...)
	default:
		_, err = d.run(ctx, string(kind), "rm", "--", ref)
	}
	if IsNotFound(err) {
		return nil
	}
	return err
}

// TeardownLease removes every container, network and volume labeled with
// leaseID, in that order (brief §7.6). Compose projects and kind clusters
// must be handled by the caller first. It is idempotent; partial failures are
// returned in Inventory.Errors and the caller retries later.
func (d *Docker) TeardownLease(ctx context.Context, leaseID string) v1.Inventory {
	inv := v1.Inventory{}
	if !v1.ValidLeaseID(leaseID) {
		inv.Errors = append(inv.Errors, fmt.Sprintf("refusing teardown: invalid lease id %q", leaseID))
		return inv
	}
	f := v1.LeaseFilter(leaseID)
	fail := func(format string, a ...any) { inv.Errors = append(inv.Errors, fmt.Sprintf(format, a...)) }

	cs, err := d.Containers(ctx, f)
	if err != nil {
		fail("listing containers: %v", err)
	}
	// Stop all first (in parallel would be faster; sequential keeps load low).
	for _, c := range cs {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			if err := d.StopOwned(ctx, c.ID, leaseID); err != nil && !IsNotFound(err) {
				fail("stopping container %s: %v", c.Names, err)
			}
		}
	}
	for _, c := range cs {
		if err := d.RemoveOwned(ctx, KindContainer, c.ID, leaseID, true); err != nil && !IsNotFound(err) && v1.CodeOf(err) != v1.CodeNotFound {
			fail("removing container %s: %v", c.Names, err)
			continue
		}
		inv.Containers = append(inv.Containers, c.Names)
	}
	ns, err := d.Networks(ctx, f)
	if err != nil {
		fail("listing networks: %v", err)
	}
	for _, n := range ns {
		if err := d.RemoveOwned(ctx, KindNetwork, n.ID, leaseID, false); err != nil && v1.CodeOf(err) != v1.CodeNotFound {
			fail("removing network %s: %v", n.Name, err)
			continue
		}
		inv.Networks = append(inv.Networks, n.Name)
	}
	vs, err := d.Volumes(ctx, f)
	if err != nil {
		fail("listing volumes: %v", err)
	}
	for _, v := range vs {
		if err := d.RemoveOwned(ctx, KindVolume, v.Name, leaseID, false); err != nil && v1.CodeOf(err) != v1.CodeNotFound {
			fail("removing volume %s: %v", v.Name, err)
			continue
		}
		inv.Volumes = append(inv.Volumes, v.Name)
	}
	return inv
}

// LeaseInventory lists resources labeled with leaseID without changing them.
func (d *Docker) LeaseInventory(ctx context.Context, leaseID string) (v1.Inventory, error) {
	inv := v1.Inventory{}
	f := v1.LeaseFilter(leaseID)
	cs, err := d.Containers(ctx, f)
	if err != nil {
		return inv, err
	}
	for _, c := range cs {
		inv.Containers = append(inv.Containers, c.Names)
	}
	ns, err := d.Networks(ctx, f)
	if err != nil {
		return inv, err
	}
	for _, n := range ns {
		inv.Networks = append(inv.Networks, n.Name)
	}
	vs, err := d.Volumes(ctx, f)
	if err != nil {
		return inv, err
	}
	for _, v := range vs {
		inv.Volumes = append(inv.Volumes, v.Name)
	}
	return inv, nil
}

// ManagedLeaseIDs returns the distinct lease IDs found on managed
// containers, networks and volumes (for orphan detection).
func (d *Docker) ManagedLeaseIDs(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	format := `{{.Label "` + v1.LabelLease + `"}}`
	for _, args := range [][]string{
		{"ps", "-a", "--filter", v1.ManagedFilter, "--format", format},
		{"network", "ls", "--filter", v1.ManagedFilter, "--format", format},
		{"volume", "ls", "--filter", v1.ManagedFilter, "--format", format},
	} {
		out, err := d.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				seen[l] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// CountUnlabeled counts containers, volumes and networks without our managed
// label. Docker's built-in networks (bridge, host, none) are excluded.
func (d *Docker) CountUnlabeled(ctx context.Context) (v1.UnlabeledCounts, error) {
	var c v1.UnlabeledCounts
	format := `{{.Label "` + v1.LabelManaged + `"}}|{{.Names}}`
	out, err := d.run(ctx, "ps", "-a", "--format", format)
	if err != nil {
		return c, err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && !strings.HasPrefix(l, "true|") && !isKindNode(l) {
			c.Containers++
		}
	}
	// "|{{.Name}}" keeps a line for objects without the label (an empty
	// label alone would render as an empty line and be skipped).
	vfmt := `{{.Label "` + v1.LabelManaged + `"}}`
	out, err = d.run(ctx, "volume", "ls", "--format", vfmt+"|{{.Name}}")
	if err != nil {
		return c, err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && !strings.HasPrefix(l, "true|") {
			c.Volumes++
		}
	}
	out, err = d.run(ctx, "network", "ls", "--format", vfmt+"|{{.Name}}")
	if err != nil {
		return c, err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := l[strings.IndexByte(l, '|')+1:]
		if l == "" || strings.HasPrefix(l, "true|") || name == "bridge" || name == "host" || name == "none" || name == "kind" {
			continue
		}
		c.Networks++
	}
	return c, nil
}

// isKindNode reports whether a "label|names" ps line is a node container of a
// dol-managed kind cluster (kind node containers cannot carry our labels;
// they are tracked on the lease by cluster name instead).
func isKindNode(line string) bool {
	name := line[strings.IndexByte(line, '|')+1:]
	return strings.HasPrefix(name, "dol-l-")
}
