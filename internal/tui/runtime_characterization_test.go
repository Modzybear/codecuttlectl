package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/Modzybear/codecuttlectl/internal/conversation"
	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/prompt"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/provider/openrouter"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/swarm"
)

// KnownDivergence tests assert the measured baseline, NOT the desired contract.
// Replace these assertions with desired invariants in the linked repair PR.
type recordingRuntimeProvider struct {
	provider.Provider
	id       string
	requests []string // immutable JSON snapshots, not aliases of live requests
}

func (p *recordingRuntimeProvider) ID() string   { return p.id }
func (p *recordingRuntimeProvider) Name() string { return p.id }
func (p *recordingRuntimeProvider) ConverseStream(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
	data, err := json.Marshal(req)
	if err != nil {
		panic(err) // fixture only: requests must be serializable
	}
	p.requests = append(p.requests, string(data))
	ch := make(chan provider.StreamEvent)
	close(ch)
	return ch
}

type runtimePool struct {
	provider.Pool
	nodes map[string]provider.Provider
}

func (p runtimePool) GetNode(id string) (provider.Provider, bool) { v, ok := p.nodes[id]; return v, ok }
func (p runtimePool) Primary() provider.Provider                  { return p.nodes["lead"] }

func TestTUICatalogAndTodoAuthorization(t *testing.T) {
	manager := pluginhost.NewManager(false)
	p := &recordingRuntimeProvider{id: "fixture:lead"}
	agent, err := conversation.NewAgent(conversation.Config{Provider: p, PluginMgr: manager, Workbench: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Provider: p, PluginMgr: manager, Agent: agent})
	m.launchStream()
	defer m.stopModelStream()
	if len(p.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(p.requests))
	}
	if strings.Contains(p.requests[0], "todo_manage") {
		t.Fatalf("expected tool catalog to exclude todo_manage when restricted by workbench, got %s", p.requests[0])
	}
	input := json.RawMessage(`{"todos":[{"content":"forged native task","status":"pending","priority":"medium"}]}`)
	if _, status := agent.ExecuteTool(context.Background(), "todo_manage", input); status != types.ToolResultStatusError {
		t.Fatal("restricted Agent should deny forged todo call")
	}

	// Verify that TUI executePendingTools properly respects workbench sandboxing and denies unauthorized todo_manage
	m.pendingToolCalls = []pendingTool{{id: "forged", name: "todo_manage", input: input}}
	result := m.executePendingTools()().(ContinueStreamMsg)
	if len(result.Messages) != 1 || !result.Messages[0].IsError || !strings.Contains(result.Messages[0].Content, "not authorized") {
		t.Fatalf("expected TUI to deny unauthorized todo_manage with error result, got: %+v", result.Messages)
	}

	// Verify ContinueStreamMsg todo application also guards against unauthorized workbench mutation
	updated, _ := m.Update(ContinueStreamMsg{
		Messages:   []provider.ToolResultBlock{{ToolUseID: "forged", Name: "todo_manage", Content: ""}},
		TodoInputs: []json.RawMessage{input},
	})
	m = updated.(Model)
	if !m.todos.IsEmpty() {
		t.Fatalf("expected m.todos to remain empty after denied forged todo_manage, got %v", m.todos.Items())
	}
}

