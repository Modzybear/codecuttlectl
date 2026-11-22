package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

type scriptedRolloverProvider struct {
	id            string
	contextWindow int32
	requests      []provider.Request
	streams       [][]provider.StreamEvent
}

func (p *scriptedRolloverProvider) ID() string           { return p.id }
func (p *scriptedRolloverProvider) Name() string         { return p.id }
func (p *scriptedRolloverProvider) ContextWindow() int32 { return p.contextWindow }
func (p *scriptedRolloverProvider) Converse(_ context.Context, _ provider.Request) (*provider.Response, error) {
	return &provider.Response{Content: "ok"}, nil
}
func (p *scriptedRolloverProvider) ConverseStream(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
	p.requests = append(p.requests, req)
	idx := len(p.requests) - 1
	ch := make(chan provider.StreamEvent, 16)
	if idx < len(p.streams) {
		for _, ev := range p.streams[idx] {
			ch <- ev
		}
	}
	close(ch)
	return ch
}

func TestCommandRegistryAndRolloverArgParsing(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})

	// /help
	handled, _ := reg.Dispatch(&m, "/help")
	if !handled {
		t.Fatal("expected /help to be handled")
	}
	if len(m.messages) == 0 {
		t.Fatalf("expected /help output message, got empty")
	}
	helpOut := m.messages[len(m.messages)-1].content
	if !strings.Contains(helpOut, "### Available Slash Commands") || !strings.Contains(helpOut, "- `/rollover") {
		t.Fatalf("expected /help output to use Markdown bullets, got:\n%s", helpOut)
	}
	m.width = 100
	rendered := m.renderMessages()
	if strings.Count(rendered, "\n") < 5 {
		t.Fatalf("expected rendered /help to span multiple lines, got:\n%s", rendered)
	}
	m.messages = append(m.messages, chatMessage{role: "system_nudge", content: "────────── Epoch 2 ──────────"})
	if !strings.Contains(m.renderMessages(), "Epoch 2") {
		t.Fatalf("expected renderMessages() to render system_nudge content")
	}

	// /epochs (initial)
	handled, _ = reg.Dispatch(&m, "/epochs")
	if !handled || !strings.Contains(m.messages[len(m.messages)-1].content, "Epoch 1") {
		t.Fatalf("unexpected /epochs output: %+v", m.messages[len(m.messages)-1])
	}

	// /auto-rollover on / off / status
	handled, _ = reg.Dispatch(&m, "/auto-rollover on")
	if !handled || !m.rolloverCfg.AutoRollover {
		t.Fatal("expected /auto-rollover on to enable auto-rollover")
	}
	handled, _ = reg.Dispatch(&m, "/auto-rollover off")
	if !handled || m.rolloverCfg.AutoRollover {
		t.Fatal("expected /auto-rollover off to disable auto-rollover")
	}

	// Unregistered slash text is NOT intercepted as a command
	handled, _ = reg.Dispatch(&m, "/not-a-command")
	if handled {
		t.Fatal("expected unknown slash text to pass through unhandled")
	}

	// ParseRolloverCommandArgs
	docs, hint, err := ParseRolloverCommandArgs([]string{"--docs", "focus", "on", "tests"})
	if err != nil || docs == nil || *docs != rollover.DocsPolicyAlways || hint != "focus on tests" {
		t.Fatalf("unexpected parse result: docs=%v hint=%q err=%v", docs, hint, err)
	}
	docs, hint, err = ParseRolloverCommandArgs([]string{"--no-docs", "keep", "clean"})
	if err != nil || docs == nil || *docs != rollover.DocsPolicyNone || hint != "keep clean" {
		t.Fatalf("unexpected parse --no-docs result: docs=%v hint=%q err=%v", docs, hint, err)
	}
	if _, _, err := ParseRolloverCommandArgs([]string{"--bogus"}); err == nil {
		t.Fatal("expected error for unknown flag --bogus")
	}
}

