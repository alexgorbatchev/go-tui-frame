package main

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

var version = "dev"

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func newRootCommand() (*cobra.Command, error) {
	var showcase bool
	var noTerminalInheritance bool
	cmd := &cobra.Command{
		Use:          "tui-frame -- <command> [args...]",
		Short:        "Wrap a terminal application with a live frame",
		Version:      version,
		SilenceUsage: true,
		// execute reports errors itself so that a child's exit status stays
		// a status rather than a diagnostic.
		SilenceErrors: true,
		Args:          commandArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDemo(cmd.Context(), exec.Command(args[0], args[1:]...), showcase, !noTerminalInheritance)
		},
	}
	cmd.Flags().BoolVar(&showcase, "showcase", false, "Play the frame's layouts, colors, and border changes once")
	cmd.Flags().BoolVar(&noTerminalInheritance, "no-terminal-inheritance", false, "Start the child with independent terminal defaults")
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.SetFlagErrorFunc(explainUsage)
	cmd.AddCommand(newSkillCommand())
	if err := setupHelp(cmd); err != nil {
		return nil, fmt.Errorf("configure command help: %w", err)
	}
	return cmd, nil
}

func commandArgs(cmd *cobra.Command, args []string) error {
	if cmd.ArgsLenAtDash() != 0 {
		return explainUsage(cmd, errors.New("put the child command after --, for example: tui-frame -- nvim"))
	}
	if err := cobra.MinimumNArgs(1)(cmd, args); err != nil {
		return explainUsage(cmd, err)
	}
	return nil
}

func explainUsage(cmd *cobra.Command, err error) error {
	return &exitError{code: 2, err: errors.Join(err, cmd.Usage())}
}

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return explainUsage(cmd, err)
	}
	return nil
}
