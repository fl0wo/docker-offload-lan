package main

import "github.com/spf13/cobra"

func clientCommands() []*cobra.Command {
	return []*cobra.Command{
		notYet("mcp", "Run the stdio MCP server for Claude Code", "M3"),
		notYet("init", "Interactive setup: add a node, ssh config, docker context, MCP registration", "M1"),
		notYet("doctor", "Diagnose every link between this machine and its nodes", "M1"),
		notYet("status", "Show nodes, capacity and leases", "M3"),
		notYet("leases", "List leases", "M3"),
		notYet("release <lease-id>", "Release a lease and tear down its resources", "M3"),
	}
}
