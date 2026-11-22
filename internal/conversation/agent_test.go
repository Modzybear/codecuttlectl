package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/swarm"
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
	res, status := agent.executeTool(context.Background(), "bash_exec", json.RawMessage(`{}`), false)

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
	res, status = agent.executeTool(context.Background(), "todo_manage", json.RawMessage(`{"todos":[]}`), false)

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

type scriptedConverseProvider struct {
	id            string
	contextWindow int32
	responses     []*provider.Response
	errs          []error
	requests      []provider.Request
}

func (s *scriptedConverseProvider) ID() string           { return s.id }
func (s *scriptedConverseProvider) Name() string         { return s.id }
func (s *scriptedConverseProvider) ContextWindow() int32 { return s.contextWindow }
func (s *scriptedConverseProvider) Converse(_ context.Context, req provider.Request) (*provider.Response, error) {
	s.requests = append(s.requests, req)
	idx := len(s.requests) - 1
	if idx < len(s.errs) && s.errs[idx] != nil {
		return nil, s.errs[idx]
	}
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return &provider.Response{Content: "done"}, nil
}
func (s *scriptedConverseProvider) ConverseStream(_ context.Context, _ provider.Request) <-chan provider.StreamEvent {
	ch := make(chan provider.StreamEvent)
	close(ch)
	return ch
}

func TestAgentRolloverArchivesInkwellAndSeedsNextEpoch(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	invalidJSON := json.RawMessage(`{"summary":"missing fields"}`)
	validJSON := json.RawMessage(`{
		"summary": "Completed Epoch 1 investigation",
		"lessons_learned": ["Check filestore subdirectory scanning"],
		"key_files": [{"path": "internal/session/filestore.go", "purpose": "Session storage"}],
		"resume_instructions": "Start Phase 2 controller"
	}`)

	prov := &scriptedConverseProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 1_048_576,
		responses: []*provider.Response{
			{
				ToolUses: []provider.ToolUseRequest{
					{ToolUseID: "c1", Name: rollover.SaveCheckpointToolName, Input: invalidJSON},
				},
			},
			{
				ToolUses: []provider.ToolUseRequest{
					{ToolUseID: "c2", Name: rollover.SaveCheckpointToolName, Input: validJSON},
				},
			},
		},
	}

	a := &Agent{
		provider:          prov,
		store:             store,
		sessionID:         sessID,
		workDir:           t.TempDir(),
		maxSteps:          5,
		todos:             todo.NewList(),
		currentEpoch:      1,
		turn:              3,
		originalObjective: "Build Epoch Rollover",
		provHistory: []provider.Message{
			provider.BuildUserTextMessage("Build Epoch Rollover"),
			provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "Initial work done"}}),
		},
		inkwell: []session.InkEntry{
			{ToolName: "read_file", Output: "full file contents"},
			{ToolName: "grep", Output: "matches"},
		},
	}

	cp, err := a.Rollover(context.Background(), rollover.DocsPolicyNone, "focus on tests")
	if err != nil {
		t.Fatalf("Agent.Rollover: %v", err)
	}
	if cp.Epoch != 1 || a.CurrentEpoch() != 2 {
		t.Fatalf("expected Epoch 1 -> 2, got cp.Epoch=%d current=%d", cp.Epoch, a.CurrentEpoch())
	}
	if len(a.inkwell) != 0 {
		t.Fatalf("expected active inkwell cleared after archival, got %d entries", len(a.inkwell))
	}
	if a.archivedToolCalls != 2 {
		t.Fatalf("archivedToolCalls = %d, want 2", a.archivedToolCalls)
	}

	// Verify Epoch 1 archive preserved both messages and Inkwell entries
	arch, err := store.LoadEpochArchive(sessID, 1)
	if err != nil {
		t.Fatalf("LoadEpochArchive(1): %v", err)
	}
	if len(arch.Messages) != 2 || len(arch.Inkwell) != 2 {
		t.Fatalf("archive mismatch: msgs=%d inkwell=%d", len(arch.Messages), len(arch.Inkwell))
	}

	// Verify active session file on disk has Epoch 2, cumulative ToolCalls==2, empty Inkwell, and bootstrap seed
	saved, err := store.Load(sessID)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if saved.Meta.CurrentEpoch != 2 || saved.Meta.Stats.ToolCalls != 2 || len(saved.Inkwell) != 0 || len(saved.Epochs) != 1 {
		t.Fatalf("saved active session mismatch: epoch=%d toolCalls=%d ink=%d epochs=%d",
			saved.Meta.CurrentEpoch, saved.Meta.Stats.ToolCalls, len(saved.Inkwell), len(saved.Epochs))
	}
	if len(a.provHistory) != 1 || !strings.Contains(a.provHistory[0].Content[0].(provider.TextBlock).Text, "Completed Epoch 1 investigation") {
		t.Fatalf("unexpected Epoch 2 bootstrap history: %+v", a.provHistory)
	}
}

