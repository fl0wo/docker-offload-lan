package mcpserver

// M4/M5 tools: tunnels, kind, workspace sync and compose (brief §7.8–7.10).

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/fl0wo/docker-offload-lan/internal/compose"
	dolsync "github.com/fl0wo/docker-offload-lan/internal/sync"
	"github.com/fl0wo/docker-offload-lan/internal/tunnel"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// WorkspaceAPI is the part of the node API used by M4/M5 tools.
type WorkspaceAPI interface {
	KindCreate(ctx context.Context, id string, req v1.KindCreateRequest) (*v1.KindCreateResponse, error)
	KindLoad(ctx context.Context, id, name string, images []string) (*v1.KindLoadResponse, error)
	KindDelete(ctx context.Context, id, name string) error
	RegisterCompose(ctx context.Context, id, project string) error
	RegisterSyncDir(ctx context.Context, id, path string) error
}

func (c *Core) workspaceAPI(l *v1.ClientLease) (WorkspaceAPI, nodeEntryRef, error) {
	n, err := c.node(l.Node)
	if err != nil {
		return nil, nodeEntryRef{}, err
	}
	w, ok := n.api.(WorkspaceAPI)
	if !ok {
		return nil, nodeEntryRef{}, fmt.Errorf("node client for %s does not support kind/compose/sync", l.Node)
	}
	return w, nodeEntryRef{name: n.ref.Name, sshHost: n.ref.SSHHost}, nil
}

type nodeEntryRef struct{ name, sshHost string }

// tunnelForwarder adapts tunnel.Manager to Forwarder.
type tunnelForwarder struct{ m *tunnel.Manager }

func (t tunnelForwarder) CloseLease(_ context.Context, l v1.ClientLease) ([]int, error) {
	return t.m.CloseLease(l.ID), nil
}
func (t tunnelForwarder) CloseAll() { t.m.CloseAll() }

// ---- forwards ----

// ForwardInput is the input of dol_forward.
type ForwardInput struct {
	LeaseID    string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	RemotePort int    `json:"remote_port" jsonschema:"port the service is published on, on the node (e.g. 8080 for -p 127.0.0.1:8080:80)"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"preferred local port; default = remote_port, or a free one if busy"`
	RemoteHost string `json:"remote_host,omitempty" jsonschema:"host as seen from the node; default 127.0.0.1"`
}

// ForwardResult is the output of dol_forward.
type ForwardResult struct {
	v1.PlacementInfo
	LocalPort int    `json:"local_port"`
	URL       string `json:"url"`
}

// Forward opens an SSH tunnel localhost:<local> → node <remote_host>:<remote_port>.
func (c *Core) Forward(ctx context.Context, in ForwardInput) (*ForwardResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	if in.RemotePort <= 0 || in.RemotePort > 65535 || in.LocalPort < 0 || in.LocalPort > 65535 {
		return nil, badInput("remote_port must be 1-65535 and local_port 0-65535")
	}
	res := &ForwardResult{PlacementInfo: c.placementInfo(*l)}
	if l.Placement == v1.PlacementLocal {
		res.LocalPort = in.RemotePort
		res.URL = fmt.Sprintf("http://localhost:%d", in.RemotePort)
		res.Warnings = append(res.Warnings, "the lease is local: published ports are already on localhost; no tunnel needed")
		return res, nil
	}
	n, err := c.node(l.Node)
	if err != nil {
		return nil, err
	}
	f, warn, err := c.tunnels.Open(ctx, tunnel.Spec{
		LeaseID: l.ID, SSHHost: n.ref.SSHHost, LocalPort: cmp.Or(in.LocalPort, in.RemotePort),
		RemoteHost: in.RemoteHost, RemotePort: in.RemotePort, Purpose: "user",
	})
	if err != nil {
		return nil, err
	}
	if warn != "" {
		res.Warnings = append(res.Warnings, warn)
	}
	c.recordForward(ctx, l.ID, f.Forward)
	res.LocalPort = f.LocalPort
	res.URL = fmt.Sprintf("http://localhost:%d", f.LocalPort)
	res.NextSteps = append(res.NextSteps, fmt.Sprintf("Reach the service at localhost:%d (e.g. curl %s). Close it with dol_unforward or dol_release.", f.LocalPort, res.URL))
	return res, nil
}

