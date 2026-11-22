package compact

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Modzybear/codecuttlectl/internal/provider"
)

func TestCompactProvider_PreservesRecentTurns(t *testing.T) {
	// Build a provider message history with 3 user text turns
	// and tool results between them. Use multi-line content so summarizer works.
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d: some content here for the file", i))
	}
	longContent := strings.Join(lines, "\n")

	messages := []provider.Message{
		// Turn 1: user + assistant + tool result
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Read the file"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu1", Name: "read_file", Input: []byte(`{"path":"/foo"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Content: longContent}}},
		// Turn 2: user + assistant + tool result
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Now edit it"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu2", Name: "edit_file", Input: []byte(`{"path":"/foo"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu2", Content: longContent}}},
		// Turn 3 (most recent): user text
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Check the result"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu3", Name: "bash_exec", Input: []byte(`{"command":"cat /foo"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu3", Content: longContent}}},
	}

	cfg := SmallModelConfig()
	// PreserveRecentTurns: 2 — should preserve turns 2 and 3, compact turn 1

	result := CompactProvider(messages, 3, cfg)

	if result.Compacted != 1 {
		t.Errorf("expected 1 compacted tool result (turn 1), got %d", result.Compacted)
	}

	// Check that turn 1's tool result was compacted
	trBlock, ok := result.Messages[2].Content[0].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock at messages[2], got %T", result.Messages[2].Content[0])
	}
	if len(trBlock.Content) >= len(longContent) {
		t.Errorf("turn 1 tool result should have been compacted (len=%d, original=%d)", len(trBlock.Content), len(longContent))
	}

	// Check that turn 3's tool result is preserved
	trBlock3, ok := result.Messages[8].Content[0].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock at messages[8], got %T", result.Messages[8].Content[0])
	}
	if trBlock3.Content != longContent {
		t.Error("turn 3 tool result should be preserved verbatim")
	}
}

func TestCompactProvider_SmallResultsNotCompacted(t *testing.T) {
	shortContent := "OK"
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Do something"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu1", Name: "bash_exec", Input: []byte(`{}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Content: shortContent}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Next step"}}},
	}

	cfg := SmallModelConfig()
	result := CompactProvider(messages, 2, cfg)

	if result.Compacted != 0 {
		t.Errorf("expected 0 compacted (content too small), got %d", result.Compacted)
	}
}

func TestCompactProviderIfNeeded_AlwaysCompactsWhenZeroPercent(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("line %d: content here for padding", i))
	}
	longContent := strings.Join(lines, "\n")

	messages := []provider.Message{
		// Turn 1 (oldest — should be compacted)
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Initial request"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu1", Name: "read_file", Input: []byte(`{}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Content: longContent}}},
		// Turn 2
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Continue working"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.TextBlock{Text: "OK"}}},
		// Turn 3 (most recent)
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Final step"}}},
	}

	cfg := SmallModelConfig() // MaxContextPercent: 0.0, PreserveRecentTurns: 2

	// With 0 tokens and 0 window, it should still compact because MaxContextPercent is 0
	result, compacted := CompactProviderIfNeeded(messages, 3, 0, 0, cfg)
	if !compacted {
		t.Error("expected compaction to occur when MaxContextPercent is 0")
	}

	// Verify the tool result was actually compacted (turn 1 is outside preserve window)
	trBlock, ok := result[2].Content[0].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", result[2].Content[0])
	}
	if len(trBlock.Content) >= len(longContent) {
		t.Errorf("tool result should have been compacted (len=%d, original=%d)", len(trBlock.Content), len(longContent))
	}
}

func TestFindProviderPreserveBoundary(t *testing.T) {
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 1"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.TextBlock{Text: "Response 1"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Content: "result"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 2"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.TextBlock{Text: "Response 2"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 3"}}},
	}

	tests := []struct {
		name          string
		preserveTurns int
		wantIdx       int
	}{
		{"preserve 1", 1, 5},
		{"preserve 2", 2, 3},
		{"preserve 3", 3, 0},
		{"preserve 0", 0, 6}, // preserve nothing → len(messages)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findProviderPreserveBoundary(messages, tt.preserveTurns)
			if got != tt.wantIdx {
				t.Errorf("findProviderPreserveBoundary(..., %d) = %d, want %d", tt.preserveTurns, got, tt.wantIdx)
			}
		})
	}
}

func buildProviderSingleTurnToolRounds(numRounds int, contentFn func(round int) string) []provider.Message {
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Investigate codebase"}}},
	}
	for r := 1; r <= numRounds; r++ {
		id := fmt.Sprintf("tu_%d", r)
		messages = append(messages,
			provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				provider.ToolUseBlock{ToolUseID: id, Name: "read_file", Input: []byte(`{"path":"/foo"}`)},
			}},
			provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{
				provider.ToolResultBlock{ToolUseID: id, Name: "read_file", Content: contentFn(r)},
			}},
		)
	}
	return messages
}

func extractProviderToolResultText(t *testing.T, msg provider.Message) string {
	t.Helper()
	for _, block := range msg.Content {
		if tr, ok := block.(provider.ToolResultBlock); ok {
			return tr.Content
		}
	}
	t.Fatalf("no ToolResultBlock found in message")
	return ""
}

func TestCompactProvider_SingleTurnManyToolRounds(t *testing.T) {
	tests := []struct {
		name             string
		cfg              Config
		totalRounds      int
		wantCompacted    int
		wantPreservedMin int
	}{
		{
			name:             "DefaultConfig preserves last 12 of 20 rounds",
			cfg:              DefaultConfig(),
			totalRounds:      20,
			wantCompacted:    8,
			wantPreservedMin: 9,
		},
		{
			name:             "SmallModelConfig preserves last 4 of 20 rounds",
			cfg:              SmallModelConfig(),
			totalRounds:      20,
			wantCompacted:    16,
			wantPreservedMin: 17,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := buildProviderSingleTurnToolRounds(tt.totalRounds, func(round int) string {
				return makeMultiLineContent(100, fmt.Sprintf("prov-round-%d", round))
			})

			result := CompactProvider(messages, 1, tt.cfg)
			if result.Compacted != tt.wantCompacted {
				t.Fatalf("CompactProvider() compacted %d, want %d", result.Compacted, tt.wantCompacted)
			}

			for r := 1; r <= tt.totalRounds; r++ {
				msgIdx := 2 * r
				orig := extractProviderToolResultText(t, messages[msgIdx])
				got := extractProviderToolResultText(t, result.Messages[msgIdx])
				if r < tt.wantPreservedMin {
					if !strings.Contains(got, omissionMarkerHint) {
						t.Errorf("round %d should be compacted, got %q", r, got[:min(80, len(got))])
					}
					if len(got) >= len(orig) {
						t.Errorf("round %d summary len=%d should be shorter than orig len=%d", r, len(got), len(orig))
					}
				} else {
					if got != orig {
						t.Errorf("round %d should be preserved verbatim", r)
					}
				}
			}
		})
	}
}

func TestCompactProvider_ToolRoundBoundaryDisabledWhenZero(t *testing.T) {
	messages := buildProviderSingleTurnToolRounds(20, func(round int) string {
		return makeMultiLineContent(100, fmt.Sprintf("prov-round-%d", round))
	})

	if got := findProviderToolRoundBoundary(messages, 0); got != 0 {
		t.Fatalf("findProviderToolRoundBoundary(..., 0) = %d, want 0", got)
	}
	if got := findProviderToolRoundBoundary(messages, -2); got != 0 {
		t.Fatalf("findProviderToolRoundBoundary(..., -2) = %d, want 0", got)
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentToolRounds = 0

	result := CompactProvider(messages, 1, cfg)
	if result.Compacted != 0 {
		t.Errorf("expected 0 compacted when tool-round boundary disabled, got %d", result.Compacted)
	}
	for r := 1; r <= 20; r++ {
		msgIdx := 2 * r
		if extractProviderToolResultText(t, result.Messages[msgIdx]) != extractProviderToolResultText(t, messages[msgIdx]) {
			t.Errorf("round %d should remain untouched when tool-round boundary disabled", r)
		}
	}
}

func TestCompactProvider_IdempotentNoReCompaction(t *testing.T) {
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %03d: %s", i, strings.Repeat("y", 80)))
	}
	bigContent := strings.Join(lines, "\n")

	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 1"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu1", Name: "read_file", Input: []byte(`{}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Name: "read_file", Content: bigContent}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 2"}}},
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1
	cfg.MinResultSize = 100

	first := CompactProvider(messages, 2, cfg)
	if first.Compacted != 1 {
		t.Fatalf("first pass compacted = %d, want 1", first.Compacted)
	}
	firstSummary := extractProviderToolResultText(t, first.Messages[2])
	if len(firstSummary) < cfg.MinResultSize {
		t.Fatalf("test setup error: first summary len=%d should be >= MinResultSize=%d", len(firstSummary), cfg.MinResultSize)
	}
	if !strings.Contains(firstSummary, "92 lines omitted") {
		t.Fatalf("expected '92 lines omitted' in first summary, got:\n%s", firstSummary)
	}

	second := CompactProvider(first.Messages, 2, cfg)
	if second.Compacted != 0 {
		t.Errorf("second pass compacted = %d, want 0", second.Compacted)
	}
	secondSummary := extractProviderToolResultText(t, second.Messages[2])
	if secondSummary != firstSummary {
		t.Errorf("second pass mutated already-compacted summary:\nfirst:\n%s\nsecond:\n%s", firstSummary, secondSummary)
	}
}

