package compact

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func buildToolResultMsg(toolUseID, content string) types.Message {
	return types.Message{
		Role: types.ConversationRoleUser,
		Content: []types.ContentBlock{
			&types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: aws.String(toolUseID),
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: content},
					},
					Status: types.ToolResultStatusSuccess,
				},
			},
		},
	}
}

func buildUserTextMsg(text string) types.Message {
	return types.Message{
		Role: types.ConversationRoleUser,
		Content: []types.ContentBlock{
			&types.ContentBlockMemberText{Value: text},
		},
	}
}

func buildAssistantMsg(text string) types.Message {
	return types.Message{
		Role: types.ConversationRoleAssistant,
		Content: []types.ContentBlock{
			&types.ContentBlockMemberText{Value: text},
		},
	}
}

func TestSummarizeToolResult(t *testing.T) {
	// Build a 100-line file content
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d: content here", i))
	}
	content := strings.Join(lines, "\n")

	summary := summarizeToolResult(content, 8)

	// Should contain head lines
	if !strings.Contains(summary, "line 1: content here") {
		t.Error("summary should contain first line")
	}
	if !strings.Contains(summary, "line 4: content here") {
		t.Error("summary should contain line 4 (head)")
	}

	// Should contain tail lines
	if !strings.Contains(summary, "line 100: content here") {
		t.Error("summary should contain last line")
	}
	if !strings.Contains(summary, "line 97: content here") {
		t.Error("summary should contain line 97 (tail)")
	}

	// Should contain omission marker
	if !strings.Contains(summary, "lines omitted") {
		t.Error("summary should contain omission marker")
	}
	if !strings.Contains(summary, "read_file with offset/limit") {
		t.Error("summary should hint at how to retrieve full content")
	}

	// Should NOT contain middle content
	if strings.Contains(summary, "line 50: content here") {
		t.Error("summary should NOT contain middle lines")
	}

	// Should be significantly shorter
	if len(summary) >= len(content) {
		t.Errorf("summary (%d chars) should be shorter than original (%d chars)", len(summary), len(content))
	}
}

func TestSummarizeShortContent(t *testing.T) {
	content := "line 1\nline 2\nline 3"
	summary := summarizeToolResult(content, 8)

	// Short content should pass through unchanged
	if summary != content {
		t.Errorf("short content should not be modified, got %q", summary)
	}
}

