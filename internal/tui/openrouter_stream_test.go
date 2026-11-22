package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/provider/openrouter"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

func TestOpenRouterReasoningToolsAndLateUsageReachTUI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range []string{
			`{"choices":[{"delta":{"reasoning":"plan"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{\"pattern\":"}},{"index":0,"id":"a","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"usage":{"prompt_tokens":1234,"completion_tokens":42}}`,
			`{"usage":{}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.history = []provider.Message{provider.BuildUserTextMessage("inspect files")}
	m.streaming = true
	m.streamCh = openrouter.New(openrouter.Config{BaseURL: server.URL}).ConverseStream(ctx, provider.Request{})
	// Consume through the real adapter and TUI reader. Do not run the command
	// returned by StreamDone: that is the actual tool execution boundary.
	for steps := 0; steps < 30; steps++ {
		msg := m.readNextStreamEvent()()
		updated, cmd := m.Update(msg)
		m = updated.(Model)
		if errMsg, ok := msg.(StreamErrorMsg); ok {
			t.Fatal(errMsg.Err)
		}
		if _, ok := msg.(StreamDoneMsg); !ok {
			continue
		}
		if cmd == nil || len(m.lastExecutedTools) != 2 {
			t.Fatalf("no continuation for tools: %+v", m.lastExecutedTools)
		}
		if m.lastExecutedTools[0].id != "a" || string(m.lastExecutedTools[0].input) != `{"path":"a.go"}` || m.lastExecutedTools[1].id != "b" || string(m.lastExecutedTools[1].input) != `{"pattern":"x"}` {
			t.Fatalf("corrupted tool calls: %+v", m.lastExecutedTools)
		}
		if len(m.history) != 2 || len(m.history[1].Content) != 3 {
			t.Fatalf("reasoning and two tools missing from history: %+v", m.history)
		}
		if m.lastCallInputTokens != 1234 || m.totalInputTokens != 1234 || m.totalOutputTokens != 42 {
			t.Fatalf("incorrect usage: context=%d input=%d output=%d", m.lastCallInputTokens, m.totalInputTokens, m.totalOutputTokens)
		}
		return
	}
	t.Fatal("stream failed to reach tool continuation")
}

func TestPartialUsagePreservesTUIContextButExplicitZeroDoesNot(t *testing.T) {
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.history = []provider.Message{provider.BuildUserTextMessage("keep this history")}
	m.lastCallInputTokens = 500
	m.lastCallCacheReadInputTokens = 100
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.UsageEvent{OutputTokens: 7, InputTokensUnknown: true}
	close(ch)
	m.streamCh = ch
	msg := m.readNextStreamEvent()()
	updated, _ := m.Update(msg)
	m = updated.(Model)
	if m.lastCallInputTokens != 500 || m.lastCallCacheReadInputTokens != 100 || m.totalOutputTokens != 7 {
		t.Fatal("partial usage erased known context or lost output tokens")
	}
	updated, _ = m.Update(StreamUsageMsg{})
	m = updated.(Model)
	if m.lastCallInputTokens != 0 || m.lastCallCacheReadInputTokens != 0 {
		t.Fatal("explicit zero usage must remain authoritative")
	}
	if len(m.history) != 1 {
		t.Fatal("usage handling mutated history")
	}
}

func TestOpenRouterPrematureEOFSurfacesInTUI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"plan\"}}]}\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.streaming = true
	m.history = []provider.Message{provider.BuildUserTextMessage("inspect")}
	m.streamCh = openrouter.New(openrouter.Config{BaseURL: server.URL}).ConverseStream(ctx, provider.Request{})
	for steps := 0; steps < 10; steps++ {
		msg := m.readNextStreamEvent()()
		updated, _ := m.Update(msg)
		m = updated.(Model)
		if _, ok := msg.(StreamDoneMsg); ok {
			t.Fatal("premature EOF became successful completion")
		}
		if _, ok := msg.(StreamErrorMsg); ok {
			if m.streaming || len(m.history) != 1 || len(m.pendingToolCalls) != 0 {
				t.Fatal("failed stream continued execution or changed conversation history")
			}
			if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].content, "before finish reason") {
				t.Fatal("error was not visible in the UI")
			}
			return
		}
	}
	t.Fatal("no terminal error")
}

func TestOpenRouterEncryptedReasoningDetailsPersistThroughTUIAndSessionRestore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range []string{
			`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","index":0,"format":"google-gemini-v1","data":"enc_sig_1"}],"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
	}))
	defer server.Close()

	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	prov := openrouter.New(openrouter.Config{BaseURL: server.URL, Model: "google/gemini-4-neon"})
	m := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
	})
	m.history = []provider.Message{provider.BuildUserTextMessage("read a.go")}
	m.streaming = true
	m.streamCh = prov.ConverseStream(ctx, provider.Request{})

	for steps := 0; steps < 20; steps++ {
		msg := m.readNextStreamEvent()()
		updated, _ := m.Update(msg)
		m = updated.(Model)
		if errMsg, ok := msg.(StreamErrorMsg); ok {
			t.Fatal(errMsg.Err)
		}
		if _, ok := msg.(StreamDoneMsg); ok {
			break
		}
	}

	if len(m.history) != 2 {
		t.Fatalf("expected 2 history messages, got %d", len(m.history))
	}
	rb, ok := m.history[1].Content[0].(provider.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock at history[1].Content[0], got %T", m.history[1].Content[0])
	}
	if len(rb.ReasoningDetails) != 1 || rb.ReasoningDetails[0].Data != "enc_sig_1" || rb.ReasoningDetails[0].ID != "call_1" {
		t.Fatalf("unexpected ReasoningDetails in TUI history: %+v", rb.ReasoningDetails)
	}

	// Save and restore via New() -> restoreSession() and verify lossless round-trip.
	m.saveSession()
	restored := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
	})
	if len(restored.history) != 2 {
		t.Fatalf("expected 2 restored messages, got %d", len(restored.history))
	}
	restoredRB, ok := restored.history[1].Content[0].(provider.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock after restoreSession, got %T", restored.history[1].Content[0])
	}
	if len(restoredRB.ReasoningDetails) != 1 || restoredRB.ReasoningDetails[0].Data != "enc_sig_1" || restoredRB.ReasoningDetails[0].ID != "call_1" {
		t.Fatalf("ReasoningDetails lost across restoreSession: %+v", restoredRB.ReasoningDetails)
	}
}

