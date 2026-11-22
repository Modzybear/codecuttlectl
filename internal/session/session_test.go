package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

// --- ID Generation Tests ---

func TestGenerateID(t *testing.T) {
	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID() error: %v", err)
	}
	if len(id) != 12 { // "ses_" (4) + 8 hex chars
		t.Errorf("expected ID length 12, got %d: %q", len(id), id)
	}
	if id[:4] != "ses_" {
		t.Errorf("expected ID prefix 'ses_', got %q", id[:4])
	}
}

func TestGenerateIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id, err := GenerateID()
		if err != nil {
			t.Fatalf("GenerateID() iteration %d error: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated at iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestSessionStateDraftPersistence(t *testing.T) {
	state := SessionState{
		DraftMessage: "This is a draft message that should survive crashes.",
		Meta: SessionMeta{
			CreatedAt: time.Now(),
		},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}

	var decoded SessionState
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}

	if decoded.DraftMessage != state.DraftMessage {
		t.Errorf("expected DraftMessage %q, got %q", state.DraftMessage, decoded.DraftMessage)
	}
}

// --- Message Serialization Tests ---

func TestMarshalUnmarshalTextOnly(t *testing.T) {
	messages := []types.Message{
		{
			Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Hello, world!"},
			},
		},
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Hi there! How can I help?"},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	if len(serialized) != 2 {
		t.Fatalf("expected 2 serialized messages, got %d", len(serialized))
	}
	if serialized[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", serialized[0].Role)
	}
	if serialized[0].Blocks[0].Type != "text" {
		t.Errorf("expected type 'text', got %q", serialized[0].Blocks[0].Type)
	}
	if serialized[0].Blocks[0].Text != "Hello, world!" {
		t.Errorf("expected text 'Hello, world!', got %q", serialized[0].Blocks[0].Text)
	}

	// Round-trip
	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	if len(restored) != 2 {
		t.Fatalf("expected 2 restored messages, got %d", len(restored))
	}
	if restored[0].Role != types.ConversationRoleUser {
		t.Errorf("expected role user, got %v", restored[0].Role)
	}
	textBlock, ok := restored[0].Content[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", restored[0].Content[0])
	}
	if textBlock.Value != "Hello, world!" {
		t.Errorf("expected 'Hello, world!', got %q", textBlock.Value)
	}
}

func TestMarshalUnmarshalToolUse(t *testing.T) {
	toolInput := map[string]interface{}{
		"path":   "/tmp/test.go",
		"offset": float64(0),
		"limit":  float64(100),
	}

	messages := []types.Message{
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Let me read that file."},
				&types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String("tu_abc123"),
						Name:      aws.String("read_file"),
						Input:     document.NewLazyDocument(toolInput),
					},
				},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	if len(serialized) != 1 {
		t.Fatalf("expected 1 message, got %d", len(serialized))
	}
	if len(serialized[0].Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(serialized[0].Blocks))
	}

	// Verify tool_use block
	tuBlock := serialized[0].Blocks[1]
	if tuBlock.Type != "tool_use" {
		t.Errorf("expected type 'tool_use', got %q", tuBlock.Type)
	}
	if tuBlock.ToolUseID != "tu_abc123" {
		t.Errorf("expected tool_use_id 'tu_abc123', got %q", tuBlock.ToolUseID)
	}
	if tuBlock.Name != "read_file" {
		t.Errorf("expected name 'read_file', got %q", tuBlock.Name)
	}

	// Verify input JSON
	var parsedInput map[string]interface{}
	if err := json.Unmarshal(tuBlock.Input, &parsedInput); err != nil {
		t.Fatalf("parsing tool input JSON: %v", err)
	}
	if parsedInput["path"] != "/tmp/test.go" {
		t.Errorf("expected path '/tmp/test.go', got %v", parsedInput["path"])
	}

	// Round-trip back to Bedrock types
	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	if len(restored) != 1 {
		t.Fatalf("expected 1 restored message, got %d", len(restored))
	}
	if restored[0].Role != types.ConversationRoleAssistant {
		t.Errorf("expected assistant role, got %v", restored[0].Role)
	}

	// Verify text block survived
	restoredText, ok := restored[0].Content[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", restored[0].Content[0])
	}
	if restoredText.Value != "Let me read that file." {
		t.Errorf("expected text, got %q", restoredText.Value)
	}

	// Verify tool_use block survived with document.Interface
	restoredTU, ok := restored[0].Content[1].(*types.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("expected ContentBlockMemberToolUse, got %T", restored[0].Content[1])
	}
	if aws.ToString(restoredTU.Value.ToolUseId) != "tu_abc123" {
		t.Errorf("expected tool_use_id 'tu_abc123', got %q", aws.ToString(restoredTU.Value.ToolUseId))
	}
	if aws.ToString(restoredTU.Value.Name) != "read_file" {
		t.Errorf("expected name 'read_file', got %q", aws.ToString(restoredTU.Value.Name))
	}

	// Verify input was reconstructed as a document.Interface
	if restoredTU.Value.Input == nil {
		t.Fatal("expected non-nil Input document")
	}
	// Use MarshalSmithyDocument to get JSON and verify content
	inputBytes, err := restoredTU.Value.Input.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("MarshalSmithyDocument: %v", err)
	}
	var restoredMap map[string]interface{}
	if err := json.Unmarshal(inputBytes, &restoredMap); err != nil {
		t.Fatalf("json.Unmarshal restored input: %v", err)
	}
	if restoredMap["path"] != "/tmp/test.go" {
		t.Errorf("expected path '/tmp/test.go', got %v", restoredMap["path"])
	}
}

func TestMarshalUnmarshalToolResult(t *testing.T) {
	messages := []types.Message{
		{
			Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String("tu_abc123"),
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{Value: "file contents here"},
						},
						Status: types.ToolResultStatusSuccess,
					},
				},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	if serialized[0].Blocks[0].Type != "tool_result" {
		t.Errorf("expected type 'tool_result', got %q", serialized[0].Blocks[0].Type)
	}
	if serialized[0].Blocks[0].Status != "success" {
		t.Errorf("expected status 'success', got %q", serialized[0].Blocks[0].Status)
	}
	if serialized[0].Blocks[0].ResultFor != "tu_abc123" {
		t.Errorf("expected result_for 'tu_abc123', got %q", serialized[0].Blocks[0].ResultFor)
	}

	// Round-trip
	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	trBlock, ok := restored[0].Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("expected ContentBlockMemberToolResult, got %T", restored[0].Content[0])
	}
	if aws.ToString(trBlock.Value.ToolUseId) != "tu_abc123" {
		t.Errorf("expected tool_use_id 'tu_abc123', got %q", aws.ToString(trBlock.Value.ToolUseId))
	}
	if trBlock.Value.Status != types.ToolResultStatusSuccess {
		t.Errorf("expected success status, got %v", trBlock.Value.Status)
	}
}

func TestMarshalUnmarshalToolResultError(t *testing.T) {
	messages := []types.Message{
		{
			Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String("tu_err456"),
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{Value: "Error: file not found"},
						},
						Status: types.ToolResultStatusError,
					},
				},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	if serialized[0].Blocks[0].Status != "error" {
		t.Errorf("expected status 'error', got %q", serialized[0].Blocks[0].Status)
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	trBlock := restored[0].Content[0].(*types.ContentBlockMemberToolResult)
	if trBlock.Value.Status != types.ToolResultStatusError {
		t.Errorf("expected error status, got %v", trBlock.Value.Status)
	}
}

