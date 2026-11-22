package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Modzybear/codecuttlectl/internal/provider"
)

func TestConverse_Basic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		// Verify request body
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.Model != "qwen/qwen3.8-max" {
			t.Errorf("model=%q, want qwen/qwen3.8-max", req.Model)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming")
		}
		if len(req.Messages) != 2 { // system + user
			t.Errorf("messages=%d, want 2", len(req.Messages))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatCompletion{
			ID:    "test-1",
			Model: "qwen/qwen3.8-max",
			Choices: []chatChoice{
				{
					Index:        0,
					Message:      chatMessage{Role: "assistant", Content: "hello!"},
					FinishReason: "stop",
				},
			},
			Usage: &chatUsage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
		})
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "qwen/qwen3.8-max"})

	resp, err := client.Converse(context.Background(), provider.Request{
		System: "You are helpful.",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{
				provider.TextBlock{Text: "hi"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if resp.Content != "hello!" {
		t.Errorf("content=%q, want hello!", resp.Content)
	}
	if resp.StopReason != "stop" {
		t.Errorf("stop=%q, want stop", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("input_tokens=%d, want 10", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens=%d, want 5", resp.Usage.OutputTokens)
	}
}

func TestBuildRequest_ZDRAndFallbacks(t *testing.T) {
	c := New(Config{
		Model:      "qwen/qwen3.8-max",
		Fallbacks:  []string{"anthropic/claude-3-5-sonnet"},
		EnforceZDR: true,
	})

	body := c.buildRequest(provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "hi"}}},
		},
	}, false)

	var req chatRequest
	json.Unmarshal(body, &req)

	if req.Provider == nil {
		t.Fatalf("expected Provider settings, got nil")
	}
	if !req.Provider.ZDR {
		t.Errorf("expected ZDR=true")
	}
	if req.Provider.DataCollection != "deny" {
		t.Errorf("expected DataCollection=deny, got %q", req.Provider.DataCollection)
	}
	if len(req.Models) != 1 {
		t.Fatalf("models len=%d, want 1", len(req.Models))
	}
	if req.Models[0] != "anthropic/claude-3-5-sonnet" {
		t.Errorf("expected fallback in Models, got %v", req.Models)
	}
}

