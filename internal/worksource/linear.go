package worksource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/haha-systems/conductor/internal/domain"
)

const linearAPIURL = "https://api.linear.app/graphql"

// LinearSource polls a Linear team for issues and claims them by updating state.
type LinearSource struct {
	client       *http.Client
	token        string
	teamID       string
	stateFilter  []string
	claimedState string // Linear state name to set when claiming (e.g. "In Progress")
}

// NewLinearSource creates a LinearSource authenticated with the given API key.
func NewLinearSource(token, teamID string, stateFilter []string) (*LinearSource, error) {
	if token == "" {
		return nil, fmt.Errorf("linear: API token required")
	}
	if teamID == "" {
		return nil, fmt.Errorf("linear: team_id required")
	}
	claimedState := "In Progress"
	if len(stateFilter) > 0 {
		claimedState = stateFilter[0]
	}
	return &LinearSource{
		client:       &http.Client{Timeout: 15 * time.Second},
		token:        token,
		teamID:       teamID,
		stateFilter:  stateFilter,
		claimedState: claimedState,
	}, nil
}

func (s *LinearSource) Name() string { return "linear" }

// Poll fetches issues matching the state filter that don't have the claimed label.
func (s *LinearSource) Poll(ctx context.Context) ([]*domain.Task, error) {
	query := `
query($teamId: String!, $states: [String!]!) {
  issues(filter: {
    team: { id: { eq: $teamId } }
    state: { name: { in: $states } }
    labels: { name: { nin: ["conductor:claimed", "conductor:running"] } }
  }) {
    nodes {
      id
      title
      description
      url
      createdAt
      updatedAt
      labels { nodes { name } }
    }
  }
}`

	vars := map[string]any{
		"teamId": s.teamID,
		"states": s.stateFilter,
	}

	var result struct {
		Data struct {
			Issues struct {
				Nodes []linearIssue `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []linearError `json:"errors"`
	}

	if err := s.gql(ctx, query, vars, &result); err != nil {
		return nil, err
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("linear poll: %s", result.Errors[0].Message)
	}

	tasks := make([]*domain.Task, 0, len(result.Data.Issues.Nodes))
	for _, issue := range result.Data.Issues.Nodes {
		tasks = append(tasks, linearIssueToTask(issue))
	}
	return tasks, nil
}

// Claim adds the "conductor:claimed" label to the issue.
func (s *LinearSource) Claim(ctx context.Context, task *domain.Task) error {
	// First, ensure the label exists and get its ID.
	labelID, err := s.ensureLabel(ctx, "conductor:claimed", "#e74c3c")
	if err != nil {
		return fmt.Errorf("claim %s: ensure label: %w", task.ID, err)
	}

	mutation := `
mutation($issueId: String!, $labelIds: [String!]!) {
  issueAddLabel(id: $issueId, labelId: $labelIds[0]) {
    success
  }
}`
	// Linear's issueAddLabel takes a single label — call it once.
	mutation = `
mutation($issueId: String!, $labelId: String!) {
  issueAddLabel(id: $issueId, labelId: $labelId) {
    success
  }
}`

	vars := map[string]any{
		"issueId": task.ID,
		"labelId": labelID,
	}

	var result struct {
		Data   map[string]any `json:"data"`
		Errors []linearError  `json:"errors"`
	}

	if err := s.gql(ctx, mutation, vars, &result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("linear claim: %s", result.Errors[0].Message)
	}
	return nil
}

// ListOpenPRs returns nil for Linear — PR rebase detection is not supported.
func (s *LinearSource) ListOpenPRs(_ context.Context) ([]*domain.Task, error) {
	return nil, nil
}

// RecordRebaseOutcome is a no-op for Linear — PR rebase detection is not supported.
func (s *LinearSource) RecordRebaseOutcome(_ context.Context, _ *domain.Task, _ bool, _ string) error {
	return nil
}

// PostResult adds a comment to the Linear issue.
func (s *LinearSource) PostResult(ctx context.Context, task *domain.Task, summary string) error {
	mutation := `
mutation($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
  }
}`

	vars := map[string]any{
		"issueId": task.ID,
		"body":    "## Conductor Run Complete\n\n" + summary,
	}

	var result struct {
		Data   map[string]any `json:"data"`
		Errors []linearError  `json:"errors"`
	}

	if err := s.gql(ctx, mutation, vars, &result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("linear post result: %s", result.Errors[0].Message)
	}
	return nil
}

// ensureLabel returns the ID of the given label, creating it if it doesn't exist.
func (s *LinearSource) ensureLabel(ctx context.Context, name, color string) (string, error) {
	query := `
query($teamId: String!, $name: String!) {
  issueLabels(filter: { team: { id: { eq: $teamId } }, name: { eq: $name } }) {
    nodes { id }
  }
}`
	var result struct {
		Data struct {
			IssueLabels struct {
				Nodes []struct{ ID string `json:"id"` } `json:"nodes"`
			} `json:"issueLabels"`
		} `json:"data"`
		Errors []linearError `json:"errors"`
	}
	if err := s.gql(ctx, query, map[string]any{"teamId": s.teamID, "name": name}, &result); err != nil {
		return "", err
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("%s", result.Errors[0].Message)
	}
	if len(result.Data.IssueLabels.Nodes) > 0 {
		return result.Data.IssueLabels.Nodes[0].ID, nil
	}

	// Create it.
	createMutation := `
mutation($teamId: String!, $name: String!, $color: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name, color: $color }) {
    issueLabel { id }
  }
}`
	var createResult struct {
		Data struct {
			IssueLabelCreate struct {
				IssueLabel struct{ ID string `json:"id"` } `json:"issueLabel"`
			} `json:"issueLabelCreate"`
		} `json:"data"`
		Errors []linearError `json:"errors"`
	}
	if err := s.gql(ctx, createMutation, map[string]any{
		"teamId": s.teamID,
		"name":   name,
		"color":  color,
	}, &createResult); err != nil {
		return "", err
	}
	if len(createResult.Errors) > 0 {
		return "", fmt.Errorf("%s", createResult.Errors[0].Message)
	}
	return createResult.Data.IssueLabelCreate.IssueLabel.ID, nil
}

func (s *LinearSource) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearAPIURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("linear API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear API: status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// linearIssue is the GraphQL response shape for an issue.
type linearIssue struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	URL         string         `json:"url"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	Labels      struct {
		Nodes []struct{ Name string `json:"name"` } `json:"nodes"`
	} `json:"labels"`
}

type linearError struct {
	Message string `json:"message"`
}

func linearIssueToTask(issue linearIssue) *domain.Task {
	desc := issue.Description
	cfg, body := domain.ParseFrontMatter(desc)

	labels := make([]string, 0, len(issue.Labels.Nodes))
	for _, l := range issue.Labels.Nodes {
		labels = append(labels, l.Name)
	}

	return &domain.Task{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: body,
		Labels:      labels,
		Config:      cfg,
		Status:      domain.TaskStatusPending,
		Source:      "linear",
		SourceURL:   issue.URL,
		CreatedAt:   issue.CreatedAt,
		UpdatedAt:   issue.UpdatedAt,
	}
}
