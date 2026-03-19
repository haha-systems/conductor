package proof

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	charmlog "github.com/charmbracelet/log"

	"github.com/haha-systems/conductor/internal/domain"
)

// CostEstimator is implemented by provider adapters that can estimate run cost.
type CostEstimator interface {
	CostEstimate(promptLen int) (float64, bool)
}

// ReviewNotifier is implemented by work sources that can mark a PR as needing review.
type ReviewNotifier interface {
	MarkPRNeedsReview(ctx context.Context, prNumber int, issueNumber int) error
}

// Config controls what proof collection does.
type Config struct {
	RequireCIPass bool
	PRBaseBranch  string
	// CICommand is the command to run tests (default: inferred from repo).
	CICommand []string
}

// Collector gathers proof-of-work artifacts after a successful run.
type Collector struct {
	cfg Config
}

func New(cfg Config) *Collector {
	return &Collector{cfg: cfg}
}

// Collect runs CI, computes diff stats, optionally opens a PR, and writes
// proof/summary.json into the run's worktree. It returns the ProofBundle.
// provider may be nil; if non-nil and able to estimate cost, CostUSD is populated.
// notifier may be nil; if non-nil and the bundle has a PRUrl, MarkPRNeedsReview is called.
func (c *Collector) Collect(ctx context.Context, run *domain.Run, task *domain.Task, provider CostEstimator, notifier ReviewNotifier) (*domain.ProofBundle, error) {
	charmlog.Info("proof collecting", "run_id", run.ID, "task_id", run.TaskID)
	started := time.Now()

	bundle := &domain.ProofBundle{
		RunID:    run.ID,
		TaskID:   run.TaskID,
		Provider: run.Provider,
	}

	// 1. Run CI.
	ci, err := c.runCI(ctx, run.WorktreePath)
	if err != nil && c.cfg.RequireCIPass {
		return nil, fmt.Errorf("CI failed: %w", err)
	}
	bundle.CI = ci

	// 2. Diff stats.
	diff, err := c.diffStats(run.WorktreePath, c.cfg.PRBaseBranch)
	if err != nil {
		// Non-fatal: best-effort diff.
		diff = domain.DiffStat{}
	}
	bundle.Diff = diff

	// 3. Duration.
	if run.FinishedAt != nil {
		bundle.DurationSeconds = run.FinishedAt.Sub(run.StartedAt).Seconds()
	} else {
		bundle.DurationSeconds = time.Since(started).Seconds()
	}

	// 4. Cost estimate from provider (best-effort; zero if not available).
	if provider != nil {
		if promptData, err := os.ReadFile(filepath.Join(run.WorktreePath, ".conductor-task.md")); err == nil {
			if cost, ok := provider.CostEstimate(len(promptData)); ok {
				bundle.CostUSD = cost
			}
		}
	}

	// 5. Merge agent-written metadata (pr_url, walkthrough, etc.).
	readAgentMetadata(run.WorktreePath, bundle)

	// 5a. If a PR was opened, mark it as needing QA review.
	if bundle.PRUrl != "" && notifier != nil {
		prNum := parsePRNumber(bundle.PRUrl)
		issueNum, _ := strconv.Atoi(task.ID)
		if prNum > 0 && issueNum > 0 {
			notifier.MarkPRNeedsReview(ctx, prNum, issueNum) //nolint:errcheck
		}
	}

	// 6. Write summary.json.
	summaryPath := filepath.Join(run.WorktreePath, "proof", "summary.json")
	if err := writeSummary(summaryPath, bundle); err != nil {
		return nil, fmt.Errorf("write summary: %w", err)
	}

	diffLines := fmt.Sprintf("+%d/-%d", bundle.Diff.Insertions, bundle.Diff.Deletions)
	charmlog.Info("proof collected",
		"run_id", run.ID,
		"pr", bundle.PRUrl,
		"diff_lines", diffLines,
		"cost", fmt.Sprintf("$%.4f", bundle.CostUSD),
	)

	return bundle, nil
}

