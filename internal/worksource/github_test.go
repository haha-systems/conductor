package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/haha-systems/conductor/internal/domain"
)

// newTestGitHubSource creates a GitHubSource pointed at a test HTTP server.
// The mux should NOT register a catch-all handler — this function adds one
// that returns a proper JSON 404 so that go-github can parse it as ErrorResponse.
func newTestGitHubSource(t *testing.T, mux *http.ServeMux) *GitHubSource {
	t.Helper()

	// Catch-all: return a JSON 404 for any unregistered path so go-github can
	// parse it as a *gh.ErrorResponse (the default HTML 404 breaks errors.As).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found","documentation_url":""}`)) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})
	tc := oauth2.NewClient(context.Background(), ts)
	client := gh.NewClient(tc)
	// Point the client at our test server.
	baseURL := srv.URL + "/"
	client.BaseURL, _ = client.BaseURL.Parse(baseURL)

	return &GitHubSource{
		client: client,
		owner:  "org",
		repo:   "repo",
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// --- helper PR builder ---

func makePR(num int, head, base string, labels ...string) map[string]any {
	lbls := make([]map[string]any, len(labels))
	for i, l := range labels {
		lbls[i] = map[string]any{"name": l}
	}
	return map[string]any{
		"number":   num,
		"title":    fmt.Sprintf("PR %d", num),
		"html_url": fmt.Sprintf("https://github.com/org/repo/pull/%d", num),
		"head": map[string]any{
			"sha": head,
			"ref": fmt.Sprintf("feature/pr-%d", num),
		},
		"base": map[string]any{
			"sha": base,
			"ref": "main",
		},
		"labels": lbls,
	}
}

func makePRWithBody(num int, head, base, body string, labels ...string) map[string]any {
	m := makePR(num, head, base, labels...)
	m["body"] = body
	return m
}

func TestNewGitHubSource_InvalidRepo(t *testing.T) {
	cases := []string{"", "noslash", "/nope", "nope/"}
	for _, repo := range cases {
		_, err := NewGitHubSource("token", repo, nil)
		if err == nil {
			t.Errorf("expected error for repo %q", repo)
		}
	}
}

func TestNewGitHubSource_ValidRepo(t *testing.T) {
	s, err := NewGitHubSource("token", "org/repo", []string{"conductor"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name() != "github" {
		t.Errorf("unexpected name: %s", s.Name())
	}
}

func TestHasLabel(t *testing.T) {
	issue := &gh.Issue{
		Labels: []*gh.Label{
			{Name: gh.Ptr("conductor")},
			{Name: gh.Ptr("bug")},
		},
	}
	if !hasLabel(issue, "conductor") {
		t.Error("expected hasLabel to return true for 'conductor'")
	}
	if hasLabel(issue, "enhancement") {
		t.Error("expected hasLabel to return false for 'enhancement'")
	}
}

func TestMatchesFilter(t *testing.T) {
	s := &GitHubSource{labelFilter: []string{"conductor", "ready"}}

	issue := &gh.Issue{
		Labels: []*gh.Label{
			{Name: gh.Ptr("conductor")},
			{Name: gh.Ptr("ready")},
		},
	}
	if !s.matchesFilter(issue) {
		t.Error("expected matchesFilter to return true")
	}

	issueMissing := &gh.Issue{
		Labels: []*gh.Label{
			{Name: gh.Ptr("conductor")},
		},
	}
	if s.matchesFilter(issueMissing) {
		t.Error("expected matchesFilter to return false when label missing")
	}
}

// --- ListOpenPRs tests ---

func TestListOpenPRs_BehindBase(t *testing.T) {
	mux := http.NewServeMux()
	// Return one PR that is behind base.
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(7, "headSHA", "baseSHA")})
	})
	// CompareCommits: head is 1 behind base (using branch ref "main").
	mux.HandleFunc("/repos/org/repo/compare/main...headSHA", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"behind_by": 1, "ahead_by": 2})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	task := tasks[0]
	if task.Type != domain.TaskTypeRebase {
		t.Errorf("expected type=rebase, got %q", task.Type)
	}
	if task.ID != "7" {
		t.Errorf("expected ID=7, got %q", task.ID)
	}
	if task.BaseBranch != "main" {
		t.Errorf("expected BaseBranch=main, got %q", task.BaseBranch)
	}
}