func TestManualRolloverValidationRetryCommitAndSessionResume(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	invalidPayload, _ := json.Marshal(map[string]any{
		"summary":             "Incomplete checkpoint without key_files",
		"resume_instructions": "continue",
	})
	validPayload, _ := json.Marshal(map[string]any{
		"summary":             "Finished parser and unit tests",
		"lessons_learned":     []string{"Preserve provenance metadata on messages"},
		"key_files":           []map[string]string{{"path": "internal/session/filestore.go", "purpose": "Epoch archive persistence"}},
		"key_dirs":            []map[string]string{{"path": "internal/rollover", "purpose": "Rollover controller & prompts"}},
		"resume_instructions": "Proceed to Phase 2 auto-rollover state machine",
	})

	prov := &scriptedRolloverProvider{
		id: "openrouter:google/gemini-4-neon",
		streams: [][]provider.StreamEvent{
			// Attempt 1: invalid save_checkpoint (missing key_files)
			{
				provider.ToolUseStartEvent{ToolUseID: "cp_1", Name: rollover.SaveCheckpointToolName},
				provider.ToolInputDeltaEvent{Delta: string(invalidPayload)},
				provider.ToolUseStopEvent{},
				provider.MessageStopEvent{StopReason: "tool_calls"},
			},
			// Attempt 2: valid save_checkpoint
			{
				provider.ToolUseStartEvent{ToolUseID: "cp_2", Name: rollover.SaveCheckpointToolName},
				provider.ToolInputDeltaEvent{Delta: string(validPayload)},
				provider.ToolUseStopEvent{},
				provider.MessageStopEvent{StopReason: "tool_calls"},
			},
		},
	}

	m := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
		WorkDir:   t.TempDir(),
	})
	m.originalObjective = "Implement Epoch Rollover Compaction"
	m.history = []provider.Message{
		m.stampMessage(provider.BuildUserTextMessage("Implement Epoch Rollover Compaction")),
		m.stampMessage(provider.BuildAssistantMessage([]provider.ContentBlock{
			provider.ReasoningBlock{ReasoningDetails: []provider.ReasoningDetail{{Type: "reasoning.encrypted", Data: "stale_sig"}}},
			provider.TextBlock{Text: "Working on it."},
		})),
	}
	m.lastCallInputTokens = 48000
	m.reasoningDetails = []provider.ReasoningDetail{{Type: "reasoning.encrypted", Data: "stale_sig"}}

	// Trigger manual /rollover with steering hint
	cmd := m.submitMessage("/rollover focus on Phase 2")
	if cmd == nil {
		t.Fatal("expected /rollover to launch closeout stream")
	}
	if m.rolloverState != rollover.StateCloseout {
		t.Fatalf("rolloverState = %q, want closeout", m.rolloverState)
	}

	// Verify save_checkpoint was injected in closeout request tools
	if len(prov.requests) != 1 {
		t.Fatalf("expected 1 closeout request so far, got %d", len(prov.requests))
	}
	hasSaveCheckpoint := false
	for _, td := range prov.requests[0].Tools {
		if td.Name == rollover.SaveCheckpointToolName {
			hasSaveCheckpoint = true
		}
		if td.Name == "handoff" || td.Name == "todo_manage" {
			t.Errorf("tool %q must not be exposed during closeout", td.Name)
		}
	}
	if !hasSaveCheckpoint {
		t.Fatal("expected save_checkpoint tool definition during closeout")
	}

	// Drive stream & tool execution loop until rollover completes (unpacking tea.BatchMsg)
	queue := []tea.Cmd{cmd}
	for step := 0; step < 60 && len(queue) > 0; step++ {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg := next()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				if sub != nil {
					queue = append(queue, sub)
				}
			}
			continue
		}
		// Skip periodic spinner ticks so the deterministic queue drains cleanly.
		if strings.Contains(strings.ToLower(typeName(msg)), "tickmsg") {
			continue
		}
		updated, nextCmd := m.Update(msg)
		m = updated.(Model)
		if nextCmd != nil {
			queue = append(queue, nextCmd)
		}
	}

	if m.rolloverState != rollover.StateNormal {
		t.Fatalf("expected rolloverState back to normal after commit, got %q", m.rolloverState)
	}
	if m.currentEpoch != 2 {
		t.Fatalf("currentEpoch = %d, want 2", m.currentEpoch)
	}
	if len(m.epochs) != 1 || m.epochs[0].Epoch != 1 {
		t.Fatalf("expected 1 archived epoch checkpoint, got %+v", m.epochs)
	}
	if m.epochs[0].ArchiveFile != "epochs/"+sessID+"/epoch-001.json" {
		t.Errorf("ArchiveFile = %q", m.epochs[0].ArchiveFile)
	}
	if m.lastCallInputTokens != 0 || len(m.reasoningDetails) != 0 {
		t.Errorf("expected stale token counters and reasoningDetails cleared on new epoch")
	}
	if len(m.history) != 1 {
		t.Fatalf("expected fresh Epoch 2 history with 1 bootstrap seed message, got %d", len(m.history))
	}
	bootText := m.history[0].Content[0].(provider.TextBlock).Text
	if !strings.Contains(bootText, "# Session Rollover — Epoch 2 (Resumed from Epoch 1)") ||
		!strings.Contains(bootText, "Finished parser and unit tests") ||
		!strings.Contains(bootText, "focus on Phase 2") {
		t.Errorf("bootstrap seed message missing expected sections:\n%s", bootText)
	}

	// Verify immutable archive on disk contains original 2 messages (NOT the closeout prompts)
	archive, err := store.LoadEpochArchive(sessID, 1)
	if err != nil {
		t.Fatalf("LoadEpochArchive(1): %v", err)
	}
	if len(archive.Messages) != 2 {
		t.Fatalf("expected 2 messages in Epoch 1 archive, got %d", len(archive.Messages))
	}

	// Verify session resume loads cleanly into Epoch 2 with epoch metadata intact
	resumed := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
	})
	if resumed.currentEpoch != 2 || len(resumed.epochs) != 1 || resumed.originalObjective != "Implement Epoch Rollover Compaction" {
		t.Fatalf("resumed epoch metadata mismatch: epoch=%d epochs=%d obj=%q", resumed.currentEpoch, len(resumed.epochs), resumed.originalObjective)
	}
	if !strings.Contains(resumed.renderStatusBar(), "E2") {
		t.Errorf("expected status bar to render E2 badge, got %q", resumed.renderStatusBar())
	}
}