// readAgentMetadata merges fields from .conductor/metadata.json (written by the
// agent) into bundle. Unknown fields are ignored; missing file is not an error.
func readAgentMetadata(worktreePath string, bundle *domain.ProofBundle) {
	path := filepath.Join(worktreePath, ".conductor", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var meta struct {
		PRUrl       string `json:"pr_url"`
		Walkthrough string `json:"walkthrough"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return
	}
	if meta.PRUrl != "" {
		bundle.PRUrl = meta.PRUrl
	}
	if meta.Walkthrough != "" {
		bundle.Walkthrough = meta.Walkthrough
	}
}

// runCI executes the test suite in the worktree directory.
func (c *Collector) runCI(ctx context.Context, dir string) (domain.CIResult, error) {
	ciCmd := c.cfg.CICommand
	if len(ciCmd) == 0 {
		ciCmd = inferCICommand(dir)
	}
	if len(ciCmd) == 0 {
		return domain.CIResult{Passed: true}, nil
	}

	cmd := exec.CommandContext(ctx, ciCmd[0], ciCmd[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	result := domain.CIResult{Passed: err == nil}
	// parseTestOutput understands go test -v format only; skip for other runners
	// to avoid reporting misleading zero counts.
	if ciCmd[0] == "go" {
		result.TestCount, result.Failures = parseTestOutput(out.String())
	}
	return result, err
}

// diffStats returns line-count statistics comparing the worktree HEAD to baseBranch.
func (c *Collector) diffStats(dir, baseBranch string) (domain.DiffStat, error) {
	// Find the common ancestor explicitly — more reliable in detached worktrees
	// than the three-dot syntax.
	baseRef := "origin/" + baseBranch
	mbCmd := exec.Command("git", "merge-base", "HEAD", baseRef)
	mbCmd.Dir = dir
	mergeBase, err := mbCmd.Output()
	if err != nil {
		// Fall back to the local branch name.
		mbCmd2 := exec.Command("git", "merge-base", "HEAD", baseBranch)
		mbCmd2.Dir = dir
		mergeBase, err = mbCmd2.Output()
		if err != nil {
			return domain.DiffStat{}, fmt.Errorf("git merge-base: %w", err)
		}
	}
	base := strings.TrimSpace(string(mergeBase))

	cmd := exec.Command("git", "diff", "--shortstat", base, "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return domain.DiffStat{}, fmt.Errorf("git diff: %w", err)
	}
	return parseShortstat(string(out)), nil
}

// inferCICommand looks for common CI entry points in the repo.
func inferCICommand(dir string) []string {
	if fileExists(filepath.Join(dir, "go.mod")) {
		return []string{"go", "test", "./..."}
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		return []string{"npm", "test", "--", "--passWithNoTests"}
	}
	if fileExists(filepath.Join(dir, "Makefile")) {
		return []string{"make", "test"}
	}
	return nil
}

// parseShortstat parses output like:
//
//	3 files changed, 87 insertions(+), 12 deletions(-)
func parseShortstat(s string) domain.DiffStat {
	var stat domain.DiffStat
	s = strings.TrimSpace(s)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		fields := strings.Fields(part)
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.Atoi(fields[0])
		switch {
		case strings.Contains(fields[1], "file"):
			stat.FilesChanged = n
		case strings.Contains(part, "insertion"):
			stat.Insertions = n
		case strings.Contains(part, "deletion"):
			stat.Deletions = n
		}
	}
	return stat
}

// parseTestOutput extracts test count and failure count from go test -v output.
// This is best-effort; returns 0,0 if it can't parse.
func parseTestOutput(output string) (total, failures int) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- PASS") || strings.HasPrefix(line, "--- FAIL") {
			total++
		}
		if strings.HasPrefix(line, "--- FAIL") {
			failures++
		}
	}
	return
}

func writeSummary(path string, bundle *domain.ProofBundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create proof dir: %w", err)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parsePRNumber extracts the PR number from a GitHub PR URL.
// e.g. "https://github.com/org/repo/pull/42" -> 42
func parsePRNumber(prURL string) int {
	parts := strings.Split(prURL, "/pull/")
	if len(parts) != 2 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimRight(parts[1], "/"))
	if err != nil {
		return 0
	}
	return n
}