func TestListOpenPRs_NotBehind_Excluded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(8, "headSHA", "baseSHA")})
	})
	// behind_by = 0 — not behind.
	mux.HandleFunc("/repos/org/repo/compare/main...headSHA", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"behind_by": 0, "ahead_by": 1})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for up-to-date PR, got %d", len(tasks))
	}
}

func TestListOpenPRs_ExcludesAbandoned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(9, "headSHA", "baseSHA", rebaseAbandonedLabel)})
	})
	// CompareCommits should never be called for abandoned PR.
	mux.HandleFunc("/repos/org/repo/compare/", func(w http.ResponseWriter, r *http.Request) {
		t.Error("CompareCommits should not be called for abandoned PR")
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for abandoned PR, got %d", len(tasks))
	}
}

func TestListOpenPRs_ExcludesRebasing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(10, "headSHA", "baseSHA", rebasingLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for PR already being rebased, got %d", len(tasks))
	}
}

func TestListOpenPRs_ExcludesThreeAttempts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(11, "headSHA", "baseSHA", rebaseAttemptsPrefix+"3")})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for PR with 3 attempts, got %d", len(tasks))
	}
}

func TestListOpenPRs_ParsesAttempts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(12, "headSHA", "baseSHA", rebaseAttemptsPrefix+"2")})
	})
	mux.HandleFunc("/repos/org/repo/compare/main...headSHA", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"behind_by": 1, "ahead_by": 0})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Attempts != 2 {
		t.Errorf("expected Attempts=2, got %d", tasks[0].Attempts)
	}
}

// --- RecordRebaseOutcome tests ---

func TestRecordRebaseOutcome_Success(t *testing.T) {
	var deletedPaths []string
	mux := http.NewServeMux()
	// Handle all label operations under issue 5 with trailing-slash prefix match.
	mux.HandleFunc("/repos/org/repo/issues/5/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletedPaths = append(deletedPaths, r.URL.Path)
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{})
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "5", Type: domain.TaskTypeRebase, BaseBranch: "main", Attempts: 0}
	err := s.RecordRebaseOutcome(context.Background(), task, true, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// At least one DELETE should have been made to remove the rebasing label.
	if len(deletedPaths) == 0 {
		t.Errorf("expected at least one DELETE for rebasing label removal")
	}
}

func TestRecordRebaseOutcome_FirstFailure(t *testing.T) {
	var addedLabels []string
	mux := http.NewServeMux()
	// Handle all issue 5 label operations.
	mux.HandleFunc("/repos/org/repo/issues/5/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			writeJSON(w, []any{})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") {
			// go-github sends labels as a raw JSON array ["label1", "label2"]
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{})
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "5", Type: domain.TaskTypeRebase, BaseBranch: "main", Attempts: 0}
	err := s.RecordRebaseOutcome(context.Background(), task, false, "conflict in foo.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addedLabels) == 0 {
		t.Fatal("expected a label to be added")
	}
	if addedLabels[0] != rebaseAttemptsPrefix+"1" {
		t.Errorf("expected label %q, got %q", rebaseAttemptsPrefix+"1", addedLabels[0])
	}
}

