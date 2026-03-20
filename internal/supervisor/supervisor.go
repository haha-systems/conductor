package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	charmlog "github.com/charmbracelet/log"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
	"github.com/haha-systems/conductor/internal/provider"
)

// RebaseRecorder is satisfied by any WorkSource that supports rebase outcome
// recording. It is used by the supervisor without importing the worksource package.
type RebaseRecorder interface {
	RecordRebaseOutcome(ctx context.Context, task *domain.Task, succeeded bool, reason string) error
}

// ReviewRecorder is satisfied by any WorkSource that supports QA review outcome
// recording. It is used by the supervisor without importing the worksource package.
type ReviewRecorder interface {
	RecordReviewOutcome(ctx context.Context, task *domain.Task, approved bool, body string) error
	MarkPRNeedsReview(ctx context.Context, prNumber int, issueNumber int) error
}

// RunRequest is submitted to the supervisor to start a run.
type RunRequest struct {
	Run      *domain.Run
	Task     *domain.Task
	Provider provider.AgentProvider
	// GlobalEnv are extra env vars from conductor.toml [sandbox].
	GlobalEnv map[string]string
	// Persona is the resolved persona for this run, or nil if none.
	Persona *config.PersonaConfig
	// Source is used by rebase tasks to record outcomes. Nil is safe for issue tasks.
	Source RebaseRecorder
	// ReviewSource is used by review/revise tasks to record outcomes. Nil is safe for other task types.
	ReviewSource ReviewRecorder
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
	switch req.Task.Type {
	case domain.TaskTypeRebase:
		return s.executeRebase(ctx, req)
	case domain.TaskTypeReview:
		return s.executeReview(ctx, req)
	case domain.TaskTypeRevise:
		return s.executeRevise(ctx, req)
	}

	run := req.Run
	now := time.Now()
	run.StartedAt = now
	run.Status = domain.RunStatusRunning

	worktreePath := filepath.Join(s.cfg.RepoRoot, s.cfg.WorktreeBaseDir, run.ID)
	run.WorktreePath = worktreePath

	charmlog.Info("run starting", "run_id", run.ID, "task_id", run.TaskID, "type", "issue", "provider", req.Provider.Name())

	// 1. Create git worktree.
	if err := s.createWorktree(worktreePath); err != nil {
		return s.fail(run, fmt.Errorf("create worktree: %w", err))
	}
	charmlog.Info("worktree created", "run_id", run.ID, "path", worktreePath)

	// 2. Copy persona files (e.g. CLAUDE.md) into the worktree root if a persona is set.
	if req.Persona != nil {
		if err := copyPersonaFiles(req.Persona, worktreePath); err != nil {
			s.cleanup(run)
			return s.fail(run, fmt.Errorf("copy persona files: %w", err))
		}
	}

	if err := configureGitAuthor(worktreePath, req.Persona); err != nil {
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("configure git author: %w", err))
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

	logWriter := newProviderLogWriter(logFile, run.ID)

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
	charmlog.Info("agent launched", "run_id", run.ID, "provider", req.Provider.Name())

	// 8. Wait for completion.
	// exec.CommandContext kills the process when runCtx expires, so Wait() will
	// return in all cases. We check runCtx.Err() after Wait() to distinguish
	// timeout from ordinary failure. We do NOT call Cancel() here — the process
	// is already dead once Wait() returns, and calling Cancel() would trigger a
	// second Wait() call on the same Cmd (which is unsafe).
	waitErr := handle.Wait()

	finished := time.Now()
	run.FinishedAt = &finished
	duration := finished.Sub(run.StartedAt).Round(time.Second)

	if runCtx.Err() == context.DeadlineExceeded {
		run.Status = domain.RunStatusTimeout
		logEvent(logFile, "run_timeout", map[string]any{"run_id": run.ID})
		charmlog.Warn("run timed out", "run_id", run.ID, "task_id", run.TaskID, "timeout", fmt.Sprintf("%dm", s.cfg.TimeoutMinutes))
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: fmt.Errorf("run timed out after %d minutes", s.cfg.TimeoutMinutes)}
	}

	if waitErr != nil {
		run.Status = domain.RunStatusFailed
		run.ErrorMsg = waitErr.Error()
		logEvent(logFile, "run_failed", map[string]any{"run_id": run.ID, "error": waitErr.Error()})
		charmlog.Error("run failed", "run_id", run.ID, "task_id", run.TaskID, "error", waitErr)
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: waitErr}
	}

	run.Status = domain.RunStatusSucceeded
	logEvent(logFile, "run_succeeded", map[string]any{"run_id": run.ID})
	charmlog.Info("run succeeded", "run_id", run.ID, "task_id", run.TaskID, "duration", duration)

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

