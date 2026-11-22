package rollover

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

func TestParseDocsPolicy(t *testing.T) {
	tests := []struct {
		input   string
		want    DocsPolicy
		wantErr bool
	}{
		{"", DocsPolicyNone, false},
		{"none", DocsPolicyNone, false},
		{"NONE", DocsPolicyNone, false},
		{"milestone", DocsPolicyMilestone, false},
		{"Milestone", DocsPolicyMilestone, false},
		{"always", DocsPolicyAlways, false},
		{"ALWAYS", DocsPolicyAlways, false},
		{"invalid", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseDocsPolicy(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseDocsPolicy(%q) error = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ParseDocsPolicy(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SoftPct != 0.45 || cfg.HardPct != 0.65 || cfg.MaxArmedRounds != 8 || cfg.CloseoutMaxSteps != 4 || cfg.MinEpochTurns != 2 || cfg.DocsPolicy != DocsPolicyNone {
		t.Fatalf("unexpected DefaultConfig values: %+v", cfg)
	}
}

func TestSaveCheckpointToolDef(t *testing.T) {
	def := SaveCheckpointToolDef()
	if def.Name != SaveCheckpointToolName {
		t.Errorf("tool name = %q, want %q", def.Name, SaveCheckpointToolName)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("invalid InputSchema JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
}

func TestParseAndValidateCheckpoint(t *testing.T) {
	validJSON := `{
		"summary": "Implemented parser",
		"lessons_learned": ["Always check EOF"],
		"recently_changed_files": [{"path": "ast.go", "purpose": "AST nodes"}],
		"key_files": [{"path": "parser.go", "purpose": "Core parser logic"}],
		"key_dirs": [{"path": "internal/ast", "purpose": "AST package"}],
		"open_questions": ["Handle comments?"],
		"resume_instructions": "Implement tokenizer next",
		"docs_updated": ["README.md"]
	}`

	payload, err := ParseAndValidateCheckpoint(json.RawMessage(validJSON))
	if err != nil {
		t.Fatalf("ParseAndValidateCheckpoint valid JSON error: %v", err)
	}
	if payload.Summary != "Implemented parser" || payload.ResumeInstructions != "Implement tokenizer next" {
		t.Errorf("unexpected payload summary/resume: %+v", payload)
	}
	if len(payload.KeyFiles) != 1 || payload.KeyFiles[0].Path != "parser.go" {
		t.Errorf("unexpected key files: %+v", payload.KeyFiles)
	}

	// Missing summary
	missingSummary := `{"resume_instructions": "go", "key_files": [{"path": "a.go", "purpose": "a"}]}`
	if _, err := ParseAndValidateCheckpoint(json.RawMessage(missingSummary)); err == nil || !strings.Contains(err.Error(), "missing required field 'summary'") {
		t.Errorf("expected missing summary error, got %v", err)
	}

	// Missing resume_instructions
	missingResume := `{"summary": "done", "key_files": [{"path": "a.go", "purpose": "a"}]}`
	if _, err := ParseAndValidateCheckpoint(json.RawMessage(missingResume)); err == nil || !strings.Contains(err.Error(), "missing required field 'resume_instructions'") {
		t.Errorf("expected missing resume_instructions error, got %v", err)
	}

	// Empty key_files
	emptyKeyFiles := `{"summary": "done", "resume_instructions": "next", "key_files": []}`
	if _, err := ParseAndValidateCheckpoint(json.RawMessage(emptyKeyFiles)); err == nil || !strings.Contains(err.Error(), "must contain at least 1 file") {
		t.Errorf("expected empty key_files error, got %v", err)
	}

	// KeyFile with missing purpose
	invalidKeyFile := `{"summary": "done", "resume_instructions": "next", "key_files": [{"path": "a.go", "purpose": ""}]}`
	if _, err := ParseAndValidateCheckpoint(json.RawMessage(invalidKeyFile)); err == nil || !strings.Contains(err.Error(), "must have non-empty path and purpose") {
		t.Errorf("expected key_files missing purpose error, got %v", err)
	}

	// Empty JSON
	if _, err := ParseAndValidateCheckpoint(nil); err == nil {
		t.Error("expected error on empty JSON")
	}
}

func TestIsCloseoutToolAllowed(t *testing.T) {
	if !IsCloseoutToolAllowed("save_checkpoint", false) {
		t.Error("save_checkpoint must always be allowed")
	}
	if !IsCloseoutToolAllowed("read_file", false) || !IsCloseoutToolAllowed("grep", false) {
		t.Error("inspection tools must be allowed")
	}
	if IsCloseoutToolAllowed("handoff", true) || IsCloseoutToolAllowed("todo_manage", true) {
		t.Error("handoff/todo_manage must never be allowed during closeout")
	}
	if IsCloseoutToolAllowed("edit_file", false) || IsCloseoutToolAllowed("write_file", false) {
		t.Error("write tools must be forbidden when allowDocWrites is false")
	}
	if !IsCloseoutToolAllowed("edit_file", true) || !IsCloseoutToolAllowed("write_file", true) {
		t.Error("write tools must be allowed when allowDocWrites is true")
	}
	if IsCloseoutToolAllowed("bash_exec", true) || IsCloseoutToolAllowed("git", true) {
		t.Error("bash_exec/git must be forbidden during closeout")
	}
}

func TestCollectPinnedState_GitRepo(t *testing.T) {
	dir := t.TempDir()

	// Initialize git repo in dir
	runCmd := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\nOutput: %s", args, err, string(out))
		}
	}

	runCmd("init")
	runCmd("config", "user.email", "test@example.com")
	runCmd("config", "user.name", "Test User")

	testFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCmd("add", "hello.txt")
	runCmd("commit", "-m", "initial commit")

	// Create an uncommitted change
	if err := os.WriteFile(testFile, []byte("hello world modified\n"), 0644); err != nil {
		t.Fatalf("WriteFile modify: %v", err)
	}

	todos := []todo.Item{
		{Content: "Build parser", Status: todo.StatusCompleted},
		{Content: "Build codegen", Status: todo.StatusInProgress},
	}

	ctx := context.Background()
	pinned := CollectPinnedState(ctx, dir, "Original User Objective", todos, []string{"async task 1"})

	if pinned.OriginalObjective != "Original User Objective" {
		t.Errorf("OriginalObjective = %q", pinned.OriginalObjective)
	}
	if pinned.WorkDir != dir {
		t.Errorf("WorkDir = %q, want %q", pinned.WorkDir, dir)
	}
	if pinned.GitBranch == "" {
		t.Error("expected non-empty GitBranch")
	}
	if !strings.Contains(pinned.GitHeadCommit, "initial commit") {
		t.Errorf("GitHeadCommit = %q, want to contain 'initial commit'", pinned.GitHeadCommit)
	}
	if !strings.Contains(pinned.GitStatusShort, "M hello.txt") {
		t.Errorf("GitStatusShort = %q, want 'M hello.txt'", pinned.GitStatusShort)
	}
	if len(pinned.Todos) != 2 || len(pinned.PendingAsyncTasks) != 1 {
		t.Errorf("unexpected todos/async tasks: %+v", pinned)
	}
}

func TestCollectPinnedState_NonGitDirectory(t *testing.T) {
	dir := t.TempDir()
	pinned := CollectPinnedState(context.Background(), dir, "Goal", nil, nil)
	if pinned.OriginalObjective != "Goal" || pinned.WorkDir != dir {
		t.Errorf("unexpected pinned state in non-git dir: %+v", pinned)
	}
	// Git fields must gracefully remain empty without errors/panics
	if pinned.GitBranch != "" || pinned.GitHeadCommit != "" {
		t.Errorf("expected empty git fields for non-git dir, got branch=%q head=%q", pinned.GitBranch, pinned.GitHeadCommit)
	}
}

func TestResolveOriginalObjective(t *testing.T) {
	if got := ResolveOriginalObjective("Existing Goal", nil); got != "Existing Goal" {
		t.Errorf("ResolveOriginalObjective with existing = %q, want 'Existing Goal'", got)
	}

	msgs := []provider.Message{
		provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "intro"}}),
		provider.BuildUserTextMessage("First user objective statement"),
		provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "working on it"}}),
	}

	if got := ResolveOriginalObjective("", msgs); got != "First user objective statement" {
		t.Errorf("ResolveOriginalObjective from msgs = %q", got)
	}

	if got := ResolveOriginalObjective("", nil); got != "" {
		t.Errorf("ResolveOriginalObjective empty = %q, want empty", got)
	}
}