func TestRecordRebaseOutcome_ThirdFailure_PostsComment(t *testing.T) {
	var commentBody string
	var addedLabels []string
	mux := http.NewServeMux()

	// Handle all label operations and comments under issue 5.
	mux.HandleFunc("/repos/org/repo/issues/5/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			// go-github sends labels as a raw JSON array ["label1", "label2"]
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			commentBody = body.Body
			writeJSON(w, map[string]any{"id": 1})
		default:
			writeJSON(w, []any{})
		}
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "5", Type: domain.TaskTypeRebase, BaseBranch: "main", Attempts: 2}
	err := s.RecordRebaseOutcome(context.Background(), task, false, "unresolvable conflict")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasAttempts3 := false
	hasAbandoned := false
	for _, l := range addedLabels {
		if l == rebaseAttemptsPrefix+"3" {
			hasAttempts3 = true
		}
		if l == rebaseAbandonedLabel {
			hasAbandoned = true
		}
	}
	if !hasAttempts3 {
		t.Errorf("expected label %q to be added, got %v", rebaseAttemptsPrefix+"3", addedLabels)
	}
	if !hasAbandoned {
		t.Errorf("expected label %q to be added, got %v", rebaseAbandonedLabel, addedLabels)
	}
	if !strings.Contains(commentBody, "Rebase Abandoned") {
		t.Errorf("expected abandonment comment, got %q", commentBody)
	}
	if !strings.Contains(commentBody, "main") {
		t.Errorf("expected base branch in comment, got %q", commentBody)
	}
	if !strings.Contains(commentBody, "unresolvable conflict") {
		t.Errorf("expected error reason in comment, got %q", commentBody)
	}
}

// --- helper label tests ---

func TestPrHasLabel(t *testing.T) {
	pr := &gh.PullRequest{
		Labels: []*gh.Label{
			{Name: gh.Ptr("conductor:rebasing")},
		},
	}
	if !prHasLabel(pr, "conductor:rebasing") {
		t.Error("expected prHasLabel to return true")
	}
	if prHasLabel(pr, "conductor:rebase-abandoned") {
		t.Error("expected prHasLabel to return false for absent label")
	}
}

func TestRebaseAttempts_Parsing(t *testing.T) {
	cases := []struct {
		labels   []string
		expected int
	}{
		{nil, 0},
		{[]string{"conductor:rebase-attempts-1"}, 1},
		{[]string{"conductor:rebase-attempts-2"}, 2},
		{[]string{"other-label"}, 0},
	}
	for _, tc := range cases {
		lbls := make([]*gh.Label, len(tc.labels))
		for i, l := range tc.labels {
			l := l
			lbls[i] = &gh.Label{Name: &l}
		}
		pr := &gh.PullRequest{Labels: lbls}
		got := rebaseAttempts(pr)
		if got != tc.expected {
			t.Errorf("labels=%v: expected %d attempts, got %d", tc.labels, tc.expected, got)
		}
	}
}

// --- MarkPRNeedsReview tests ---

func TestMarkPRNeedsReview(t *testing.T) {
	var addedLabels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{})
	})

	s := newTestGitHubSource(t, mux)
	err := s.MarkPRNeedsReview(context.Background(), 7, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasNeedsReview := false
	for _, l := range addedLabels {
		if l == needsReviewLabel {
			hasNeedsReview = true
		}
	}
	if !hasNeedsReview {
		t.Errorf("expected %q label, got %v", needsReviewLabel, addedLabels)
	}
	for _, l := range addedLabels {
		if strings.HasPrefix(l, "conductor:issue-") {
			t.Errorf("unexpected dynamic issue label added: %q", l)
		}
	}
}

// --- ListPRsNeedingReview tests ---

