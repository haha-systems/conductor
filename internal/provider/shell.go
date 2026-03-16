package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const killGrace = 5 * time.Second

// shellAdapter is the shared implementation for CLI-based providers.
type shellAdapter struct {
	name            string
	binary          string
	extraArgs       []string
	costPer1kTokens float64
	capabilities    []Capability
}

func (a *shellAdapter) adapterName() string            { return a.name }
func (a *shellAdapter) adapterCapabilities() []Capability { return a.capabilities }

func (a *shellAdapter) adapterCostEstimate(promptLen int) (float64, bool) {
	if a.costPer1kTokens <= 0 {
		return 0, false
	}
	tokens := float64(promptLen) / 4.0
	return (tokens / 1000.0) * a.costPer1kTokens, true
}

func (a *shellAdapter) adapterRun(ctx context.Context, rc RunContext, prependArgs ...string) (RunHandle, error) {
	return a.adapterRunWithStdin(ctx, rc, nil, prependArgs...)
}

func (a *shellAdapter) adapterRunWithStdin(ctx context.Context, rc RunContext, stdin *os.File, prependArgs ...string) (RunHandle, error) {
	args := append(prependArgs, a.extraArgs...)

	cmd := exec.CommandContext(ctx, a.binary, args...)
	cmd.Dir = rc.RepoPath
	cmd.Env = buildEnv(rc)
	cmd.Stdin = stdin
	cmd.Stdout = rc.LogWriter
	cmd.Stderr = rc.LogWriter
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		if stdin != nil {
			stdin.Close()
		}
		return nil, fmt.Errorf("%s: start: %w", a.name, err)
	}
	return &processHandle{cmd: cmd, stdin: stdin}, nil
}

// processHandle implements RunHandle for an os/exec.Cmd.
type processHandle struct {
	cmd   *exec.Cmd
	stdin *os.File // closed after Wait if non-nil
}

func (h *processHandle) Wait() error {
	err := h.cmd.Wait()
	if h.stdin != nil {
		h.stdin.Close()
	}
	return err
}

// Cancel sends SIGTERM to the process group, then schedules SIGKILL after a
// grace period. It does NOT call Wait — the caller owns exactly one Wait call.
func (h *processHandle) Cancel() error {
	if h.cmd.Process == nil {
		return nil
	}
	pgid := -h.cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil // already exited
		}
		return fmt.Errorf("cancel SIGTERM: %w", err)
	}
	time.AfterFunc(killGrace, func() {
		syscall.Kill(pgid, syscall.SIGKILL) //nolint:errcheck
	})
	return nil
}

// buildEnv merges RunContext.Env on top of the current process environment.
func buildEnv(rc RunContext) []string {
	env := os.Environ()
	env = append(env, fmt.Sprintf("CONDUCTOR_TASK_FILE=%s", rc.TaskFile))
	for k, v := range rc.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}