func TestManualRolloverEscCancelRestoresHistory(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	prov := &scriptedRolloverProvider{
		id:      "openrouter:google/gemini-4-neon",
		streams: [][]provider.StreamEvent{{}},
	}
	m := New(Config{
		PluginMgr: pluginhost.NewManager(false),
		Provider:  prov,
		Store:     store,
		SessionID: sessID,
	})
	m.history = []provider.Message{
		m.stampMessage(provider.BuildUserTextMessage("original user turn")),
		m.stampMessage(provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "original reply"}})),
	}

	_ = m.submitMessage("/rollover")
	if m.rolloverState != rollover.StateCloseout || !m.streaming {
		t.Fatalf("expected active closeout stream, state=%q streaming=%v", m.rolloverState, m.streaming)
	}

	// Press Esc twice to cancel closeout
	escMsg := tea.KeyPressMsg{Code: tea.KeyEscape}
	updated, _ := m.Update(escMsg)
	m = updated.(Model)
	updated, _ = m.Update(escMsg)
	m = updated.(Model)

	if m.rolloverState != rollover.StateNormal {
		t.Fatalf("expected rolloverState normal after Esc cancel, got %q", m.rolloverState)
	}
	if m.currentEpoch != 1 {
		t.Fatalf("expected currentEpoch to remain 1, got %d", m.currentEpoch)
	}
	if len(m.history) != 2 {
		t.Fatalf("expected original 2 messages restored after cancel, got %d", len(m.history))
	}
	archives, err := store.ListEpochArchives(sessID)
	if err != nil || len(archives) != 0 {
		t.Fatalf("expected no epoch archives created on cancel, got %v (err=%v)", archives, err)
	}
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}

