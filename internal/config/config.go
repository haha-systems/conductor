package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the top-level conductor.toml structure.
type Config struct {
	Conductor   ConductorConfig           `toml:"conductor"`
	WorkSources WorkSourcesConfig         `toml:"work_sources"`
	Providers   map[string]ProviderConfig `toml:"providers"`
	Routing     RoutingConfig             `toml:"routing"`
	Proof       ProofConfig               `toml:"proof"`
	Sandbox     SandboxConfig             `toml:"sandbox"`
	Hooks       []string                  `toml:"hooks"`
	// Personas is populated by discoverPersonas; not read from TOML directly.
	Personas map[string]PersonaConfig
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
	Strategy     string            `toml:"strategy"`
	LabelRoutes  map[string]string `toml:"label_routes"`
	PersonaRoutes map[string]string `toml:"persona_routes"`
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

// PersonaConfig describes a named agent persona discovered from .conductor/personas/<name>/.
type PersonaConfig struct {
	Name     string // populated from directory name
	Provider string `toml:"provider"` // from persona.toml, optional
	Dir      string // absolute path to persona directory
}

// personaTOML holds the optional persona.toml fields.
type personaTOML struct {
	Provider string `toml:"provider"`
}

// Load reads and parses a conductor.toml file, applying defaults.
// repoRoot is the directory containing conductor.toml (used to discover personas).
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

	// Discover personas relative to the config file location.
	repoRoot := filepath.Dir(path)
	cfg.Personas = discoverPersonas(repoRoot)

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
			Strategy:      "round-robin",
			LabelRoutes:   map[string]string{},
			PersonaRoutes: map[string]string{},
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
		Personas:  map[string]PersonaConfig{},
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

// discoverPersonas scans <repoRoot>/.conductor/personas/ for subdirectories.
// Returns an empty map if the directory doesn't exist.
func discoverPersonas(repoRoot string) map[string]PersonaConfig {
	personasDir := filepath.Join(repoRoot, ".conductor", "personas")
	entries, err := os.ReadDir(personasDir)
	if err != nil {
		return map[string]PersonaConfig{} // directory absent — not an error
	}

	personas := make(map[string]PersonaConfig)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dir := filepath.Join(personasDir, name)
		p := PersonaConfig{Name: name, Dir: dir}

		// Read optional persona.toml.
		tomlPath := filepath.Join(dir, "persona.toml")
		if data, err := os.ReadFile(tomlPath); err == nil {
			var pt personaTOML
			if _, err := toml.Decode(string(data), &pt); err == nil {
				p.Provider = pt.Provider
			}
		}

		personas[name] = p
	}
	return personas
}