func TestNeedsGroundingAssistAndBatchedCompaction(t *testing.T) {
	// Cloud provider with unknown (0) context window should NOT enable small-model grounding.
	orProv := openrouter.New(openrouter.Config{Model: "google/gemini-4-neon"})
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.llmProvider = orProv
	m.contextWindow = 0
	if m.needsGroundingAssist() {
		t.Error("expected openrouter provider with unknown context window (0) not to trigger needsGroundingAssist")
	}

	// Provider explicitly reporting small context window (128k) SHOULD enable grounding.
	m.contextWindow = 131_072
	if !m.needsGroundingAssist() {
		t.Error("expected provider with 131k context window to trigger needsGroundingAssist")
	}

	// Large context window (1M) should NOT enable grounding.
	m.contextWindow = 1_048_576
	if m.needsGroundingAssist() {
		t.Error("expected 1M context window not to trigger needsGroundingAssist")
	}

	// Verify batched mid-turn compaction on large model: 15 rounds (3 stale < MinCompactBatch=6)
	// stays untouched mid-turn (streamStep > 0) to preserve prefix cache, while 18 rounds (6 stale)
	// compacts in a single batch.
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %02d: payload content for cache-preserving batch compaction", i))
	}
	bigContent := strings.Join(lines, "\n")
	buildRounds := func(n int) []provider.Message {
		msgs := []provider.Message{provider.BuildUserTextMessage("long task")}
		for i := 1; i <= n; i++ {
			id := fmt.Sprintf("c_%d", i)
			msgs = append(msgs,
				provider.BuildAssistantMessage([]provider.ContentBlock{
					provider.ToolUseBlock{ToolUseID: id, Name: "read_file", Input: []byte(`{}`)},
				}),
				provider.BuildToolResultMessage([]provider.ToolResultBlock{
					{ToolUseID: id, Name: "read_file", Content: bigContent},
				}),
			)
		}
		return msgs
	}

	m.history = buildRounds(15)
	m.streamStep = 5
	m.maybeCompact()
	tr1 := m.history[2].Content[0].(provider.ToolResultBlock)
	if tr1.Content != bigContent {
		t.Error("expected mid-turn compaction with only 3 stale rounds (< batch 6) to be deferred")
	}

	m.history = buildRounds(18)
	m.streamStep = 5
	m.maybeCompact()
	tr1 = m.history[2].Content[0].(provider.ToolResultBlock)
	if len(tr1.Content) >= len(bigContent) {
		t.Error("expected mid-turn compaction to fire once 6 stale rounds accumulate")
	}
}