func (c *Core) recordForward(ctx context.Context, id string, f v1.Forward) {
	_, _ = c.store.UpdateLease(ctx, id, func(x *v1.ClientLease) error {
		x.Forwards = slices.DeleteFunc(x.Forwards, func(o v1.Forward) bool { return o.LocalPort == f.LocalPort })
		x.Forwards = append(x.Forwards, f)
		return nil
	})
}

// UnforwardInput is the input of dol_unforward.
type UnforwardInput struct {
	LeaseID   string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	LocalPort int    `json:"local_port" jsonschema:"local port returned by dol_forward"`
}

// Unforward closes a tunnel.
func (c *Core) Unforward(ctx context.Context, in UnforwardInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	owned := slices.ContainsFunc(c.tunnels.List(l.ID), func(f v1.Forward) bool { return f.LocalPort == in.LocalPort })
	if !owned {
		return nil, v1.Errorf(v1.CodeNotFound, "list forwards with dol_list", "lease %s has no open forward on local port %d in this session", l.ID, in.LocalPort)
	}
	c.tunnels.Close(in.LocalPort)
	_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		x.Forwards = slices.DeleteFunc(x.Forwards, func(o v1.Forward) bool { return o.LocalPort == in.LocalPort })
		return nil
	})
	return &v1.ActionResult{PlacementInfo: c.placementInfo(*l), Done: []string{fmt.Sprintf("closed forward on localhost:%d", in.LocalPort)}}, nil
}

// ---- sync ----

// SyncInput is the input of dol_sync.
type SyncInput struct {
	LeaseID string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	Path    string `json:"path,omitempty" jsonschema:"directory to sync; default the project directory"`
}

// SyncResult is the output of dol_sync.
type SyncResult struct {
	v1.PlacementInfo
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	Millis     int64  `json:"ms"`
}

func (c *Core) resolveDir(p string) (string, error) {
	if p == "" {
		p = c.projectDir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.projectDir, p)
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return "", badInput("path %q is not a directory on this machine", p)
	}
	return p, nil
}

// Sync mirrors a local directory to <remote_root>/<lease>/<name> on the node.
func (c *Core) Sync(ctx context.Context, in SyncInput) (*SyncResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	dir, err := c.resolveDir(in.Path)
	if err != nil {
		return nil, err
	}
	res := &SyncResult{PlacementInfo: c.placementInfo(*l), LocalPath: dir, RemotePath: dir}
	if l.Placement == v1.PlacementLocal {
		res.Warnings = append(res.Warnings, "the lease is local: bind-mount the local path directly; nothing to sync")
		return res, nil
	}
	remote, err := c.syncDir(ctx, l, dir)
	if err != nil {
		return nil, err
	}
	res.RemotePath = remote
	res.NextSteps = append(res.NextSteps,
		fmt.Sprintf("Bind-mount the remote path, e.g. `%s run -v %s:/app …`; call dol_sync again after local edits.", res.Commands.Docker, remote))
	return res, nil
}

func (c *Core) syncDir(ctx context.Context, l *v1.ClientLease, dir string) (string, error) {
	api, ref, err := c.workspaceAPI(l)
	if err != nil {
		return "", err
	}
	remote := dolsync.RemoteDir(c.cfg.Sync.RemoteRoot, l.ID, filepath.Base(dir))
	// Register first so a partial sync is still cleaned up with the lease.
	rctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS)*time.Millisecond)
	err = api.RegisterSyncDir(rctx, l.ID, remote)
	cancel()
	if err != nil {
		return "", c.markIfGone(ctx, l, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := dolsync.Run(sctx, dolsync.Options{SSH: c.ssh, SSHHost: ref.sshHost, LocalDir: dir, RemoteDir: remote,
		RespectGitignore: c.cfg.Sync.RespectGitignore, Rsync: c.rsync}); err != nil {
		return "", err
	}
	_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		if !slices.Contains(x.Resources.SyncDirs, remote) {
			x.Resources.SyncDirs = append(x.Resources.SyncDirs, remote)
		}
		return nil
	})
	return remote, nil
}

