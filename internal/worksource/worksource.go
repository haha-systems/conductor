package worksource

import (
	"context"

	"github.com/haha-systems/conductor/internal/domain"
)

// WorkSource is the interface all work source implementations must satisfy.
type WorkSource interface {
	// Name returns the source identifier (e.g. "github", "linear").
	Name() string
	// Poll returns pending tasks from this source.
	Poll(ctx context.Context) ([]*domain.Task, error)
	// Claim atomically marks a task as claimed in the source system.
	// Returns an error if the task was already claimed by another process.
	Claim(ctx context.Context, task *domain.Task) error
	// PostResult posts a proof summary comment back to the originating task/issue.
	PostResult(ctx context.Context, task *domain.Task, summary string) error
	// ListOpenPRs returns open PRs that are behind their base branch and
	// eligible for an automated rebase (not already claimed or abandoned).
	ListOpenPRs(ctx context.Context) ([]*domain.Task, error)
	// RecordRebaseOutcome updates the source system after a rebase attempt.
	// On success, removes the in-progress marker. On failure, increments the
	// attempt counter and, after 3 failures, marks the PR as abandoned and
	// posts a comment explaining why.
	RecordRebaseOutcome(ctx context.Context, task *domain.Task, succeeded bool, reason string) error
}