func TestCompactPreservesRecentTurns(t *testing.T) {
	// Build a conversation: 3 user turns with tool results
	// Content must be multi-line and > MinResultSize to be eligible
	var bigLines []string
	for i := 0; i < 200; i++ {
		bigLines = append(bigLines, fmt.Sprintf("line %d: some file content that is realistic", i))
	}
	bigContent := strings.Join(bigLines, "\n")

	messages := []types.Message{
		// Turn 1 (old)
		buildUserTextMsg("first question"),
		buildAssistantMsg("let me check"),
		buildToolResultMsg("tool_1", bigContent),
		// Turn 2 (old)
		buildUserTextMsg("second question"),
		buildAssistantMsg("checking again"),
		buildToolResultMsg("tool_2", bigContent),
		// Turn 3 (recent — should be preserved)
		buildUserTextMsg("third question"),
		buildAssistantMsg("one more check"),
		buildToolResultMsg("tool_3", bigContent),
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1 // Only preserve last 1 user turn

	result := Compact(messages, 3, cfg)

	// Turn 1 and 2 tool results should be compacted
	if result.Compacted < 2 {
		t.Errorf("expected at least 2 compacted results, got %d", result.Compacted)
	}

	// Turn 3 tool result should be preserved (full content)
	lastToolResult := result.Messages[8]
	text := extractMsgText(lastToolResult)
	if len(text) < len(bigContent) {
		t.Errorf("recent turn tool result should be preserved full, got %d chars (expected %d)", len(text), len(bigContent))
	}

	// Token savings should be positive
	if result.TokensEstimate <= 0 {
		t.Error("expected positive token savings")
	}
}

func TestCompactSkipsSmallResults(t *testing.T) {
	smallContent := "short result"

	messages := []types.Message{
		buildUserTextMsg("question"),
		buildAssistantMsg("checking"),
		buildToolResultMsg("tool_1", smallContent),
		buildUserTextMsg("follow up"), // This makes the above "old"
		buildAssistantMsg("done"),
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1

	result := Compact(messages, 2, cfg)

	// Small content should not be compacted
	if result.Compacted != 0 {
		t.Errorf("expected 0 compacted (content too small), got %d", result.Compacted)
	}
}

func TestShouldCompact(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxContextPercent = 0.50

	tests := []struct {
		name       string
		tokens     int32
		window     int32
		wantResult bool
	}{
		{"below threshold", 400_000, 1_000_000, false},
		{"at threshold", 500_000, 1_000_000, true},
		{"above threshold", 700_000, 1_000_000, true},
		{"zero tokens", 0, 1_000_000, false},
		{"zero window", 100_000, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldCompact(tt.tokens, tt.window, cfg)
			if got != tt.wantResult {
				t.Errorf("ShouldCompact(%d, %d) = %v, want %v", tt.tokens, tt.window, got, tt.wantResult)
			}
		})
	}
}

func TestCompactEmptyHistory(t *testing.T) {
	result := Compact(nil, 0, DefaultConfig())
	if result.Messages != nil {
		t.Error("nil input should return nil messages")
	}
	if result.Compacted != 0 {
		t.Error("nil input should have 0 compacted")
	}
}

func makeMultiLineContent(numLines int, prefix string) string {
	lines := make([]string, 0, numLines)
	for i := 1; i <= numLines; i++ {
		lines = append(lines, fmt.Sprintf("%s line %d: some realistic file content to exceed min result size", prefix, i))
	}
	return strings.Join(lines, "\n")
}

func buildSingleTurnToolRounds(numRounds int, contentFn func(round int) string) []types.Message {
	messages := []types.Message{
		buildUserTextMsg("investigate the entire codebase"),
	}
	for r := 1; r <= numRounds; r++ {
		messages = append(messages,
			buildAssistantMsg(fmt.Sprintf("calling tool round %d", r)),
			buildToolResultMsg(fmt.Sprintf("tool_%d", r), contentFn(r)),
		)
	}
	return messages
}

func TestCompact_SingleTurnManyToolRounds(t *testing.T) {
	tests := []struct {
		name             string
		cfg              Config
		totalRounds      int
		wantCompacted    int
		wantPreservedMin int // 1-based round index from which rounds are preserved
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
			messages := buildSingleTurnToolRounds(tt.totalRounds, func(round int) string {
				return makeMultiLineContent(100, fmt.Sprintf("round-%d", round))
			})

			result := Compact(messages, 1, tt.cfg)
			if result.Compacted != tt.wantCompacted {
				t.Fatalf("Compact() compacted %d results, want %d", result.Compacted, tt.wantCompacted)
			}

			for r := 1; r <= tt.totalRounds; r++ {
				msgIdx := 2 * r // 0=user text, 1=asst1, 2=tr1, 3=asst2, 4=tr2...
				orig := extractMsgText(messages[msgIdx])
				got := extractMsgText(result.Messages[msgIdx])
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

func TestCompact_ToolRoundBoundaryDisabledWhenZero(t *testing.T) {
	messages := buildSingleTurnToolRounds(20, func(round int) string {
		return makeMultiLineContent(100, fmt.Sprintf("round-%d", round))
	})

	if got := findToolRoundBoundary(messages, 0); got != 0 {
		t.Fatalf("findToolRoundBoundary(..., 0) = %d, want 0", got)
	}
	if got := findToolRoundBoundary(messages, -3); got != 0 {
		t.Fatalf("findToolRoundBoundary(..., -3) = %d, want 0", got)
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentToolRounds = 0

	// Only 1 user turn < PreserveRecentTurns (7), and tool-round boundary is disabled.
	result := Compact(messages, 1, cfg)
	if result.Compacted != 0 {
		t.Errorf("expected 0 compacted when tool-round boundary is disabled and turns <= PreserveRecentTurns, got %d", result.Compacted)
	}
	for r := 1; r <= 20; r++ {
		msgIdx := 2 * r
		if extractMsgText(result.Messages[msgIdx]) != extractMsgText(messages[msgIdx]) {
			t.Errorf("round %d should remain untouched when tool-round boundary is disabled", r)
		}
	}
}

func TestCompact_IdempotentNoReCompaction(t *testing.T) {
	// Long lines ensure the first-pass summary stays well above MinResultSize
	// and has > SummaryMaxLines lines (head + blank + marker + blank + tail = 11 lines for maxLines=8).
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %03d: %s", i, strings.Repeat("x", 80)))
	}
	bigContent := strings.Join(lines, "\n")

	messages := []types.Message{
		buildUserTextMsg("turn 1"),
		buildAssistantMsg("reading"),
		buildToolResultMsg("tool_1", bigContent),
		buildUserTextMsg("turn 2"),
		buildAssistantMsg("done"),
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1
	cfg.MinResultSize = 100 // Ensure compacted summary (>= 100 chars) would be eligible if not guarded

	first := Compact(messages, 2, cfg)
	if first.Compacted != 1 {
		t.Fatalf("first pass compacted = %d, want 1", first.Compacted)
	}
	firstSummary := extractMsgText(first.Messages[2])
	if len(firstSummary) < cfg.MinResultSize {
		t.Fatalf("test setup error: first summary len=%d should be >= MinResultSize=%d", len(firstSummary), cfg.MinResultSize)
	}
	if !strings.Contains(firstSummary, "92 lines omitted") {
		t.Fatalf("expected original omission count '92 lines omitted' in first summary, got:\n%s", firstSummary)
	}

	// Second pass must be a no-op and preserve the exact original omitted line count.
	second := Compact(first.Messages, 2, cfg)
	if second.Compacted != 0 {
		t.Errorf("second pass compacted = %d, want 0", second.Compacted)
	}
	secondSummary := extractMsgText(second.Messages[2])
	if secondSummary != firstSummary {
		t.Errorf("second pass mutated already-compacted summary:\nfirst:\n%s\nsecond:\n%s", firstSummary, secondSummary)
	}
}

func TestCompact_HugeSingleLineToolOutput(t *testing.T) {
	// 10,000-char minified JSON with 0 newlines.
	minified := `{"data":"` + strings.Repeat("abcdefghij", 1000) + `"}`
	if strings.Contains(minified, "\n") {
		t.Fatal("test setup error: expected 0 newlines")
	}

	messages := []types.Message{
		buildUserTextMsg("turn 1"),
		buildAssistantMsg("fetching json"),
		buildToolResultMsg("tool_1", minified),
		buildUserTextMsg("turn 2"),
		buildAssistantMsg("next"),
	}

	cfg := DefaultConfig()
	cfg.PreserveRecentTurns = 1

	first := Compact(messages, 2, cfg)
	if first.Compacted != 1 {
		t.Fatalf("first pass compacted = %d, want 1", first.Compacted)
	}

	summary := extractMsgText(first.Messages[2])
	if !strings.Contains(summary, charOmissionMarkerHint) {
		t.Errorf("expected char omission marker in summary, got %q", summary)
	}
	if len(summary) >= len(minified) {
		t.Errorf("summary len=%d should be shorter than original len=%d", len(summary), len(minified))
	}
	if !strings.HasPrefix(summary, `{"data":"`) || !strings.HasSuffix(summary, `"}`) {
		t.Errorf("summary should preserve head and tail of minified JSON")
	}

	// Second pass should not re-compact even with a low MinResultSize.
	cfg.MinResultSize = 100
	second := Compact(first.Messages, 2, cfg)
	if second.Compacted != 0 {
		t.Errorf("second pass compacted = %d, want 0", second.Compacted)
	}
	if extractMsgText(second.Messages[2]) != summary {
		t.Errorf("second pass altered already-compacted single-line summary")
	}
}

// extractMsgText extracts text from a tool_result message for testing.
func extractMsgText(msg types.Message) string {
	for _, block := range msg.Content {
		if tr, ok := block.(*types.ContentBlockMemberToolResult); ok {
			for _, c := range tr.Value.Content {
				if tb, ok := c.(*types.ToolResultContentBlockMemberText); ok {
					return tb.Value
				}
			}
		}
	}
	return ""
}