// providerLogWriter wraps the run log file and streams each line to stderr
// with a run ID prefix so concurrent runs stay distinguishable.
type providerLogWriter struct {
	w      *os.File // run.jsonl
	prefix string   // e.g. "[run_123] "
	buf    []byte   // line buffer for prefix injection
}

func newProviderLogWriter(w *os.File, runID string) *providerLogWriter {
	return &providerLogWriter{
		w:      w,
		prefix: "[" + runID + "] ",
	}
}

func (lw *providerLogWriter) Write(p []byte) (int, error) {
	lw.buf = append(lw.buf, p...)

	for {
		idx := bytes.IndexByte(lw.buf, '\n')
		if idx < 0 {
			break
		}
		line := lw.buf[:idx+1]

		// Write to run.jsonl as structured JSON.
		entry := map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"event":     "provider_stdout",
			"line":      string(bytes.TrimRight(line, "\n")),
		}
		encoded, _ := json.Marshal(entry)
		lw.w.Write(append(encoded, '\n')) //nolint:errcheck

		// Stream to stderr with run ID prefix.
		fmt.Fprintf(os.Stderr, "%s%s", lw.prefix, line)

		lw.buf = lw.buf[idx+1:]
	}

	return len(p), nil
}

func mergeEnv(maps ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged
}

func configureGitAuthor(worktreePath string, persona *config.PersonaConfig) error {
	if persona == nil {
		return nil
	}
	name := persona.DisplayName
	if name == "" {
		name = persona.Name
	}
	if name != "" {
		cmd := exec.Command("git", "config", "user.name", name)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config user.name: %w: %s", err, out)
		}
	}
	if persona.Email != "" {
		cmd := exec.Command("git", "config", "user.email", persona.Email)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config user.email: %w: %s", err, out)
		}
	}
	return nil
}

func taskEnv(task *domain.Task) map[string]string {
	if task.Config == nil {
		return nil
	}
	return task.Config.Env
}

// loadWorkflow reads the workflow content. If a persona is provided and has
// an AGENTS.md file, that file is used instead of sandbox.workflow_file.
// Returns empty string if no workflow is available.
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

// copyPersonaFiles copies special persona files to the worktree root.
// Currently copies CLAUDE.md so the claude CLI picks it up automatically.
func copyPersonaFiles(persona *config.PersonaConfig, worktreePath string) error {
	claudePath := filepath.Join(persona.Dir, "CLAUDE.md")
	data, err := os.ReadFile(claudePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // CLAUDE.md is optional
		}
		return fmt.Errorf("reading CLAUDE.md: %w", err)
	}
	dest := filepath.Join(worktreePath, "CLAUDE.md")
	return os.WriteFile(dest, data, 0644)
}

