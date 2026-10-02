package frame

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

func (c *console) startChild(cmd *exec.Cmd, size *pty.Winsize) (master *os.File, err error) {
	if !c.inherit {
		return pty.StartWithSize(cmd, size)
	}
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer func() {
		// The child owns its duplicated slave after Start; closing the parent's
		// descriptor is best-effort, as in pty.StartWithAttrs.
		_ = slave.Close()
		if err != nil {
			err = errors.Join(err, master.Close())
			master = nil
		}
	}()
	if err := pty.Setsize(master, size); err != nil {
		return master, fmt.Errorf("set child PTY size: %w", err)
	}
	if err := term.SetState(slave.Fd(), c.raw); err != nil {
		return master, fmt.Errorf("inherit child PTY settings: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty = true, true
	if err := cmd.Start(); err != nil {
		return master, err
	}
	return master, nil
}
