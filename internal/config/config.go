package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is the top-level conductor.toml structure.
type Config struct {
	Conductor   ConductorConfig             `toml:"conductor"`
	WorkSources WorkSourcesConfig           `toml:"work_sources"`
	Providers   map[string]ProviderConfig   `toml:"providers"`
	Routing     RoutingConfig               `toml:"routing"`
	Proof       ProofConfig                 `toml:"proof"`
	Sandbox     SandboxConfig               `toml:"sandbox"`
	Hooks       []string                    `toml:"hooks"`
}

type ConductorConfig struct {
	MaxConcurrentRuns   int    `toml:"max_concurrent_runs"`
	DefaultProvider     string `toml:"default_provider"`
	WorkIntervalSeconds int    `toml:"work_interval_seconds"`
}

type WorkSourcesConfig struct {
	GitHub *GitHubSourceConfig `toml:"github"`
	Linear *LinearSourceConfig `toml:"linear"`
}

type GitHubSourceConfig struct {
	Repo        string   `toml:"repo"`
	LabelFilter []string `toml:"label_filter"`
}

type LinearSourceConfig struct {
	TeamID      string   `toml:"team_id"`
	StateFilter []string `toml:"state_filter"`
}

type ProviderConfig struct {
	Enabled         bool     `toml:"enabled"`
	Binary          string   `toml:"binary"`
	ExtraArgs       []string `toml:"extra_args"`
	CostPer1kTokens float64  `toml:"cost_per_1k_tokens"`
}

type RoutingConfig struct {
	Strategy    string            `toml:"strategy"`
	LabelRoutes map[string]string `toml:"label_routes"`
}

type ProofConfig struct {
	RequireCIPass bool   `toml:"require_ci_pass"`
	PRBaseBranch  string `toml:"pr_base_branch"`
}

type SandboxConfig struct {
	WorktreeDir       string `toml:"worktree_dir"`
	TimeoutMinutes    int    `toml:"timeout_minutes"`
	PreserveOnFailure bool   `toml:"preserve_on_failure"`
	WorkflowFile      string `toml:"workflow_file"`
}

// Load reads and parses a conductor.toml file, applying defaults.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Conductor: ConductorConfig{
			MaxConcurrentRuns:   4,
			DefaultProvider:     "claude",
			WorkIntervalSeconds: 30,
		},
		Routing: RoutingConfig{
			Strategy:    "round-robin",
			LabelRoutes: map[string]string{},
		},
		Proof: ProofConfig{
			PRBaseBranch: "main",
		},
		Sandbox: SandboxConfig{
			WorktreeDir:       ".conductor/runs",
			TimeoutMinutes:    45,
			PreserveOnFailure: true,
			WorkflowFile:      ".conductor/WORKFLOW.md",
		},
		Providers: map[string]ProviderConfig{},
	}
}

func validate(cfg *Config) error {
	if cfg.Conductor.MaxConcurrentRuns <= 0 {
		return fmt.Errorf("max_concurrent_runs must be > 0")
	}
	if cfg.Conductor.DefaultProvider == "" {
		return fmt.Errorf("default_provider must be set")
	}
	if cfg.Sandbox.TimeoutMinutes <= 0 {
		return fmt.Errorf("sandbox.timeout_minutes must be > 0")
	}
	for name, p := range cfg.Providers {
		if p.Enabled && p.Binary == "" {
			return fmt.Errorf("provider %q: binary must be set when enabled", name)
		}
	}
	return nil
}
