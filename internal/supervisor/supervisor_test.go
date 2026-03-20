package supervisor

import (
	"os"
	"os/exec"
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

func TestBuildRebasePrompt_Format(t *testing.T) {
	task := &domain.Task{
		Type:       domain.TaskTypeRebase,
		Branch:     "feature/my-pr",
		BaseBranch: "main",
		SourceURL:  "https://github.com/org/repo/pull/9",
		Attempts:   1,
	}
	prompt := string(buildRebasePrompt(task, ""))

	if !strings.Contains(prompt, "# Task: Rebase `feature/my-pr` onto `main`") {
		t.Error("missing rebase title")
	}
	if !strings.Contains(prompt, "**Attempt:** 2 of 3") {
		t.Error("expected attempt count 2 (Attempts+1)")
	}
	if !strings.Contains(prompt, "git rebase origin/main") {
		t.Error("missing rebase command")
	}
	if !strings.Contains(prompt, "git push --force-with-lease origin feature/my-pr") {
		t.Error("missing push command")
	}
	if !strings.Contains(prompt, "https://github.com/org/repo/pull/9") {
		t.Error("missing PR URL")
	}
}

func TestBuildRebasePrompt_WithWorkflow(t *testing.T) {
	task := &domain.Task{
		Type:       domain.TaskTypeRebase,
		Branch:     "fix/thing",
		BaseBranch: "main",
		SourceURL:  "https://github.com/org/repo/pull/3",
		Attempts:   0,
	}
	workflow := "You are rebasing a PR. Be careful."
	prompt := string(buildRebasePrompt(task, workflow))

	if !strings.HasPrefix(prompt, workflow) {
		t.Error("workflow should appear at start of prompt")
	}
	if !strings.Contains(prompt, "---") {
		t.Error("expected separator between workflow and task")
	}
}

func TestBuildReviewPrompt_Format(t *testing.T) {
	repoDir := t.TempDir()

	// Create QA persona AGENTS.md
	personasDir := filepath.Join(repoDir, ".conductor", "personas", "qa-engineer")
	os.MkdirAll(personasDir, 0755) //nolint:errcheck
	os.WriteFile(filepath.Join(personasDir, "AGENTS.md"), []byte("You are a QA reviewer."), 0644) //nolint:errcheck

	sup := &Supervisor{cfg: Config{RepoRoot: repoDir, TimeoutMinutes: 5}}
	task := &domain.Task{
		ID:              "7",
		Type:            domain.TaskTypeReview,
		SourceURL:       "https://github.com/org/repo/pull/7",
		Branch:          "feat/my-feature",
		ReviewCycle:     1,
		SpecIssueNumber: 42,
	}

	prompt, err := sup.buildReviewPrompt(t.Context(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(prompt)

	if !strings.Contains(s, "You are a QA reviewer.") {
		t.Error("missing QA AGENTS.md content")
	}
	if !strings.Contains(s, "# Task: Review PR #7 — cycle 1 of 3") {
		t.Error("missing review task title")
	}
	if !strings.Contains(s, "**PR:** https://github.com/org/repo/pull/7") {
		t.Error("missing PR URL")
	}
	if !strings.Contains(s, "**Branch:** feat/my-feature") {
		t.Error("missing branch")
	}
	if !strings.Contains(s, "**Original issue:** #42") {
		t.Error("missing original issue number")
	}
	if !strings.Contains(s, "gh pr diff 7") {
		t.Error("missing gh pr diff instruction")
	}
	if !strings.Contains(s, "gh pr review 7 --approve") {
		t.Error("missing approve instruction")
	}
	if !strings.Contains(s, "gh pr review 7 --request-changes") {
		t.Error("missing request-changes instruction")
	}
}

func TestBuildReviewPrompt_NoPersonaFile(t *testing.T) {
	repoDir := t.TempDir() // no qa-engineer persona

	sup := &Supervisor{cfg: Config{RepoRoot: repoDir, TimeoutMinutes: 5}}
	task := &domain.Task{
		ID:              "7",
		Type:            domain.TaskTypeReview,
		SourceURL:       "https://github.com/org/repo/pull/7",
		Branch:          "feat/my-feature",
		ReviewCycle:     0,
		SpecIssueNumber: 42,
	}

	prompt, err := sup.buildReviewPrompt(t.Context(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(prompt), "# Task: Review PR #7") {
		t.Error("missing review task title when no persona file")
	}
}

func TestBuildRevisionPrompt_Format(t *testing.T) {
	repoDir := t.TempDir()

	sup := &Supervisor{cfg: Config{RepoRoot: repoDir, TimeoutMinutes: 5}}
	task := &domain.Task{
		ID:              "7",
		Type:            domain.TaskTypeRevise,
		SourceURL:       "https://github.com/org/repo/pull/7",
		Branch:          "feat/my-feature",
		ReviewCycle:     1,
		SpecIssueNumber: 42,
	}

	prompt, err := sup.buildRevisionPrompt(t.Context(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(prompt)

	if !strings.Contains(s, "# Task: Address QA feedback on PR #7 — cycle 1 of 3") {
		t.Error("missing revision task title")
	}
	if !strings.Contains(s, "**PR:** https://github.com/org/repo/pull/7") {
		t.Error("missing PR URL")
	}
	if !strings.Contains(s, "**Branch:** feat/my-feature") {
		t.Error("missing branch")
	}
	if !strings.Contains(s, "## Original Spec (Issue #42)") {
		t.Error("missing original spec section")
	}
	if !strings.Contains(s, "## QA Feedback") {
		t.Error("missing QA Feedback section")
	}
	if !strings.Contains(s, "address QA feedback (cycle 1)") {
		t.Error("missing commit message with cycle number")
	}
	if !strings.Contains(s, "git push origin feat/my-feature") {
		t.Error("missing push instruction")
	}
}

func TestExtractRepoSlug(t *testing.T) {
	cases := []struct {
		url      string
		expected string
	}{
		{"https://github.com/org/repo/pull/7", "org/repo"},
		{"https://github.com/my-org/my-repo/issues/42", "my-org/my-repo"},
		{"https://notgithub.com/org/repo/pull/1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := extractRepoSlug(tc.url)
		if got != tc.expected {
			t.Errorf("extractRepoSlug(%q) = %q, want %q", tc.url, got, tc.expected)
		}
	}
}

func TestConfigureGitAuthor_SetsNameAndEmail(t *testing.T) {
	dir := t.TempDir()
	// Initialize a real git repo so git config works.
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	persona := &config.PersonaConfig{
		Name:        "lead-engineer",
		DisplayName: "Alex",
		Email:       "alex@example.com",
	}
	if err := configureGitAuthor(dir, persona); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nameOut, _ := exec.Command("git", "-C", dir, "config", "user.name").Output()
	if strings.TrimSpace(string(nameOut)) != "Alex" {
		t.Errorf("expected user.name=Alex, got %q", strings.TrimSpace(string(nameOut)))
	}
	emailOut, _ := exec.Command("git", "-C", dir, "config", "user.email").Output()
	if strings.TrimSpace(string(emailOut)) != "alex@example.com" {
		t.Errorf("expected user.email=alex@example.com, got %q", strings.TrimSpace(string(emailOut)))
	}
}

func TestConfigureGitAuthor_FallsBackToName(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	persona := &config.PersonaConfig{
		Name:        "lead-engineer",
		DisplayName: "", // empty — should fall back to Name
	}
	if err := configureGitAuthor(dir, persona); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nameOut, _ := exec.Command("git", "-C", dir, "config", "user.name").Output()
	if strings.TrimSpace(string(nameOut)) != "lead-engineer" {
		t.Errorf("expected user.name=lead-engineer, got %q", strings.TrimSpace(string(nameOut)))
	}
}

func TestConfigureGitAuthor_NilPersona(t *testing.T) {
	if err := configureGitAuthor(t.TempDir(), nil); err != nil {
		t.Errorf("expected nil error for nil persona, got %v", err)
	}
}

func TestMergeEnv_ThreeMaps(t *testing.T) {
	a := map[string]string{"A": "1", "B": "base"}
	b := map[string]string{"B": "mid", "C": "3"}
	c := map[string]string{"B": "last", "D": "4"}

	merged := mergeEnv(a, b, c)
	if merged["A"] != "1" {
		t.Errorf("expected A=1, got %s", merged["A"])
	}
	if merged["B"] != "last" {
		t.Errorf("expected B=last (last map wins), got %s", merged["B"])
	}
	if merged["C"] != "3" {
		t.Errorf("expected C=3, got %s", merged["C"])
	}
	if merged["D"] != "4" {
		t.Errorf("expected D=4, got %s", merged["D"])
	}
}

func TestExecute_RebaseTask_TakesRebasePath(t *testing.T) {
	// We verify that Execute() with a rebase task does NOT fall through to the
	// issue path by checking it calls executeRebase (which tries git fetch).
	// Since there's no real git repo in a temp dir, it will fail at that step —
	// but the error message must reflect the rebase path, not the issue path.
	repoDir := t.TempDir()
	sup := New(Config{
		RepoRoot:       repoDir,
		WorktreeBaseDir: ".conductor/runs",
		TimeoutMinutes: 1,
	})
	task := &domain.Task{
		ID:         "pr-99",
		Type:       domain.TaskTypeRebase,
		Branch:     "feature/test",
		BaseBranch: "main",
	}
	run := &domain.Run{ID: "test-run", TaskID: task.ID}
	result := sup.Execute(t.Context(), RunRequest{Run: run, Task: task})
	if result == nil {
		t.Fatal("expected a result")
	}
	// Should fail in the rebase path (git fetch fails in a non-git temp dir).
	if result.Err == nil {
		t.Fatal("expected error (no git repo in temp dir)")
	}
	// The error comes from git fetch, not from createWorktree (issue path).
	// Both produce exec errors, but rebase path says "git fetch".
	if !strings.Contains(result.Err.Error(), "git fetch") &&
		!strings.Contains(result.Err.Error(), "fetch") &&
		!strings.Contains(result.Err.Error(), "create rebase worktree") {
		t.Errorf("expected rebase-path error, got: %v", result.Err)
	}
}
