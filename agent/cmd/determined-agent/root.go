package main

import (
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	rootCmd = newRootCmd()
	runCmd  = newRunCmd()
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "determined-agent",
		Version: version,
	}

	cmd.AddCommand(newCompletionCmd(), newVersionCmd(), newGPUTopologyCmd(), runCmd)

	return cmd
}
