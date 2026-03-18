package domain

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusClaimed   TaskStatus = "claimed"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusReview    TaskStatus = "review"
	TaskStatusLanded    TaskStatus = "landed"
)

// TaskType distinguishes the kind of work a task represents.
type TaskType string

const (
	// TaskTypeIssue is the default — a task sourced from an issue/ticket.
	TaskTypeIssue TaskType = "issue"
	// TaskTypeRebase is a task to rebase a PR branch onto its base branch.
	TaskTypeRebase TaskType = "rebase"
)

// TaskConfig holds the optional conductor: front-matter parsed from a task description.
type TaskConfig struct {
	Agent          string            `yaml:"agent"`
	Persona        string            `yaml:"persona"`
	Routing        string            `yaml:"routing"`
	TimeoutMinutes int               `yaml:"timeout_minutes"`
	Env            map[string]string `yaml:"env"`
}

// Task is a unit of work sourced from an external board.
type Task struct {
	ID          string
	Title       string
	Description string // raw description (may include front-matter)
	Labels      []string
	Config      *TaskConfig // nil if no front-matter present
	Status      TaskStatus
	Source      string // e.g. "github", "linear"
	SourceURL   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Type identifies the kind of task. Defaults to TaskTypeIssue for backward compat.
	Type TaskType
	// Branch is the PR head branch for rebase tasks (e.g. "ci/github-actions-workflows").
	Branch string
	// BaseBranch is the branch to rebase onto for rebase tasks (e.g. "main").
	BaseBranch string
	// Attempts is the number of rebase attempts already recorded by the WorkSource.
	Attempts int
}

// frontMatterMarker is the YAML front-matter delimiter.
const frontMatterMarker = "---"

// ParseFrontMatter extracts a conductor: YAML block from a task description.
// It returns nil config and the original body if no front-matter is present.
func ParseFrontMatter(description string) (*TaskConfig, string) {
	description = strings.TrimSpace(description)
	if !strings.HasPrefix(description, frontMatterMarker) {
		return nil, description
	}

	// Find closing ---
	rest := description[len(frontMatterMarker):]
	idx := strings.Index(rest, "\n"+frontMatterMarker)
	if idx < 0 {
		return nil, description
	}

	yamlBlock := strings.TrimSpace(rest[:idx])
	body := strings.TrimSpace(rest[idx+len("\n"+frontMatterMarker):])

	var wrapper struct {
		Conductor TaskConfig `yaml:"conductor"`
	}
	if err := yaml.Unmarshal([]byte(yamlBlock), &wrapper); err != nil {
		return nil, description
	}

	return &wrapper.Conductor, body
}