func TestBuildCloseoutPromptAndBootstrapMessage(t *testing.T) {
	closeout := BuildCloseoutPrompt(true, true, "focus on unit tests")
	if !strings.Contains(closeout, "save_checkpoint") || !strings.Contains(closeout, "Documentation update policy is active") || !strings.Contains(closeout, "focus on unit tests") {
		t.Errorf("closeout prompt missing expected text:\n%s", closeout)
	}

	pinned := session.PinnedState{
		OriginalObjective: "Build compiler",
		WorkDir:           "/workspace",
		GitBranch:         "feat/parser",
		GitHeadCommit:     "abc1234 Add AST",
		GitStatusShort:    "M parser.go",
		Todos: []todo.Item{
			{Content: "Implement AST", Status: todo.StatusCompleted},
			{Content: "Implement Parser", Status: todo.StatusInProgress},
		},
	}
	payload := session.CheckpointPayload{
		Summary:            "AST complete, parser in progress",
		LessonsLearned:     []string{"Check operator precedence"},
		KeyFiles:           []session.FileContextNote{{Path: "parser.go", Purpose: "Main parser"}},
		KeyDirs:            []session.DirContextNote{{Path: "internal/ast", Purpose: "Node definitions"}},
		ResumeInstructions: "Finish parseExpr and run tests",
	}

	bootstrap := BuildBootstrapMessage(2, 1, pinned, payload, "make sure tests pass")
	if !strings.Contains(bootstrap, "# Session Rollover — Epoch 2 (Resumed from Epoch 1)") {
		t.Errorf("bootstrap header missing:\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "Build compiler") || !strings.Contains(bootstrap, "feat/parser") || !strings.Contains(bootstrap, "parser.go") {
		t.Errorf("bootstrap pinned state missing:\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "AST complete, parser in progress") || !strings.Contains(bootstrap, "Check operator precedence") {
		t.Errorf("bootstrap handoff payload missing:\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "Finish parseExpr and run tests") || !strings.Contains(bootstrap, "make sure tests pass") {
		t.Errorf("bootstrap resume instructions missing:\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "**Orientation Protocol**") {
		t.Errorf("bootstrap orientation protocol missing:\n%s", bootstrap)
	}
}

func TestFormatEpochsTable(t *testing.T) {
	emptyTable := FormatEpochsTable(1, nil)
	if !strings.Contains(emptyTable, "No prior epochs archived") {
		t.Errorf("empty table text missing:\n%s", emptyTable)
	}

	epochs := []session.EpochCheckpoint{
		{
			Epoch:            1,
			CreatedAt:        time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC),
			Trigger:          session.RolloverTriggerSoftThreshold,
			TokensAtRollover: 45000,
			EstimatedCostUSD: 0.1250,
			Payload: session.CheckpointPayload{
				Summary: "Completed initial feature setup",
			},
		},
	}
	table := FormatEpochsTable(2, epochs)
	if !strings.Contains(table, "Session Epochs (Current: Epoch 2)") || !strings.Contains(table, "Epoch 1") || !strings.Contains(table, "soft_threshold") || !strings.Contains(table, "45000") {
		t.Errorf("epochs table missing expected entries:\n%s", table)
	}
}
