package conversation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

func TestIsToolAllowed(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		workbench []string
		want      bool
	}{
		{"nil workbench allows all", "bash_exec", nil, true},
		{"empty workbench allows all", "read_file", []string{}, true},
		{"wildcard allows all", "write_file", []string{"*", "read_file"}, true},
		{"exact match allowed", "read_file", []string{"read_file", "list_directory"}, true},
		{"missing tool rejected", "bash_exec", []string{"read_file", "list_directory"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsToolAllowed(tc.tool, tc.workbench)
			if got != tc.want {
				t.Errorf("IsToolAllowed(%q, %v) = %v, want %v", tc.tool, tc.workbench, got, tc.want)
			}
		})
	}
}

func TestExecuteTool_SandboxEnforcement(t *testing.T) {
	agent := &Agent{
		workbench: []string{"read_file", "list_directory"},
		todos:     todo.NewList(),
	}

	// Allowed tool (should pass sandbox, but fail further down because we didn't mock pluginMgr.
	// But let's check a built-in that IS allowed, or just observe the error message.
	// Actually, if it's rejected by sandbox, we get a specific error string.
	res, status := agent.executeTool(context.Background(), "bash_exec", json.RawMessage(`{}`))

	if status != types.ToolResultStatusError {
		t.Errorf("expected error status for unauthorized tool, got %v", status)
	}

	if !strings.Contains(res, "not authorized in this node's workbench") {
		t.Errorf("expected sandbox error message, got: %v", res)
	}

	// Try a tool that is allowed but doesn't exist in our mock (will panic or hit pluginMgr nil error,
	// but we just want to ensure it DOESN'T return the sandbox error).
	// Let's use a built-in tool that is allowed, like "todo_manage", by adding it to workbench.
	agent.workbench = append(agent.workbench, "todo_manage")
	res, status = agent.executeTool(context.Background(), "todo_manage", json.RawMessage(`{"todos":[]}`))

	if strings.Contains(res, "not authorized") {
		t.Errorf("did not expect sandbox error for authorized tool, got: %v", res)
	}
}

type stubAgentProvider struct {
	id            string
	contextWindow int32
	converseResp  *provider.Response
	streamEvents  []provider.StreamEvent
}

func (s *stubAgentProvider) ID() string            { return s.id }
func (s *stubAgentProvider) Name() string          { return s.id }
func (s *stubAgentProvider) ContextWindow() int32  { return s.contextWindow }
func (s *stubAgentProvider) Converse(_ context.Context, _ provider.Request) (*provider.Response, error) {
	return s.converseResp, nil
}
func (s *stubAgentProvider) ConverseStream(_ context.Context, _ provider.Request) <-chan provider.StreamEvent {
	ch := make(chan provider.StreamEvent, len(s.streamEvents))
	for _, ev := range s.streamEvents {
		ch <- ev
	}
	close(ch)
	return ch
}

func TestAgentNeedsGroundingAssist(t *testing.T) {
	tests := []struct {
		name string
		prov provider.Provider
		want bool
	}{
		{"nil provider", nil, false},
		{"openrouter unknown window (0)", &stubAgentProvider{id: "openrouter:google/gemini-4-neon", contextWindow: 0}, false},
		{"openrouter 1M window", &stubAgentProvider{id: "openrouter:google/gemini-4-neon", contextWindow: 1_048_576}, false},
		{"openrouter small 128k window", &stubAgentProvider{id: "openrouter:meta-llama/llama-3", contextWindow: 131_072}, true},
		{"bedrock 1M window", &stubAgentProvider{id: "bedrock:us.anthropic.claude-opus-4-6-v1", contextWindow: 1_000_000}, false},
		{"ollama unknown window", &stubAgentProvider{id: "ollama:gemma4:31b", contextWindow: 0}, true},
		{"ollama 262k window", &stubAgentProvider{id: "ollama:gemma4:31b", contextWindow: 262_144}, true},
		{"fallback wrapping ollama", provider.NewFallbackProvider("local", &stubAgentProvider{id: "ollama:gemma4:31b", contextWindow: 0}), true},
		{"fallback wrapping openrouter 1M", provider.NewFallbackProvider("orchestrator", &stubAgentProvider{id: "openrouter:google/gemini-4-neon", contextWindow: 1_048_576}), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{provider: tc.prov}
			if got := a.needsGroundingAssist(); got != tc.want {
				t.Errorf("needsGroundingAssist() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentProviderCapturesEncryptedReasoningDetails(t *testing.T) {
	idx0 := 0
	detail := provider.ReasoningDetail{
		Type:   "reasoning.encrypted",
		ID:     "call_1",
		Format: "google-gemini-v1",
		Index:  &idx0,
		Data:   "enc_payload",
	}

	// Non-streaming path (turnProvider)
	nonStreamProv := &stubAgentProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 1_048_576,
		converseResp: &provider.Response{
			Content:          "final answer",
			ReasoningDetails: []provider.ReasoningDetail{detail},
			StopReason:       "stop",
		},
	}
	a1 := &Agent{provider: nonStreamProv, maxSteps: 2, todos: todo.NewList()}
	if _, err := a1.turnProvider(context.Background(), "hi"); err != nil {
		t.Fatalf("turnProvider: %v", err)
	}
	if len(a1.provHistory) != 2 {
		t.Fatalf("expected 2 history messages, got %d", len(a1.provHistory))
	}
	rb1, ok := a1.provHistory[1].Content[0].(provider.ReasoningBlock)
	if !ok || len(rb1.ReasoningDetails) != 1 || rb1.ReasoningDetails[0].Data != "enc_payload" {
		t.Fatalf("turnProvider dropped ReasoningDetails: %+v", a1.provHistory[1].Content)
	}

	// Streaming path (streamTurnProvider) with encrypted-only reasoning (no plaintext reasoning deltas)
	streamProv := &stubAgentProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 1_048_576,
		streamEvents: []provider.StreamEvent{
			provider.ReasoningSignatureEvent{Details: []provider.ReasoningDetail{detail}},
			provider.TextDeltaEvent{Text: "done"},
			provider.MessageStopEvent{StopReason: "stop"},
		},
	}
	a2 := &Agent{provider: streamProv, maxSteps: 2, todos: todo.NewList()}
	if _, err := a2.streamTurnProvider(context.Background(), "hi", nil); err != nil {
		t.Fatalf("streamTurnProvider: %v", err)
	}
	if len(a2.provHistory) != 2 {
		t.Fatalf("expected 2 history messages, got %d", len(a2.provHistory))
	}
	rb2, ok := a2.provHistory[1].Content[0].(provider.ReasoningBlock)
	if !ok || len(rb2.ReasoningDetails) != 1 || rb2.ReasoningDetails[0].Data != "enc_payload" {
		t.Fatalf("streamTurnProvider dropped encrypted-only ReasoningDetails: %+v", a2.provHistory[1].Content)
	}
}