func TestAgentRolloverFailureRestoresHistory(t *testing.T) {
	prov := &scriptedConverseProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 1_048_576,
		errs:          []error{context.Canceled},
	}
	a := &Agent{
		provider:     prov,
		maxSteps:     5,
		todos:        todo.NewList(),
		currentEpoch: 1,
		provHistory: []provider.Message{
			provider.BuildUserTextMessage("keep me"),
		},
	}
	if _, err := a.Rollover(context.Background(), rollover.DocsPolicyNone, ""); err == nil {
		t.Fatal("expected error when closeout provider call fails")
	}
	if a.CurrentEpoch() != 1 || len(a.provHistory) != 1 {
		t.Fatalf("expected original history preserved on rollback: epoch=%d len=%d", a.CurrentEpoch(), len(a.provHistory))
	}
}

func TestAgentHeadlessAutoRolloverMidTurnAndSwarmGuard(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	validCheckpoint := json.RawMessage(`{
		"summary": "Reached hard threshold mid-turn",
		"lessons_learned": ["Step counter resets cleanly on new epoch"],
		"key_files": [{"path": "internal/conversation/rollover.go", "purpose": "Agent rollover"}],
		"resume_instructions": "Return final result in Epoch 2"
	}`)

	prov := &scriptedConverseProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 100_000,
		responses: []*provider.Response{
			// Turn 1: normal reply at 20% context
			{
				Content: "turn 1 done",
				Usage:   provider.Usage{InputTokens: 20_000, OutputTokens: 100},
			},
			// Turn 2, step 0: calls tool_info and hits 70% (hard threshold 65%)
			{
				ToolUses: []provider.ToolUseRequest{
					{ToolUseID: "ti_1", Name: "tool_info", Input: json.RawMessage(`{}`)},
				},
				Usage: provider.Usage{InputTokens: 70_000, OutputTokens: 100},
			},
			// Automatic Closeout call: calls save_checkpoint
			{
				ToolUses: []provider.ToolUseRequest{
					{ToolUseID: "cp_1", Name: rollover.SaveCheckpointToolName, Input: validCheckpoint},
				},
				Usage: provider.Usage{InputTokens: 72_000, OutputTokens: 100},
			},
			// Epoch 2 continuation (same Turn 2, fresh context!)
			{
				Content: "completed cleanly in Epoch 2",
				Usage:   provider.Usage{InputTokens: 5_000, OutputTokens: 50},
			},
		},
	}

	rcfg := rollover.DefaultConfig()
	rcfg.AutoRollover = true
	rcfg.SoftPct = 0.45
	rcfg.HardPct = 0.65
	rcfg.MinEpochTurns = 2

	a := &Agent{
		provider:     prov,
		store:        store,
		sessionID:    sessID,
		workDir:      t.TempDir(),
		maxSteps:     3,
		todos:        todo.NewList(),
		currentEpoch: 1,
		rolloverCtrl: rollover.NewController(rcfg),
	}

	if _, err := a.turnProvider(context.Background(), "turn 1 task"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if a.CurrentEpoch() != 1 {
		t.Fatalf("expected Epoch 1 after turn 1, got %d", a.CurrentEpoch())
	}

	out, err := a.turnProvider(context.Background(), "turn 2 heavy task")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if out != "completed cleanly in Epoch 2" {
		t.Fatalf("turn 2 output = %q, want %q", out, "completed cleanly in Epoch 2")
	}
	if a.CurrentEpoch() != 2 || len(a.Epochs()) != 1 || a.Epochs()[0].Trigger != session.RolloverTriggerHardThreshold {
		t.Fatalf("expected hard-threshold rollover to Epoch 2, got epoch=%d epochs=%+v", a.CurrentEpoch(), a.Epochs())
	}

	// Verify non-orchestrator swarm node NEVER triggers automatic rollover even at 95%
	a.morph = &swarm.Morphology{
		Nodes: map[string]swarm.Node{
			"orchestrator": {IsPrimary: true},
			"deep_coder":   {IsPrimary: false},
		},
	}
	a.activeNode = "deep_coder"
	a.epochTurns = 5
	a.lastCallTokens = 95_000
	a.rolloverCtrl.MarkUsageSeen()
	if a.maybeAutoRollover(context.Background(), true, false) {
		t.Fatal("non-orchestrator swarm node must never trigger auto-rollover")
	}
}