// buildTaskPrompt formats a task into a structured prompt for the agent.
// If workflow is non-empty it is prepended. If persona is non-nil, a Role
// section is injected from SOUL.md and PERSONALITY.md, and any additional
// .md files are included under a Persona Context heading.
func buildTaskPrompt(task *domain.Task, workflow string, persona *config.PersonaConfig) []byte {
	var b strings.Builder
	if workflow != "" {
		b.WriteString(workflow)
		b.WriteString("\n\n---\n\n")
	}

	if persona != nil {
		// Inject Role section from SOUL.md and optionally PERSONALITY.md.
		soulPath := filepath.Join(persona.Dir, "SOUL.md")
		if soul, err := os.ReadFile(soulPath); err == nil {
			b.WriteString("## Role\n\n")
			b.WriteString(string(soul))
			b.WriteString("\n")

			personalityPath := filepath.Join(persona.Dir, "PERSONALITY.md")
			if personality, err := os.ReadFile(personalityPath); err == nil {
				b.WriteString("\n")
				b.WriteString(string(personality))
				b.WriteString("\n")
			}

			b.WriteString("\n---\n\n")
		}

		// Inject additional .md files under Persona Context.
		extras := personaExtraFiles(persona)
		if len(extras) > 0 {
			b.WriteString("## Persona Context\n\n")
			for _, name := range extras {
				data, err := os.ReadFile(filepath.Join(persona.Dir, name))
				if err != nil {
					continue
				}
				b.WriteString(string(data))
				b.WriteString("\n")
			}
			b.WriteString("\n---\n\n")
		}
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

// personaExtraFiles returns sorted names of .md files in the persona directory
// that are not SOUL.md, PERSONALITY.md, CLAUDE.md, or AGENTS.md.
func personaExtraFiles(persona *config.PersonaConfig) []string {
	known := map[string]bool{
		"SOUL.md":        true,
		"PERSONALITY.md": true,
		"CLAUDE.md":      true,
		"AGENTS.md":      true,
	}
	entries, err := os.ReadDir(persona.Dir)
	if err != nil {
		return nil
	}
	var extras []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") || known[name] {
			continue
		}
		extras = append(extras, name)
	}
	sort.Strings(extras)
	return extras
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

// executeRebase handles a TaskTypeRebase run: it sets up a worktree with the PR
// branch, builds a rebase prompt, runs the agent, and records the outcome.
func (s *Supervisor) executeRebase(ctx context.Context, req RunRequest) *Result {
	run := req.Run
	now := time.Now()
	run.StartedAt = now
	run.Status = domain.RunStatusRunning

	worktreePath := filepath.Join(s.cfg.RepoRoot, s.cfg.WorktreeBaseDir, run.ID)
	run.WorktreePath = worktreePath

	charmlog.Info("rebase starting", "run_id", run.ID, "pr", req.Task.ID, "branch", req.Task.Branch, "cycle", req.Task.Attempts+1)

	// Fetch so origin/<branch> is current.
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = s.cfg.RepoRoot
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("git fetch: %w: %s", err, out))
	}

	// Create worktree with the PR branch checked out.
	if err := s.createRebaseBranchWorktree(worktreePath, req.Task.Branch); err != nil {
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("create rebase worktree: %w", err))
	}

	// Build rebase prompt.
	workflow := s.loadRebaseWorkflow()
	prompt := buildRebasePrompt(req.Task, workflow)
	taskFile := filepath.Join(worktreePath, ".conductor-task.md")
	if err := os.WriteFile(taskFile, prompt, 0600); err != nil {
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("write task file: %w", err))
	}

	// Open run log.
	if err := os.MkdirAll(filepath.Join(worktreePath, "proof"), 0755); err != nil {
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create proof dir: %w", err))
	}
	logPath := filepath.Join(worktreePath, "run.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create run log: %w", err))
	}
	defer logFile.Close()

	logWriter := newProviderLogWriter(logFile, run.ID)

	// Apply timeout.
	timeout := time.Duration(s.cfg.TimeoutMinutes) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := mergeEnv(req.GlobalEnv, taskEnv(req.Task))
	rc := provider.RunContext{
		RepoPath:       worktreePath,
		TaskFile:       taskFile,
		Env:            env,
		LogWriter:      logWriter,
		TimeoutSeconds: int(timeout.Seconds()),
	}

	logEvent(logFile, "run_started", map[string]any{
		"run_id":   run.ID,
		"provider": req.Provider.Name(),
		"task_id":  run.TaskID,
		"type":     "rebase",
		"branch":   req.Task.Branch,
	})

	handle, err := req.Provider.Run(runCtx, rc)
	if err != nil {
		s.cleanup(run)
		s.recordRebaseOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("launch provider: %w", err))
	}
	charmlog.Info("agent launched", "run_id", run.ID, "provider", req.Provider.Name())

	waitErr := handle.Wait()
	finished := time.Now()
	run.FinishedAt = &finished

	if runCtx.Err() == context.DeadlineExceeded {
		run.Status = domain.RunStatusTimeout
		reason := fmt.Sprintf("run timed out after %d minutes", s.cfg.TimeoutMinutes)
		logEvent(logFile, "run_timeout", map[string]any{"run_id": run.ID})
		charmlog.Warn("run timed out", "run_id", run.ID, "pr", req.Task.ID, "branch", req.Task.Branch)
		s.recordRebaseOutcome(ctx, req, false, reason)
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: fmt.Errorf("%s", reason)}
	}

	if waitErr != nil {
		run.Status = domain.RunStatusFailed
		run.ErrorMsg = waitErr.Error()
		logEvent(logFile, "run_failed", map[string]any{"run_id": run.ID, "error": waitErr.Error()})
		charmlog.Error("rebase failed", "run_id", run.ID, "pr", req.Task.ID, "attempt", req.Task.Attempts+1, "error", waitErr)
		s.recordRebaseOutcome(ctx, req, false, waitErr.Error())
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: waitErr}
	}

	run.Status = domain.RunStatusSucceeded
	logEvent(logFile, "run_succeeded", map[string]any{"run_id": run.ID})
	charmlog.Info("rebase succeeded", "run_id", run.ID, "pr", req.Task.ID, "branch", req.Task.Branch)
	s.recordRebaseOutcome(ctx, req, true, "")
	if !s.cfg.PreserveOnFailure {
		s.cleanup(run)
	}
	return &Result{Run: run}
}

