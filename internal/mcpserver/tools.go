package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fl0wo/docker-offload-lan/internal/version"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Instructions are sent to the client at initialization; they summarize
// rules/agent.md.
const Instructions = `docker-offload-lan runs your Docker work on a LAN node (or falls back to this laptop) so the laptop's RAM stays free.
1. Call dol_acquire before starting any container, compose stack, kind cluster or heavy build, with a realistic est_memory_gb.
2. Put the returned commands.docker / commands.compose prefix in front of EVERY docker command; environment variables do not persist between commands.
3. Add commands.label_flags to every docker run, network create and volume create; dol only stops, removes and cleans up labeled resources.
4. Build images with the same prefix (build where you run); never docker save | docker load across machines. Publish ports on 127.0.0.1.
5. If placement is "local" you are on the laptop: keep the footprint small. Use dol_resize when the workload grows.
6. When finished call dol_release; use dol_pin only if the user asks to keep things running.`

// Tool descriptions are prompts for LLMs (brief §8): when to use, what to
// pass, what comes back. Keep each under ~80 words (tested).
const (
	descStatus = "Show where Docker work can run: each node's health, architecture and free memory, local Docker status, and this session's leases. " +
		"Use it when unsure whether the remote node is up, before a large dol_acquire, or to recover a lease_id. Takes no input."
	descAcquire = "Call before starting containers, compose stacks, kind clusters or heavy builds. Pass workload and a generous est_memory_gb " +
		"(small container 0.5; compose stack 1-4; kind ≈ 2 + 0.5 per service; JVM services ≥1 each). Returns placement (remote node or local fallback) " +
		"with the reason, lease_id, and commands: use the docker/compose prefix in every command and add label_flags to every docker run/network/volume create."
	descRelease = "Call when done with a lease: tears down every labeled container, network and volume and tracked compose project of the lease " +
		"(remote or local), closes its tunnels and deletes its kubeconfigs. Pass lease_id; keep_sync_dir=true keeps synced files on the node. Returns the removed inventory."
	descList = "List leases and the Docker resources they own (label-scoped, local and remote): containers with state and ports, networks, volumes, " +
		"compose projects. Pass lease_id for one lease, or all_sessions=true to include other sessions of this user. Use it to find targets for dol_stop / dol_remove."
	descStop = "Stop a container (name or id) or a compose project registered on the lease, on the lease's Docker host. Pass lease_id and target. " +
		"Refuses anything that does not carry this lease's labels, so it never touches resources created outside dol."
	descRemove = "Remove a container, network or volume (name or id), or bring down a compose project registered on the lease. Pass lease_id and target; " +
		"volumes=true also deletes its volumes. Refuses anything that does not carry this lease's labels."
	descResize = "Change a lease's memory estimate when the workload grows or shrinks (e.g. adding services). Pass lease_id and the new total est_memory_gb. " +
		"A remote node re-runs admission and may refuse; then keep the current size or release and re-acquire."
	descForward = "Make a service on the lease's node reachable at localhost on this machine via an SSH tunnel. Pass lease_id and remote_port " +
		"(the port published on the node, e.g. 8080 for -p 127.0.0.1:8080:80); optional local_port. Returns the actual local port (another one if busy). " +
		"Not needed for local leases."
	descUnforward = "Close a tunnel opened by dol_forward. Pass lease_id and the local_port it returned. dol_release closes all tunnels of a lease automatically."
	descSync      = "Copy a project directory to the lease's node (rsync, honors .gitignore, skips .git) so it can be bind-mounted there. " +
		"Pass lease_id and optionally path (default: project dir). Returns remote_path to use in -v remote_path:/target. Call again after edits. Not needed for local leases."
	descComposeUp = "Start a docker compose project on the lease's host. Prefer this over raw `compose up`: it syncs the project to the node, " +
		"rewrites bind mounts, adds lease labels to every service, network and volume, then runs up -d. Pass lease_id and optionally files, project_name, extra_args. " +
		"Returns published ports to dol_forward."
	descComposeDown = "Stop and remove a compose project started with dol_compose_up on the lease's host. Pass lease_id and project_name; volumes=true also removes its volumes. " +
		"dol_release does this automatically for every project of the lease."
	descKindCreate = "Create a kind Kubernetes cluster for the lease (remote node, or local fallback). Pass lease_id and a short name like e2e; " +
		"optional config_yaml for nodes/extraPortMappings. Waits until ready (1-3 min) and returns commands.kubectl (`kubectl --kubeconfig …`) " +
		"to use in every kubectl command. The API server is reached through an SSH tunnel automatically."
	descKindLoad = "Load images into a kind cluster of the lease. Build them first with the lease's docker prefix (so they exist where the cluster runs), " +
		"then pass lease_id, cluster (short name) and images (tags). Never docker save/load across machines."
	descKindDelete = "Delete a kind cluster of the lease (and its tunnel and kubeconfig). Pass lease_id and cluster. dol_release also deletes all clusters of a lease."
	descPin        = "Keep a lease alive after this session ends (pinned=true), e.g. when the user asks to leave a stack running; pinned=false makes it session-bound again. " +
		"Pass lease_id, pinned and optionally ttl_minutes as a hard expiry. Use only when the user asks."
)

