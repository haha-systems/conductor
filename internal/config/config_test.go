package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "ariadne-*.toml")
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
[ariadne]
default_provider = "claude"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ariadne.MaxConcurrentRuns != 4 {
		t.Errorf("expected default max_concurrent_runs=4, got %d", cfg.Ariadne.MaxConcurrentRuns)
	}
	if cfg.Ariadne.WorkIntervalSeconds != 30 {
		t.Errorf("expected default work_interval_seconds=30, got %d", cfg.Ariadne.WorkIntervalSeconds)
	}
	if cfg.Sandbox.TimeoutMinutes != 45 {
		t.Errorf("expected default timeout_minutes=45, got %d", cfg.Sandbox.TimeoutMinutes)
	}
	if cfg.Sandbox.WorktreeDir != ".ariadne/runs" {
		t.Errorf("unexpected worktree_dir: %s", cfg.Sandbox.WorktreeDir)
	}
}

func TestLoad_FullConfig(t *testing.T) {
	path := writeTemp(t, `
[ariadne]
max_concurrent_runs = 2
default_provider = "codex"
work_interval_seconds = 60

[work_sources.github]
repo = "org/repo"
label_filter = ["ariadne"]

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
worktree_dir = ".ariadne/runs"
timeout_minutes = 30
preserve_on_failure = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ariadne.MaxConcurrentRuns != 2 {
		t.Errorf("got max_concurrent_runs=%d", cfg.Ariadne.MaxConcurrentRuns)
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

func TestLoad_PersonaRoutes(t *testing.T) {
	path := writeTemp(t, `
[ariadne]
default_provider = "claude"

[routing.persona_routes]
"feature" = "lead-engineer"
"planning" = "project-manager"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Routing.PersonaRoutes["feature"] != "lead-engineer" {
		t.Errorf("expected feature->lead-engineer, got %v", cfg.Routing.PersonaRoutes)
	}
	if cfg.Routing.PersonaRoutes["planning"] != "project-manager" {
		t.Errorf("expected planning->project-manager, got %v", cfg.Routing.PersonaRoutes)
	}
}

func TestDiscoverPersonas_NoDir(t *testing.T) {
	dir := t.TempDir()
	personas := discoverPersonas(dir)
	if len(personas) != 0 {
		t.Errorf("expected empty map when personas dir absent, got %v", personas)
	}
}

func TestDiscoverPersonas_WithPersonas(t *testing.T) {
	dir := t.TempDir()
	personasDir := filepath.Join(dir, ".ariadne", "personas")

	// Create lead-engineer persona with SOUL.md and CLAUDE.md.
	leDir := filepath.Join(personasDir, "lead-engineer")
	if err := os.MkdirAll(leDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(leDir, "SOUL.md"), []byte("I am a lead engineer."), 0644)   //nolint:errcheck
	os.WriteFile(filepath.Join(leDir, "CLAUDE.md"), []byte("# Claude context"), 0644)      //nolint:errcheck

	// Create project-manager persona with persona.toml.
	pmDir := filepath.Join(personasDir, "project-manager")
	if err := os.MkdirAll(pmDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pmDir, "persona.toml"), []byte(`provider = "gemini"`), 0644) //nolint:errcheck

	personas := discoverPersonas(dir)

	le, ok := personas["lead-engineer"]
	if !ok {
		t.Fatal("expected lead-engineer persona")
	}
	if le.Name != "lead-engineer" {
		t.Errorf("unexpected name: %s", le.Name)
	}
	if le.Provider != "" {
		t.Errorf("expected no provider override, got %s", le.Provider)
	}

	pm, ok := personas["project-manager"]
	if !ok {
		t.Fatal("expected project-manager persona")
	}
	if pm.Provider != "gemini" {
		t.Errorf("expected provider=gemini, got %s", pm.Provider)
	}
}

func TestDiscoverPersonas_IdentityFields(t *testing.T) {
	dir := t.TempDir()
	personasDir := filepath.Join(dir, ".ariadne", "personas")

	// Persona with display name and email.
	withFieldsDir := filepath.Join(personasDir, "lead-engineer")
	if err := os.MkdirAll(withFieldsDir, 0755); err != nil {
		t.Fatal(err)
	}
	tomlContent := `provider = "claude"
name = "Alex"
email = "alex@haha-systems.bot"
`
	os.WriteFile(filepath.Join(withFieldsDir, "persona.toml"), []byte(tomlContent), 0644) //nolint:errcheck

	// Persona with no persona.toml — fields should be empty strings.
	noTomlDir := filepath.Join(personasDir, "no-toml")
	if err := os.MkdirAll(noTomlDir, 0755); err != nil {
		t.Fatal(err)
	}

	personas := discoverPersonas(dir)

	le, ok := personas["lead-engineer"]
	if !ok {
		t.Fatal("expected lead-engineer persona")
	}
	if le.DisplayName != "Alex" {
		t.Errorf("expected DisplayName=Alex, got %q", le.DisplayName)
	}
	if le.Email != "alex@haha-systems.bot" {
		t.Errorf("expected Email=alex@haha-systems.bot, got %q", le.Email)
	}

	nt, ok := personas["no-toml"]
	if !ok {
		t.Fatal("expected no-toml persona")
	}
	if nt.DisplayName != "" {
		t.Errorf("expected empty DisplayName, got %q", nt.DisplayName)
	}
	if nt.Email != "" {
		t.Errorf("expected empty Email, got %q", nt.Email)
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
[ariadne]
default_provider = "claude"
max_concurrent_runs = 0
`,
		},
		{
			name: "empty default_provider",
			content: `
[ariadne]
max_concurrent_runs = 4
default_provider = ""
`,
		},
		{
			name: "enabled provider without binary",
			content: `
[ariadne]
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