// ---- compose ----

// ComposeUpInput is the input of dol_compose_up.
type ComposeUpInput struct {
	LeaseID             string   `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	Files               []string `json:"files,omitempty" jsonschema:"compose files, relative to the project directory; default compose.yaml / docker-compose.yml"`
	ProjectName         string   `json:"project_name,omitempty" jsonschema:"compose project name; default the directory name"`
	Sync                *bool    `json:"sync,omitempty" jsonschema:"sync the project directory to the node first (default true)"`
	Detach              *bool    `json:"detach,omitempty" jsonschema:"run in the background (default true)"`
	ExtraArgs           []string `json:"extra_args,omitempty" jsonschema:"extra arguments for 'compose up', e.g. [\"--build\", \"--wait\", \"web\"]"`
	AllowExternalMounts bool     `json:"allow_external_mounts,omitempty" jsonschema:"keep bind mounts outside the project dir as node paths instead of failing"`
	ProjectDir          string   `json:"project_dir,omitempty" jsonschema:"directory containing the compose files; default the project directory"`
}

// ComposeUpResult is the output of dol_compose_up.
type ComposeUpResult struct {
	v1.PlacementInfo
	Project      string         `json:"project"`
	RenderedFile string         `json:"rendered_file"`
	RemoteDir    string         `json:"remote_dir,omitempty"`
	Ports        []compose.Port `json:"ports"`
	Output       string         `json:"output"`
}

var forbiddenComposeArgs = []string{"-f", "--file", "-p", "--project-name", "--project-directory", "--context", "-H", "--host", "--env-file"}

// ComposeUp renders, rewrites, labels, syncs and starts a compose project.
func (c *Core) ComposeUp(ctx context.Context, in ComposeUpInput) (*ComposeUpResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	dir, err := c.resolveDir(in.ProjectDir)
	if err != nil {
		return nil, err
	}
	for _, a := range in.ExtraArgs {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(forbiddenComposeArgs, name) {
			return nil, badInput("extra_args may not contain %s: dol sets files, project and context itself", name)
		}
	}
	files := in.Files
	if len(files) == 0 {
		for _, f := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				files = []string{f}
				break
			}
		}
		if len(files) == 0 {
			return nil, badInput("no compose file found in %s; pass files", dir)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	doc, err := compose.Render(rctx, c.runner, dir, in.ProjectName, files)
	cancel()
	if err != nil {
		return nil, err
	}
	project := doc.Name()
	// `compose up` over an existing project of someone else would recreate
	// its containers with this lease's labels, and the lease's teardown
	// would then delete them and their volumes.
	if err := c.docker(l.DockerContext).RequireComposeOwned(ctx, project, l.ID); err != nil {
		return nil, err
	}
	res := &ComposeUpResult{PlacementInfo: c.placementInfo(*l), Project: project}
	if l.Placement == v1.PlacementRemote {
		remote := dolsync.RemoteDir(c.cfg.Sync.RemoteRoot, l.ID, filepath.Base(dir))
		if in.Sync == nil || *in.Sync {
			if remote, err = c.syncDir(ctx, l, dir); err != nil {
				return nil, err
			}
		}
		res.RemoteDir = remote
		w, err := doc.Rewrite(dir, remote, in.AllowExternalMounts)
		if err != nil {
			return nil, err
		}
		res.Warnings = append(res.Warnings, w...)
	}
	doc.InjectLabels(l.Labels)
	ports, pw := doc.PublishedPorts()
	res.Ports = ports
	if l.Placement == v1.PlacementRemote {
		res.Warnings = append(res.Warnings, pw...)
	}
	b, err := doc.Marshal()
	if err != nil {
		return nil, err
	}
	rendered := c.store.Path("compose", l.ID+"-"+dolsync.SanitizeProject(project)+".json")
	if err := writeFileAtomic(rendered, b, 0o600); err != nil {
		return nil, fmt.Errorf("writing rendered compose file: %w", err)
	}
	res.RenderedFile = rendered

	// Register before `up` so a partial start is still torn down with the lease.
	if l.Placement == v1.PlacementRemote {
		api, _, err := c.workspaceAPI(l)
		if err != nil {
			return nil, err
		}
		gctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Policy.LeaseRequestTimeoutMS)*time.Millisecond)
		err = api.RegisterCompose(gctx, l.ID, project)
		cancel()
		if err != nil {
			return nil, c.markIfGone(ctx, l, err)
		}
	}
	_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		if !slices.Contains(x.Resources.ComposeProjects, project) {
			x.Resources.ComposeProjects = append(x.Resources.ComposeProjects, project)
		}
		return nil
	})

	args := []string{"compose", "-p", project, "-f", rendered, "up"}
	if in.Detach == nil || *in.Detach {
		args = append(args, "-d")
	}
	args = append(args, in.ExtraArgs...)
	uctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	out, err := c.docker(l.DockerContext).Run(uctx, nil, args...)
	cancel()
	res.Output = tail(string(out), 2000)
	if err != nil {
		return nil, v1.Errorf(v1.CodeInternal,
			"fix the compose project and call dol_compose_up again; dol_compose_down cleans up the partial start",
			"compose up of %s on %s failed: %v", project, l.DockerContext, err)
	}
	for _, p := range ports {
		if l.Placement == v1.PlacementRemote {
			res.NextSteps = append(res.NextSteps, fmt.Sprintf("Service %s listens on node port %d: call dol_forward with remote_port %d to reach it at localhost.", p.Service, p.Published, p.Published))
		} else {
			res.NextSteps = append(res.NextSteps, fmt.Sprintf("Service %s is at localhost:%d.", p.Service, p.Published))
		}
	}
	res.NextSteps = append(res.NextSteps, fmt.Sprintf("Inspect with `%s -p %s ps` / `logs`; stop with dol_compose_down.", res.Commands.Compose, project))
	return res, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ComposeDownInput is the input of dol_compose_down.
type ComposeDownInput struct {
	LeaseID     string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	ProjectName string `json:"project_name" jsonschema:"compose project started with dol_compose_up"`
	Volumes     bool   `json:"volumes,omitempty" jsonschema:"also remove the project's volumes"`
}

// ComposeDown brings down a compose project registered on the lease.
func (c *Core) ComposeDown(ctx context.Context, in ComposeDownInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	if !c.hasCompose(l, in.ProjectName) {
		return nil, v1.Errorf(v1.CodeNotOwned, "only projects started with dol_compose_up on this lease can be brought down; see dol_list",
			"compose project %q is not registered on lease %s", in.ProjectName, l.ID)
	}
	return c.Remove(ctx, RemoveInput{TargetInput: TargetInput{LeaseID: l.ID, Target: in.ProjectName}, Volumes: in.Volumes})
}

// ---- kind ----

// KindCreateInput is the input of dol_kind_create.
type KindCreateInput struct {
	LeaseID    string `json:"lease_id" jsonschema:"lease id from dol_acquire (workload kind)"`
	Name       string `json:"name" jsonschema:"short cluster name, e.g. e2e (lowercase, digits, -; max 16)"`
	ConfigYAML string `json:"config_yaml,omitempty" jsonschema:"optional kind Cluster config (nodes, extraPortMappings…); networking is set by dol"`
	AccessMode string `json:"access_mode,omitempty" jsonschema:"tunnel (default, API server via ssh tunnel) or lan"`
}

// KindCreateResult is the output of dol_kind_create.
type KindCreateResult struct {
	v1.PlacementInfo
	Cluster      string           `json:"cluster"`
	Kubeconfig   string           `json:"kubeconfig"`
	APIServer    string           `json:"api_server"`
	AccessMode   string           `json:"access_mode"`
	PortMappings []v1.PortMapping `json:"port_mappings"`
}

func (c *Core) kubeconfigPath(leaseID, name string) string {
	return c.store.Path("kube", leaseID+"-"+name+".yaml")
}

// KindCreate creates a kind cluster on the lease's host and writes a
// kubeconfig usable from this machine.
func (c *Core) KindCreate(ctx context.Context, in KindCreateInput) (*KindCreateResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	if err := v1.ValidateKindName(in.Name); err != nil {
		return nil, err
	}
	if in.AccessMode != "" && in.AccessMode != v1.AccessTunnel && in.AccessMode != v1.AccessLAN {
		return nil, badInput("access_mode %q is invalid: use tunnel or lan", in.AccessMode)
	}
	if slices.ContainsFunc(l.Resources.KindClusters, func(k v1.KindCluster) bool { return k.Name == in.Name }) {
		return nil, v1.Errorf(v1.CodeConflict, "use the existing cluster (see dol_list) or pick another name", "lease %s already has a kind cluster %q", l.ID, in.Name)
	}
	path := c.kubeconfigPath(l.ID, in.Name)
	res := &KindCreateResult{PlacementInfo: c.placementInfo(*l), Kubeconfig: path, PortMappings: []v1.PortMapping{}}
	var kc v1.KindCluster
	if l.Placement == v1.PlacementLocal {
		kc, err = c.localKindCreate(ctx, l, in, path)
		if err != nil {
			return nil, err
		}
		res.AccessMode = "local"
		res.APIServer = "(local)"
	} else {
		api, ref, err := c.workspaceAPI(l)
		if err != nil {
			return nil, err
		}
		kctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		resp, err := api.KindCreate(kctx, l.ID, v1.KindCreateRequest{Name: in.Name, ConfigYAML: in.ConfigYAML, AccessMode: in.AccessMode})
		cancel()
		if err != nil {
			return nil, c.markIfGone(ctx, l, err)
		}
		kc = v1.KindCluster{Name: in.Name, ClusterName: resp.Cluster, APIServerPort: resp.APIServer.Port, AccessMode: resp.AccessMode}
		server := fmt.Sprintf("https://%s:%d", resp.APIServer.Host, resp.APIServer.Port)
		if resp.AccessMode != v1.AccessLAN {
			f, warn, err := c.tunnels.Open(ctx, tunnel.Spec{LeaseID: l.ID, SSHHost: ref.sshHost, LocalPort: resp.APIServer.Port,
				RemoteHost: "127.0.0.1", RemotePort: resp.APIServer.Port, Purpose: "kind-api:" + resp.Cluster})
			if err != nil {
				res.Warnings = append(res.Warnings, "API server tunnel failed: "+err.Error()+"; call dol_forward with remote_port "+strconv.Itoa(resp.APIServer.Port))
			} else {
				if warn != "" {
					res.Warnings = append(res.Warnings, warn)
				}
				c.recordForward(ctx, l.ID, f.Forward)
				server = fmt.Sprintf("https://127.0.0.1:%d", f.LocalPort)
			}
		}
		b, err := RewriteKubeconfigServer([]byte(resp.Kubeconfig), server)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(path, b, 0o600); err != nil {
			return nil, err
		}
		res.AccessMode = resp.AccessMode
		res.APIServer = server
		res.PortMappings = append(res.PortMappings, resp.PortMappings...)
	}
	res.Cluster = kc.ClusterName
	_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		x.Resources.KindClusters = append(x.Resources.KindClusters, kc)
		if x.Kubeconfigs == nil {
			x.Kubeconfigs = map[string]string{}
		}
		x.Kubeconfigs[in.Name] = path
		return nil
	})
	res.Commands.Kubectl = "kubectl --kubeconfig " + v1.ShellQuote(path)
	res.NextSteps = append(res.NextSteps,
		fmt.Sprintf("Run kubectl as `%s …` in every command (e.g. `%s get nodes`).", res.Commands.Kubectl, res.Commands.Kubectl),
		fmt.Sprintf("Build images with `%s build -t <img> .`, then dol_kind_load them into cluster %q.", res.Commands.Docker, in.Name))
	for _, pm := range res.PortMappings {
		if l.Placement == v1.PlacementRemote {
			res.NextSteps = append(res.NextSteps, fmt.Sprintf("Node port %d maps into the cluster (container port %d): dol_forward remote_port %d to reach it.", pm.HostPort, pm.ContainerPort, pm.HostPort))
		}
	}
	return res, nil
}

// RewriteKubeconfigServer sets clusters[*].cluster.server, preserving the
// rest of the document.
func RewriteKubeconfigServer(kubeconfig []byte, server string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(kubeconfig, &root); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig from kind: %w", err)
	}
	n := 0
	if len(root.Content) == 1 {
		if clusters := mapGet(root.Content[0], "clusters"); clusters != nil {
			for _, item := range clusters.Content {
				if cl := mapGet(item, "cluster"); cl != nil {
					if s := mapGet(cl, "server"); s != nil {
						s.Value = server
						n++
					}
				}
			}
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("kubeconfig from kind has no clusters[].cluster.server")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func (c *Core) kindCmd(ctx context.Context, dockerContext string, args ...string) ([]byte, error) {
	bin := cmp.Or(c.kindBin, "kind")
	if _, err := exec.LookPath(bin); err != nil {
		return nil, v1.Errorf(v1.CodeUnavailable, "install kind on this Mac (`brew install kind`) or free the remote node and acquire again",
			"the lease is local but the kind binary %q was not found", bin)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "DOCKER_CONTEXT="+dockerContext)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return out.Bytes(), v1.Errorf(v1.CodeKindFailed, "see the output above; `kind` logs more with -v 1",
			"kind %s failed: %v: %s", args[0], err, tail(out.String(), 1500))
	}
	return out.Bytes(), nil
}

func (c *Core) localKindCreate(ctx context.Context, l *v1.ClientLease, in KindCreateInput, kubeconfig string) (v1.KindCluster, error) {
	name := v1.KindClusterName(l.ID, in.Name)
	args := []string{"create", "cluster", "--name", name, "--kubeconfig", kubeconfig, "--wait", "180s"}
	if in.ConfigYAML != "" {
		f := c.store.Path("kube", l.ID+"-"+in.Name+".kind.yaml")
		if err := writeFileAtomic(f, []byte(in.ConfigYAML), 0o600); err != nil {
			return v1.KindCluster{}, err
		}
		args = append(args, "--config", f)
	}
	if err := os.MkdirAll(filepath.Dir(kubeconfig), 0o700); err != nil {
		return v1.KindCluster{}, err
	}
	kctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	if _, err := c.kindCmd(kctx, l.DockerContext, args...); err != nil {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		_, _ = c.kindCmd(dctx, l.DockerContext, "delete", "cluster", "--name", name)
		dcancel()
		return v1.KindCluster{}, err
	}
	return v1.KindCluster{Name: in.Name, ClusterName: name, AccessMode: "local"}, nil
}

// KindLoadInput is the input of dol_kind_load.
type KindLoadInput struct {
	LeaseID string   `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	Cluster string   `json:"cluster" jsonschema:"short cluster name given to dol_kind_create"`
	Images  []string `json:"images" jsonschema:"image tags already built with the lease's docker prefix"`
}