// executeReview handles a TaskTypeReview run: the QA agent reads the PR diff
// and posts an approval or change-request review via the gh CLI.
// No worktree is needed — the agent runs from the repo root.
func (s *Supervisor) executeReview(ctx context.Context, req RunRequest) *Result {
	run := req.Run
	now := time.Now()
	run.StartedAt = now
	run.Status = domain.RunStatusRunning
	run.WorktreePath = s.cfg.RepoRoot

	charmlog.Info("review starting", "run_id", run.ID, "pr", req.Task.ID, "issue", req.Task.SpecIssueNumber, "cycle", req.Task.ReviewCycle)

	// Build review prompt.
	prompt, err := s.buildReviewPrompt(ctx, req.Task)
	if err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("build review prompt: %w", err))
	}

	// Write prompt to a temp file in the repo root.
	taskFile := filepath.Join(s.cfg.RepoRoot, ".conductor-review-"+run.ID+".md")
	if err := os.WriteFile(taskFile, prompt, 0600); err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("write review prompt: %w", err))
	}
	defer os.Remove(taskFile) //nolint:errcheck

	// Open run log.
	logPath := filepath.Join(s.cfg.RepoRoot, "run.jsonl")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("open run log: %w", err))
	}
	defer logFile.Close()

	logWriter := newProviderLogWriter(logFile, run.ID)

	// Apply timeout.
	timeout := time.Duration(s.cfg.TimeoutMinutes) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := mergeEnv(req.GlobalEnv, taskEnv(req.Task))
	rc := provider.RunContext{
		RepoPath:       s.cfg.RepoRoot,
		TaskFile:       taskFile,
		Env:            env,
		LogWriter:      logWriter,
		TimeoutSeconds: int(timeout.Seconds()),
	}

	logEvent(logFile, "run_started", map[string]any{
		"run_id":  run.ID,
		"task_id": run.TaskID,
		"type":    "review",
		"pr":      req.Task.SourceURL,
	})

	handle, err := req.Provider.Run(runCtx, rc)
	if err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("launch provider: %w", err))
	}
	charmlog.Info("agent launched", "run_id", run.ID, "provider", req.Provider.Name())

	waitErr := handle.Wait()
	finished := time.Now()
	run.FinishedAt = &finished

	if runCtx.Err() == context.DeadlineExceeded {
		run.Status = domain.RunStatusTimeout
		reason := fmt.Sprintf("review timed out after %d minutes", s.cfg.TimeoutMinutes)
		logEvent(logFile, "run_timeout", map[string]any{"run_id": run.ID})
		charmlog.Warn("run timed out", "run_id", run.ID, "pr", req.Task.ID)
		s.recordReviewOutcome(ctx, req, false, reason)
		return &Result{Run: run, Err: fmt.Errorf("%s", reason)}
	}

	if waitErr != nil {
		run.Status = domain.RunStatusFailed
		run.ErrorMsg = waitErr.Error()
		logEvent(logFile, "run_failed", map[string]any{"run_id": run.ID, "error": waitErr.Error()})
		charmlog.Error("run failed", "run_id", run.ID, "pr", req.Task.ID, "error", waitErr)
		s.recordReviewOutcome(ctx, req, false, waitErr.Error())
		return &Result{Run: run, Err: waitErr}
	}

	run.Status = domain.RunStatusSucceeded
	logEvent(logFile, "run_succeeded", map[string]any{"run_id": run.ID})
	// Agent exit 0 means it approved via gh pr review --approve.
	charmlog.Info("review approved", "run_id", run.ID, "pr", req.Task.ID)
	s.recordReviewOutcome(ctx, req, true, "")
	return &Result{Run: run}
}