func TestRestoreSessionSanitizesLegacyUnsignedGeminiToolCalls(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a legacy pre-PR #75 session with unsigned tool_use + plain-text reasoning (no reasoning_details).
	legacyMsgs := []provider.Message{
		provider.BuildUserTextMessage("Audit the repo"),
		{
			Role: provider.RoleAssistant,
			Content: []provider.ContentBlock{
				provider.ReasoningBlock{Text: "Checking files first"},
				provider.ToolUseBlock{
					ToolUseID: "call_legacy_1",
					Name:      "read_file",
					Input:     json.RawMessage(`{"path":"README.md"}`),
				},
			},
		},
		provider.BuildToolResultMessage([]provider.ToolResultBlock{
			{ToolUseID: "call_legacy_1", Name: "read_file", Content: "readme contents"},
		}),
		{
			Role:    provider.RoleAssistant,
			Content: []provider.ContentBlock{provider.TextBlock{Text: "Audit complete."}},
		},
	}
	serialized, err := session.MarshalProviderHistory(legacyMsgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sessID, &session.SessionState{
		Meta:     session.SessionMeta{ID: sessID, Model: "openrouter:google/gemini-4-neon"},
		Messages: serialized,
	}); err != nil {
		t.Fatal(err)
	}

	prov := openrouter.New(openrouter.Config{Model: "google/gemini-4-neon"})
	restored := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
	})

	if restored.totalToolCalls != 1 {
		t.Fatalf("expected totalToolCalls=1 preserved for status bar, got %d", restored.totalToolCalls)
	}
	var sawToolCallUI, sawToolResultUI bool
	for _, cm := range restored.messages {
		if cm.role == "tool_call" {
			sawToolCallUI = true
		}
		if cm.role == "tool_result" {
			sawToolResultUI = true
		}
	}
	if !sawToolCallUI || !sawToolResultUI {
		t.Fatalf("expected original UI tool_call/tool_result cards preserved, got %+v", restored.messages)
	}

	// Wire history must have zero unsigned ToolUseBlock/ToolResultBlock for OpenRouter Gemini.
	var sawTranscodedCall, sawTranscodedResult bool
	for _, msg := range restored.history {
		for _, blk := range msg.Content {
			switch b := blk.(type) {
			case provider.ToolUseBlock:
				t.Fatalf("unexpected unsigned ToolUseBlock left in restored Gemini history: %+v", b)
			case provider.ToolResultBlock:
				t.Fatalf("unexpected ToolResultBlock left in restored Gemini history: %+v", b)
			case provider.TextBlock:
				if strings.Contains(b.Text, "[Prior tool call: read_file(") {
					sawTranscodedCall = true
				}
				if strings.Contains(b.Text, "[Prior tool result (read_file, ok): readme contents]") {
					sawTranscodedResult = true
				}
			}
		}
	}
	if !sawTranscodedCall || !sawTranscodedResult {
		t.Fatalf("expected transcoded prior tool call/result text blocks, got history=%+v", restored.history)
	}
}
