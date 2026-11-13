// Command dol is the single binary of docker-offload-lan: the MCP server and
// CLI on the client, and the node agent on the node.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fl0wo/docker-offload-lan/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "dol:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dol",
		Short:         "Run your agents' Docker workloads on another machine on your LAN",
		Long:          "docker-offload-lan (dol) places Docker, compose and kind workloads on a LAN node over SSH,\nwith admission control, leases and automatic fallback to local Docker.\n\nIndependent community project; not affiliated with Docker, Inc.",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newNodeCmd())
	for _, c := range clientCommands() {
		root.AddCommand(c)
	}
	return root
}