// executeRevise handles a TaskTypeRevise run: the implementing agent addresses
// QA feedback and pushes to the existing PR branch.
func (s *Supervisor) executeRevise(ctx context.Context, req RunRequest) *Result {
	run := req.Run
	now := time.Now()
	run.StartedAt = now
	run.Status = domain.RunStatusRunning

	worktreePath := filepath.Join(s.cfg.RepoRoot, s.cfg.WorktreeBaseDir, run.ID)
	run.WorktreePath = worktreePath

	charmlog.Info("revision starting", "run_id", run.ID, "pr", req.Task.ID, "issue", req.Task.SpecIssueNumber, "cycle", req.Task.ReviewCycle)

	// Fetch so origin/<branch> is current.
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = s.cfg.RepoRoot
	if out, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
		s.recordReviewOutcome(ctx, req, false, fetchErr.Error())
		return s.fail(run, fmt.Errorf("git fetch: %w: %s", fetchErr, out))
	}

	// Create worktree with the PR branch checked out.
	if err := s.createRebaseBranchWorktree(worktreePath, req.Task.Branch); err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("create revise worktree: %w", err))
	}

	if err := configureGitAuthor(worktreePath, req.Persona); err != nil {
		s.cleanup(run)
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("configure git author: %w", err))
	}

	// Build revision prompt.
	prompt, err := s.buildRevisionPrompt(ctx, req.Task)
	if err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("build revision prompt: %w", err))
	}
	taskFile := filepath.Join(worktreePath, ".conductor-task.md")
	if err := os.WriteFile(taskFile, prompt, 0600); err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("write task file: %w", err))
	}

	// Open run log.
	if err := os.MkdirAll(filepath.Join(worktreePath, "proof"), 0755); err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create proof dir: %w", err))
	}
	logPath := filepath.Join(worktreePath, "run.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		s.recordReviewOutcome(ctx, req, false, err.Error())
		s.cleanup(run)
		return s.fail(run, fmt.Errorf("create run log: %w", err))
	}
	defer logFile.Close()

	logWriter := newProviderLogWriter(logFile, run.ID)

	// Apply timeout.
	timeout := time.Duration(s.cfg.TimeoutMinutes) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := mergeEnv(req.GlobalEnv, taskEnv(req.Task))
	rc := provider.RunContext{
		RepoPath:       worktreePath,
		TaskFile:       taskFile,
		Env:            env,
		LogWriter:      logWriter,
		TimeoutSeconds: int(timeout.Seconds()),
	}

	logEvent(logFile, "run_started", map[string]any{
		"run_id":  run.ID,
		"task_id": run.TaskID,
		"type":    "revise",
		"branch":  req.Task.Branch,
	})

	handle, err := req.Provider.Run(runCtx, rc)
	if err != nil {
		s.cleanup(run)
		s.recordReviewOutcome(ctx, req, false, err.Error())
		return s.fail(run, fmt.Errorf("launch provider: %w", err))
	}
	charmlog.Info("agent launched", "run_id", run.ID, "provider", req.Provider.Name())

	waitErr := handle.Wait()
	finished := time.Now()
	run.FinishedAt = &finished

	if runCtx.Err() == context.DeadlineExceeded {
		run.Status = domain.RunStatusTimeout
		reason := fmt.Sprintf("revise timed out after %d minutes", s.cfg.TimeoutMinutes)
		logEvent(logFile, "run_timeout", map[string]any{"run_id": run.ID})
		charmlog.Warn("run timed out", "run_id", run.ID, "pr", req.Task.ID)
		s.recordReviewOutcome(ctx, req, false, reason)
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: fmt.Errorf("%s", reason)}
	}

	if waitErr != nil {
		run.Status = domain.RunStatusFailed
		run.ErrorMsg = waitErr.Error()
		logEvent(logFile, "run_failed", map[string]any{"run_id": run.ID, "error": waitErr.Error()})
		charmlog.Error("run failed", "run_id", run.ID, "pr", req.Task.ID, "error", waitErr)
		s.recordReviewOutcome(ctx, req, false, "revision agent failed: "+waitErr.Error())
		if !s.cfg.PreserveOnFailure {
			s.cleanup(run)
		}
		return &Result{Run: run, Err: waitErr}
	}

	run.Status = domain.RunStatusSucceeded
	logEvent(logFile, "run_succeeded", map[string]any{"run_id": run.ID})
	charmlog.Info("revision pushed", "run_id", run.ID, "pr", req.Task.ID, "branch", req.Task.Branch)

	// On success, re-trigger QA review.
	if req.ReviewSource != nil {
		prNum, err := strconv.Atoi(req.Task.ID)
		if err == nil {
			req.ReviewSource.MarkPRNeedsReview(ctx, prNum, req.Task.SpecIssueNumber) //nolint:errcheck
		}
	}

	if !s.cfg.PreserveOnFailure {
		s.cleanup(run)
	}
	return &Result{Run: run}
}