type nodePoolStub struct {
	provider.Pool
	primary provider.Provider
	nodes   map[string]provider.Provider
}

func (p nodePoolStub) Primary() provider.Provider { return p.primary }
func (p nodePoolStub) GetNode(id string) (provider.Provider, bool) {
	prov, ok := p.nodes[id]
	return prov, ok
}

func TestNewAgentInitialNodeResolvesTargetProfile(t *testing.T) {
	leadProv := &scriptedConverseProvider{id: "bedrock:lead"}
	workerProv := &scriptedConverseProvider{id: "openrouter:google/gemini-4-neon"}
	pool := nodePoolStub{
		primary: leadProv,
		nodes: map[string]provider.Provider{
			"orchestrator": leadProv,
			"flash_coder":  workerProv,
		},
	}
	morph := &swarm.Morphology{
		Nodes: map[string]swarm.Node{
			"orchestrator": {IsPrimary: true, SystemPrompt: "LEAD_PROMPT", Workbench: []string{"todo_manage", "handoff"}},
			"flash_coder":  {IsPrimary: false, SystemPrompt: "WORKER_PROMPT", Workbench: []string{"read_file", "edit_file"}},
		},
	}
	a, err := NewAgent(Config{
		Pool:        pool,
		Morph:       morph,
		InitialNode: "flash_coder",
		WorkDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if a.ActiveNode() != "flash_coder" {
		t.Fatalf("ActiveNode = %q, want flash_coder", a.ActiveNode())
	}
	if a.provider != workerProv {
		t.Fatalf("provider = %v, want workerProv (%s)", a.provider.ID(), workerProv.ID())
	}
	if !IsToolAllowed("read_file", a.Workbench()) || IsToolAllowed("todo_manage", a.Workbench()) {
		t.Fatalf("unexpected worker workbench: %v", a.Workbench())
	}
}

type multiStepProvider struct {
	provider.Provider
	toolSteps int
	calls     int
}

func (p *multiStepProvider) ID() string   { return "test:multi" }
func (p *multiStepProvider) Name() string { return "multi" }
func (p *multiStepProvider) Converse(_ context.Context, _ provider.Request) (*provider.Response, error) {
	p.calls++
	if p.calls <= p.toolSteps {
		return &provider.Response{
			StopReason: "tool_use",
			ToolUses: []provider.ToolUseRequest{
				{
					ToolUseID: fmt.Sprintf("tc_%d", p.calls),
					Name:      "todo_manage",
					Input:     json.RawMessage(`{"todos":[]}`),
				},
			},
		}, nil
	}
	return &provider.Response{
		StopReason: "end_turn",
		Content:    "done after many steps",
	}, nil
}

func (p *multiStepProvider) ConverseStream(ctx context.Context, req provider.Request) <-chan provider.StreamEvent {
	ch := make(chan provider.StreamEvent, 6)
	go func() {
		defer close(ch)
		resp, err := p.Converse(ctx, req)
		if err != nil {
			ch <- provider.StreamErrorEvent{Err: err}
			return
		}
		if resp.Content != "" {
			ch <- provider.TextDeltaEvent{Text: resp.Content}
		}
		for _, tu := range resp.ToolUses {
			ch <- provider.ToolUseStartEvent{ToolUseID: tu.ToolUseID, Name: tu.Name}
			ch <- provider.ToolInputDeltaEvent{Delta: string(tu.Input)}
			ch <- provider.ToolUseStopEvent{}
		}
		ch <- provider.MessageStopEvent{StopReason: resp.StopReason}
	}()
	return ch
}

func TestMaxStepsZeroIsUnlimitedByDefault(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		prov := &multiStepProvider{toolSteps: 30}
		agent, err := NewAgent(Config{
			Provider: prov,
			WorkDir:  t.TempDir(),
			// MaxSteps omitted (0 = unlimited)
		})
		if err != nil {
			t.Fatal(err)
		}
		var out string
		if streaming {
			out, err = agent.StreamTurn(context.Background(), "run 30 steps", nil)
		} else {
			out, err = agent.Turn(context.Background(), "run 30 steps")
		}
		if err != nil {
			t.Fatalf("streaming=%v: expected unlimited MaxSteps=0 to succeed past 25 steps, got err: %v", streaming, err)
		}
		if out != "done after many steps" || prov.calls != 31 {
			t.Fatalf("streaming=%v: unexpected output=%q calls=%d", streaming, out, prov.calls)
		}
	}
}
