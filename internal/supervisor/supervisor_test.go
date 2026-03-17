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

func TestBuildTaskPrompt_NoPersona(t *testing.T) {
	task := &domain.Task{
		Title:       "Do the thing",
		Description: "Details here.",
		Source:      "github",
		SourceURL:   "https://github.com/org/repo/issues/42",
		Labels:      []string{"conductor"},
	}
	prompt := string(buildTaskPrompt(task, "", nil))
	if !strings.Contains(prompt, "# Task: Do the thing") {
		t.Error("missing task title")
	}
	if !strings.Contains(prompt, "Issue number:** 42") {
		t.Error("missing issue number")
	}
	if strings.Contains(prompt, "## Role") {
		t.Error("unexpected Role section when persona is nil")
	}
	if strings.Contains(prompt, "**Persona:**") {
		t.Error("unexpected Persona line when persona is nil")
	}
}

func TestBuildTaskPrompt_WithPersona_SoulAndPersonality(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("I am a lead engineer."), 0644)        //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "PERSONALITY.md"), []byte("I prefer minimal code."), 0644) //nolint:errcheck

	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}
	task := &domain.Task{Title: "Build feature", Description: "Implement X."}

	prompt := string(buildTaskPrompt(task, "", persona))

	if !strings.Contains(prompt, "## Role") {
		t.Error("missing Role section")
	}
	if !strings.Contains(prompt, "I am a lead engineer.") {
		t.Error("SOUL.md content missing from prompt")
	}
	if !strings.Contains(prompt, "I prefer minimal code.") {
		t.Error("PERSONALITY.md content missing from prompt")
	}
	if !strings.Contains(prompt, "**Persona:** lead-engineer") {
		t.Error("missing Persona line in task header")
	}
}

func TestBuildTaskPrompt_WithPersona_WorkflowFromAgentsMd(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Agent workflow here."), 0644) //nolint:errcheck

	sup := &Supervisor{cfg: Config{WorkflowFile: ""}}
	persona := &config.PersonaConfig{Name: "pm", Dir: dir}
	workflow := sup.loadWorkflow(persona)

	if workflow != "Agent workflow here." {
		t.Errorf("expected AGENTS.md content as workflow, got %q", workflow)
	}
}

func TestBuildTaskPrompt_WithPersona_FallsBackToWorkflowFile(t *testing.T) {
	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "workflow.md"), []byte("Global workflow."), 0644) //nolint:errcheck

	personaDir := t.TempDir() // no AGENTS.md

	sup := &Supervisor{cfg: Config{RepoRoot: repoDir, WorkflowFile: "workflow.md"}}
	persona := &config.PersonaConfig{Name: "pm", Dir: personaDir}
	workflow := sup.loadWorkflow(persona)

	if workflow != "Global workflow." {
		t.Errorf("expected global workflow, got %q", workflow)
	}
}

func TestBuildTaskPrompt_WithPersona_ExtraFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Soul content."), 0644)         //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "CONTEXT.md"), []byte("Extra context here."), 0644) //nolint:errcheck

	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: dir}
	task := &domain.Task{Title: "Task", Description: "Do it."}
	prompt := string(buildTaskPrompt(task, "", persona))

	if !strings.Contains(prompt, "## Persona Context") {
		t.Error("missing Persona Context section for extra .md files")
	}
	if !strings.Contains(prompt, "Extra context here.") {
		t.Error("extra .md file content missing from prompt")
	}
}

func TestCopyPersonaFiles_CopiesCLAUDEMd(t *testing.T) {
	personaDir := t.TempDir()
	worktreeDir := t.TempDir()
	os.WriteFile(filepath.Join(personaDir, "CLAUDE.md"), []byte("# Claude instructions"), 0644) //nolint:errcheck

	persona := &config.PersonaConfig{Name: "lead-engineer", Dir: personaDir}
	if err := copyPersonaFiles(persona, worktreeDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dest := filepath.Join(worktreeDir, "CLAUDE.md")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("CLAUDE.md not found in worktree: %v", err)
	}
	if string(data) != "# Claude instructions" {
		t.Errorf("unexpected CLAUDE.md content: %q", data)
	}
}

func TestCopyPersonaFiles_NoCLAUDEMd(t *testing.T) {
	personaDir := t.TempDir()
	worktreeDir := t.TempDir()

	persona := &config.PersonaConfig{Name: "pm", Dir: personaDir}
	// Should not error when CLAUDE.md is absent.
	if err := copyPersonaFiles(persona, worktreeDir); err != nil {
		t.Errorf("unexpected error when CLAUDE.md absent: %v", err)
	}
}