func (s *Supervisor) recordReviewOutcome(ctx context.Context, req RunRequest, approved bool, reason string) {
	if req.ReviewSource == nil {
		return
	}
	req.ReviewSource.RecordReviewOutcome(ctx, req.Task, approved, reason) //nolint:errcheck
}

// buildReviewPrompt builds the review task prompt for the QA agent.
func (s *Supervisor) buildReviewPrompt(ctx context.Context, task *domain.Task) ([]byte, error) {
	var b strings.Builder

	// Load QA persona AGENTS.md if present.
	agentsPath := filepath.Join(s.cfg.RepoRoot, ".conductor", "personas", "qa-engineer", "AGENTS.md")
	if data, err := os.ReadFile(agentsPath); err == nil {
		b.WriteString(string(data))
		b.WriteString("\n\n---\n\n")
	}

	prNum := task.ID
	cycle := task.ReviewCycle

	fmt.Fprintf(&b, "# Task: Review PR #%s — cycle %d of 3\n\n", prNum, cycle)
	fmt.Fprintf(&b, "**PR:** %s\n", task.SourceURL)
	fmt.Fprintf(&b, "**Branch:** %s\n", task.Branch)
	fmt.Fprintf(&b, "**Original issue:** #%d\n\n", task.SpecIssueNumber)

	// Fetch original issue body.
	issueBody := s.fetchIssueBody(ctx, task)
	b.WriteString("## Original Spec\n\n")
	b.WriteString(issueBody)
	b.WriteString("\n\n")

	b.WriteString("## Instructions\n\n")
	fmt.Fprintf(&b, "1. Run `gh pr diff %s` to read the full implementation diff\n", prNum)
	b.WriteString("2. Verify that every requirement in the spec above is satisfied by the implementation\n")
	b.WriteString("3. Check for obvious bugs or spec gaps — do NOT flag style issues or unsolicited improvements\n")
	b.WriteString("4. If the implementation is complete and correct:\n")
	fmt.Fprintf(&b, "   `gh pr review %s --approve --body \"<brief approval summary>\"`\n", prNum)
	b.WriteString("5. If there are genuine gaps or bugs:\n")
	fmt.Fprintf(&b, "   `gh pr review %s --request-changes --body \"<specific, actionable list of what is missing or wrong>\"`\n\n", prNum)
	b.WriteString("Exit 0 after posting your review. Do not make any code changes.\n")

	return []byte(b.String()), nil
}

