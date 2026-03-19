package proof

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/haha-systems/conductor/internal/domain"
)

func TestParseShortstat(t *testing.T) {
	cases := []struct {
		input      string
		files, ins, del int
	}{
		{" 4 files changed, 87 insertions(+), 12 deletions(-)", 4, 87, 12},
		{" 1 file changed, 3 insertions(+)", 1, 3, 0},
		{" 2 files changed, 5 deletions(-)", 2, 0, 5},
		{"", 0, 0, 0},
	}

	for _, tc := range cases {
		stat := parseShortstat(tc.input)
		if stat.FilesChanged != tc.files || stat.Insertions != tc.ins || stat.Deletions != tc.del {
			t.Errorf("parseShortstat(%q) = {%d,%d,%d}, want {%d,%d,%d}",
				tc.input, stat.FilesChanged, stat.Insertions, stat.Deletions,
				tc.files, tc.ins, tc.del)
		}
	}
}

func TestParseTestOutput(t *testing.T) {
	output := "--- PASS: TestFoo (0.01s)\n--- PASS: TestBar (0.00s)\n--- FAIL: TestBaz (0.03s)\nFAIL\n"
	total, failures := parseTestOutput(output)
	if total != 3 {
		t.Errorf("expected total=3, got %d", total)
	}
	if failures != 1 {
		t.Errorf("expected failures=1, got %d", failures)
	}
}

func TestInferCICommand_GoMod(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	cmd := inferCICommand(dir)
	if len(cmd) == 0 || cmd[0] != "go" {
		t.Errorf("expected go test command, got %v", cmd)
	}
}

func TestInferCICommand_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	cmd := inferCICommand(dir)
	if len(cmd) == 0 || cmd[0] != "npm" {
		t.Errorf("expected npm test command, got %v", cmd)
	}
}

func TestInferCICommand_Unknown(t *testing.T) {
	cmd := inferCICommand(t.TempDir())
	if cmd != nil {
		t.Errorf("expected nil for unknown repo type, got %v", cmd)
	}
}

// stubEstimator is a test double for CostEstimator.
type stubEstimator struct {
	rate float64 // USD per 1k tokens; 0 means no estimate
}

func (s *stubEstimator) CostEstimate(promptLen int) (float64, bool) {
	if s.rate <= 0 {
		return 0, false
	}
	tokens := float64(promptLen) / 4.0
	return (tokens / 1000.0) * s.rate, true
}

func TestCollect_CostUSD_WithRate(t *testing.T) {
	dir := t.TempDir()

	// Write a fake .conductor-task.md (4000 bytes → 1000 tokens).
	prompt := make([]byte, 4000)
	for i := range prompt {
		prompt[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, ".conductor-task.md"), prompt, 0644); err != nil {
		t.Fatal(err)
	}
	// Create proof/ dir so writeSummary can write.
	if err := os.MkdirAll(filepath.Join(dir, "proof"), 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	run := &domain.Run{
		ID:           "r1",
		TaskID:       "t1",
		Provider:     "claude",
		WorktreePath: dir,
		StartedAt:    now,
		FinishedAt:   &now,
	}
	task := &domain.Task{ID: "t1"}

	c := New(Config{CICommand: []string{"true"}})
	// rate = $1.00 per 1k tokens; 1000 tokens → $1.00
	est := &stubEstimator{rate: 1.0}

	bundle, err := c.Collect(context.Background(), run, task, est)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if bundle.CostUSD == 0 {
		t.Error("expected CostUSD > 0 when provider has a rate, got 0")
	}
	// 4000 bytes / 4 = 1000 tokens; 1000/1000 * $1.00 = $1.00
	const want = 1.0
	if bundle.CostUSD != want {
		t.Errorf("CostUSD = %f, want %f", bundle.CostUSD, want)
	}
}

func TestCollect_CostUSD_NoRate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".conductor-task.md"), []byte("task"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "proof"), 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	run := &domain.Run{
		ID:           "r2",
		TaskID:       "t2",
		Provider:     "codex",
		WorktreePath: dir,
		StartedAt:    now,
		FinishedAt:   &now,
	}
	task := &domain.Task{ID: "t2"}

	c := New(Config{CICommand: []string{"true"}})
	// rate = 0 → no estimate
	est := &stubEstimator{rate: 0}

	bundle, err := c.Collect(context.Background(), run, task, est)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if bundle.CostUSD != 0 {
		t.Errorf("expected CostUSD = 0 for provider with no rate, got %f", bundle.CostUSD)
	}
}

func TestCollect_CostUSD_NilProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proof"), 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	run := &domain.Run{
		ID:           "r3",
		TaskID:       "t3",
		Provider:     "shell",
		WorktreePath: dir,
		StartedAt:    now,
		FinishedAt:   &now,
	}
	task := &domain.Task{ID: "t3"}

	c := New(Config{CICommand: []string{"true"}})

	bundle, err := c.Collect(context.Background(), run, task, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if bundle.CostUSD != 0 {
		t.Errorf("expected CostUSD = 0 with nil provider, got %f", bundle.CostUSD)
	}
}
