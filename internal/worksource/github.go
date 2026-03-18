package worksource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/haha-systems/conductor/internal/domain"
)

const (
	claimedLabel        = "conductor:claimed"
	runningLabel        = "conductor:running"
	rebasingLabel       = "conductor:rebasing"
	rebaseAbandonedLabel = "conductor:rebase-abandoned"
	rebaseAttemptsPrefix = "conductor:rebase-attempts-"
)

// GitHubSource polls a GitHub repository for issues and claims them via labels.
type GitHubSource struct {
	client      *gh.Client
	owner       string
	repo        string
	labelFilter []string
}

// NewGitHubSource creates a GitHubSource authenticated with the given token.
// repo should be in "owner/repo" format.
func NewGitHubSource(token, repo string, labelFilter []string) (*GitHubSource, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("repo must be in owner/repo format, got %q", repo)
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)

	return &GitHubSource{
		client:      gh.NewClient(tc),
		owner:       parts[0],
		repo:        parts[1],
		labelFilter: labelFilter,
	}, nil
}

func (s *GitHubSource) Name() string { return "github" }

// Poll fetches open issues that have all required labels but have NOT been claimed yet.
func (s *GitHubSource) Poll(ctx context.Context) ([]*domain.Task, error) {
	opts := &gh.IssueListByRepoOptions{
		State: "open",
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}

	issues, _, err := s.client.Issues.ListByRepo(ctx, s.owner, s.repo, opts)
	if err != nil {
		return nil, fmt.Errorf("github poll: %w", err)
	}

	var tasks []*domain.Task
	for _, issue := range issues {
		if !s.matchesFilter(issue) {
			continue
		}
		if hasLabel(issue, claimedLabel) || hasLabel(issue, runningLabel) {
			continue
		}
		tasks = append(tasks, issueToTask(issue, s.owner+"/"+s.repo))
	}
	return tasks, nil
}

// Claim adds the conductor:claimed label to the issue atomically, or
// conductor:rebasing to the PR for rebase tasks.
func (s *GitHubSource) Claim(ctx context.Context, task *domain.Task) error {
	num, err := strconv.Atoi(task.ID)
	if err != nil {
		return fmt.Errorf("invalid github id %q: %w", task.ID, err)
	}

	if task.Type == domain.TaskTypeRebase {
		_, _, err = s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, num, []string{rebasingLabel})
		if err != nil {
			return fmt.Errorf("claim rebase PR #%d: %w", num, err)
		}
		return nil
	}

	_, _, err = s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, num, []string{claimedLabel})
	if err != nil {
		return fmt.Errorf("claim issue #%d: %w", num, err)
	}
	return nil
}

// PostResult adds a comment to the issue with the proof summary.
func (s *GitHubSource) PostResult(ctx context.Context, task *domain.Task, summary string) error {
	issueNum, err := strconv.Atoi(task.ID)
	if err != nil {
		return fmt.Errorf("invalid github issue id %q: %w", task.ID, err)
	}

	comment := &gh.IssueComment{
		Body: gh.Ptr("## Conductor Run Complete\n\n" + summary),
	}
	_, _, err = s.client.Issues.CreateComment(ctx, s.owner, s.repo, issueNum, comment)
	return err
}

// matchesFilter returns true if the issue has all labels in the filter list.
func (s *GitHubSource) matchesFilter(issue *gh.Issue) bool {
	for _, required := range s.labelFilter {
		if !hasLabel(issue, required) {
			return false
		}
	}
	return true
}

func hasLabel(issue *gh.Issue, name string) bool {
	for _, l := range issue.Labels {
		if l.GetName() == name {
			return true
		}
	}
	return false
}

func issueToTask(issue *gh.Issue, repo string) *domain.Task {
	desc := issue.GetBody()
	cfg, body := domain.ParseFrontMatter(desc)

	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, l.GetName())
	}

	return &domain.Task{
		ID:          strconv.Itoa(issue.GetNumber()),
		Title:       issue.GetTitle(),
		Description: body,
		Labels:      labels,
		Config:      cfg,
		Status:      domain.TaskStatusPending,
		Source:      "github",
		SourceURL:   issue.GetHTMLURL(),
		CreatedAt:   issue.GetCreatedAt().Time,
		UpdatedAt:   issue.GetUpdatedAt().Time,
	}
}