// buildRevisionPrompt builds the revision task prompt for the implementing agent.
func (s *Supervisor) buildRevisionPrompt(ctx context.Context, task *domain.Task) ([]byte, error) {
	var b strings.Builder

	// Load default workflow or persona AGENTS.md.
	workflow := s.loadWorkflow(nil)
	if workflow != "" {
		b.WriteString(workflow)
		b.WriteString("\n\n---\n\n")
	}

	prNum := task.ID
	cycle := task.ReviewCycle

	fmt.Fprintf(&b, "# Task: Address QA feedback on PR #%s — cycle %d of 3\n\n", prNum, cycle)
	fmt.Fprintf(&b, "**PR:** %s\n", task.SourceURL)
	fmt.Fprintf(&b, "**Branch:** %s\n\n", task.Branch)

	// Fetch original issue body.
	issueBody := s.fetchIssueBody(ctx, task)
	fmt.Fprintf(&b, "## Original Spec (Issue #%d)\n\n", task.SpecIssueNumber)
	b.WriteString(issueBody)
	b.WriteString("\n\n")

	// Fetch QA feedback from PR comments.
	prComments := s.fetchPRComments(ctx, task)
	b.WriteString("## QA Feedback\n\n")
	b.WriteString(prComments)
	b.WriteString("\n\n")

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. Read the QA feedback carefully\n")
	b.WriteString("2. Make all necessary changes to address every point raised\n")
	fmt.Fprintf(&b, "3. `git add -A && git commit -m \"address QA feedback (cycle %d)\"`\n", cycle)
	fmt.Fprintf(&b, "4. `git push origin %s`\n\n", task.Branch)
	b.WriteString("Do not open a new PR — push to the existing branch. Do not address anything not mentioned in the QA feedback.\n")

	return []byte(b.String()), nil
}

// fetchIssueBody fetches the body of the original spec issue via the gh CLI.
// Returns a placeholder string on error rather than failing the prompt build.
func (s *Supervisor) fetchIssueBody(ctx context.Context, task *domain.Task) string {
	if task.SpecIssueNumber == 0 {
		return "(original issue body unavailable)"
	}
	repoSlug := extractRepoSlug(task.SourceURL)
	if repoSlug == "" {
		return "(original issue body unavailable)"
	}
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/issues/%d", repoSlug, task.SpecIssueNumber),
		"--jq", ".body",
	).Output()
	if err != nil {
		return "(could not fetch issue body)"
	}
	return strings.TrimSpace(string(out))
}

