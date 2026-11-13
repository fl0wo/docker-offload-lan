package main

import "github.com/spf13/cobra"

func newNodeCmd() *cobra.Command {
	c := &cobra.Command{Use: "node", Short: "Commands that run on a node (the machine hosting workloads)"}
	c.AddCommand(
		notYet("serve", "Run the node agent daemon", "M2"),
		notYet("dial-stdio", "Bridge stdin/stdout to the node agent socket (used over ssh)", "M2"),
		notYet("init", "Write node config and validate Docker/kind", "M1"),
	)
	return c
}
