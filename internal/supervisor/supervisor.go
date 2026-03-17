package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
	"github.com/haha-systems/conductor/internal/provider"
)

// RunRequest is submitted to the supervisor to start a run.
type RunRequest struct {
	Run      *domain.Run
	Task     *domain.Task
	Provider provider.AgentProvider
	// GlobalEnv are extra env vars from conductor.toml [sandbox].
	GlobalEnv map[string]string
	// Persona is the resolved persona for this run, or nil.
	Persona *config.PersonaConfig
}

// Result is returned by the supervisor after a run terminates.
type Result struct {
	Run *domain.Run
	Err error
}

// Config holds supervisor configuration.
type Config struct {
	WorktreeBaseDir   string
	TimeoutMinutes    int
	PreserveOnFailure bool
	RepoRoot          string
	// WorkflowFile is the path (relative to RepoRoot) of the workflow markdown
	// to prepend to every task prompt. Silently skipped if the file is absent.
	WorkflowFile string
}

// Supervisor manages the lifecycle of a single run.
type Supervisor struct {
	cfg Config
}

func New(cfg Config) *Supervisor {
	return &Supervisor{cfg: cfg}
}

// Execute runs the task synchronously and returns when the run reaches a terminal state.
// The caller is responsible for collecting proof afterwards.
func (s *Supervisor) Execute(ctx context.Context, req RunRequest) *Result {
	run := req.Run
	now := time.Now()
	run.StartedAt = now
	run.Status = domain.RunStatusRunning

	worktreePath := filepath.Join(s.cfg.RepoRoot, s.cfg.WorktreeBaseDir, run.ID)
	run.WorktreePath = worktreePath

	// 1. Create git worktree.
	if err := s.createWorktree(worktreePath); err != nil {
		return s.fail(run, fmt.Errorf("create worktree: %w", err))
	}

	// 2. Copy persona files (e.g. CLAUDE.md) to worktree root, if a persona is active.
	if req.Persona != nil {
		if err := copyPersonaFiles(req.Persona, worktreePath); err != nil {
			s.cleanup(run)
			return s.fail(run, fmt.Errorf("copy persona files: %w", err))
		}
	}

	// 3. Write task prompt to file.
	taskFile := filepath.Join(worktreePath, ".conductor-task.md")
	workflow := s.loadWorkflow(req.Persona)
	prompt := buildTaskPrompt(req.Task, workflow, req.Persona)
	if err := os.WriteFile(taskFile, prompt, 0600); err != nil {
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("write task file: %w", err))
	}

	// 4. Open run log.
	if err := os.MkdirAll(filepath.Join(worktreePath, "proof"), 0755); err != nil {
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create proof dir: %w", err))
	}
	logPath := filepath.Join(worktreePath, "run.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create run log: %w", err))
	}
	defer logFile.Close()

	logWriter := &providerLogWriter{w: logFile}

	// 5. Apply timeout.
	timeout := time.Duration(s.cfg.TimeoutMinutes) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 6. Build env: merge global + per-task.
	env := mergeEnv(req.GlobalEnv, taskEnv(req.Task))

	rc := provider.RunContext{
		RepoPath:       worktreePath,
		TaskFile:       taskFile,
		Env:            env,
		LogWriter:      logWriter,
		TimeoutSeconds: int(timeout.Seconds()),
	}

	// 7. Launch provider.
	logEvent(logFile, "run_started", map[string]any{
		"run_id":   run.ID,
		"provider": req.Provider.Name(),
		"task_id":  run.TaskID,
	})

	handle, err := req.Provider.Run(runCtx, rc)
	if err != nil {
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("launch provider: %w", err))
	}

	// 8. Wait for completion.
	// exec.CommandContext kills the process when runCtx expires, so Wait() will
	// return in all cases. We check runCtx.Err() after Wait() to distinguish
	// timeout from ordinary failure. We do NOT call Cancel() here — the process
	// is already dead once Wait() returns, and calling Cancel() would trigger a
	// second Wait() call on the same Cmd (which is unsafe).
	waitErr := handle.Wait()

	finished := time.Now()
	run.FinishedAt = &finished

	if runCtx.Err() == context.DeadlineExceeded {
		run.Status = domain.RunStatusTimeout
		logEvent(logFile, "run_timeout", map[string]any{"run_id": run.ID})
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: fmt.Errorf("run timed out after %d minutes", s.cfg.TimeoutMinutes)}
	}

	if waitErr != nil {
		run.Status = domain.RunStatusFailed
		run.ErrorMsg = waitErr.Error()
		logEvent(logFile, "run_failed", map[string]any{"run_id": run.ID, "error": waitErr.Error()})
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: waitErr}
	}

	run.Status = domain.RunStatusSucceeded
	logEvent(logFile, "run_succeeded", map[string]any{"run_id": run.ID})

	return &Result{Run: run}
}

// Cleanup removes the worktree. Called explicitly on success; may be skipped on failure.
func (s *Supervisor) Cleanup(run *domain.Run) error {
	return s.removeWorktree(run.WorktreePath)
}