func TestListPRsNeedingReview_ReturnsPRWithNeedsReview(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePRWithBody(7, "headSHA", "baseSHA", "Closes #42\n\nImplement the feature.", needsReviewLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingReview(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	task := tasks[0]
	if task.Type != domain.TaskTypeReview {
		t.Errorf("expected type=review, got %q", task.Type)
	}
	if task.ID != "7" {
		t.Errorf("expected ID=7, got %q", task.ID)
	}
	if task.SpecIssueNumber != 42 {
		t.Errorf("expected SpecIssueNumber=42, got %d", task.SpecIssueNumber)
	}
}

func TestListPRsNeedingReview_ExcludesReviewing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(8, "headSHA", "baseSHA", needsReviewLabel, reviewingLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingReview(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for PR already being reviewed, got %d", len(tasks))
	}
}

func TestListPRsNeedingReview_ExcludesApproved(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(9, "headSHA", "baseSHA", needsReviewLabel, approvedLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingReview(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for approved PR, got %d", len(tasks))
	}
}

func TestListPRsNeedingReview_ExcludesAbandoned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(10, "headSHA", "baseSHA", needsReviewLabel, reviewAbandonedLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingReview(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for abandoned PR, got %d", len(tasks))
	}
}

// --- ListPRsNeedingRevision tests ---

func TestListPRsNeedingRevision_ReturnsPRWithNeedsRevision(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePRWithBody(11, "headSHA", "baseSHA", "Closes #42\n\nImplement the feature.", needsRevisionLabel, reviewCyclePrefix+"1")})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingRevision(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	task := tasks[0]
	if task.Type != domain.TaskTypeRevise {
		t.Errorf("expected type=revise, got %q", task.Type)
	}
	if task.SpecIssueNumber != 42 {
		t.Errorf("expected SpecIssueNumber=42, got %d", task.SpecIssueNumber)
	}
	if task.ReviewCycle != 1 {
		t.Errorf("expected ReviewCycle=1, got %d", task.ReviewCycle)
	}
}

func TestListPRsNeedingRevision_ExcludesRevising(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(12, "headSHA", "baseSHA", needsRevisionLabel, revisingLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingRevision(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for PR already being revised, got %d", len(tasks))
	}
}

func TestListPRsNeedingRevision_ExcludesAbandoned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{makePR(13, "headSHA", "baseSHA", needsRevisionLabel, reviewAbandonedLabel)})
	})

	s := newTestGitHubSource(t, mux)
	tasks, err := s.ListPRsNeedingRevision(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for abandoned PR, got %d", len(tasks))
	}
}

// --- RecordReviewOutcome tests ---

func TestRecordReviewOutcome_Approved(t *testing.T) {
	var addedLabels []string
	var deletedLabels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			// Extract label name from path
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) > 0 {
				deletedLabels = append(deletedLabels, parts[len(parts)-1])
			}
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
		default:
			writeJSON(w, []any{})
		}
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "7", Type: domain.TaskTypeReview, ReviewCycle: 0}
	err := s.RecordReviewOutcome(context.Background(), task, true, "Looks good!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasApproved := false
	for _, l := range addedLabels {
		if l == approvedLabel {
			hasApproved = true
		}
	}
	if !hasApproved {
		t.Errorf("expected %q label to be added, got %v", approvedLabel, addedLabels)
	}
}

func TestRecordReviewOutcome_Rejected_FirstCycle(t *testing.T) {
	var addedLabels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
		default:
			writeJSON(w, []any{})
		}
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "7", Type: domain.TaskTypeReview, ReviewCycle: 0}
	err := s.RecordReviewOutcome(context.Background(), task, false, "Missing X feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasCycle1 := false
	hasNeedsRevision := false
	for _, l := range addedLabels {
		if l == reviewCyclePrefix+"1" {
			hasCycle1 = true
		}
		if l == needsRevisionLabel {
			hasNeedsRevision = true
		}
	}
	if !hasCycle1 {
		t.Errorf("expected %q label, got %v", reviewCyclePrefix+"1", addedLabels)
	}
	if !hasNeedsRevision {
		t.Errorf("expected %q label, got %v", needsRevisionLabel, addedLabels)
	}
}