func TestConverseStream_ToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			// Tool call starts
			`{"id":"1","object":"chat.completion.chunk","created":1000,"model":"qwen/qwen3.8-max","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_xyz","index":0,"type":"function","function":{"name":"list_dir","arguments":""}}]},"finish_reason":null}]}`,
			// Tool call arguments streamed
			`{"id":"1","object":"chat.completion.chunk","created":1000,"model":"qwen/qwen3.8-max","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"path\""}}]},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","created":1000,"model":"qwen/qwen3.8-max","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":":\".\"}"}}]},"finish_reason":null}]}`,
			// Finish
			`{"id":"1","object":"chat.completion.chunk","created":1000,"model":"qwen/qwen3.8-max","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}

		for _, chunk := range chunks {
			io.WriteString(w, "data: "+chunk+"\n\n")
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "qwen/qwen3.8-max"})

	ch := client.ConverseStream(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{
				provider.TextBlock{Text: "list files"},
			}},
		},
		Tools: []provider.ToolDefinition{
			{Name: "list_dir", Description: "List files", InputSchema: json.RawMessage(`{}`)},
		},
	})

	var toolName, toolID string
	var toolInput strings.Builder
	var gotStart, gotStop, gotMsgStop bool

	for ev := range ch {
		switch e := ev.(type) {
		case provider.ToolUseStartEvent:
			gotStart = true
			toolName = e.Name
			toolID = e.ToolUseID
		case provider.ToolInputDeltaEvent:
			toolInput.WriteString(e.Delta)
		case provider.ToolUseStopEvent:
			gotStop = true
		case provider.MessageStopEvent:
			gotMsgStop = true
			if e.StopReason != "tool_calls" {
				t.Errorf("stop=%q, want tool_calls", e.StopReason)
			}
		case provider.StreamErrorEvent:
			t.Fatalf("unexpected error: %v", e.Err)
		}
	}

	if !gotStart {
		t.Error("expected ToolUseStartEvent")
	}
	if toolName != "list_dir" {
		t.Errorf("tool_name=%q, want list_dir", toolName)
	}
	if toolID != "call_xyz" {
		t.Errorf("tool_id=%q, want call_xyz", toolID)
	}
	if toolInput.String() != `{"path":"."}` {
		t.Errorf("tool_input=%q, want {\"path\":\".\"}", toolInput.String())
	}
	if !gotStop {
		t.Error("expected ToolUseStopEvent")
	}
	if !gotMsgStop {
		t.Error("expected MessageStopEvent")
	}
}

func TestProviderMsgToOAI_ReasoningOnlyAssistant(t *testing.T) {
	// An assistant message containing only reasoning must not serialize to an
	// empty assistant message — some upstream providers (e.g. Google) reject
	// those with 400 INVALID_ARGUMENT. Reasoning is downgraded to content.
	msg := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{
			provider.ReasoningBlock{Text: "planning the next step"},
		},
	}

	out := providerMsgToOAI(msg)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].Content != "planning the next step" {
		t.Errorf("content=%q, want reasoning text as content", out[0].Content)
	}
}

func TestProviderMsgToOAI_EmptyAssistantDropped(t *testing.T) {
	// A completely empty assistant message is dropped rather than sent.
	msg := provider.Message{
		Role:    provider.RoleAssistant,
		Content: nil,
	}

	out := providerMsgToOAI(msg)
	if len(out) != 0 {
		t.Fatalf("expected empty assistant message to be dropped, got %d messages", len(out))
	}
}

func TestProviderMsgToOAI_ReasoningWithTextAndTools(t *testing.T) {
	// When text or tool calls are present, reasoning is dropped (not replayed)
	// and the rest of the message is preserved.
	msg := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{
			provider.ReasoningBlock{Text: "thinking"},
			provider.TextBlock{Text: "here is the answer"},
			provider.ToolUseBlock{
				ToolUseID: "call_1",
				Name:      "read_file",
				Input:     []byte(`{"path":"a.go"}`),
			},
		},
	}

	out := providerMsgToOAI(msg)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].Content != "here is the answer" {
		t.Errorf("content=%q, want text only (reasoning dropped)", out[0].Content)
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "call_1" {
		t.Errorf("tool calls not preserved: %+v", out[0].ToolCalls)
	}
}

func TestProviderMsgToOAI_EchoesReasoningDetails(t *testing.T) {
	idx0 := 0
	msg := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{
			provider.ReasoningBlock{
				ReasoningDetails: []provider.ReasoningDetail{
					{Type: "reasoning.encrypted", ID: "call_1", Format: "google-gemini-v1", Index: &idx0, Data: "enc_payload"},
				},
			},
			provider.ToolUseBlock{
				ToolUseID: "call_1",
				Name:      "read_file",
				Input:     []byte(`{"path":"a.go"}`),
			},
		},
	}

	out := providerMsgToOAI(msg)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if len(out[0].ReasoningDetails) != 1 || out[0].ReasoningDetails[0].Data != "enc_payload" || out[0].ReasoningDetails[0].ID != "call_1" {
		t.Fatalf("reasoning_details not echoed: %+v", out[0].ReasoningDetails)
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls missing: %+v", out[0].ToolCalls)
	}
}

func TestProviderMsgToOAI_ToolResultsBeforeUserText(t *testing.T) {
	msg := provider.Message{
		Role: provider.RoleUser,
		Content: []provider.ContentBlock{
			provider.TextBlock{Text: "follow-up note"},
			provider.ToolResultBlock{ToolUseID: "call_1", Name: "read_file", Content: "file body"},
		},
	}

	out := providerMsgToOAI(msg)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(out), out)
	}
	if out[0].Role != "tool" || out[0].ToolCallID != "call_1" {
		t.Fatalf("expected tool result first, got %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "follow-up note" {
		t.Fatalf("expected user text second, got %+v", out[1])
	}
}

func TestBuildRequest_CoalescesConsecutiveUserMessages(t *testing.T) {
	c := New(Config{Model: "google/gemini-4-neon"})
	raw := c.buildRequest(provider.Request{
		System: "sys",
		Messages: []provider.Message{
			provider.BuildUserTextMessage("first user msg"),
			provider.BuildUserTextMessage("second user msg"),
			provider.BuildAssistantMessage([]provider.ContentBlock{
				provider.ToolUseBlock{ToolUseID: "call_1", Name: "read_file", Input: []byte(`{}`)},
			}),
			provider.BuildToolResultMessage([]provider.ToolResultBlock{
				{ToolUseID: "call_1", Name: "read_file", Content: "ok"},
			}),
			provider.BuildUserTextMessage("nudge 1"),
			provider.BuildUserTextMessage("nudge 2"),
		},
	}, false)

	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	// Expected roles: system, user (coalesced), assistant, tool, user (coalesced)
	wantRoles := []string{"system", "user", "assistant", "tool", "user"}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("got %d messages (%+v), want %d", len(req.Messages), req.Messages, len(wantRoles))
	}
	for i, want := range wantRoles {
		if req.Messages[i].Role != want {
			t.Errorf("msg[%d].Role = %q, want %q", i, req.Messages[i].Role, want)
		}
	}
	if req.Messages[1].Content != "first user msg\n\nsecond user msg" {
		t.Errorf("coalesced user[1] = %q", req.Messages[1].Content)
	}
	if req.Messages[4].Content != "nudge 1\n\nnudge 2" {
		t.Errorf("coalesced user[4] = %q", req.Messages[4].Content)
	}
}

func TestCompletionToResponse_CopiesReasoningDetailsAndCost(t *testing.T) {
	idx0 := 0
	costVal := 0.0042
	resp := completionToResponse(chatCompletion{
		Choices: []chatChoice{
			{
				FinishReason: "stop",
				Message: chatMessage{
					Role:      "assistant",
					Content:   "done",
					Reasoning: "thought",
					ReasoningDetails: []provider.ReasoningDetail{
						{Type: "reasoning.encrypted", ID: "c1", Format: "google-gemini-v1", Index: &idx0, Data: "enc"},
					},
				},
			},
		},
		Usage: &chatUsage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
			Cost:             &costVal,
		},
	})
	if resp.Reasoning != "thought" {
		t.Errorf("Reasoning = %q, want thought", resp.Reasoning)
	}
	if len(resp.ReasoningDetails) != 1 || resp.ReasoningDetails[0].Data != "enc" {
		t.Errorf("ReasoningDetails = %+v, want 1 item", resp.ReasoningDetails)
	}
	if resp.Usage.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v, want 0.0042", resp.Usage.CostUSD)
	}
}