func TestTUIApprovalPreservesEarlierResultsAndExecutesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	dir := t.TempDir()
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "cuttlebone-runtime-fixture"), "../../testdata/runtime-plugin")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	manager := pluginhost.NewManager(false)
	defer manager.Shutdown()
	if err := manager.DiscoverPlugins(ctx, dir); err != nil {
		t.Fatal(err)
	}
	gated := json.RawMessage(`{"subcommand":"rebase","args":["-i"]}`)
	for _, approve := range []bool{false, true} {
		work := t.TempDir()
		agent, err := conversation.NewAgent(conversation.Config{PluginMgr: manager, WorkDir: work})
		if err != nil {
			t.Fatal(err)
		}
		m := New(Config{Agent: agent, PluginMgr: manager, WorkDir: work})
		m.pendingToolCalls = []pendingTool{
			{id: "first", name: "git", input: json.RawMessage(`{"subcommand":"status"}`)},
			{id: "gated", name: "git", input: gated},
		}
		request := m.executePendingTools()().(ApprovalRequestMsg)
		if len(request.CompletedResults) != 1 || request.CompletedResults[0].ToolUseID != "first" {
			t.Fatal("fixture did not execute first tool before approval")
		}
		updated, _ := m.Update(request)
		m = updated.(Model)
		updated, execute := m.Update(ApprovalDecisionMsg{ToolUseID: "gated", Approved: approve})
		m = updated.(Model)
		results := execute().(ContinueStreamMsg)
		if len(results.Messages) != 2 {
			t.Fatalf("expected 2 results (first + gated), got %d: %+v", len(results.Messages), results.Messages)
		}
		if results.Messages[0].ToolUseID != "first" || results.Messages[0].IsError {
			t.Fatalf("expected first tool result preserved with success, got %+v", results.Messages[0])
		}
		if results.Messages[1].ToolUseID != "gated" {
			t.Fatalf("expected second tool result to be gated, got %+v", results.Messages[1])
		}
		if approve {
			if results.Messages[1].IsError {
				t.Fatalf("expected approved tool to succeed without double-gate denial, got %s", results.Messages[1].Content)
			}
			calls, err := os.ReadFile(filepath.Join(work, "fixture-calls.log"))
			if err != nil || strings.Count(string(calls), "\n") != 2 {
				t.Fatalf("expected fixture to execute status + rebase: %q %v", calls, err)
			}
		} else {
			if !results.Messages[1].IsError || !strings.Contains(results.Messages[1].Content, "denied by user") {
				t.Fatalf("expected denied tool error result, got %+v", results.Messages[1])
			}
			calls, err := os.ReadFile(filepath.Join(work, "fixture-calls.log"))
			if err != nil || strings.Count(string(calls), "\n") != 1 {
				t.Fatalf("expected fixture to execute only status: %q %v", calls, err)
			}
		}
	}

	// Control: the same inert gated tool DOES run once with an Agent approval callback.
	work := t.TempDir()
	decisions := 0
	agent, err := conversation.NewAgent(conversation.Config{PluginMgr: manager, WorkDir: work, ApprovalFunc: func(_, _, _, _ string) bool { decisions++; return true }})
	if err != nil {
		t.Fatal(err)
	}
	if _, status := agent.ExecuteTool(ctx, "git", gated); status != types.ToolResultStatusSuccess || decisions != 1 {
		t.Fatal("Agent control approval did not execute once")
	}
	calls, err := os.ReadFile(filepath.Join(work, "fixture-calls.log"))
	if err != nil || strings.Count(string(calls), "\n") != 1 {
		t.Fatal("Agent control side effect count != 1")
	}
}