func TestMarshalUnmarshalMultiTurn(t *testing.T) {
	// Simulate: user asks → assistant calls tool → tool result → assistant responds
	messages := []types.Message{
		{
			Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Read my config file"},
			},
		},
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String("tu_001"),
						Name:      aws.String("read_file"),
						Input:     document.NewLazyDocument(map[string]interface{}{"path": "/etc/config.yaml"}),
					},
				},
			},
		},
		{
			Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String("tu_001"),
						Content: []types.ToolResultContentBlock{
							&types.ToolResultContentBlockMemberText{Value: "key: value\nport: 8080"},
						},
						Status: types.ToolResultStatusSuccess,
					},
				},
			},
		},
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Your config has key=value and port=8080."},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	if len(serialized) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(serialized))
	}

	// Verify the full round-trip
	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	if len(restored) != 4 {
		t.Fatalf("expected 4 restored messages, got %d", len(restored))
	}

	// Verify roles
	expectedRoles := []types.ConversationRole{
		types.ConversationRoleUser,
		types.ConversationRoleAssistant,
		types.ConversationRoleUser,
		types.ConversationRoleAssistant,
	}
	for i, expected := range expectedRoles {
		if restored[i].Role != expected {
			t.Errorf("message %d: expected role %v, got %v", i, expected, restored[i].Role)
		}
	}

	// Verify final text
	finalText := restored[3].Content[0].(*types.ContentBlockMemberText)
	if finalText.Value != "Your config has key=value and port=8080." {
		t.Errorf("unexpected final text: %q", finalText.Value)
	}
}

func TestMarshalEmptyHistory(t *testing.T) {
	serialized, err := MarshalHistory(nil)
	if err != nil {
		t.Fatalf("MarshalHistory(nil) error: %v", err)
	}
	if len(serialized) != 0 {
		t.Errorf("expected empty result, got %d messages", len(serialized))
	}

	restored, err := UnmarshalHistory(nil)
	if err != nil {
		t.Fatalf("UnmarshalHistory(nil) error: %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("expected empty result, got %d messages", len(restored))
	}
}

func TestMarshalToolUseEmptyInput(t *testing.T) {
	messages := []types.Message{
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String("tu_empty"),
						Name:      aws.String("some_tool"),
						Input:     document.NewLazyDocument(map[string]interface{}{}),
					},
				},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	// Verify the input serialized as empty object
	if string(serialized[0].Blocks[0].Input) != "{}" {
		t.Errorf("expected '{}', got %q", string(serialized[0].Blocks[0].Input))
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	tu := restored[0].Content[0].(*types.ContentBlockMemberToolUse)
	if tu.Value.Input == nil {
		t.Fatal("expected non-nil input document for empty input")
	}
}

func TestMarshalToolUseComplexNestedInput(t *testing.T) {
	complexInput := map[string]interface{}{
		"command": "find / -name '*.go' | head -10",
		"nested": map[string]interface{}{
			"array": []interface{}{1.0, "two", true, nil},
			"deep": map[string]interface{}{
				"key": "value",
			},
		},
		"timeout": 30.0,
	}

	messages := []types.Message{
		{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: aws.String("tu_complex"),
						Name:      aws.String("bash_exec"),
						Input:     document.NewLazyDocument(complexInput),
					},
				},
			},
		},
	}

	serialized, err := MarshalHistory(messages)
	if err != nil {
		t.Fatalf("MarshalHistory() error: %v", err)
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}

	tu := restored[0].Content[0].(*types.ContentBlockMemberToolUse)
	// Use MarshalSmithyDocument to verify round-tripped content
	inputBytes, err := tu.Value.Input.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("MarshalSmithyDocument: %v", err)
	}
	var restoredInput map[string]interface{}
	if err := json.Unmarshal(inputBytes, &restoredInput); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if restoredInput["command"] != "find / -name '*.go' | head -10" {
		t.Errorf("command mismatch: %v", restoredInput["command"])
	}
	nested, ok := restoredInput["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested map, got %T", restoredInput["nested"])
	}
	arr, ok := nested["array"].([]interface{})
	if !ok {
		t.Fatalf("expected array, got %T", nested["array"])
	}
	if len(arr) != 4 {
		t.Errorf("expected 4 array elements, got %d", len(arr))
	}
}