// ListOpenPRs returns open PRs that are behind their base branch and eligible
// for an automated rebase. PRs are skipped if they are already claimed
// (conductor:rebasing), abandoned (conductor:rebase-abandoned), or have
// exhausted all attempts (conductor:rebase-attempts-3).
func (s *GitHubSource) ListOpenPRs(ctx context.Context) ([]*domain.Task, error) {
	opts := &gh.PullRequestListOptions{
		State: "open",
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}

	prs, _, err := s.client.PullRequests.List(ctx, s.owner, s.repo, opts)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	var tasks []*domain.Task
	for _, pr := range prs {
		if prHasLabel(pr, rebasingLabel) ||
			prHasLabel(pr, rebaseAbandonedLabel) ||
			prHasLabel(pr, rebaseAttemptsPrefix+"3") {
			continue
		}

		// Check if the PR is behind its base.
		comparison, _, err := s.client.Repositories.CompareCommits(
			ctx, s.owner, s.repo,
			pr.GetBase().GetRef(), // branch name resolves to current HEAD at query time
			pr.GetHead().GetSHA(),
			nil,
		)
		if err != nil {
			// Log and skip this PR rather than failing the whole list.
			continue
		}
		if comparison.GetBehindBy() <= 0 {
			continue
		}

		attempts := rebaseAttempts(pr)
		tasks = append(tasks, prToRebaseTask(pr, s.owner+"/"+s.repo, attempts))
	}
	return tasks, nil
}

// RecordRebaseOutcome updates labels and optionally posts a comment after a
// rebase attempt. On success, removes conductor:rebasing. On failure,
// increments the attempt counter; after 3 failures adds conductor:rebase-abandoned
// and posts an abandonment comment.
func (s *GitHubSource) RecordRebaseOutcome(ctx context.Context, task *domain.Task, succeeded bool, reason string) error {
	prNum, err := strconv.Atoi(task.ID)
	if err != nil {
		return fmt.Errorf("invalid PR id %q: %w", task.ID, err)
	}

	// Always remove conductor:rebasing.
	_, err = s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, prNum, rebasingLabel)
	if err != nil {
		// Ignore 404 — label may not be present.
		if !isNotFound(err) {
			return fmt.Errorf("remove rebasing label: %w", err)
		}
	}

	if succeeded {
		return nil
	}

	newAttempts := task.Attempts + 1

	// Remove old attempts label if present, then add the new one.
	if task.Attempts > 0 {
		old := rebaseAttemptsPrefix + strconv.Itoa(task.Attempts)
		_, _ = s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, prNum, old) //nolint:errcheck
	}
	_, _, err = s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, prNum, []string{rebaseAttemptsPrefix + strconv.Itoa(newAttempts)})
	if err != nil {
		return fmt.Errorf("add rebase-attempts label: %w", err)
	}

	if newAttempts >= 3 {
		_, _, err = s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, prNum, []string{rebaseAbandonedLabel})
		if err != nil {
			return fmt.Errorf("add rebase-abandoned label: %w", err)
		}

		body := fmt.Sprintf("## Conductor: Rebase Abandoned\n\nAfter 3 attempts, the conductor was unable to rebase this branch onto `%s`.\n\n**Last error:** %s\n\nPlease rebase manually and force-push, or close and re-open the PR.",
			task.BaseBranch, reason)
		comment := &gh.IssueComment{Body: gh.Ptr(body)}
		_, _, err = s.client.Issues.CreateComment(ctx, s.owner, s.repo, prNum, comment)
		if err != nil {
			return fmt.Errorf("post abandonment comment: %w", err)
		}
	}

	return nil
}

// prHasLabel returns true if the PR has the named label.
func prHasLabel(pr *gh.PullRequest, name string) bool {
	for _, l := range pr.Labels {
		if l.GetName() == name {
			return true
		}
	}
	return false
}

// rebaseAttempts returns the current attempt count encoded in PR labels.
func rebaseAttempts(pr *gh.PullRequest) int {
	for _, l := range pr.Labels {
		name := l.GetName()
		if strings.HasPrefix(name, rebaseAttemptsPrefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(name, rebaseAttemptsPrefix))
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// prToRebaseTask converts a GitHub PR to a rebase domain.Task.
func prToRebaseTask(pr *gh.PullRequest, repo string, attempts int) *domain.Task {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.GetName())
	}
	return &domain.Task{
		ID:         strconv.Itoa(pr.GetNumber()),
		Title:      fmt.Sprintf("Rebase #%d: %s", pr.GetNumber(), pr.GetTitle()),
		Labels:     labels,
		Status:     domain.TaskStatusPending,
		Source:     "github",
		SourceURL:  pr.GetHTMLURL(),
		Type:       domain.TaskTypeRebase,
		Branch:     pr.GetHead().GetRef(),
		BaseBranch: pr.GetBase().GetRef(),
		Attempts:   attempts,
	}
}

// isNotFound returns true if the GitHub API error is a 404.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var errResp *gh.ErrorResponse
	if errors.As(err, &errResp) {
		return errResp.Response.StatusCode == http.StatusNotFound
	}
	return strings.Contains(err.Error(), "404")
}

