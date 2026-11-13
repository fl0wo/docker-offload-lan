package main

import "github.com/spf13/cobra"

func clientCommands() []*cobra.Command {
	return []*cobra.Command{
		notYet("mcp", "Run the stdio MCP server for Claude Code", "M3"),
		newInitCmd(),
		newDoctorCmd(),
		notYet("status", "Show nodes, capacity and leases", "M3"),
		notYet("leases", "List leases", "M3"),
		notYet("release <lease-id>", "Release a lease and tear down its resources", "M3"),
	}
}
