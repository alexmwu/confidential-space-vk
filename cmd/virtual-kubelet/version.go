package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand creates a new version subcommand command
func NewCommand(version, buildTime string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version of the program",
		Long:  `Show the version of the program`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Version: %s, Built: %s\n", version, buildTime)
		},
	}
}