// Empty is the input of tools without arguments.
type Empty struct{}

// ReleaseInput is the input of dol_release.
type ReleaseInput struct {
	LeaseID     string `json:"lease_id" jsonschema:"lease id from dol_acquire, e.g. l-abcdefghij"`
	KeepSyncDir bool   `json:"keep_sync_dir,omitempty" jsonschema:"keep the synced workspace directory on the node"`
}

// NewServer builds the MCP server with every M3 tool registered on core.
func NewServer(core *Core) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "docker-offload-lan", Title: "docker-offload-lan", Version: version.Version},
		&mcp.ServerOptions{Instructions: Instructions})

	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	mcp.AddTool(s, &mcp.Tool{Name: "dol_status", Description: descStatus, Annotations: ro},
		handler("dol_status", func(ctx context.Context, _ Empty) (*v1.StatusResult, string, error) {
			r := core.Status(ctx, StatusOptions{})
			return r, summarizeStatus(r), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "dol_acquire", Description: descAcquire, InputSchema: acquireSchema(),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}},
		handler("dol_acquire", func(ctx context.Context, in AcquireInput) (*v1.AcquireResult, string, error) {
			r, err := core.Acquire(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAcquire(r), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "dol_release", Description: descRelease,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}},
		handler("dol_release", func(ctx context.Context, in ReleaseInput) (*v1.ReleaseResult, string, error) {
			r, err := core.Release(ctx, in.LeaseID, in.KeepSyncDir)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeRelease(r), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "dol_list", Description: descList, Annotations: ro},
		handler("dol_list", func(ctx context.Context, in ListInput) (*v1.ListResult, string, error) {
			r, err := core.List(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeList(r), nil
		}))

	destructive := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	mcp.AddTool(s, &mcp.Tool{Name: "dol_stop", Description: descStop, Annotations: destructive},
		handler("dol_stop", func(ctx context.Context, in TargetInput) (*v1.ActionResult, string, error) {
			r, err := core.Stop(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_remove", Description: descRemove, Annotations: destructive},
		handler("dol_remove", func(ctx context.Context, in RemoveInput) (*v1.ActionResult, string, error) {
			r, err := core.Remove(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_resize", Description: descResize},
		handler("dol_resize", func(ctx context.Context, in ResizeInput) (*v1.ActionResult, string, error) {
			r, err := core.Resize(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_pin", Description: descPin, Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)}},
		handler("dol_pin", func(ctx context.Context, in PinInput) (*v1.ActionResult, string, error) {
			r, err := core.Pin(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))

	open := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	mcp.AddTool(s, &mcp.Tool{Name: "dol_forward", Description: descForward, Annotations: open},
		handler("dol_forward", func(ctx context.Context, in ForwardInput) (*ForwardResult, string, error) {
			r, err := core.Forward(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, lines(fmt.Sprintf("Forwarding localhost:%d → %s (lease %s).", r.LocalPort, r.Node+" port", r.LeaseID), warnLine(r.Warnings)), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_unforward", Description: descUnforward},
		handler("dol_unforward", func(ctx context.Context, in UnforwardInput) (*v1.ActionResult, string, error) {
			r, err := core.Unforward(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_sync", Description: descSync, Annotations: open},
		handler("dol_sync", func(ctx context.Context, in SyncInput) (*SyncResult, string, error) {
			r, err := core.Sync(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, lines(fmt.Sprintf("Synced %s → %s (lease %s).", r.LocalPath, r.RemotePath, r.LeaseID), warnLine(r.Warnings)), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_compose_up", Description: descComposeUp, Annotations: open},
		handler("dol_compose_up", func(ctx context.Context, in ComposeUpInput) (*ComposeUpResult, string, error) {
			r, err := core.ComposeUp(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, lines(fmt.Sprintf("Compose project %s is up %s (lease %s); %d published port(s).", r.Project, where(r.PlacementInfo), r.LeaseID, len(r.Ports)),
				fmt.Sprintf("Use `%s -p %s …` for ps/logs.", r.Commands.Compose, r.Project), warnLine(r.Warnings)), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_compose_down", Description: descComposeDown, Annotations: destructive},
		handler("dol_compose_down", func(ctx context.Context, in ComposeDownInput) (*v1.ActionResult, string, error) {
			r, err := core.ComposeDown(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_kind_create", Description: descKindCreate, Annotations: open},
		handler("dol_kind_create", func(ctx context.Context, in KindCreateInput) (*KindCreateResult, string, error) {
			r, err := core.KindCreate(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, lines(fmt.Sprintf("kind cluster %s is ready %s (lease %s).", r.Cluster, where(r.PlacementInfo), r.LeaseID),
				fmt.Sprintf("Use `%s …` for every kubectl command.", r.Commands.Kubectl), warnLine(r.Warnings)), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_kind_load", Description: descKindLoad, Annotations: open},
		handler("dol_kind_load", func(ctx context.Context, in KindLoadInput) (*v1.ActionResult, string, error) {
			r, err := core.KindLoad(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "dol_kind_delete", Description: descKindDelete, Annotations: destructive},
		handler("dol_kind_delete", func(ctx context.Context, in KindDeleteInput) (*v1.ActionResult, string, error) {
			r, err := core.KindDelete(ctx, in)
			if err != nil {
				return nil, "", err
			}
			return r, summarizeAction(r), nil
		}))
	return s
}

func where(p v1.PlacementInfo) string {
	if p.Placement == v1.PlacementRemote {
		return "on node " + p.Node
	}
	return "locally"
}

func ptr[T any](v T) *T { return &v }

// acquireSchema adds enums to the inferred input schema so agents see the
// allowed values.
func acquireSchema() *jsonschema.Schema {
	s, err := jsonschema.For[AcquireInput](nil)
	if err != nil {
		panic(err)
	}
	s.Properties["workload"].Enum = []any{"container", "compose", "kind", "build"}
	s.Properties["prefer"].Enum = []any{"remote", "local"}
	s.Properties["est_memory_gb"].ExclusiveMinimum = ptr(0.0)
	s.Properties["ttl_minutes"].Minimum = ptr(0.0)
	return s
}

// ToolError is a tool-level failure: the result has isError=true and its
// text says what happened, why, and what to do next.
type ToolError struct {
	Tool string
	Err  error
}

func (e *ToolError) Error() string {
	if ve, ok := v1.AsError(e.Err); ok {
		s := fmt.Sprintf("%s failed [%s]: %s", e.Tool, ve.Code, ve.Message)
		if ve.Hint != "" {
			s += "\nNext: " + ve.Hint
		}
		return s
	}
	return fmt.Sprintf("%s failed: %v\nNext: run `dol doctor` to diagnose, or call dol_status", e.Tool, e.Err)
}

func (e *ToolError) Unwrap() error { return e.Err }

// handler adapts a Core call to a typed MCP handler that returns structured
// content plus a short human summary.
func handler[In any, Out any](name string, fn func(context.Context, In) (Out, string, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, summary, err := fn(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, &ToolError{Tool: name, Err: err}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, out, nil
	}
}

// ---- summaries (2–4 lines) ----

func lines(ls ...string) string {
	var out []string
	for _, l := range ls {
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func warnLine(ws []string) string {
	if len(ws) == 0 {
		return ""
	}
	if len(ws) == 1 {
		return "Warning: " + ws[0]
	}
	return fmt.Sprintf("Warnings (%d): %s", len(ws), strings.Join(ws, " | "))
}

func proxyLine(r *v1.AcquireResult) string {
	if r.Commands.DockerProxied == "" {
		return ""
	}
	return fmt.Sprintf(" Or use `%s …`: labels and project bind mounts are then handled by the proxy.", r.Commands.DockerProxied)
}

func summarizeAcquire(r *v1.AcquireResult) string {
	where := "LOCAL (this laptop, docker context " + r.DockerContext + ")"
	if r.Placement == v1.PlacementRemote {
		where = "REMOTE on node " + r.Node
	}
	return lines(
		fmt.Sprintf("Placed %s: lease %s, %s.", where, r.LeaseID, v1.FormatBytes(r.EstMemoryBytes)),
		fmt.Sprintf("Use `%s …` for every docker command and add label_flags to every create.", r.Commands.Docker)+proxyLine(r),
		"Reason: "+r.Reason,
		warnLine(r.Warnings),
	)
}

func summarizeStatus(r *v1.StatusResult) string {
	var ns []string
	for _, n := range r.Nodes {
		if !n.Healthy {
			ns = append(ns, n.Name+": DOWN")
			continue
		}
		s := n.Name + ": healthy"
		if n.Health != nil && n.Health.Arch != "" {
			s += " " + n.Health.Arch
		}
		if n.Capacity != nil {
			s += ", " + v1.FormatBytes(n.Capacity.Memory.SchedulableFreeBytes) + " free"
		}
		ns = append(ns, s)
	}
	nodes := "Nodes: none configured (everything runs locally; run `dol init`)."
	if len(ns) > 0 {
		nodes = "Nodes: " + strings.Join(ns, "; ") + "."
	}
	local := fmt.Sprintf("Local Docker (%s): ", r.Local.DockerContext)
	if r.Local.Reachable {
		local += v1.FormatBytes(r.Local.MemTotalBytes) + " total."
	} else {
		local += "unreachable."
	}
	return lines(nodes, local, fmt.Sprintf("This session has %d lease(s); policy %s.", len(r.Leases), r.Policy))
}

func summarizeRelease(r *v1.ReleaseResult) string {
	where := "locally"
	if r.Placement == v1.PlacementRemote {
		where = "on node " + r.Node
	}
	rm := r.Removed
	return lines(
		fmt.Sprintf("Released lease %s %s.", r.LeaseID, where),
		fmt.Sprintf("Removed %d container(s), %d network(s), %d volume(s), %d compose project(s), %d kind cluster(s); closed %d forward(s).",
			len(rm.Containers), len(rm.Networks), len(rm.Volumes), len(rm.ComposeProjects), len(rm.KindClusters), len(r.Forwards)),
		warnLine(r.Warnings),
	)
}

func summarizeList(r *v1.ListResult) string {
	counts := map[string]int{}
	for _, x := range r.Resources {
		counts[x.Kind]++
	}
	return lines(
		fmt.Sprintf("%d lease(s); %d resource(s): %d container(s), %d network(s), %d volume(s), %d compose project(s), %d kind cluster(s).",
			len(r.Leases), len(r.Resources), counts["container"], counts["network"], counts["volume"], counts["compose_project"], counts["kind_cluster"]),
		warnLine(r.Warnings),
	)
}

func summarizeAction(r *v1.ActionResult) string {
	where := "locally"
	if r.Placement == v1.PlacementRemote {
		where = "on node " + r.Node
	}
	return lines(
		fmt.Sprintf("%s (%s, lease %s).", strings.Join(r.Done, "; "), where, r.LeaseID),
		warnLine(r.Warnings),
	)
}

// Run serves MCP over stdio until the client disconnects (stdin closed) or
// ctx is cancelled (SIGTERM/SIGINT).
func Run(ctx context.Context, core *Core) error {
	if err := core.Start(ctx); err != nil {
		return err
	}
	defer core.Close()
	err := NewServer(core).Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil // signalled: normal shutdown
	}
	return err
}