func TestAutoRolloverArmedHintQuiescentTriggerAndBudget(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessID, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	validPayload, _ := json.Marshal(map[string]any{
		"summary":             "Auto-rollover checkpoint from Epoch 1",
		"lessons_learned":     []string{"Quiescent boundary worked cleanly"},
		"key_files":           []map[string]string{{"path": "internal/rollover/controller.go", "purpose": "State machine"}},
		"resume_instructions": "Continue in Epoch 2",
	})

	prov := &scriptedRolloverProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 100_000,
		streams: [][]provider.StreamEvent{
			// Stream 1: closeout call triggered automatically on turn idle at 50%
			{
				provider.ToolUseStartEvent{ToolUseID: "cp_auto", Name: rollover.SaveCheckpointToolName},
				provider.ToolInputDeltaEvent{Delta: string(validPayload)},
				provider.ToolUseStopEvent{},
				provider.MessageStopEvent{StopReason: "tool_calls"},
			},
			// Stream 2: automatic bootstrap orientation turn in Epoch 2
			{
				provider.TextDeltaEvent{Text: "Oriented on Epoch 2."},
				provider.MessageStopEvent{StopReason: "stop"},
			},
		},
	}

	cfg := rollover.DefaultConfig()
	cfg.AutoRollover = true
	cfg.SoftPct = 0.45
	cfg.HardPct = 0.65
	cfg.MinEpochTurns = 2

	m := New(Config{
		PluginMgr:      pluginhost.NewManager(false),
		Provider:       prov,
		Store:          store,
		SessionID:      sessID,
		WorkDir:        t.TempDir(),
		System:         "base system prompt",
		RolloverConfig: &cfg,
	})
	m.history = []provider.Message{
		m.stampMessage(provider.BuildUserTextMessage("turn 1")),
		m.stampMessage(provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "ack 1"}})),
		m.stampMessage(provider.BuildUserTextMessage("turn 2")),
		m.stampMessage(provider.BuildAssistantMessage([]provider.ContentBlock{provider.TextBlock{Text: "ack 2"}})),
	}

	// Simulate fresh usage event at 50% context window (above 45% soft, below 65% hard)
	m.lastCallInputTokens = 50_000
	m.rolloverCtrl.MarkUsageSeen()

	// Mid-turn evaluation (turnIdle=false) -> Arms rollover without interrupting tool loop
	cmd, triggered := m.maybeAutoRollover(false)
	if triggered || cmd != nil {
		t.Fatalf("expected mid-turn 50%% to arm, not trigger closeout")
	}
	if m.rolloverState != rollover.StateArmed {
		t.Fatalf("rolloverState = %q, want armed", m.rolloverState)
	}
	if !strings.Contains(m.renderStatusBar(), "E1 (armed)") {
		t.Errorf("expected status bar to show E1 (armed), got %q", m.renderStatusBar())
	}

	// Quiescent evaluation (turnIdle=true) -> Triggers closeout and rolls over to Epoch 2
	cmd, triggered = m.maybeAutoRollover(true)
	if !triggered || cmd == nil {
		t.Fatal("expected quiescent turnIdle at 50% to trigger auto-rollover closeout")
	}
	if m.rolloverState != rollover.StateCloseout {
		t.Fatalf("rolloverState = %q, want closeout", m.rolloverState)
	}

	// Drain Bubble Tea commands through closeout -> commit -> Epoch 2 bootstrap
	queue := []tea.Cmd{cmd}
	for step := 0; step < 60 && len(queue) > 0; step++ {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg := next()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				if sub != nil {
					queue = append(queue, sub)
				}
			}
			continue
		}
		if strings.Contains(strings.ToLower(typeName(msg)), "tickmsg") {
			continue
		}
		updated, nextCmd := m.Update(msg)
		m = updated.(Model)
		if nextCmd != nil {
			queue = append(queue, nextCmd)
		}
	}

	if m.currentEpoch != 2 || len(m.epochs) != 1 || m.epochs[0].Trigger != session.RolloverTriggerSoftThreshold {
		t.Fatalf("unexpected post-auto-rollover state: epoch=%d epochs=%+v", m.currentEpoch, m.epochs)
	}
	if len(m.history) == 0 || m.history[0].Turn != 1 {
		t.Fatalf("expected bootstrap message to have Turn=1, got %+v", m.history)
	}
	// Verify subsequent user turn gets Turn=2 and tool-result user message stays on Turn=2
	userMsg := m.stampMessage(provider.BuildUserTextMessage("follow up in epoch 2"))
	if userMsg.Turn != 2 {
		t.Fatalf("expected follow-up user turn=2, got %d", userMsg.Turn)
	}
	m.history = append(m.history, userMsg)
	toolResMsg := m.stampMessage(provider.BuildToolResultMessage([]provider.ToolResultBlock{
		{ToolUseID: "t1", Name: "read_file", Content: "ok"},
	}))
	if toolResMsg.Turn != 2 {
		t.Fatalf("expected tool-result message to stay on turn=2, got %d", toolResMsg.Turn)
	}
	// Restore history to just bootstrap before budget pause check below
	m.history = m.history[:1]
	if m.system != "base system prompt" {
		t.Errorf("m.system was mutated: %q", m.system)
	}
	if !strings.Contains(m.renderStatusBar(), "E2 · auto") {
		t.Errorf("expected status bar to show E2 · auto, got %q", m.renderStatusBar())
	}

	// Now verify budget pause (--max-epochs=2) pauses cleanly without rolling over to Epoch 3
	m.rolloverCfg.MaxEpochs = 2
	m.rolloverCtrl.SetConfig(m.rolloverCfg)
	m.history = append(m.history, m.stampMessage(provider.BuildUserTextMessage("turn 2 in epoch 2")))
	m.lastCallInputTokens = 55_000
	m.rolloverCtrl.MarkUsageSeen()

	_, triggered = m.maybeAutoRollover(true)
	if triggered {
		t.Fatal("expected auto-rollover to pause at --max-epochs=2")
	}
	if m.currentEpoch != 2 {
		t.Fatalf("currentEpoch = %d, want 2", m.currentEpoch)
	}
	lastMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(lastMsg, "--max-epochs=2") {
		t.Errorf("expected budget pause notice in chat, got %q", lastMsg)
	}
}

