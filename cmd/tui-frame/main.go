package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() { os.Exit(execute()) }

func execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, err := newRootCommand()
	if err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := cmd.ExecuteContext(ctx); err != nil {
		var status *exitError
		if errors.As(err, &status) {
			return status.code
		}
		return 1
	}
	return 0
}