// fetchPRComments fetches review comments from the PR via the gh CLI.
func (s *Supervisor) fetchPRComments(ctx context.Context, task *domain.Task) string {
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", task.ID, "--comments").Output()
	if err != nil {
		return "(could not fetch PR comments)"
	}
	return strings.TrimSpace(string(out))
}

// extractRepoSlug extracts "owner/repo" from a GitHub PR or issue URL.
// e.g. "https://github.com/org/repo/pull/7" -> "org/repo"
func extractRepoSlug(sourceURL string) string {
	// Strip scheme and host.
	const prefix = "https://github.com/"
	if !strings.HasPrefix(sourceURL, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(sourceURL, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func (s *Supervisor) recordRebaseOutcome(ctx context.Context, req RunRequest, succeeded bool, reason string) {
	if req.Source == nil {
		return
	}
	req.Source.RecordRebaseOutcome(ctx, req.Task, succeeded, reason) //nolint:errcheck
}

func (s *Supervisor) createRebaseBranchWorktree(path, branch string) error {
	// Remove stale worktree metadata.
	exec.Command("git", "-C", s.cfg.RepoRoot, "worktree", "prune").Run() //nolint:errcheck

	// If another worktree has this branch checked out, remove it first.
	listOut, err := exec.Command("git", "-C", s.cfg.RepoRoot, "worktree", "list", "--porcelain").Output()
	if err == nil {
		if stale := findWorktreeForBranch(string(listOut), branch); stale != "" && stale != path {
			exec.Command("git", "-C", s.cfg.RepoRoot, "worktree", "remove", "--force", stale).Run() //nolint:errcheck
		}
	}

	cmd := exec.Command("git", "worktree", "add", "-B", branch, path, "origin/"+branch)
	cmd.Dir = s.cfg.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// findWorktreeForBranch parses `git worktree list --porcelain` output and
// returns the worktree path that has the given branch checked out, or "".
func findWorktreeForBranch(porcelain, branch string) string {
	var current string
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			current = strings.TrimPrefix(line, "worktree ")
		}
		if line == "branch refs/heads/"+branch {
			return current
		}
	}
	return ""
}

// loadRebaseWorkflow reads .conductor/REBASE_WORKFLOW.md from the repo root.
// Returns empty string if the file is absent.
func (s *Supervisor) loadRebaseWorkflow() string {
	path := filepath.Join(s.cfg.RepoRoot, ".conductor", "REBASE_WORKFLOW.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// buildRebasePrompt formats a rebase task into a prompt for the agent.
func buildRebasePrompt(task *domain.Task, workflow string) []byte {
	var b strings.Builder
	if workflow != "" {
		b.WriteString(workflow)
		b.WriteString("\n\n---\n\n")
	}
	fmt.Fprintf(&b, "# Task: Rebase `%s` onto `%s`\n\n", task.Branch, task.BaseBranch)
	fmt.Fprintf(&b, "**PR:** %s\n", task.SourceURL)
	fmt.Fprintf(&b, "**Branch:** %s\n", task.Branch)
	fmt.Fprintf(&b, "**Base:** %s\n", task.BaseBranch)
	fmt.Fprintf(&b, "**Attempt:** %d of 3\n\n", task.Attempts+1)
	b.WriteString("## Instructions\n\n")
	b.WriteString("1. Run `git fetch origin`\n")
	fmt.Fprintf(&b, "2. Run `git rebase origin/%s`\n", task.BaseBranch)
	b.WriteString("3. If there are merge conflicts, resolve them carefully — preserve the intent of both sides\n")
	fmt.Fprintf(&b, "4. Run `git push --force-with-lease origin %s`\n", task.Branch)
	fmt.Fprintf(&b, "5. Do NOT open a new PR — one already exists at %s\n\n", task.SourceURL)
	b.WriteString("If the rebase cannot be completed cleanly after your best effort, exit with a non-zero status and print a brief explanation of what conflicted.\n")
	return []byte(b.String())
}
