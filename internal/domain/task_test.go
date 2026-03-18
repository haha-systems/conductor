package domain

import "testing"

func TestParseFrontMatter_NoFrontMatter(t *testing.T) {
	desc := "Implement the thing described below..."
	cfg, body := ParseFrontMatter(desc)
	if cfg != nil {
		t.Error("expected nil config when no front-matter")
	}
	if body != desc {
		t.Errorf("body should be unchanged, got %q", body)
	}
}

func TestParseFrontMatter_WithConfig(t *testing.T) {
	desc := `---
conductor:
  agent: claude
  timeout_minutes: 30
  env:
    NODE_ENV: test
---
Implement the thing described below...`

	cfg, body := ParseFrontMatter(desc)
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Agent != "claude" {
		t.Errorf("expected agent=claude, got %q", cfg.Agent)
	}
	if cfg.TimeoutMinutes != 30 {
		t.Errorf("expected timeout_minutes=30, got %d", cfg.TimeoutMinutes)
	}
	if cfg.Env["NODE_ENV"] != "test" {
		t.Errorf("expected NODE_ENV=test, got %q", cfg.Env["NODE_ENV"])
	}
	if body != "Implement the thing described below..." {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestParseFrontMatter_Persona(t *testing.T) {
	desc := `---
conductor:
  persona: lead-engineer
---
Build the feature`

	cfg, body := ParseFrontMatter(desc)
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Persona != "lead-engineer" {
		t.Errorf("expected persona=lead-engineer, got %q", cfg.Persona)
	}
	if body != "Build the feature" {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestParseFrontMatter_RoutingStrategy(t *testing.T) {
	desc := `---
conductor:
  routing: race 2
---
Do the thing`

	cfg, _ := ParseFrontMatter(desc)
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Routing != "race 2" {
		t.Errorf("expected routing='race 2', got %q", cfg.Routing)
	}
}

func TestTaskType_DefaultIsIssue(t *testing.T) {
	task := &Task{}
	// Zero value of TaskType is "", not "issue" — callers should treat "" as issue.
	// Verify the constant values are as expected.
	if TaskTypeIssue != "issue" {
		t.Errorf("expected TaskTypeIssue=\"issue\", got %q", TaskTypeIssue)
	}
	if TaskTypeRebase != "rebase" {
		t.Errorf("expected TaskTypeRebase=\"rebase\", got %q", TaskTypeRebase)
	}
	// A task constructed without setting Type has the zero value (empty string),
	// which should be treated as issue by callers.
	if task.Type == TaskTypeRebase {
		t.Error("zero-value task should not be treated as rebase")
	}
}

func TestTask_RebaseFields(t *testing.T) {
	task := &Task{
		ID:         "99",
		Type:       TaskTypeRebase,
		Branch:     "ci/github-actions-workflows",
		BaseBranch: "main",
		Attempts:   1,
	}
	if task.Type != TaskTypeRebase {
		t.Errorf("expected type=rebase, got %q", task.Type)
	}
	if task.Branch != "ci/github-actions-workflows" {
		t.Errorf("unexpected branch: %q", task.Branch)
	}
	if task.BaseBranch != "main" {
		t.Errorf("unexpected base branch: %q", task.BaseBranch)
	}
	if task.Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", task.Attempts)
	}
}

func TestParseFrontMatter_MissingClosingMarker(t *testing.T) {
	desc := `---
conductor:
  agent: claude
No closing marker`

	cfg, body := ParseFrontMatter(desc)
	if cfg != nil {
		t.Error("expected nil config when closing marker missing")
	}
	if body != desc {
		t.Error("body should be the original description")
	}
}
