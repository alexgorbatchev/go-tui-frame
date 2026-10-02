package main

import (
	_ "embed"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

//go:embed SKILL.md
var skill string

func newSkillCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print the embedded operating guide",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := io.WriteString(cmd.OutOrStdout(), skill); err != nil {
				return fmt.Errorf("write operating guide: %w", err)
			}
			return nil
		},
	}
}
