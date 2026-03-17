package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
)

func TestMergeEnv(t *testing.T) {
	global := map[string]string{"A": "1", "B": "2"}
	perTask := map[string]string{"B": "override", "C": "3"}

	merged := mergeEnv(global, perTask)

	if merged["A"] != "1" {
		t.Errorf("expected A=1, got %s", merged["A"])
	}
	if merged["B"] != "override" {
		t.Errorf("expected B=override (per-task wins), got %s", merged["B"])
	}
	if merged["C"] != "3" {
		t.Errorf("expected C=3, got %s", merged["C"])
	}
}

func TestMergeEnv_NilPerTask(t *testing.T) {
	global := map[string]string{"X": "y"}
	merged := mergeEnv(global, nil)
	if merged["X"] != "y" {
		t.Errorf("expected X=y, got %s", merged["X"])
	}
}

func TestTaskEnv_NilConfig(t *testing.T) {
	task := &domain.Task{}
	env := taskEnv(task)
	if env != nil {
		t.Errorf("expected nil env for task with no config, got %v", env)
	}
}

func TestTaskEnv_WithConfig(t *testing.T) {
	task := &domain.Task{
		Config: &domain.TaskConfig{
			Env: map[string]string{"NODE_ENV": "test"},
		},
	}
	env := taskEnv(task)
	if env["NODE_ENV"] != "test" {
		t.Errorf("expected NODE_ENV=test, got %s", env["NODE_ENV"])
	}
}

// makeTestPersonaDir creates a temporary persona directory with the given files.
func makeTestPersonaDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildTaskPrompt_NoPersona(t *testing.T) {
	task := &domain.Task{
		Title:       "Do something",
		Description: "Description here",
	}
	prompt := string(buildTaskPrompt(task, "", nil))
	if !strings.Contains(prompt, "# Task: Do something") {
		t.Errorf("prompt missing task title: %s", prompt)
	}
	if strings.Contains(prompt, "## Role") {
		t.Error("prompt should not contain Role section without persona")
	}
}

func TestBuildTaskPrompt_WithPersona_SOULInjected(t *testing.T) {
	dir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md": "I am a lead engineer.",
	})
	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}

	task := &domain.Task{
		Title:       "Build feature",
		Description: "Do the thing",
	}
	prompt := string(buildTaskPrompt(task, "", persona))

	if !strings.Contains(prompt, "## Role") {
		t.Error("expected ## Role section")
	}
	if !strings.Contains(prompt, "I am a lead engineer.") {
		t.Error("expected SOUL.md content in prompt")
	}
	if !strings.Contains(prompt, "**Persona:** lead-engineer") {
		t.Error("expected Persona label in prompt")
	}
}

func TestBuildTaskPrompt_PersonaWithPersonality(t *testing.T) {
	dir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md":        "Core identity.",
		"PERSONALITY.md": "Behavioral traits.",
	})
	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}
	task := &domain.Task{Title: "Task", Description: "desc"}
	prompt := string(buildTaskPrompt(task, "", persona))

	if !strings.Contains(prompt, "Core identity.") {
		t.Error("expected SOUL.md content")
	}
	if !strings.Contains(prompt, "Behavioral traits.") {
		t.Error("expected PERSONALITY.md content")
	}
}

func TestBuildTaskPrompt_PersonaExtraContextFiles(t *testing.T) {
	dir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md":    "Identity.",
		"context.md": "Extra context here.",
	})
	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}
	task := &domain.Task{Title: "Task", Description: "desc"}
	prompt := string(buildTaskPrompt(task, "", persona))

	if !strings.Contains(prompt, "## Persona Context") {
		t.Error("expected ## Persona Context section")
	}
	if !strings.Contains(prompt, "Extra context here.") {
		t.Error("expected extra context file content")
	}
}

func TestBuildTaskPrompt_PersonaCLAUDEmd_NotInPrompt(t *testing.T) {
	dir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md":   "Identity.",
		"CLAUDE.md": "Should NOT appear in prompt.",
	})
	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}
	task := &domain.Task{Title: "Task", Description: "desc"}
	prompt := string(buildTaskPrompt(task, "", persona))

	if strings.Contains(prompt, "Should NOT appear in prompt.") {
		t.Error("CLAUDE.md content must not be injected into prompt")
	}
}

func TestBuildTaskPrompt_PersonaAGENTSmd_NotInExtraContext(t *testing.T) {
	// AGENTS.md is used as workflow replacement, not extra context.
	dir := makeTestPersonaDir(t, map[string]string{
		"AGENTS.md": "Workflow instructions.",
	})
	persona := &config.PersonaConfig{Name: "p", Dir: dir}
	task := &domain.Task{Title: "Task", Description: "desc"}
	// Pass empty workflow — AGENTS.md is loaded separately via loadWorkflow.
	prompt := string(buildTaskPrompt(task, "", persona))

	if strings.Contains(prompt, "## Persona Context") {
		t.Error("AGENTS.md should not appear under Persona Context")
	}
}

func TestCopyPersonaFiles_CopiesCLAUDEmd(t *testing.T) {
	personaDir := makeTestPersonaDir(t, map[string]string{
		"CLAUDE.md": "# Project context\nSome instructions.",
	})
	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: personaDir}

	worktreePath := t.TempDir()
	if err := copyPersonaFiles(persona, worktreePath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dst := filepath.Join(worktreePath, "CLAUDE.md")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("CLAUDE.md not found in worktree: %v", err)
	}
	if !strings.Contains(string(data), "Some instructions.") {
		t.Errorf("unexpected CLAUDE.md content: %s", data)
	}
}

func TestCopyPersonaFiles_NoCLAUDEmd_NoError(t *testing.T) {
	personaDir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md": "identity",
	})
	persona := &config.PersonaConfig{Name: "p", Dir: personaDir}
	worktreePath := t.TempDir()

	if err := copyPersonaFiles(persona, worktreePath); err != nil {
		t.Errorf("unexpected error when CLAUDE.md absent: %v", err)
	}
}

func TestLoadWorkflow_PersonaAGENTSmd_UsedWhenPresent(t *testing.T) {
	personaDir := makeTestPersonaDir(t, map[string]string{
		"AGENTS.md": "Persona workflow instructions.",
	})
	persona := &config.PersonaConfig{Name: "p", Dir: personaDir}

	sup := &Supervisor{cfg: Config{WorkflowFile: ""}}
	wf := sup.loadWorkflow(persona)
	if wf != "Persona workflow instructions." {
		t.Errorf("expected persona AGENTS.md content, got: %q", wf)
	}
}

func TestLoadWorkflow_FallsBackToGlobalWhenNoAGENTSmd(t *testing.T) {
	personaDir := makeTestPersonaDir(t, map[string]string{
		"SOUL.md": "identity",
	})
	persona := &config.PersonaConfig{Name: "p", Dir: personaDir}

	// Create a global workflow file.
	repoRoot := t.TempDir()
	wfPath := filepath.Join(repoRoot, "WORKFLOW.md")
	os.WriteFile(wfPath, []byte("Global workflow."), 0644) //nolint:errcheck

	sup := &Supervisor{cfg: Config{RepoRoot: repoRoot, WorkflowFile: "WORKFLOW.md"}}
	wf := sup.loadWorkflow(persona)
	if wf != "Global workflow." {
		t.Errorf("expected global workflow content, got: %q", wf)
	}
}