func TestHandoffUpdatesTUIProviderAndPersona(t *testing.T) {
	manager := pluginhost.NewManager(false)
	source := &recordingRuntimeProvider{id: "bedrock:claude-3-5"}
	target := &recordingRuntimeProvider{id: "openrouter:deepseek"}
	pool := runtimePool{nodes: map[string]provider.Provider{"lead": source, "review": target}}
	morph := &swarm.Morphology{Nodes: map[string]swarm.Node{
		"lead":   {Provider: "bedrock", Model: source.id, IsPrimary: true},
		"review": {Provider: "openrouter", Model: target.id, SystemPrompt: "TARGET_PERSONA", Workbench: []string{"read_file", "handoff"}},
	}}
	pm, err := prompt.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.NewAgent(conversation.Config{Provider: source, Pool: pool, PluginMgr: manager, PromptMgr: pm, Morph: morph, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Provider: source, Pool: pool, Agent: agent, PluginMgr: manager, Morph: morph, System: "SOURCE_PERSONA"})
	m.pendingToolCalls = []pendingTool{{id: "handoff", name: "handoff", input: json.RawMessage(`{"target":"review","instructions":"review"}`)}}
	result := m.executePendingTools()().(ContinueStreamMsg)
	if result.Messages[0].IsError || agent.ActiveNode() != "review" || !strings.Contains(agent.SystemPrompt(), "TARGET_PERSONA") {
		t.Fatal("fixture did not switch Agent node/persona")
	}
	updated, _ := m.Update(result)
	m = updated.(Model)
	defer m.stopModelStream()
	if len(target.requests) != 1 || !strings.Contains(m.system, "TARGET_PERSONA") {
		t.Fatalf("expected next TUI request on target provider and updated persona, got target.requests=%d, m.system=%q", len(target.requests), m.system)
	}
	// Test NodeChangedMsg listener re-arm
	updatedNodeMsg, nodeCmd := m.Update(swarm.NodeChangedMsg{Source: "lead", Target: "review", ProviderID: "openrouter:deepseek"})
	if updatedNodeMsg == nil || nodeCmd == nil {
		t.Errorf("expected NodeChangedMsg to return valid model and re-arm listener command")
	}

	// Test context window reset for provider without ContextWindowProvider
	if m.contextWindow != 0 {
		t.Errorf("expected contextWindow to be 0 for recordingRuntimeProvider without ContextWindowProvider interface, got %d", m.contextWindow)
	}

	// Test return handoff: target reviews and hands back to lead
	m.pendingToolCalls = []pendingTool{{id: "handoff_back", name: "handoff", input: json.RawMessage(`{"target":"lead","instructions":"completed review"}`)}}
	resultBack := m.executePendingTools()().(ContinueStreamMsg)
	if resultBack.Messages[0].IsError || agent.ActiveNode() != "lead" {
		t.Fatalf("fixture did not switch Agent back to lead: %+v", resultBack.Messages)
	}
	updatedBack, _ := m.Update(resultBack)
	mBack := updatedBack.(Model)
	defer mBack.stopModelStream()
	if len(source.requests) != 1 {
		t.Fatalf("expected next TUI request to route back to source provider, got source.requests=%d", len(source.requests))
	}
	if mBack.llmProvider.ID() != "bedrock:claude-3-5" {
		t.Fatalf("expected TUI llmProvider restored to lead, got %s", mBack.llmProvider.ID())
	}
}

