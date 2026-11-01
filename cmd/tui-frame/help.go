package main

import (
	"fmt"
	"io"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v4"
	"github.com/spf13/cobra"
)

const (
	agentHelpAlert = "ALERT: Agents must read `AGENT=1 tui-frame skill` before using this tool."
	helpWidth      = 80
)

func setupHelp(cmd *cobra.Command) error {
	width := helptree.GetTerminalWidth()
	if width == 0 {
		width = helpWidth
	}
	options := helptree.HelpOptions{
		Tree: helptree.TreeOptions{HideGeneratedCommands: true, TerminalWidth: width},
		Catalog: helptree.TechCatalog{
			"tui-frame": {
				Args: []helptree.ArgSpec{
					{Name: "<command>", Description: "Child program after --"},
					{Name: "[args...]", Description: "Child arguments, passed unchanged"},
				},
				Env: []helptree.EnvSpec{
					{Name: "AGENT", Description: "1, true, or yes selects plain frame chrome and agent help"},
					{Name: "COLUMNS", Description: "Positive width for human help; otherwise terminal width or 80"},
				},
				Quickstart: []helptree.QuickstartItem{
					{Command: "tui-frame -- nvim", Comment: "Ctrl+B 1/2/3: layout/colour/border; Ctrl+Q quit"},
					{Command: "tui-frame -- less README.md", Comment: "Wrap any command with a terminal interface"},
				},
			},
		},
	}
	if err := helptree.SetupWithOptions(cmd, options); err != nil {
		return err
	}
	if helptree.IsAgentMode() {
		cmd.SetErrPrefix("ERR:")
	} else {
		cmd.SetErrPrefix("[ERROR]")
	}
	wrapHelp(cmd, options)
	return nil
}

func wrapHelp(cmd *cobra.Command, options helptree.HelpOptions) {
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		if err := writeHelpAlert(c.OutOrStdout()); err != nil {
			c.PrintErrln(err)
			return
		}
		if _, err := fmt.Fprint(c.OutOrStdout(), helpScreen(c, options, true)); err != nil {
			c.PrintErrln(err)
		}
	})
	cmd.SetUsageFunc(func(c *cobra.Command) error {
		if err := writeHelpAlert(c.ErrOrStderr()); err != nil {
			return err
		}
		_, err := fmt.Fprint(c.ErrOrStderr(), helpScreen(c, options, false))
		if err != nil {
			c.PrintErrln(err)
		}
		return err
	})
}

func helpScreen(cmd *cobra.Command, options helptree.HelpOptions, help bool) string {
	if helptree.IsAgentMode() {
		return helptree.RenderAgentHelp(cmd, options.Catalog)
	}
	if help {
		return helptree.RenderTreeHelp(cmd, options.Catalog, options.Tree)
	}
	return helptree.RenderTreeUsage(cmd, options.Catalog, options.Tree)
}

func writeHelpAlert(out io.Writer) error {
	if !helptree.IsAgentMode() {
		return nil
	}
	if _, err := fmt.Fprintln(out, agentHelpAlert); err != nil {
		return fmt.Errorf("write agent help alert: %w", err)
	}
	return nil
}
