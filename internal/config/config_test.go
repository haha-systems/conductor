package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "conductor-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTemp(t, `
[conductor]
default_provider = "claude"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Conductor.MaxConcurrentRuns != 4 {
		t.Errorf("expected default max_concurrent_runs=4, got %d", cfg.Conductor.MaxConcurrentRuns)
	}
	if cfg.Conductor.WorkIntervalSeconds != 30 {
		t.Errorf("expected default work_interval_seconds=30, got %d", cfg.Conductor.WorkIntervalSeconds)
	}
	if cfg.Sandbox.TimeoutMinutes != 45 {
		t.Errorf("expected default timeout_minutes=45, got %d", cfg.Sandbox.TimeoutMinutes)
	}
	if cfg.Sandbox.WorktreeDir != ".conductor/runs" {
		t.Errorf("unexpected worktree_dir: %s", cfg.Sandbox.WorktreeDir)
	}
}

func TestLoad_FullConfig(t *testing.T) {
	path := writeTemp(t, `
[conductor]
max_concurrent_runs = 2
default_provider = "codex"
work_interval_seconds = 60

[work_sources.github]
repo = "org/repo"
label_filter = ["conductor"]

[providers.claude]
enabled = true
binary = "claude"
extra_args = ["--model", "claude-sonnet-4-6"]
cost_per_1k_tokens = 0.003

[routing]
strategy = "round-robin"

[routing.label_routes]
"big-context" = "gemini"

[proof]
require_ci_pass = true
pr_base_branch = "main"

[sandbox]
worktree_dir = ".conductor/runs"
timeout_minutes = 30
preserve_on_failure = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Conductor.MaxConcurrentRuns != 2 {
		t.Errorf("got max_concurrent_runs=%d", cfg.Conductor.MaxConcurrentRuns)
	}
	if cfg.WorkSources.GitHub == nil {
		t.Fatal("expected github work source")
	}
	if cfg.WorkSources.GitHub.Repo != "org/repo" {
		t.Errorf("unexpected repo: %s", cfg.WorkSources.GitHub.Repo)
	}
	p, ok := cfg.Providers["claude"]
	if !ok {
		t.Fatal("expected claude provider")
	}
	if !p.Enabled {
		t.Error("expected claude to be enabled")
	}
	if cfg.Routing.LabelRoutes["big-context"] != "gemini" {
		t.Errorf("unexpected label route: %v", cfg.Routing.LabelRoutes)
	}
	if cfg.Proof.PRBaseBranch != "main" {
		t.Errorf("unexpected pr_base_branch: %s", cfg.Proof.PRBaseBranch)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name: "zero max_concurrent_runs",
			content: `
[conductor]
default_provider = "claude"
max_concurrent_runs = 0
`,
		},
		{
			name: "empty default_provider",
			content: `
[conductor]
max_concurrent_runs = 4
default_provider = ""
`,
		},
		{
			name: "enabled provider without binary",
			content: `
[conductor]
default_provider = "claude"

[providers.claude]
enabled = true
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Error("expected validation error")
			}
		})
	}
}