func findKind(l *v1.ClientLease, name string) (v1.KindCluster, bool) {
	for _, k := range l.Resources.KindClusters {
		if k.Name == name || k.ClusterName == name {
			return k, true
		}
	}
	return v1.KindCluster{}, false
}

// KindLoad loads images (present on the lease's host) into a kind cluster.
func (c *Core) KindLoad(ctx context.Context, in KindLoadInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	k, ok := findKind(l, in.Cluster)
	if !ok {
		return nil, v1.Errorf(v1.CodeNotFound, "create it with dol_kind_create first", "lease %s has no kind cluster %q", l.ID, in.Cluster)
	}
	if len(in.Images) == 0 {
		return nil, badInput("images is empty")
	}
	for _, img := range in.Images {
		if img == "" || strings.HasPrefix(img, "-") || strings.ContainsAny(img, " \t\n") {
			return nil, badInput("invalid image reference %q", img)
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if l.Placement == v1.PlacementLocal {
		args := append([]string{"load", "docker-image", "--name", k.ClusterName}, in.Images...)
		if _, err := c.kindCmd(lctx, l.DockerContext, args...); err != nil {
			return nil, err
		}
	} else {
		api, _, err := c.workspaceAPI(l)
		if err != nil {
			return nil, err
		}
		if _, err := api.KindLoad(lctx, l.ID, k.Name, in.Images); err != nil {
			return nil, c.markIfGone(ctx, l, err)
		}
	}
	return &v1.ActionResult{PlacementInfo: c.placementInfo(*l),
		Done: []string{fmt.Sprintf("loaded %s into kind cluster %s", strings.Join(in.Images, ", "), k.Name)}}, nil
}

// KindDeleteInput is the input of dol_kind_delete.
type KindDeleteInput struct {
	LeaseID string `json:"lease_id" jsonschema:"lease id from dol_acquire"`
	Cluster string `json:"cluster" jsonschema:"short cluster name given to dol_kind_create"`
}

// KindDelete deletes a kind cluster, its tunnel and kubeconfig.
func (c *Core) KindDelete(ctx context.Context, in KindDeleteInput) (*v1.ActionResult, error) {
	l, err := c.lease(in.LeaseID)
	if err != nil {
		return nil, err
	}
	k, ok := findKind(l, in.Cluster)
	if !ok {
		return nil, v1.Errorf(v1.CodeNotFound, "see dol_list", "lease %s has no kind cluster %q", l.ID, in.Cluster)
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if l.Placement == v1.PlacementLocal {
		if _, err := c.kindCmd(dctx, l.DockerContext, "delete", "cluster", "--name", k.ClusterName); err != nil {
			return nil, err
		}
	} else {
		api, _, err := c.workspaceAPI(l)
		if err != nil {
			return nil, err
		}
		if err := api.KindDelete(dctx, l.ID, k.Name); err != nil && v1.CodeOf(err) != v1.CodeNotFound {
			return nil, c.markIfGone(ctx, l, err)
		}
	}
	for _, f := range c.tunnels.List(l.ID) {
		if f.Purpose == "kind-api:"+k.ClusterName {
			c.tunnels.Close(f.LocalPort)
		}
	}
	path := c.kubeconfigPath(l.ID, k.Name)
	if c.store.Contains(path) {
		_ = os.Remove(path)
	}
	_, _ = c.store.UpdateLease(ctx, l.ID, func(x *v1.ClientLease) error {
		x.Resources.KindClusters = slices.DeleteFunc(x.Resources.KindClusters, func(o v1.KindCluster) bool { return o.Name == k.Name })
		delete(x.Kubeconfigs, k.Name)
		x.Forwards = slices.DeleteFunc(x.Forwards, func(o v1.Forward) bool { return o.Purpose == "kind-api:"+k.ClusterName })
		return nil
	})
	return &v1.ActionResult{PlacementInfo: c.placementInfo(*l), Done: []string{"deleted kind cluster " + k.Name}}, nil
}
