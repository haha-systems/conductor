//go:build !windows

package provider

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func cancelProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("cancel SIGTERM: %w", err)
	}
	time.AfterFunc(killGrace, func() {
		syscall.Kill(pgid, syscall.SIGKILL) //nolint:errcheck
	})
	return nil
}