func TestCrossProviderHandoffTwoEndpointTransportRoundTrip(t *testing.T) {
	// Endpoint A: simulates non-Gemini provider (e.g. DeepSeek/Claude via OpenRouter)
	// Rejects any google-gemini-v1 reasoning_details if leaked back on return handoff.
	var endpointACalls int
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		endpointACalls++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "google-gemini-v1") {
			http.Error(w, `{"error":{"message":"foreign google-gemini-v1 reasoning_details leaked to endpoint A","code":400}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if endpointACalls == 1 {
			// Step 1 on Node A: call handoff to Gemini node B (with NO Gemini thought signature)
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"reasoning":"delegating to gemini specialist","tool_calls":[{"index":0,"id":"call_handoff_to_b","type":"function","function":{"name":"handoff","arguments":"{\"target\":\"gemini_node\",\"instructions\":\"inspect and return\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			))
			return
		}
		// Step 3 on Node A (after return handoff from B): final answer
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"Round-trip handoff completed cleanly!"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
	}))
	defer srvA.Close()

	// Endpoint B: simulates OpenRouter Gemini endpoint ("openrouter:google/gemini-4-neon")
	// Strictly rejects any assistant tool_calls message that lacks google-gemini-v1 reasoning.encrypted (Issue #62).
	var endpointBCalls int
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		endpointBCalls++
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Messages []struct {
				Role             string                      `json:"role"`
				ToolCalls        [] json.RawMessage          `json:"tool_calls"`
				ReasoningDetails []provider.ReasoningDetail  `json:"reasoning_details"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &payload)
		for _, msg := range payload.Messages {
			if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
				hasEncrypted := false
				for _, rd := range msg.ReasoningDetails {
					if rd.Type == "reasoning.encrypted" && rd.Format == "google-gemini-v1" {
						hasEncrypted = true
						break
					}
				}
				if !hasEncrypted {
					http.Error(w, `{"error":{"message":"Function call is missing a thought_signature","code":400}}`, http.StatusBadRequest)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		// Node B emits a Gemini-signed handoff back to Node A
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"reasoning":"returning to lead","reasoning_details":[{"type":"reasoning.encrypted","id":"call_handoff_to_a","format":"google-gemini-v1","index":0,"data":"GEMINI_SIG_XYZ"}],"tool_calls":[{"index":0,"id":"call_handoff_to_a","type":"function","function":{"name":"handoff","arguments":"{\"target\":\"lead_node\",\"instructions\":\"done\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		))
	}))
	defer srvB.Close()

	provA := openrouter.New(openrouter.Config{BaseURL: srvA.URL, Model: "deepseek/deepseek-r1"})
	provB := openrouter.New(openrouter.Config{BaseURL: srvB.URL, Model: "google/gemini-4-neon"})
	pool := runtimePool{nodes: map[string]provider.Provider{
		"lead_node":   provA,
		"gemini_node": provB,
	}}
	morph := &swarm.Morphology{Nodes: map[string]swarm.Node{
		"lead_node":   {Provider: "openrouter", Model: "deepseek/deepseek-r1", IsPrimary: true, Workbench: []string{"handoff"}},
		"gemini_node": {Provider: "openrouter", Model: "google/gemini-4-neon", Workbench: []string{"handoff"}},
	}}
	pm, err := prompt.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	manager := pluginhost.NewManager(false)
	agent, err := conversation.NewAgent(conversation.Config{
		Provider:  provA,
		Pool:      pool,
		PluginMgr: manager,
		PromptMgr: pm,
		Morph:     morph,
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := agent.StreamTurn(context.Background(), "start round trip", nil)
	if err != nil {
		t.Fatalf("StreamTurn round-trip failed: %v", err)
	}
	if reply != "Round-trip handoff completed cleanly!" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if endpointACalls != 2 || endpointBCalls != 1 {
		t.Fatalf("unexpected endpoint call counts: A=%d (want 2), B=%d (want 1)", endpointACalls, endpointBCalls)
	}
}

func TestTUIMaxStepsParity(t *testing.T) {
	// With MaxSteps=1, after 1 round of tool execution, TUI must halt and not issue another request
	p := &recordingRuntimeProvider{
		id: "fixture:lead",
	}
	manager := pluginhost.NewManager(false)
	m := New(Config{
		Provider:  p,
		PluginMgr: manager,
		MaxSteps:  1,
	})

	m.submitMessage("start tool run")
	defer m.stopModelStream()

	// Simulate tool result returned from the first tool step
	updated, cmd := m.Update(ContinueStreamMsg{
		Messages: []provider.ToolResultBlock{
			{ToolUseID: "t1", Name: "read_file", Content: "file content"},
		},
	})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected nil command after reaching maxSteps=1")
	}
	if m.streaming {
		t.Fatal("expected streaming=false after maxSteps exceeded")
	}
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].content, "exceeded maximum steps (1)") {
		t.Fatalf("expected max steps error message in TUI, got %+v", m.messages)
	}
}

func TestTUIInkwellAdviceDoesNotLeakAcrossTurnsOrCloseout(t *testing.T) {
	p := &recordingRuntimeProvider{id: "fixture:lead"}
	manager := pluginhost.NewManager(false)
	agent, err := conversation.NewAgent(conversation.Config{
		Provider:  p,
		PluginMgr: manager,
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Record a failed tool execution in Inkwell
	_, status := agent.ExecuteTool(context.Background(), "nonexistent_tool", json.RawMessage(`{}`))
	if status != types.ToolResultStatusError {
		t.Fatalf("expected error status, got %v", status)
	}
	if agent.InkwellAdvice() == "" {
		t.Fatal("expected non-empty InkwellAdvice after tool failure")
	}

	m := New(Config{
		Provider:  p,
		Agent:     agent,
		PluginMgr: manager,
	})
	defer m.stopModelStream()

	// Step 0 (fresh user turn): must NOT inject stale Inkwell advice
	m.submitMessage("fresh user turn")
	if len(p.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(p.requests))
	}
	if strings.Contains(p.requests[0], "Inkwell Diagnostic Alert") {
		t.Fatalf("fresh turn (streamStep=0) must not inject stale Inkwell advice: %q", p.requests[0])
	}

	// Continuation step (streamStep > 0) in StateNormal: MUST inject Inkwell advice
	m.streamStep = 1
	m.launchStream()
	if len(p.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(p.requests))
	}
	if !strings.Contains(p.requests[1], "Inkwell Diagnostic Alert") {
		t.Fatalf("continuation step (streamStep=1) must inject Inkwell advice: %q", p.requests[1])
	}

	// Rollover closeout (even with streamStep > 0): must NOT inject Inkwell advice
	m.rolloverState = rollover.StateCloseout
	m.launchStream()
	if len(p.requests) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(p.requests))
	}
	if strings.Contains(p.requests[2], "Inkwell Diagnostic Alert") {
		t.Fatalf("rollover closeout must not inject Inkwell advice: %q", p.requests[2])
	}
}

