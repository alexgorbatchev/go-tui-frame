package frame

import (
	"context"
	"fmt"
	"os/exec"
)

func (f *Frame[T]) begin(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != configuring {
		return ErrAlreadyRun
	}
	f.state = running
	err := f.configErr
	if err == nil {
		err = validateCommand(f.cmd)
	}
	if err == nil && (f.input == nil || f.output == nil) {
		err = fmt.Errorf("input and output terminal files are required")
	}
	if err == nil {
		if ctx == nil {
			err = fmt.Errorf("session context is required")
		} else {
			err = context.Cause(ctx)
		}
	}
	if err != nil {
		f.state = closed
	}
	return err
}

func validateCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("an executable child command is required")
	}
	if cmd.Err != nil {
		return fmt.Errorf("resolve child command: %w", cmd.Err)
	}
	if cmd.Process != nil || cmd.ProcessState != nil {
		return fmt.Errorf("child command must be unstarted")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return fmt.Errorf("child standard streams are owned by its PTY")
	}
	if cmd.Cancel != nil || cmd.WaitDelay != 0 {
		return fmt.Errorf("child cancellation and output draining are owned by Run")
	}
	if a := cmd.SysProcAttr; a != nil && (a.Noctty || a.Foreground || a.Setpgid || a.Pgid != 0 || a.Ctty != 0 || a.Ptrace) {
		return fmt.Errorf("child process attributes conflict with PTY session ownership")
	}
	return nil
}

func (f *Frame[T]) configurationError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configErr
}
