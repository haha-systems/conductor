package provider

import (
	"context"
	"fmt"
	"os"
)

// GeminiCLIAdapter invokes Google's `gemini` CLI.
type GeminiCLIAdapter struct{ shell shellAdapter }

func NewGeminiCLIAdapter(binary string, extraArgs []string, costPer1kTokens float64) *GeminiCLIAdapter {
	if binary == "" {
		binary = "gemini"
	}
	// Default to gemini-2.5-pro unless the caller supplies a --model override.
	if !containsFlag(extraArgs, "--model") {
		extraArgs = append([]string{"--model", "gemini-2.5-pro"}, extraArgs...)
	}
	return &GeminiCLIAdapter{shell: shellAdapter{
		name:            "gemini",
		binary:          binary,
		extraArgs:       extraArgs,
		costPer1kTokens: costPer1kTokens,
		capabilities:    []Capability{CapabilityFileEdit, CapabilityBash},
	}}
}

func (a *GeminiCLIAdapter) Name() string                                    { return a.shell.adapterName() }
func (a *GeminiCLIAdapter) Capabilities() []Capability                     { return a.shell.adapterCapabilities() }
func (a *GeminiCLIAdapter) CostEstimate(n int) (float64, bool)             { return a.shell.adapterCostEstimate(n) }
func (a *GeminiCLIAdapter) Run(ctx context.Context, rc RunContext) (RunHandle, error) {
	f, err := os.Open(rc.TaskFile)
	if err != nil {
		return nil, fmt.Errorf("gemini: open task file: %w", err)
	}
	// Pipe the task file as stdin. --prompt "" triggers non-interactive
	// (headless) mode; Gemini appends the --prompt value to stdin content.
	return a.shell.adapterRunWithStdin(ctx, rc, f, "--prompt", "")
}
