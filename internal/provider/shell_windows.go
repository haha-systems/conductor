//go:build windows

package provider

import (
	"os"
	"os/exec"
)

func setProcAttr(cmd *exec.Cmd) {
	// No process group management on Windows
}

func cancelProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}
