package provider

import (
	"context"
	"fmt"
	"os"
)

// ClaudeCodeAdapter invokes the `claude` CLI.
type ClaudeCodeAdapter struct{ shell shellAdapter }

func NewClaudeCodeAdapter(binary string, extraArgs []string, costPer1kTokens float64) *ClaudeCodeAdapter {
	if binary == "" {
		binary = "claude"
	}
	return &ClaudeCodeAdapter{shell: shellAdapter{
		name:            "claude",
		binary:          binary,
		extraArgs:       extraArgs,
		costPer1kTokens: costPer1kTokens,
		capabilities:    []Capability{CapabilityFileEdit, CapabilityBash},
	}}
}

func (a *ClaudeCodeAdapter) Name() string            { return a.shell.adapterName() }
func (a *ClaudeCodeAdapter) Capabilities() []Capability { return a.shell.adapterCapabilities() }
func (a *ClaudeCodeAdapter) CostEstimate(n int) (float64, bool) {
	return a.shell.adapterCostEstimate(n)
}

// Run pipes the task file into claude via stdin so the prompt is delivered
// regardless of task length, avoiding shell-quoting issues with positional args.
func (a *ClaudeCodeAdapter) Run(ctx context.Context, rc RunContext) (RunHandle, error) {
	f, err := os.Open(rc.TaskFile)
	if err != nil {
		return nil, fmt.Errorf("claude: open task file: %w", err)
	}
	args := []string{"--print"}
	if rc.MCPConfigPath != "" {
		args = append(args, "--mcp-config", rc.MCPConfigPath)
	}
	return a.shell.adapterRunWithStdin(ctx, rc, f, args...)
}