func TestMarshalJSONRoundTrip(t *testing.T) {
	// Verify that our Message type survives a JSON marshal/unmarshal cycle
	// (simulating what happens when written to and read from disk)
	messages := []Message{
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "text", Text: "hello"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "text", Text: "I'll check."},
				{Type: "tool_use", ToolUseID: "tu_1", Name: "read_file", Input: json.RawMessage(`{"path":"/tmp/x"}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", Content: "file contents", Status: "success", ResultFor: "tu_1"},
			},
		},
	}

	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded []Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(decoded) != 3 {
		t.Fatalf("expected 3, got %d", len(decoded))
	}
	if decoded[1].Blocks[1].Name != "read_file" {
		t.Errorf("expected 'read_file', got %q", decoded[1].Blocks[1].Name)
	}
}

// --- FileStore Tests ---

func TestFileStoreCreateLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	meta := SessionMeta{
		Title:   "test session",
		Model:   "us.anthropic.claude-opus-4-6-v1",
		Region:  "us-east-1",
		WorkDir: "/home/test",
		Stats:   Stats{InputTokens: 100, OutputTokens: 50, ToolCalls: 2, Turns: 1},
	}

	id, err := store.Create(meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if id[:4] != "ses_" {
		t.Errorf("expected ID prefix 'ses_', got %q", id[:4])
	}

	// Load and verify
	state, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if state.Meta.ID != id {
		t.Errorf("expected ID %q, got %q", id, state.Meta.ID)
	}
	if state.Meta.Title != "test session" {
		t.Errorf("expected title 'test session', got %q", state.Meta.Title)
	}
	if state.Meta.Model != "us.anthropic.claude-opus-4-6-v1" {
		t.Errorf("expected model, got %q", state.Meta.Model)
	}
	if state.Meta.Stats.InputTokens != 100 {
		t.Errorf("expected 100 input tokens, got %d", state.Meta.Stats.InputTokens)
	}
}

func TestFileStoreSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, err := store.Create(SessionMeta{
		Title: "initial",
		Model: "test-model",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Load, modify, save
	state, _ := store.Load(id)
	state.Meta.Title = "updated title"
	state.Meta.Stats.Turns = 5
	state.Messages = []Message{
		{Role: "user", Blocks: []ContentItem{{Type: "text", Text: "hello"}}},
	}
	state.Todos = []todo.Item{
		{Content: "fix bug", Status: "in_progress", Priority: "high"},
	}
	state.Inkwell = []InkEntry{
		{
			Timestamp:  time.Now().UTC(),
			Turn:       1,
			ToolName:   "bash_exec",
			ToolUseID:  "tu_001",
			DurationMs: 150,
			IsError:    false,
		},
	}

	if err := store.Save(id, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify all changes persisted
	reloaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	if reloaded.Meta.Title != "updated title" {
		t.Errorf("expected 'updated title', got %q", reloaded.Meta.Title)
	}
	if reloaded.Meta.Stats.Turns != 5 {
		t.Errorf("expected 5 turns, got %d", reloaded.Meta.Stats.Turns)
	}
	if len(reloaded.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(reloaded.Messages))
	}
	if len(reloaded.Todos) != 1 {
		t.Errorf("expected 1 todo, got %d", len(reloaded.Todos))
	}
	if reloaded.Todos[0].Content != "fix bug" {
		t.Errorf("expected 'fix bug', got %q", reloaded.Todos[0].Content)
	}
	if len(reloaded.Inkwell) != 1 {
		t.Errorf("expected 1 inkwell entry, got %d", len(reloaded.Inkwell))
	}
	if reloaded.Inkwell[0].ToolName != "bash_exec" {
		t.Errorf("expected 'bash_exec', got %q", reloaded.Inkwell[0].ToolName)
	}

	// Verify updated_at changed
	if !reloaded.Meta.UpdatedAt.After(reloaded.Meta.CreatedAt) {
		t.Error("expected updated_at to be after created_at")
	}
}

func TestFileStoreList(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Create 5 sessions with staggered timestamps
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		id, err := store.Create(SessionMeta{
			Title: "session " + string(rune('A'+i)),
			Model: "test-model",
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids[i] = id
		// Small delay to ensure distinct updated_at
		time.Sleep(10 * time.Millisecond)
	}

	// List all
	metas, err := store.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(metas))
	}

	// Verify sorted by most recent first
	for i := 1; i < len(metas); i++ {
		if metas[i].UpdatedAt.After(metas[i-1].UpdatedAt) {
			t.Errorf("sessions not sorted by recency: %v after %v at index %d",
				metas[i].UpdatedAt, metas[i-1].UpdatedAt, i)
		}
	}

	// Test limit
	limited, err := store.List(3)
	if err != nil {
		t.Fatalf("List(3): %v", err)
	}
	if len(limited) != 3 {
		t.Errorf("expected 3 sessions with limit, got %d", len(limited))
	}
}

func TestFileStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, err := store.Create(SessionMeta{Title: "to delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify it exists
	_, err = store.Load(id)
	if err != nil {
		t.Fatalf("Load before delete: %v", err)
	}

	// Delete
	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it's gone
	_, err = store.Load(id)
	if err == nil {
		t.Fatal("expected error loading deleted session")
	}
}

func TestFileStoreDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	err = store.Delete("ses_nonexistent")
	if err == nil {
		t.Fatal("expected error deleting non-existent session")
	}
}

func TestFileStorePrune(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Create sessions with different timestamps
	oldID, _ := store.Create(SessionMeta{Title: "old"})
	recentID, _ := store.Create(SessionMeta{Title: "recent"})

	// Manually backdate the "old" session
	oldState, _ := store.Load(oldID)
	oldState.Meta.UpdatedAt = time.Now().UTC().Add(-48 * time.Hour)
	// Write directly to bypass the auto-update of UpdatedAt
	data, _ := json.MarshalIndent(oldState, "", "  ")
	os.WriteFile(filepath.Join(dir, oldID+".json"), data, 0600)

	// Prune sessions older than 24 hours
	deleted, err := store.Prune(24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	// Verify old is gone
	_, err = store.Load(oldID)
	if err == nil {
		t.Error("expected old session to be pruned")
	}

	// Verify recent survives
	_, err = store.Load(recentID)
	if err != nil {
		t.Errorf("expected recent session to survive prune: %v", err)
	}
}

func TestFileStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, _ := store.Create(SessionMeta{Title: "atomic test"})

	// Save valid state
	state, _ := store.Load(id)
	state.Messages = []Message{
		{Role: "user", Blocks: []ContentItem{{Type: "text", Text: "first"}}},
	}
	store.Save(id, state)

	// Verify no .tmp files left behind
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("found leftover .tmp file: %s", e.Name())
		}
	}

	// Verify state persisted correctly
	reloaded, _ := store.Load(id)
	if len(reloaded.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(reloaded.Messages))
	}
}

func TestFileStoreLoadNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	_, err = store.Load("ses_doesnotexist")
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
}

func TestFileStoreEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	metas, err := store.List(10)
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(metas))
	}
}

// --- Session Restore Tests (simulating TUI restore flow) ---

func TestSessionSaveAndRestoreWithHistory(t *testing.T) {
	// This test simulates the full TUI flow:
	// 1. Create a session
	// 2. Have a multi-turn conversation with tool use
	// 3. Save the session
	// 4. Load it back and verify all messages are reconstructable

	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Create session
	id, err := store.Create(SessionMeta{
		Title:   "test restore",
		Model:   "test-model",
		Region:  "us-east-1",
		WorkDir: "/home/test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Build a realistic conversation with tool use
	messages := []Message{
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "text", Text: "List the files in /tmp"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "tool_use", ToolUseID: "tu_001", Name: "list_directory", Input: json.RawMessage(`{"path":"/tmp"}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", Content: "file1.txt\nfile2.txt\nsubdir/\n", Status: "success", ResultFor: "tu_001"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "text", Text: "The /tmp directory contains file1.txt, file2.txt, and a subdirectory."},
			},
		},
	}

	// Save session with messages
	state := &SessionState{
		Meta: SessionMeta{
			ID:      id,
			Title:   "test restore",
			Model:   "test-model",
			Region:  "us-east-1",
			WorkDir: "/home/test",
			Stats:   Stats{Turns: 1, ToolCalls: 1},
		},
		Messages: messages,
		Todos: []todo.Item{
			{Content: "List files", Status: "completed", Priority: "high"},
		},
		Inkwell: []InkEntry{
			{
				ToolName:   "list_directory",
				ToolUseID:  "tu_001",
				DurationMs: 5,
				IsError:    false,
			},
		},
	}

	if err := store.Save(id, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load and verify
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify messages survived
	if len(loaded.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(loaded.Messages))
	}

	// Verify we can reconstruct display messages (what the TUI does)
	for i, msg := range loaded.Messages {
		for _, block := range msg.Blocks {
			switch block.Type {
			case "text":
				if block.Text == "" {
					t.Errorf("message %d: empty text block", i)
				}
			case "tool_use":
				if block.Name == "" {
					t.Errorf("message %d: empty tool_use name", i)
				}
				if block.ToolUseID == "" {
					t.Errorf("message %d: empty tool_use id", i)
				}
			case "tool_result":
				if block.ResultFor == "" {
					t.Errorf("message %d: empty result_for", i)
				}
			default:
				t.Errorf("message %d: unexpected block type %q", i, block.Type)
			}
		}
	}

	// Verify the messages can be converted back to Bedrock types
	bedrockMsgs, err := UnmarshalHistory(loaded.Messages)
	if err != nil {
		t.Fatalf("UnmarshalHistory from loaded session: %v", err)
	}
	if len(bedrockMsgs) != 4 {
		t.Fatalf("expected 4 bedrock messages, got %d", len(bedrockMsgs))
	}

	// Verify roles
	if bedrockMsgs[0].Role != types.ConversationRoleUser {
		t.Errorf("msg 0: expected user, got %v", bedrockMsgs[0].Role)
	}
	if bedrockMsgs[1].Role != types.ConversationRoleAssistant {
		t.Errorf("msg 1: expected assistant, got %v", bedrockMsgs[1].Role)
	}
	if bedrockMsgs[2].Role != types.ConversationRoleUser {
		t.Errorf("msg 2: expected user, got %v", bedrockMsgs[2].Role)
	}
	if bedrockMsgs[3].Role != types.ConversationRoleAssistant {
		t.Errorf("msg 3: expected assistant, got %v", bedrockMsgs[3].Role)
	}

	// Verify tool_use block has valid input
	tuBlock, ok := bedrockMsgs[1].Content[0].(*types.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("msg 1: expected tool_use, got %T", bedrockMsgs[1].Content[0])
	}
	if aws.ToString(tuBlock.Value.Name) != "list_directory" {
		t.Errorf("expected list_directory, got %q", aws.ToString(tuBlock.Value.Name))
	}
	if tuBlock.Value.Input == nil {
		t.Fatal("tool_use input should not be nil")
	}

	// Verify todos survived
	if len(loaded.Todos) != 1 {
		t.Fatalf("expected 1 todo, got %d", len(loaded.Todos))
	}
	if loaded.Todos[0].Content != "List files" {
		t.Errorf("expected 'List files', got %q", loaded.Todos[0].Content)
	}

	// Verify inkwell survived
	if len(loaded.Inkwell) != 1 {
		t.Fatalf("expected 1 inkwell entry, got %d", len(loaded.Inkwell))
	}
	if loaded.Inkwell[0].ToolName != "list_directory" {
		t.Errorf("expected 'list_directory', got %q", loaded.Inkwell[0].ToolName)
	}
}