func TestRecordReviewOutcome_ThirdCycle_Abandoned(t *testing.T) {
	var addedLabels []string
	var commentBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			commentBody = body.Body
			writeJSON(w, map[string]any{"id": 1})
		default:
			writeJSON(w, []any{})
		}
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "7", Type: domain.TaskTypeReview, ReviewCycle: 2}
	err := s.RecordReviewOutcome(context.Background(), task, false, "still missing Y")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasCycle3 := false
	hasAbandoned := false
	for _, l := range addedLabels {
		if l == reviewCyclePrefix+"3" {
			hasCycle3 = true
		}
		if l == reviewAbandonedLabel {
			hasAbandoned = true
		}
	}
	if !hasCycle3 {
		t.Errorf("expected %q label, got %v", reviewCyclePrefix+"3", addedLabels)
	}
	if !hasAbandoned {
		t.Errorf("expected %q label, got %v", reviewAbandonedLabel, addedLabels)
	}
	if !strings.Contains(commentBody, "Review Abandoned") {
		t.Errorf("expected abandonment comment, got %q", commentBody)
	}
	if !strings.Contains(commentBody, "still missing Y") {
		t.Errorf("expected feedback in comment, got %q", commentBody)
	}
}

// --- Claim for review/revise tasks ---

func TestClaim_ReviewTask(t *testing.T) {
	var addedLabels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
		}
		writeJSON(w, []any{})
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "7", Type: domain.TaskTypeReview}
	err := s.Claim(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasReviewing := false
	for _, l := range addedLabels {
		if l == reviewingLabel {
			hasReviewing = true
		}
	}
	if !hasReviewing {
		t.Errorf("expected %q label, got %v", reviewingLabel, addedLabels)
	}
}

func TestClaim_ReviseTask(t *testing.T) {
	var addedLabels []string
	var deletedPaths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var labels []string
			json.NewDecoder(r.Body).Decode(&labels) //nolint:errcheck
			addedLabels = append(addedLabels, labels...)
			writeJSON(w, []any{})
		case r.Method == http.MethodDelete:
			deletedPaths = append(deletedPaths, r.URL.Path)
			writeJSON(w, []any{})
		default:
			writeJSON(w, []any{})
		}
	})

	s := newTestGitHubSource(t, mux)
	task := &domain.Task{ID: "7", Type: domain.TaskTypeRevise}
	err := s.Claim(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasRevising := false
	for _, l := range addedLabels {
		if l == revisingLabel {
			hasRevising = true
		}
	}
	if !hasRevising {
		t.Errorf("expected %q label, got %v", revisingLabel, addedLabels)
	}
	if len(deletedPaths) == 0 {
		t.Error("expected needs-revision label to be removed")
	}
}

func TestParseIssueRef(t *testing.T) {
	cases := []struct {
		body     string
		expected int
	}{
		{"Closes #42\n\nSome description", 42},
		{"closes #7", 7},
		{"CLOSES #100\n\nMore text", 100},
		{"Closes  #42", 42},
		{"No closing reference here", 0},
		{"", 0},
	}
	for _, tc := range cases {
		pr := &gh.PullRequest{Body: gh.Ptr(tc.body)}
		got := parseIssueRef(pr)
		if got != tc.expected {
			t.Errorf("body=%q: expected %d, got %d", tc.body, tc.expected, got)
		}
	}
}

func TestIssueToTask_FrontMatter(t *testing.T) {
	body := "---\nconductor:\n  agent: claude\n---\nDo the thing"
	num := 42
	issue := &gh.Issue{
		Number: &num,
		Title:  gh.Ptr("My Issue"),
		Body:   gh.Ptr(body),
		Labels: []*gh.Label{
			{Name: gh.Ptr("conductor")},
		},
		HTMLURL: gh.Ptr("https://github.com/org/repo/issues/42"),
	}

	task := issueToTask(issue, "org/repo")

	if task.ID != "42" {
		t.Errorf("expected ID=42, got %s", task.ID)
	}
	if task.Config == nil {
		t.Fatal("expected parsed front-matter config")
	}
	if task.Config.Agent != "claude" {
		t.Errorf("expected agent=claude, got %q", task.Config.Agent)
	}
	if task.Description != "Do the thing" {
		t.Errorf("unexpected description: %q", task.Description)
	}
	if task.Status != domain.TaskStatusPending {
		t.Errorf("expected pending status")
	}
}