func TestCompactProvider_HugeSingleLineToolOutput(t *testing.T) {
	minified := `{"payload":"` + strings.Repeat("0123456789", 1000) + `"}`
	if strings.Contains(minified, "\n") {
		t.Fatal("test setup error: expected 0 newlines")
	}

	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 1"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{provider.ToolUseBlock{ToolUseID: "tu1", Name: "bash_exec", Input: []byte(`{}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock{ToolUseID: "tu1", Name: "bash_exec", Content: minified}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Turn 2"}}},
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1

	first := CompactProvider(messages, 2, cfg)
	if first.Compacted != 1 {
		t.Fatalf("first pass compacted = %d, want 1", first.Compacted)
	}
	summary := extractProviderToolResultText(t, first.Messages[2])
	if !strings.Contains(summary, charOmissionMarkerHint) {
		t.Errorf("expected char omission marker in summary, got %q", summary)
	}
	if len(summary) >= len(minified) {
		t.Errorf("summary len=%d should be shorter than original len=%d", len(summary), len(minified))
	}
	if !strings.HasPrefix(summary, `{"payload":"`) || !strings.HasSuffix(summary, `"}`) {
		t.Errorf("summary should preserve head and tail of minified JSON")
	}

	cfg.MinResultSize = 100
	second := CompactProvider(first.Messages, 2, cfg)
	if second.Compacted != 0 {
		t.Errorf("second pass compacted = %d, want 0", second.Compacted)
	}
	if extractProviderToolResultText(t, second.Messages[2]) != summary {
		t.Errorf("second pass altered already-compacted single-line summary")
	}
}