func TestSessionRestoreEmptySession(t *testing.T) {
	// Verify that loading a session with no messages works correctly
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, err := store.Create(SessionMeta{Title: "empty"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded.Messages) != 0 {
		t.Errorf("expected 0 messages for fresh session, got %d", len(loaded.Messages))
	}

	// Should still be convertible to Bedrock types
	bedrockMsgs, err := UnmarshalHistory(loaded.Messages)
	if err != nil {
		t.Fatalf("UnmarshalHistory for empty: %v", err)
	}
	if len(bedrockMsgs) != 0 {
		t.Errorf("expected 0 bedrock messages, got %d", len(bedrockMsgs))
	}
}

func TestUnmarshalHistorySkipsSignaturelessReasoning(t *testing.T) {
	// Reasoning blocks without signatures (e.g. from Gemini via OpenRouter)
	// must not be replayed to Bedrock: Anthropic models reject thinking
	// blocks lacking a signature ("thinking.signature: Field required").
	serialized := []Message{
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "text", Text: "hello"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "reasoning", Text: "unsigned thought"},
				{Type: "text", Text: "answer"},
			},
		},
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(restored))
	}

	assistant := restored[1]
	if len(assistant.Content) != 1 {
		t.Fatalf("expected reasoning block to be dropped, got %d blocks", len(assistant.Content))
	}
	text, ok := assistant.Content[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", assistant.Content[0])
	}
	if text.Value != "answer" {
		t.Errorf("expected 'answer', got %q", text.Value)
	}
}

func TestUnmarshalHistoryReasoningOnlyMessageDowngradesToText(t *testing.T) {
	// An assistant message consisting only of signatureless reasoning must
	// not become empty (Bedrock rejects empty messages) — the reasoning is
	// downgraded to plain text to preserve role alternation.
	serialized := []Message{
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "reasoning", Text: "thinking about the plan"},
			},
		},
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 message, got %d", len(restored))
	}
	if len(restored[0].Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(restored[0].Content))
	}
	text, ok := restored[0].Content[0].(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected ContentBlockMemberText, got %T", restored[0].Content[0])
	}
	if text.Value != "thinking about the plan" {
		t.Errorf("expected reasoning text downgraded to text, got %q", text.Value)
	}
}

func TestUnmarshalHistorySignedReasoningPreserved(t *testing.T) {
	// Reasoning blocks WITH signatures must continue to round-trip intact.
	serialized := []Message{
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "reasoning", Text: "signed thought", Signature: "sig123"},
			},
		},
	}

	restored, err := UnmarshalHistory(serialized)
	if err != nil {
		t.Fatalf("UnmarshalHistory() error: %v", err)
	}
	if len(restored) != 1 || len(restored[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 block, got %+v", restored)
	}
	reasoning, ok := restored[0].Content[0].(*types.ContentBlockMemberReasoningContent)
	if !ok {
		t.Fatalf("expected ContentBlockMemberReasoningContent, got %T", restored[0].Content[0])
	}
	rt, ok := reasoning.Value.(*types.ReasoningContentBlockMemberReasoningText)
	if !ok {
		t.Fatalf("expected ReasoningContentBlockMemberReasoningText, got %T", reasoning.Value)
	}
	if aws.ToString(rt.Value.Signature) != "sig123" {
		t.Errorf("expected signature 'sig123', got %q", aws.ToString(rt.Value.Signature))
	}
}

func TestUnmarshalProviderHistoryDisambiguatesDuplicateToolUseIDs(t *testing.T) {
	serialized := []Message{
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "tool_use", ToolUseID: "call_abc", Name: "read_file", Input: []byte(`{}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", ResultFor: "call_abc", Content: "file 1"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{Type: "tool_use", ToolUseID: "call_abc", Name: "read_file", Input: []byte(`{}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", ResultFor: "call_abc", Content: "file 2"},
			},
		},
	}

	restored := UnmarshalProviderHistory(serialized)
	if len(restored) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(restored))
	}

	tu1 := restored[0].Content[0].(provider.ToolUseBlock)
	tr1 := restored[1].Content[0].(provider.ToolResultBlock)
	tu2 := restored[2].Content[0].(provider.ToolUseBlock)
	tr2 := restored[3].Content[0].(provider.ToolResultBlock)

	if tu1.ToolUseID != "call_abc" {
		t.Errorf("expected first tool use ID 'call_abc', got %q", tu1.ToolUseID)
	}
	if tr1.ToolUseID != "call_abc" {
		t.Errorf("expected first tool result ID 'call_abc', got %q", tr1.ToolUseID)
	}
	if tu2.ToolUseID != "call_abc_2" {
		t.Errorf("expected disambiguated second tool use ID 'call_abc_2', got %q", tu2.ToolUseID)
	}
	if tr2.ToolUseID != "call_abc_2" {
		t.Errorf("expected disambiguated second tool result ID 'call_abc_2', got %q", tr2.ToolUseID)
	}
}

func TestMarshalUnmarshalProviderHistoryReasoningDetailsRoundTrip(t *testing.T) {
	idx0 := 0
	idx1 := 1
	original := []provider.Message{
		{
			Role: provider.RoleAssistant,
			Content: []provider.ContentBlock{
				provider.ReasoningBlock{
					Text:      "thinking through the file read",
					Signature: "legacy-sig",
					ReasoningDetails: []provider.ReasoningDetail{
						{
							Type:      "reasoning.text",
							ID:        "reason_1",
							Format:    "google-gemini-v1",
							Index:     &idx0,
							Text:      "thinking through the file read",
							Signature: "text-sig",
						},
						{
							Type:   "reasoning.encrypted",
							ID:     "call_1",
							Format: "google-gemini-v1",
							Index:  &idx1,
							Data:   "ZW5jcnlwdGVkX3Rob3VnaHRfc2lnbmF0dXJl",
						},
					},
				},
				provider.ToolUseBlock{
					ToolUseID: "call_1",
					Name:      "read_file",
					Input:     json.RawMessage(`{"path":"main.go"}`),
				},
			},
		},
	}

	serialized, err := MarshalProviderHistory(original)
	if err != nil {
		t.Fatalf("MarshalProviderHistory() error: %v", err)
	}

	// Mutate original slice and Index pointer to verify defensive copy in MarshalProviderHistory.
	origRB := original[0].Content[0].(provider.ReasoningBlock)
	origRB.ReasoningDetails[1].Data = "mutated"
	*origRB.ReasoningDetails[0].Index = 99

	// JSON round-trip
	raw, err := json.Marshal(serialized)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	var decoded []Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}

	restored := UnmarshalProviderHistory(decoded)
	if len(restored) != 1 {
		t.Fatalf("expected 1 message, got %d", len(restored))
	}
	if len(restored[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(restored[0].Content))
	}

	rb, ok := restored[0].Content[0].(provider.ReasoningBlock)
	if !ok {
		t.Fatalf("expected provider.ReasoningBlock, got %T", restored[0].Content[0])
	}
	if rb.Text != "thinking through the file read" {
		t.Errorf("expected reasoning text preserved, got %q", rb.Text)
	}
	if rb.Signature != "legacy-sig" {
		t.Errorf("expected signature 'legacy-sig', got %q", rb.Signature)
	}

	wantIdx0 := 0
	wantIdx1 := 1
	wantDetails := []provider.ReasoningDetail{
		{
			Type:      "reasoning.text",
			ID:        "reason_1",
			Format:    "google-gemini-v1",
			Index:     &wantIdx0,
			Text:      "thinking through the file read",
			Signature: "text-sig",
		},
		{
			Type:   "reasoning.encrypted",
			ID:     "call_1",
			Format: "google-gemini-v1",
			Index:  &wantIdx1,
			Data:   "ZW5jcnlwdGVkX3Rob3VnaHRfc2lnbmF0dXJl",
		},
	}
	if !reflect.DeepEqual(rb.ReasoningDetails, wantDetails) {
		t.Errorf("ReasoningDetails mismatch:\n got: %+v\nwant: %+v", rb.ReasoningDetails, wantDetails)
	}
}

