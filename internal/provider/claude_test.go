package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCodeAdapter_Name(t *testing.T) {
	a := NewClaudeCodeAdapter("claude", nil, 0.003)
	if a.Name() != "claude" {
		t.Errorf("unexpected name: %s", a.Name())
	}
}

func TestClaudeCodeAdapter_CostEstimate(t *testing.T) {
	a := NewClaudeCodeAdapter("claude", nil, 0.003)
	cost, ok := a.CostEstimate(4000) // 4000 chars ≈ 1000 tokens
	if !ok {
		t.Error("expected cost estimate to be available")
	}
	// 1000 tokens * $0.003/1k = $0.003
	if cost < 0.002 || cost > 0.004 {
		t.Errorf("unexpected cost estimate: %f", cost)
	}
}

func TestClaudeCodeAdapter_CostEstimate_NoCost(t *testing.T) {
	a := NewClaudeCodeAdapter("claude", nil, 0)
	_, ok := a.CostEstimate(4000)
	if ok {
		t.Error("expected cost estimate to be unavailable when costPer1kTokens=0")
	}
}

func TestClaudeCodeAdapter_Run_InvalidBinary(t *testing.T) {
	a := NewClaudeCodeAdapter("no-such-binary-xyz", nil, 0)
	var buf bytes.Buffer
	rc := RunContext{
		RepoPath:  t.TempDir(),
		TaskFile:  "/tmp/task.md",
		LogWriter: &buf,
	}
	_, err := a.Run(context.Background(), rc)
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestClaudeCodeAdapter_Run_EchoCommand(t *testing.T) {
	// Use "echo" as a stand-in for the claude binary to test the run plumbing.
	a := NewClaudeCodeAdapter("echo", []string{"hello"}, 0)

	dir := t.TempDir()
	taskFile := filepath.Join(dir, "task.md")
	if err := os.WriteFile(taskFile, []byte("do stuff"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	rc := RunContext{
		RepoPath:  dir,
		TaskFile:  taskFile,
		LogWriter: &buf,
		Env:       map[string]string{"MY_VAR": "val"},
	}

	handle, err := a.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatalf("wait error: %v", err)
	}

	// echo should have written something to our log writer
	out := buf.String()
	if !strings.Contains(out, "hello") {
		t.Errorf("expected 'hello' in output, got: %q", out)
	}
}

func TestBuildEnv_ContainsTaskFile(t *testing.T) {
	rc := RunContext{
		TaskFile: "/tmp/ariadne-task.md",
		Env:      map[string]string{"FOO": "bar"},
	}
	env := buildEnv(rc)

	var hasTaskFile, hasFoo bool
	for _, e := range env {
		if e == "ARIADNE_TASK_FILE=/tmp/ariadne-task.md" {
			hasTaskFile = true
		}
		if e == "FOO=bar" {
			hasFoo = true
		}
	}
	if !hasTaskFile {
		t.Error("ARIADNE_TASK_FILE not set in env")
	}
	if !hasFoo {
		t.Error("FOO not set in env")
	}
}
