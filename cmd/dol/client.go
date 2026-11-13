package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/mcpserver"
	"github.com/fl0wo/docker-offload-lan/internal/state"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

func clientCommands() []*cobra.Command {
	return []*cobra.Command{
		newMCPCmd(),
		notYet("init", "Interactive setup: add a node, ssh config, docker context, MCP registration", "M1"),
		notYet("doctor", "Diagnose every link between this machine and its nodes", "M1"),
		newStatusCmd(),
		newLeasesCmd(),
		newReleaseCmd(),
	}
}

// newCore loads the client config and state dir. Logs go to stderr: in
// `dol mcp`, stdout is the MCP channel.
func newCore(ctx context.Context, level slog.Level) (*mcpserver.Core, error) {
	cfg, err := config.LoadClient(config.ClientConfigPath())
	if err != nil {
		return nil, err
	}
	st, err := state.Open(config.StateDir())
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	return mcpserver.NewCore(ctx, mcpserver.Options{Config: cfg, Store: st, Logger: log})
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the stdio MCP server for Claude Code",
		Long: "Run the stdio MCP server for Claude Code. Register it with:\n\n" +
			"  claude mcp add -s user docker-offload-lan -- dol mcp\n\n" +
			"On exit (stdin closed, SIGTERM) heartbeats stop; remote leases are not released and\n" +
			"are reaped by the node after its heartbeat grace period unless pinned.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if os.Getenv("DOL_DEBUG") != "" {
				level = slog.LevelDebug
			}
			core, err := newCore(cmd.Context(), level)
			if err != nil {
				return err
			}
			return mcpserver.Run(cmd.Context(), core)
		},
	}
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show nodes, capacity and leases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			core, err := newCore(cmd.Context(), slog.LevelWarn)
			if err != nil {
				return err
			}
			defer core.Close()
			st := core.Status(cmd.Context(), mcpserver.StatusOptions{AllSessions: true})
			if asJSON {
				return printJSON(cmd.OutOrStdout(), st)
			}
			printStatus(cmd.OutOrStdout(), st)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON (same shape as the dol_status MCP tool)")
	return c
}

func newLeasesCmd() *cobra.Command {
	var all, asJSON bool
	c := &cobra.Command{
		Use:   "leases",
		Short: "List leases recorded on this machine",
		Long:  "List leases recorded on this machine. By default only active leases are shown;\n--all also shows lost, failed and releasing ones.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Open(config.StateDir())
			if err != nil {
				return err
			}
			ls, errs := st.ListLeases()
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", e)
			}
			out := []v1.ClientLease{}
			for _, l := range ls {
				if all || l.State == v1.StateActive {
					out = append(out, l)
				}
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), out)
			}
			printLeases(cmd.OutOrStdout(), st, out)
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include lost, failed and releasing leases")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func newReleaseCmd() *cobra.Command {
	var keepSync bool
	c := &cobra.Command{
		Use:   "release <lease-id>",
		Short: "Release a lease and tear down its resources",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			core, err := newCore(cmd.Context(), slog.LevelWarn)
			if err != nil {
				return err
			}
			defer core.Close()
			r, err := core.Release(cmd.Context(), args[0], keepSync)
			if err != nil {
				return (&mcpserver.ToolError{Tool: "release", Err: err})
			}
			w := cmd.OutOrStdout()
			where := "locally"
			if r.Placement == v1.PlacementRemote {
				where = "on node " + r.Node
			}
			fmt.Fprintf(w, "Released %s %s.\n", r.LeaseID, where)
			rm := r.Removed
			fmt.Fprintf(w, "Removed: %d container(s), %d network(s), %d volume(s), %d compose project(s), %d kind cluster(s), %d sync dir(s).\n",
				len(rm.Containers), len(rm.Networks), len(rm.Volumes), len(rm.ComposeProjects), len(rm.KindClusters), len(rm.SyncDirs))
			for _, x := range r.Warnings {
				fmt.Fprintln(w, "warning:", x)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keepSync, "keep-sync-dir", false, "keep the synced workspace on the node")
	return c
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printStatus(w io.Writer, st *v1.StatusResult) {
	fmt.Fprintf(w, "Owner %s · policy %s\n\n", st.Owner, st.Policy)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tHEALTH\tARCH\tFREE MEM\tLEASES\tLATENCY\tDETAIL")
	if len(st.Nodes) == 0 {
		fmt.Fprintln(tw, "(none)\t-\t-\t-\t-\t-\tno nodes configured: everything runs locally (run `dol init`)")
	}
	for _, n := range st.Nodes {
		health, arch, free, leases, detail := "DOWN", "-", "-", "-", n.Error
		if n.Healthy {
			health = "ok"
			if n.Health != nil {
				arch = n.Health.Arch
				if !n.Health.Accepting {
					health = "draining"
				}
			}
			if n.Capacity != nil {
				free = v1.FormatBytes(n.Capacity.Memory.SchedulableFreeBytes)
				leases = fmt.Sprint(n.Capacity.LeasesActive)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%dms\t%s\n", n.Name, health, arch, free, leases, n.Millis, detail)
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	if st.Local.Reachable {
		fmt.Fprintf(w, "Local Docker: context %s, %s total, %s\n", st.Local.DockerContext, v1.FormatBytes(st.Local.MemTotalBytes), st.Local.Arch)
	} else {
		fmt.Fprintf(w, "Local Docker: context %s UNREACHABLE: %s\n", st.Local.DockerContext, st.Local.Error)
	}
	fmt.Fprintf(w, "\n%d lease(s)\n", len(st.Leases))
	if len(st.Leases) > 0 {
		printLeases(w, nil, st.Leases)
	}
}

func printLeases(w io.Writer, st *state.Store, ls []v1.ClientLease) {
	if len(ls) == 0 {
		fmt.Fprintln(w, "No leases.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LEASE\tWHERE\tSTATE\tMEM\tWORKLOAD\tPROJECT\tSESSION\tPINNED\tCREATED")
	for _, l := range ls {
		where := "local:" + l.DockerContext
		if l.Placement == v1.PlacementRemote {
			where = "remote:" + l.Node
		}
		sess := l.Session
		if st != nil && !st.SessionAlive(l.Session) {
			sess += " (ended)"
		}
		pinned := ""
		if l.Pinned {
			pinned = "yes"
			if l.ExpiresAt != "" {
				pinned += " until " + l.ExpiresAt
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, where, strings.ToUpper(string(l.State)),
			v1.FormatBytes(l.EstMemoryBytes), l.Workload, l.Project, sess, pinned, l.CreatedAt)
	}
	_ = tw.Flush()
}