func TestUnmarshalProviderHistoryRemapsReasoningDetailIDsOnDuplicateToolUseID(t *testing.T) {
	idx0 := 0
	idx1 := 1
	serialized := []Message{
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{
					Type: "reasoning",
					Text: "first turn reasoning",
					ReasoningDetails: []provider.ReasoningDetail{
						{
							Type:   "reasoning.encrypted",
							ID:     "call_1",
							Format: "google-gemini-v1",
							Index:  &idx0,
							Data:   "sig_turn_1",
						},
					},
				},
				{Type: "tool_use", ToolUseID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", ResultFor: "call_1", Content: "contents of a.go"},
			},
		},
		{
			Role: "assistant",
			Blocks: []ContentItem{
				{
					Type: "reasoning",
					Text: "second turn reasoning",
					ReasoningDetails: []provider.ReasoningDetail{
						{
							Type:   "reasoning.text",
							ID:     "unrelated_id",
							Format: "google-gemini-v1",
							Index:  &idx0,
							Text:   "second turn reasoning",
						},
						{
							Type:   "reasoning.encrypted",
							ID:     "call_1",
							Format: "google-gemini-v1",
							Index:  &idx1,
							Data:   "sig_turn_2",
						},
					},
				},
				{Type: "tool_use", ToolUseID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"b.go"}`)},
			},
		},
		{
			Role: "user",
			Blocks: []ContentItem{
				{Type: "tool_result", ResultFor: "call_1", Content: "contents of b.go"},
			},
		},
	}

	restored := UnmarshalProviderHistory(serialized)
	if len(restored) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(restored))
	}

	rb1 := restored[0].Content[0].(provider.ReasoningBlock)
	tu1 := restored[0].Content[1].(provider.ToolUseBlock)
	tr1 := restored[1].Content[0].(provider.ToolResultBlock)
	rb2 := restored[2].Content[0].(provider.ReasoningBlock)
	tu2 := restored[2].Content[1].(provider.ToolUseBlock)
	tr2 := restored[3].Content[0].(provider.ToolResultBlock)

	if tu1.ToolUseID != "call_1" || tr1.ToolUseID != "call_1" {
		t.Errorf("expected first turn tool IDs 'call_1', got tu=%q tr=%q", tu1.ToolUseID, tr1.ToolUseID)
	}
	if len(rb1.ReasoningDetails) != 1 || rb1.ReasoningDetails[0].ID != "call_1" {
		t.Errorf("expected first turn ReasoningDetail.ID 'call_1', got %+v", rb1.ReasoningDetails)
	}

	if tu2.ToolUseID != "call_1_2" || tr2.ToolUseID != "call_1_2" {
		t.Errorf("expected second turn tool IDs 'call_1_2', got tu=%q tr=%q", tu2.ToolUseID, tr2.ToolUseID)
	}
	if len(rb2.ReasoningDetails) != 2 {
		t.Fatalf("expected 2 ReasoningDetails in second turn, got %d", len(rb2.ReasoningDetails))
	}
	if rb2.ReasoningDetails[0].ID != "unrelated_id" {
		t.Errorf("expected unrelated ReasoningDetail.ID unchanged, got %q", rb2.ReasoningDetails[0].ID)
	}
	if rb2.ReasoningDetails[1].ID != "call_1_2" {
		t.Errorf("expected second turn ReasoningDetail.ID remapped to 'call_1_2', got %q", rb2.ReasoningDetails[1].ID)
	}

	// Verify caller's input slice was not mutated.
	if serialized[2].Blocks[0].ReasoningDetails[1].ID != "call_1" {
		t.Errorf("input serialized slice was mutated: got ID %q, want 'call_1'", serialized[2].Blocks[0].ReasoningDetails[1].ID)
	}
}

func TestMarshalUnmarshalProviderHistory_PreservesMessageProvenance(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 20, 30, 0, time.UTC)
	msgs := []provider.Message{
		{
			ID:        "msg_0001",
			Epoch:     2,
			Turn:      3,
			Timestamp: ts,
			NodeID:    "orchestrator",
			Model:     "openrouter:google/gemini-4-neon",
			Role:      provider.RoleUser,
			Content: []provider.ContentBlock{
				provider.TextBlock{Text: "run audit"},
			},
		},
		{
			ID:        "msg_0002",
			Epoch:     2,
			Turn:      3,
			Timestamp: ts.Add(time.Second),
			NodeID:    "deep_coder",
			Model:     "bedrock:us.anthropic.claude-opus-4-6-v1",
			Role:      provider.RoleAssistant,
			Content: []provider.ContentBlock{
				provider.TextBlock{Text: "audit complete"},
			},
		},
	}

	serialized, err := MarshalProviderHistory(msgs)
	if err != nil {
		t.Fatalf("MarshalProviderHistory: %v", err)
	}
	if len(serialized) != 2 {
		t.Fatalf("expected 2 serialized messages, got %d", len(serialized))
	}
	if serialized[0].ID != "msg_0001" || serialized[0].Epoch != 2 || serialized[0].Turn != 3 || !serialized[0].Timestamp.Equal(ts) || serialized[0].NodeID != "orchestrator" || serialized[0].Model != "openrouter:google/gemini-4-neon" {
		t.Fatalf("unexpected serialized msg[0]: %+v", serialized[0])
	}

	data, err := json.Marshal(serialized)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var unmarshaled []Message
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	restored := UnmarshalProviderHistory(unmarshaled)
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored messages, got %d", len(restored))
	}
	if restored[1].ID != "msg_0002" || restored[1].Epoch != 2 || restored[1].Turn != 3 || !restored[1].Timestamp.Equal(ts.Add(time.Second)) || restored[1].NodeID != "deep_coder" || restored[1].Model != "bedrock:us.anthropic.claude-opus-4-6-v1" {
		t.Fatalf("unexpected restored msg[1]: %+v", restored[1])
	}
}

func TestLegacySessionJSONCompatibility(t *testing.T) {
	legacyJSON := `{
		"meta": {
			"id": "ses_legacy01",
			"created_at": "2026-03-01T10:00:00Z",
			"updated_at": "2026-03-01T10:05:00Z",
			"title": "Legacy session",
			"model": "us.anthropic.claude-opus-4-6-v1",
			"region": "us-west-2",
			"work_dir": "/tmp",
			"stats": {"input_tokens": 100, "output_tokens": 50, "tool_calls": 1, "turns": 1}
		},
		"messages": [
			{
				"role": "user",
				"blocks": [{"type": "text", "text": "inspect legacy"}]
			},
			{
				"role": "assistant",
				"blocks": [{"type": "text", "text": "legacy response"}]
			}
		],
		"todos": [],
		"inkwell": [],
		"audit": {"events": []}
	}`

	var state SessionState
	if err := json.Unmarshal([]byte(legacyJSON), &state); err != nil {
		t.Fatalf("unmarshal legacy session JSON: %v", err)
	}

	if state.Meta.EffectiveEpoch() != 1 {
		t.Errorf("EffectiveEpoch() = %d, want 1 for legacy session", state.Meta.EffectiveEpoch())
	}
	if state.Meta.CurrentEpoch != 0 {
		t.Errorf("CurrentEpoch = %d, want 0 raw", state.Meta.CurrentEpoch)
	}
	if state.OriginalObjective != "" {
		t.Errorf("OriginalObjective = %q, want empty", state.OriginalObjective)
	}
	if len(state.Epochs) != 0 {
		t.Errorf("len(Epochs) = %d, want 0", len(state.Epochs))
	}

	restored := UnmarshalProviderHistory(state.Messages)
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored provider messages, got %d", len(restored))
	}
	if !restored[0].Timestamp.IsZero() {
		t.Errorf("expected legacy message timestamp to be zero, got %v", restored[0].Timestamp)
	}
}

func TestFileStoreEpochArchivesAtomicSaveLoadListAndCascade(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, err := fs.Create(SessionMeta{Model: "openrouter:google/gemini-4-neon", WorkDir: "/workspace"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// List initially has 0 epoch archives
	epochs, err := fs.ListEpochArchives(id)
	if err != nil {
		t.Fatalf("ListEpochArchives: %v", err)
	}
	if len(epochs) != 0 {
		t.Fatalf("expected 0 epoch archives initially, got %v", epochs)
	}

	// Save Epoch 1 archive
	arch1 := &EpochArchive{
		SessionID: id,
		Epoch:     1,
		ClosedAt:  time.Now().UTC(),
		Checkpoint: EpochCheckpoint{
			Epoch:            1,
			Trigger:          RolloverTriggerSoftThreshold,
			TokensAtRollover: 45000,
			ContextWindow:    100000,
			Pinned: PinnedState{
				OriginalObjective: "Build compiler",
				WorkDir:           "/workspace",
				GitBranch:         "main",
			},
			Payload: CheckpointPayload{
				Summary:            "Completed AST parser",
				LessonsLearned:     []string{"Use table-driven tests"},
				KeyFiles:           []FileContextNote{{Path: "parser.go", Purpose: "Core AST logic"}},
				ResumeInstructions: "Implement type checker",
			},
		},
		Messages: []Message{
			{Role: "user", Blocks: []ContentItem{{Type: "text", Text: "Build compiler"}}},
			{Role: "assistant", Blocks: []ContentItem{{Type: "text", Text: "AST parser done"}}},
		},
		Inkwell: []InkEntry{
			{ToolName: "read_file", Output: "package main"},
		},
	}

	relPath1, err := fs.SaveEpochArchive(id, arch1)
	if err != nil {
		t.Fatalf("SaveEpochArchive(1): %v", err)
	}
	wantRel1 := filepath.ToSlash(filepath.Join("epochs", id, "epoch-001.json"))
	if relPath1 != wantRel1 {
		t.Errorf("SaveEpochArchive relPath = %q, want %q", relPath1, wantRel1)
	}

	// Save Epoch 2 archive
	arch2 := &EpochArchive{
		SessionID: id,
		Epoch:     2,
		ClosedAt:  time.Now().UTC(),
		Checkpoint: EpochCheckpoint{
			Epoch:            2,
			Trigger:          RolloverTriggerManual,
			TokensAtRollover: 52000,
			ContextWindow:    100000,
			Pinned: PinnedState{
				OriginalObjective: "Build compiler",
				WorkDir:           "/workspace",
			},
			Payload: CheckpointPayload{
				Summary:            "Completed type checker",
				KeyFiles:           []FileContextNote{{Path: "checker.go", Purpose: "Type checker"}},
				ResumeInstructions: "Implement codegen",
			},
		},
		Messages: []Message{
			{Role: "user", Blocks: []ContentItem{{Type: "text", Text: "Continue type checker"}}},
		},
	}
	if _, err := fs.SaveEpochArchive(id, arch2); err != nil {
		t.Fatalf("SaveEpochArchive(2): %v", err)
	}

	// Verify ListEpochArchives returns [1, 2] sorted
	epochs, err = fs.ListEpochArchives(id)
	if err != nil {
		t.Fatalf("ListEpochArchives: %v", err)
	}
	if !reflect.DeepEqual(epochs, []int{1, 2}) {
		t.Fatalf("ListEpochArchives = %v, want [1, 2]", epochs)
	}

	// Verify LoadEpochArchive retrieves exact data
	loaded1, err := fs.LoadEpochArchive(id, 1)
	if err != nil {
		t.Fatalf("LoadEpochArchive(1): %v", err)
	}
	if loaded1.Epoch != 1 || loaded1.SessionID != id || loaded1.Checkpoint.Payload.Summary != "Completed AST parser" {
		t.Fatalf("unexpected loaded archive 1: %+v", loaded1)
	}
	if len(loaded1.Messages) != 2 || len(loaded1.Inkwell) != 1 {
		t.Fatalf("unexpected messages/inkwell count in loaded archive 1: msgs=%d ink=%d", len(loaded1.Messages), len(loaded1.Inkwell))
	}

	// Verify FileStore.List() does NOT list archive files as top-level sessions
	sessions, err := fs.List(10)
	if err != nil {
		t.Fatalf("fs.List: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != id {
		t.Fatalf("fs.List returned %d sessions (%+v), want exactly 1 session %s", len(sessions), sessions, id)
	}

	// Verify Delete cascades to epoch directory
	if err := fs.Delete(id); err != nil {
		t.Fatalf("fs.Delete: %v", err)
	}
	if _, err := fs.Load(id); err == nil {
		t.Fatal("expected Load to fail after Delete")
	}
	if _, err := fs.LoadEpochArchive(id, 1); err == nil {
		t.Fatal("expected LoadEpochArchive to fail after cascade Delete")
	}
	epochsAfterDelete, err := fs.ListEpochArchives(id)
	if err != nil {
		t.Fatalf("ListEpochArchives after delete: %v", err)
	}
	if len(epochsAfterDelete) != 0 {
		t.Fatalf("expected 0 epoch archives after delete, got %v", epochsAfterDelete)
	}
}

func TestFileStorePruneCascadesEpochArchives(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id, err := fs.Create(SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	arch := &EpochArchive{
		SessionID: id,
		Epoch:     1,
		Checkpoint: EpochCheckpoint{
			Epoch: 1,
			Payload: CheckpointPayload{
				Summary:            "Old epoch",
				KeyFiles:           []FileContextNote{{Path: "a.go", Purpose: "a"}},
				ResumeInstructions: "resume",
			},
		},
	}
	if _, err := fs.SaveEpochArchive(id, arch); err != nil {
		t.Fatalf("SaveEpochArchive: %v", err)
	}

	// Backdate the session file to 10 days ago
	oldTime := time.Now().UTC().Add(-10 * 24 * time.Hour)
	state, err := fs.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state.Meta.UpdatedAt = oldTime
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(fs.filePath(id), data, 0600)

	// Prune older than 5 days
	pruned, err := fs.Prune(5 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d sessions, want 1", pruned)
	}

	// Verify both session file and epoch archive directory are gone
	if _, err := fs.Load(id); err == nil {
		t.Fatal("expected session file to be pruned")
	}
	epochs, err := fs.ListEpochArchives(id)
	if err != nil {
		t.Fatalf("ListEpochArchives: %v", err)
	}
	if len(epochs) != 0 {
		t.Fatalf("expected 0 archives after prune, got %v", epochs)
	}
}

func TestMessageAndContentItemZeroTimestampOmission(t *testing.T) {
	// Zero timestamps on Message and ContentItem should be omitted from JSON (no "0001-01-01T00:00:00Z")
	msg := Message{
		Role: "user",
		Blocks: []ContentItem{
			{
				Type: "text",
				Text: "hello world",
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "0001-01-01") || strings.Contains(string(data), "timestamp") {
		t.Fatalf("expected zero timestamps to be omitted completely, got %s", string(data))
	}

	// Non-zero timestamps should serialize and round-trip cleanly
	now := time.Date(2026, 4, 15, 12, 30, 0, 0, time.UTC)
	msgWithTime := Message{
		Role:      "assistant",
		Timestamp: now,
		Blocks: []ContentItem{
			{
				Type:      "text",
				Text:      "acknowledged",
				Timestamp: now,
			},
		},
	}
	dataWithTime, err := json.Marshal(msgWithTime)
	if err != nil {
		t.Fatalf("Marshal with time: %v", err)
	}
	if !strings.Contains(string(dataWithTime), "2026-04-15T12:30:00Z") {
		t.Fatalf("expected timestamp to be present, got %s", string(dataWithTime))
	}

	var restored Message
	if err := json.Unmarshal(dataWithTime, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !restored.Timestamp.Equal(now) || !restored.Blocks[0].Timestamp.Equal(now) {
		t.Fatalf("restored timestamp mismatch: msg=%v block=%v want=%v", restored.Timestamp, restored.Blocks[0].Timestamp, now)
	}
}

// --- Alias Validation, Meta Updates, Resolution, and Filtering Tests ---

func TestValidateAlias(t *testing.T) {
	tests := []struct {
		name    string
		alias   string
		wantErr bool
	}{
		{"empty alias is valid", "", false},
		{"simple lowercase", "my-session", false},
		{"mixed case and numbers", "Feature123", false},
		{"dots and underscores", "v1.2.3_patch-4", false},
		{"single character", "a", false},
		{"single digit", "1", false},
		{"64 characters valid", strings.Repeat("a", 64), false},
		{"65 characters invalid", strings.Repeat("a", 65), true},
		{"starts with ses_ lowercase", "ses_1234", true},
		{"starts with SES_ uppercase", "SES_MY_ALIAS", true},
		{"starts with Ses_ mixed case", "Ses_project", true},
		{"starts with dash", "-invalid", true},
		{"starts with dot", ".invalid", true},
		{"starts with underscore", "_invalid", true},
		{"contains space", "my alias", true},
		{"contains slash", "my/alias", true},
		{"contains backslash", "my\\alias", true},
		{"contains special symbol", "my@alias", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAlias(tt.alias)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAlias(%q) err = %v, wantErr %v", tt.alias, err, tt.wantErr)
			}
		})
	}
}

func TestUpdateMeta(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	initialTime := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	meta1 := SessionMeta{
		Title:     "Session 1",
		Model:     "bedrock:claude-3-5-sonnet",
		CreatedAt: initialTime,
		UpdatedAt: initialTime,
	}
	id1, err := fs.Create(meta1)
	if err != nil {
		t.Fatalf("Create id1: %v", err)
	}

	// Backdate state explicitly to ensure initialTime is set
	state1, _ := fs.Load(id1)
	state1.Meta.UpdatedAt = initialTime
	_ = os.WriteFile(fs.filePath(id1), []byte(mustJSON(t, state1)), 0600)

	// 1. Toggle Pinned and set Alias, verify UpdatedAt is preserved
	err = fs.UpdateMeta(id1, func(m *SessionMeta) error {
		m.Pinned = true
		m.Alias = "project-alpha"
		m.Morph = "architect"
		m.Tags = []string{"core", "api"}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateMeta id1: %v", err)
	}

	updated1, err := fs.Load(id1)
	if err != nil {
		t.Fatalf("Load updated id1: %v", err)
	}
	if !updated1.Meta.Pinned {
		t.Errorf("expected Pinned = true")
	}
	if updated1.Meta.Alias != "project-alpha" {
		t.Errorf("expected Alias 'project-alpha', got %q", updated1.Meta.Alias)
	}
	if updated1.Meta.Morph != "architect" {
		t.Errorf("expected Morph 'architect', got %q", updated1.Meta.Morph)
	}
	if !reflect.DeepEqual(updated1.Meta.Tags, []string{"core", "api"}) {
		t.Errorf("expected Tags [core, api], got %v", updated1.Meta.Tags)
	}
	if !updated1.Meta.UpdatedAt.Equal(initialTime) {
		t.Errorf("expected UpdatedAt %v preserved, got %v", initialTime, updated1.Meta.UpdatedAt)
	}

	// 2. Create Session 2
	id2, err := fs.Create(SessionMeta{Title: "Session 2"})
	if err != nil {
		t.Fatalf("Create id2: %v", err)
	}

	// 3. Attempt duplicate alias (case-insensitive) on Session 2 -> must fail naming id1
	err = fs.UpdateMeta(id2, func(m *SessionMeta) error {
		m.Alias = "PROJECT-ALPHA"
		return nil
	})
	if err == nil {
		t.Fatal("expected duplicate alias error, got nil")
	}
	if !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), id1) {
		t.Fatalf("expected error naming session %s, got: %v", id1, err)
	}

	// 4. Session 1 can re-save its own alias without error
	err = fs.UpdateMeta(id1, func(m *SessionMeta) error {
		m.Title = "Updated Title"
		return nil
	})
	if err != nil {
		t.Fatalf("re-saving own alias failed: %v", err)
	}

	// 5. Clear alias on Session 1
	err = fs.UpdateMeta(id1, func(m *SessionMeta) error {
		m.Alias = ""
		return nil
	})
	if err != nil {
		t.Fatalf("clearing alias failed: %v", err)
	}

	// 6. Now Session 2 can take "project-alpha"
	err = fs.UpdateMeta(id2, func(m *SessionMeta) error {
		m.Alias = "project-alpha"
		return nil
	})
	if err != nil {
		t.Fatalf("setting released alias on session 2 failed: %v", err)
	}

	// 7. Error cases
	if err := fs.UpdateMeta("ses_nonexistent", func(m *SessionMeta) error { return nil }); err == nil {
		t.Errorf("expected error for non-existent session")
	}
	if err := fs.UpdateMeta(id1, nil); err == nil {
		t.Errorf("expected error for nil fn")
	}
}

func TestResolveMeta(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id1, err := fs.Create(SessionMeta{Title: "Canonical ID Session"})
	if err != nil {
		t.Fatalf("Create id1: %v", err)
	}

	id2, err := fs.Create(SessionMeta{Title: "Aliased Session"})
	if err != nil {
		t.Fatalf("Create id2: %v", err)
	}
	if err := fs.UpdateMeta(id2, func(m *SessionMeta) error {
		m.Alias = "deploy-bot"
		return nil
	}); err != nil {
		t.Fatalf("UpdateMeta alias: %v", err)
	}

	// 1. Resolve by canonical ID (exact and with spaces)
	meta, err := fs.ResolveMeta(id1)
	if err != nil {
		t.Fatalf("ResolveMeta(id1) error: %v", err)
	}
	if meta.ID != id1 || meta.Title != "Canonical ID Session" {
		t.Fatalf("unexpected meta for id1: %+v", meta)
	}

	meta, err = fs.ResolveMeta("  " + id1 + "  ")
	if err != nil {
		t.Fatalf("ResolveMeta trimmed id1 error: %v", err)
	}
	if meta.ID != id1 {
		t.Fatalf("unexpected meta ID: %s", meta.ID)
	}

	// 2. Resolve by alias (exact and case-insensitive)
	meta, err = fs.ResolveMeta("deploy-bot")
	if err != nil {
		t.Fatalf("ResolveMeta('deploy-bot') error: %v", err)
	}
	if meta.ID != id2 || meta.Alias != "deploy-bot" {
		t.Fatalf("unexpected meta for alias: %+v", meta)
	}

	meta, err = fs.ResolveMeta("DEPLOY-BOT")
	if err != nil {
		t.Fatalf("ResolveMeta('DEPLOY-BOT') error: %v", err)
	}
	if meta.ID != id2 {
		t.Fatalf("unexpected meta ID for case-insensitive alias: %s", meta.ID)
	}

	// 3. Error cases
	if _, err := fs.ResolveMeta("ses_nonexistent"); err == nil {
		t.Errorf("expected error for non-existent session ID")
	}
	if _, err := fs.ResolveMeta("nonexistent-alias"); err == nil {
		t.Errorf("expected error for non-existent alias")
	}
	if _, err := fs.ResolveMeta("   "); err == nil {
		t.Errorf("expected error for empty string")
	}
}

func TestListSortingWithPinned(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	// Create 4 sessions:
	// s1: unpinned, t4 (most recent unpinned)
	// s2: pinned, t2 (older pinned)
	// s3: pinned, t3 (newer pinned)
	// s4: unpinned, t1 (oldest unpinned)

	s1ID, _ := fs.Create(SessionMeta{Title: "Unpinned Recent"})
	setSessionTimeAndPin(t, fs, s1ID, base.Add(4*time.Hour), false)

	s2ID, _ := fs.Create(SessionMeta{Title: "Pinned Older"})
	setSessionTimeAndPin(t, fs, s2ID, base.Add(2*time.Hour), true)

	s3ID, _ := fs.Create(SessionMeta{Title: "Pinned Newer"})
	setSessionTimeAndPin(t, fs, s3ID, base.Add(3*time.Hour), true)

	s4ID, _ := fs.Create(SessionMeta{Title: "Unpinned Oldest"})
	setSessionTimeAndPin(t, fs, s4ID, base.Add(1*time.Hour), false)

	// List all
	metas, err := fs.List(10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(metas) != 4 {
		t.Fatalf("expected 4 sessions, got %d", len(metas))
	}

	// Pinned should come first sorted by UpdatedAt desc (s3 then s2), then unpinned sorted by UpdatedAt desc (s1 then s4)
	expectedOrder := []string{s3ID, s2ID, s1ID, s4ID}
	for i, wantID := range expectedOrder {
		if metas[i].ID != wantID {
			t.Errorf("metas[%d] = %s (pinned=%v, title=%s), want %s", i, metas[i].ID, metas[i].Pinned, metas[i].Title, wantID)
		}
	}

	// List with limit=2 should return both pinned sessions and NOT evict them
	top2, err := fs.List(2)
	if err != nil {
		t.Fatalf("List(2) error: %v", err)
	}
	if len(top2) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(top2))
	}
	if top2[0].ID != s3ID || top2[1].ID != s2ID {
		t.Fatalf("expected pinned sessions [%s, %s], got [%s, %s]", s3ID, s2ID, top2[0].ID, top2[1].ID)
	}
}

func TestPrunePreservesPinnedSessions(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	now := time.Now().UTC()
	oldTime := now.Add(-10 * 24 * time.Hour)
	recentTime := now.Add(-1 * time.Hour)

	// s1: unpinned, old -> should be pruned
	s1, _ := fs.Create(SessionMeta{Title: "Old Unpinned"})
	setSessionTimeAndPin(t, fs, s1, oldTime, false)

	// s2: pinned, old -> should be PRESERVED
	s2, _ := fs.Create(SessionMeta{Title: "Old Pinned"})
	setSessionTimeAndPin(t, fs, s2, oldTime, true)

	// s3: unpinned, recent -> should be PRESERVED
	s3, _ := fs.Create(SessionMeta{Title: "Recent Unpinned"})
	setSessionTimeAndPin(t, fs, s3, recentTime, false)

	deleted, err := fs.Prune(5 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("Prune deleted %d sessions, want 1", deleted)
	}

	if _, err := fs.Load(s1); err == nil {
		t.Errorf("expected session s1 (unpinned old) to be pruned")
	}
	if _, err := fs.Load(s2); err != nil {
		t.Errorf("expected session s2 (pinned old) to be preserved: %v", err)
	}
	if _, err := fs.Load(s3); err != nil {
		t.Errorf("expected session s3 (recent unpinned) to be preserved: %v", err)
	}
}

func TestFilterSessions(t *testing.T) {
	metas := []SessionMeta{
		{
			ID:      "ses_0001",
			Alias:   "auth-service",
			Title:   "Fix authentication tokens",
			Model:   "openrouter:anthropic/claude-3-7-sonnet",
			Morph:   "deep_coder",
			WorkDir: "/workspace/backend",
			Pinned:  true,
			Tags:    []string{"security", "auth", "v1"},
		},
		{
			ID:      "ses_0002",
			Alias:   "frontend-ui",
			Title:   "Redesign navbar component",
			Model:   "openrouter:google/gemini-2.5-flash",
			Morph:   "ui_specialist",
			WorkDir: "/workspace/frontend",
			Pinned:  false,
			Tags:    []string{"ui", "react"},
		},
		{
			ID:      "ses_0003",
			Alias:   "infra-deploy",
			Title:   "Setup Terraform and AWS",
			Model:   "openrouter:anthropic/claude-3-7-sonnet",
			Morph:   "devops",
			WorkDir: "/workspace/backend",
			Pinned:  true,
			Tags:    []string{"infra", "terraform"},
		},
	}

	// 1. PinnedOnly
	pinned := FilterSessions(metas, ListFilter{PinnedOnly: true})
	if len(pinned) != 2 || pinned[0].ID != "ses_0001" || pinned[1].ID != "ses_0003" {
		t.Errorf("PinnedOnly failed: got %v", pinned)
	}

	// 2. WorkDir (exact, cleaned path)
	backend := FilterSessions(metas, ListFilter{WorkDir: "/workspace/backend/."})
	if len(backend) != 2 || backend[0].ID != "ses_0001" || backend[1].ID != "ses_0003" {
		t.Errorf("WorkDir filter failed: got %v", backend)
	}

	frontend := FilterSessions(metas, ListFilter{WorkDir: "/workspace/frontend"})
	if len(frontend) != 1 || frontend[0].ID != "ses_0002" {
		t.Errorf("WorkDir filter frontend failed: got %v", frontend)
	}

	// 3. Query match across fields
	// Query ID
	res := FilterSessions(metas, ListFilter{Query: "0002"})
	if len(res) != 1 || res[0].ID != "ses_0002" {
		t.Errorf("Query ID failed: got %v", res)
	}

	// Query Alias
	res = FilterSessions(metas, ListFilter{Query: "AUTH-SERVICE"})
	if len(res) != 1 || res[0].ID != "ses_0001" {
		t.Errorf("Query Alias failed: got %v", res)
	}

	// Query Title
	res = FilterSessions(metas, ListFilter{Query: "navbar"})
	if len(res) != 1 || res[0].ID != "ses_0002" {
		t.Errorf("Query Title failed: got %v", res)
	}

	// Query Model
	res = FilterSessions(metas, ListFilter{Query: "gemini"})
	if len(res) != 1 || res[0].ID != "ses_0002" {
		t.Errorf("Query Model failed: got %v", res)
	}

	// Query Morph
	res = FilterSessions(metas, ListFilter{Query: "DEVOPS"})
	if len(res) != 1 || res[0].ID != "ses_0003" {
		t.Errorf("Query Morph failed: got %v", res)
	}

	// Query Tag
	res = FilterSessions(metas, ListFilter{Query: "security"})
	if len(res) != 1 || res[0].ID != "ses_0001" {
		t.Errorf("Query Tag failed: got %v", res)
	}

	// Query WorkDir substring
	res = FilterSessions(metas, ListFilter{Query: "backend"})
	if len(res) != 2 {
		t.Errorf("Query WorkDir substring failed: got %d results", len(res))
	}

	// Combined PinnedOnly + WorkDir + Query
	res = FilterSessions(metas, ListFilter{
		PinnedOnly: true,
		WorkDir:    "/workspace/backend",
		Query:      "terraform",
	})
	if len(res) != 1 || res[0].ID != "ses_0003" {
		t.Errorf("Combined filter failed: got %v", res)
	}

	// No match returns empty slice
	res = FilterSessions(metas, ListFilter{Query: "nonexistent-query-string"})
	if res == nil || len(res) != 0 {
		t.Errorf("expected empty non-nil slice, got %v", res)
	}

	// Empty input slice
	res = FilterSessions(nil, ListFilter{Query: "test"})
	if res == nil || len(res) != 0 {
		t.Errorf("expected empty non-nil slice for nil metas, got %v", res)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return string(data)
}

func setSessionTimeAndPin(t *testing.T, fs *FileStore, id string, updatedAt time.Time, pinned bool) {
	t.Helper()
	state, err := fs.Load(id)
	if err != nil {
		t.Fatalf("setSessionTimeAndPin load: %v", err)
	}
	state.Meta.UpdatedAt = updatedAt
	state.Meta.Pinned = pinned
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("setSessionTimeAndPin marshal: %v", err)
	}
	if err := os.WriteFile(fs.filePath(id), data, 0600); err != nil {
		t.Fatalf("setSessionTimeAndPin write: %v", err)
	}
}