func (s *Supervisor) createWorktree(path string) error {
	cmd := exec.Command("git", "worktree", "add", "--detach", path)
	cmd.Dir = s.cfg.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (s *Supervisor) removeWorktree(path string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", path)
	cmd.Dir = s.cfg.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (s *Supervisor) cleanup(run *domain.Run) {
	s.removeWorktree(run.WorktreePath) //nolint:errcheck
}

func (s *Supervisor) fail(run *domain.Run, err error) *Result {
	t := time.Now()
	run.Status = domain.RunStatusFailed
	run.FinishedAt = &t
	run.ErrorMsg = err.Error()
	return &Result{Run: run, Err: err}
}

// logEvent writes a structured JSON log line to the run log file.
func logEvent(w *os.File, event string, fields map[string]any) {
	fields["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["event"] = event
	line, _ := json.Marshal(fields)
	w.Write(append(line, '\n')) //nolint:errcheck
}

// providerLogWriter wraps the run log file and writes each provider output
// chunk as a structured JSON log entry.
type providerLogWriter struct {
	w *os.File
}

func (lw *providerLogWriter) Write(p []byte) (int, error) {
	entry := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"event":     "provider_stdout",
		"line":      string(p),
	}
	line, _ := json.Marshal(entry)
	lw.w.Write(append(line, '\n')) //nolint:errcheck
	return len(p), nil
}

func mergeEnv(global, perTask map[string]string) map[string]string {
	merged := make(map[string]string, len(global)+len(perTask))
	for k, v := range global {
		merged[k] = v
	}
	for k, v := range perTask {
		merged[k] = v
	}
	return merged
}

func taskEnv(task *domain.Task) map[string]string {
	if task.Config == nil {
		return nil
	}
	return task.Config.Env
}

// loadWorkflow reads the workflow content. If a persona is active and has an
// AGENTS.md file, that is used in place of the global workflow_file.
func (s *Supervisor) loadWorkflow(persona *config.PersonaConfig) string {
	if persona != nil {
		agentsPath := filepath.Join(persona.Dir, "AGENTS.md")
		if data, err := os.ReadFile(agentsPath); err == nil {
			return string(data)
		}
	}

	if s.cfg.WorkflowFile == "" {
		return ""
	}
	path := filepath.Join(s.cfg.RepoRoot, s.cfg.WorkflowFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // silently skip missing workflow file
	}
	return string(data)
}

// buildTaskPrompt formats a task into a structured prompt for the agent.
// If workflow is non-empty it is prepended so the agent sees operating
// instructions before the task-specific content.
// If persona is non-nil, a Role section and additional context are injected.
func buildTaskPrompt(task *domain.Task, workflow string, persona *config.PersonaConfig) []byte {
	var b strings.Builder
	if workflow != "" {
		b.WriteString(workflow)
		b.WriteString("\n\n---\n\n")
	}

	if persona != nil {
		writePersonaRole(&b, persona)
	}

	fmt.Fprintf(&b, "# Task: %s\n\n", task.Title)
	if task.SourceURL != "" {
		fmt.Fprintf(&b, "**Source:** %s\n", task.SourceURL)
	}
	if task.Source == "github" {
		if n := githubIssueNumber(task.SourceURL); n != "" {
			fmt.Fprintf(&b, "**Issue number:** %s (use `Closes #%s` in the PR description)\n", n, n)
		}
	}
	if len(task.Labels) > 0 {
		fmt.Fprintf(&b, "**Labels:** %s\n", strings.Join(task.Labels, ", "))
	}
	if persona != nil {
		fmt.Fprintf(&b, "**Persona:** %s\n", persona.Name)
	}
	b.WriteString("\n## Description\n\n")
	b.WriteString(task.Description)
	b.WriteString("\n")
	return []byte(b.String())
}

// writePersonaRole injects the SOUL.md, PERSONALITY.md, and additional .md
// context files from the persona directory into the prompt builder.
func writePersonaRole(b *strings.Builder, persona *config.PersonaConfig) {
	soul := readPersonaFile(persona.Dir, "SOUL.md")
	personality := readPersonaFile(persona.Dir, "PERSONALITY.md")

	if soul != "" || personality != "" {
		b.WriteString("## Role\n\n")
		if soul != "" {
			b.WriteString(soul)
			b.WriteString("\n")
		}
		if personality != "" {
			b.WriteString("\n")
			b.WriteString(personality)
			b.WriteString("\n")
		}
		b.WriteString("\n---\n\n")
	}

	// Include any other .md files (excluding known special files).
	extras := extraPersonaMDFiles(persona.Dir)
	if len(extras) > 0 {
		b.WriteString("## Persona Context\n\n")
		for _, name := range extras {
			content := readPersonaFile(persona.Dir, name)
			if content != "" {
				fmt.Fprintf(b, "### %s\n\n%s\n\n", name, content)
			}
		}
		b.WriteString("---\n\n")
	}
}

// readPersonaFile reads a file from the persona directory. Returns empty string
// if the file is absent.
func readPersonaFile(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// extraPersonaMDFiles returns sorted names of .md files in the persona directory
// that are not one of the known special files.
func extraPersonaMDFiles(dir string) []string {
	skip := map[string]bool{
		"SOUL.md":        true,
		"PERSONALITY.md": true,
		"CLAUDE.md":      true,
		"AGENTS.md":      true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".md") && !skip[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// copyPersonaFiles copies special non-prompt files from the persona directory
// to the worktree root. Currently copies CLAUDE.md if present.
func copyPersonaFiles(persona *config.PersonaConfig, worktreePath string) error {
	filesToCopy := []string{"CLAUDE.md"}
	for _, name := range filesToCopy {
		src := filepath.Join(persona.Dir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue // file not present in persona — skip
			}
			return fmt.Errorf("reading persona file %s: %w", name, err)
		}
		dst := filepath.Join(worktreePath, name)
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("writing %s to worktree: %w", name, err)
		}
	}
	return nil
}

// githubIssueNumber extracts the issue number from a GitHub issue URL.
// e.g. "https://github.com/org/repo/issues/42" -> "42"
func githubIssueNumber(sourceURL string) string {
	parts := strings.Split(sourceURL, "/issues/")
	if len(parts) == 2 && parts[1] != "" {
		return strings.TrimRight(parts[1], "/")
	}
	return ""
}