func TestTUITodoManageMatchesByNameNotEmptyContent(t *testing.T) {
	p := &recordingRuntimeProvider{id: "fixture:lead"}
	m := New(Config{
		Provider:  p,
		PluginMgr: pluginhost.NewManager(false),
	})
	defer m.stopModelStream()

	updated, _ := m.Update(ContinueStreamMsg{
		Messages: []provider.ToolResultBlock{
			{ToolUseID: "bash_1", Name: "bash_exec", Content: ""},
			{ToolUseID: "todo_1", Name: "todo_manage", Content: ""},
		},
		TodoInputs: []json.RawMessage{
			json.RawMessage(`{"todos":[{"content":"task A","status":"in_progress","priority":"high"}]}`),
		},
	})
	m = updated.(Model)

	if len(m.history) == 0 {
		t.Fatal("expected tool result message appended to history")
	}
	last := m.history[len(m.history)-1]
	if len(last.Content) != 2 {
		t.Fatalf("expected 2 tool result blocks, got %d", len(last.Content))
	}
	bashRes := last.Content[0].(provider.ToolResultBlock)
	todoRes := last.Content[1].(provider.ToolResultBlock)
	if bashRes.Content != "" {
		t.Fatalf("bash_exec empty output was overwritten by todo_manage: %q", bashRes.Content)
	}
	if !strings.Contains(todoRes.Content, "Todo list updated") {
		t.Fatalf("expected todo_manage placeholder to be populated, got: %q", todoRes.Content)
	}
}

func TestTUIAutoApproveBypassesAgentApprovalGate(t *testing.T) {
	p := &recordingRuntimeProvider{id: "fixture:lead"}
	manager := pluginhost.NewManager(false)
	// Agent has AutoApprove=false and no ApprovalFunc, while TUI Model has AutoApprove=true.
	agent, err := conversation.NewAgent(conversation.Config{
		Provider:    p,
		PluginMgr:   manager,
		WorkDir:     t.TempDir(),
		AutoApprove: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	m := New(Config{
		Provider:    p,
		Agent:       agent,
		PluginMgr:   manager,
		AutoApprove: true,
	})
	m.pendingToolCalls = []pendingTool{
		{
			id:    "call_rm",
			name:  "bash_exec",
			input: json.RawMessage(`{"command":"rm -rf /tmp/nonexistent_cuttle_test_dir"}`),
		},
	}

	msg := m.executePendingTools()()
	cont, ok := msg.(ContinueStreamMsg)
	if !ok {
		t.Fatalf("expected ContinueStreamMsg under autoApprove, got %T", msg)
	}
	if len(cont.Messages) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(cont.Messages))
	}
	if strings.Contains(cont.Messages[0].Content, "requires user approval") {
		t.Fatalf("TUI autoApprove should bypass Agent approval gate via ExecuteToolApproved, got: %q", cont.Messages[0].Content)
	}
}