func TestCompactProviderBatched_DefersUntilBatchThreshold(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %02d: payload content for batching test", i))
	}
	bigContent := strings.Join(lines, "\n")

	// Build 1 user turn with 15 tool rounds (PreserveRecentToolRounds=12 -> 3 eligible < MinCompactBatch=6).
	buildRounds := func(n int) []provider.Message {
		msgs := []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock{Text: "Run long task"}}},
		}
		for i := 1; i <= n; i++ {
			id := fmt.Sprintf("tu_%d", i)
			msgs = append(msgs,
				provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
					provider.ToolUseBlock{ToolUseID: id, Name: "read_file", Input: []byte(`{}`)},
				}},
				provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{
					provider.ToolResultBlock{ToolUseID: id, Name: "read_file", Content: bigContent},
				}},
			)
		}
		return msgs
	}

	cfg := DefaultConfig() // PreserveRecentToolRounds=12, MinCompactBatch=6

	// 15 rounds -> only 3 stale results (< 6): deferred when force=false.
	msgs15 := buildRounds(15)
	resDeferred := CompactProviderBatched(msgs15, 1, cfg, false)
	if resDeferred.Compacted != 0 {
		t.Fatalf("expected mid-turn compaction deferred with 3 < 6 stale results, got %d", resDeferred.Compacted)
	}

	// Same 15 rounds with force=true (e.g. user turn boundary or ShouldCompact): compacts immediately.
	resForced := CompactProviderBatched(msgs15, 1, cfg, true)
	if resForced.Compacted != 3 {
		t.Fatalf("expected forced compaction to compact 3 results, got %d", resForced.Compacted)
	}

	// 18 rounds -> 6 stale results (== MinCompactBatch): triggers batch compaction even with force=false.
	msgs18 := buildRounds(18)
	resBatch := CompactProviderBatched(msgs18, 1, cfg, false)
	if resBatch.Compacted != 6 {
		t.Fatalf("expected batch compaction to compact 6 results, got %d", resBatch.Compacted)
	}
}

// Silence unused import
var _ = fmt.Sprint