func TestLastCallContextTokensPersistAcrossReload(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sid, err := store.Create(session.SessionMeta{Model: "openrouter:google/gemini-4-neon", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	prov := &scriptedRolloverProvider{
		id:            "openrouter:google/gemini-4-neon",
		contextWindow: 100_000,
	}

	m := New(Config{
		Provider:  prov,
		Store:     store,
		SessionID: sid,
	})
	m.width = 120
	m.totalInputTokens = 50_000_000 // large historical uncached total should not dilute last-call cache%
	m.reportedCostUSD = 1.25
	m.lastCallInputTokens = 10_000
	m.lastCallCacheReadInputTokens = 25_000
	m.lastCallCacheWriteInputTokens = 5_000
	m.saveSession()

	// Verify persisted value on disk
	saved, err := store.Load(sid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Meta.Stats.LastCallContextTokens != 40_000 || saved.Meta.Stats.LastCallCacheReadTokens != 25_000 {
		t.Fatalf("saved last-call stats = (ctx=%d, cacheRead=%d), want (40000, 25000)",
			saved.Meta.Stats.LastCallContextTokens, saved.Meta.Stats.LastCallCacheReadTokens)
	}

	// Reload into a fresh TUI model with auto-rollover enabled (provider does NOT implement CostEstimator)
	cfg := rollover.DefaultConfig()
	cfg.AutoRollover = true
	cfg.SoftPct = 0.30
	resumed := New(Config{
		Provider:       prov,
		Store:          store,
		SessionID:      sid,
		RolloverConfig: &cfg,
	})
	resumed.width = 120

	if resumed.lastCallInputTokens != 15_000 || resumed.lastCallCacheReadInputTokens != 25_000 {
		t.Fatalf("resumed lastCall = (in=%d, cacheRead=%d), want (15000, 25000)",
			resumed.lastCallInputTokens, resumed.lastCallCacheReadInputTokens)
	}
	bar := resumed.renderStatusBar()
	if !strings.Contains(bar, "40% ctx") {
		t.Errorf("expected resumed status bar to show 40%% ctx, got %q", bar)
	}
	if !strings.Contains(bar, "62% cache") {
		t.Errorf("expected resumed status bar to show last-call 62%% cache (not diluted by 50M history), got %q", bar)
	}
	if !strings.Contains(bar, "~$1.25") {
		t.Errorf("expected resumed status bar to show ~$1.25 even without CostEstimator, got %q", bar)
	}
	// Auto-rollover must still wait for a fresh UsageEvent before arming on reload
	if _, triggered := resumed.maybeAutoRollover(true); triggered {
		t.Fatal("auto-rollover should not trigger before a fresh API UsageEvent on reloaded session")
	}
}
