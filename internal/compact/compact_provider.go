package compact

// compact_provider.go implements compaction for provider-agnostic message slices.
// This mirrors the logic in compact.go (which operates on Bedrock SDK types) but
// works directly with provider.Message slices used by the Ollama/non-Bedrock path.
//
// The agent_provider.go loop accumulates provider messages in provHistory without
// any compaction, which can exhaust the context window for small local models.

import (
	"fmt"

	"github.com/Modzybear/codecuttlectl/internal/provider"
)

// CompactProvider rewrites old tool_result blocks in provider message history with
// summaries. It returns a new message slice (the original is not mutated).
//
// currentTurn is the current conversation turn number (count of user text messages).
func CompactProvider(messages []provider.Message, currentTurn int, cfg Config) ProviderResult {
	if len(messages) == 0 {
		return ProviderResult{Messages: messages}
	}

	preserveFromIdx := max(
		findProviderPreserveBoundary(messages, cfg.PreserveRecentTurns),
		findProviderToolRoundBoundary(messages, cfg.PreserveRecentToolRounds),
	)

	result := make([]provider.Message, len(messages))
	totalFreed := 0
	compacted := 0

	for i, msg := range messages {
		if i >= preserveFromIdx {
			result[i] = msg
			continue
		}

		// Only compact user messages (tool results are sent as user role)
		if msg.Role != provider.RoleUser {
			result[i] = msg
			continue
		}

		// Check if this message contains any tool result blocks
		if !hasProviderToolResultContent(msg) {
			result[i] = msg
			continue
		}

		// Compact tool results in this message
		newContent, freed, count := compactProviderToolResults(msg.Content, cfg)
		if count > 0 {
			result[i] = provider.Message{
				Role:    msg.Role,
				Content: newContent,
			}
			totalFreed += freed
			compacted += count
		} else {
			result[i] = msg
		}
	}

	return ProviderResult{
		Messages:       result,
		TokensEstimate: totalFreed / 4,
		Compacted:      compacted,
	}
}

// ProviderResult holds the outcome of a provider compaction pass.
type ProviderResult struct {
	Messages       []provider.Message
	TokensEstimate int
	Compacted      int
}

// findProviderPreserveBoundary returns the index from which messages should be
// preserved (not compacted). It counts user text messages backward.
func findProviderPreserveBoundary(messages []provider.Message, preserveTurns int) int {
	if preserveTurns <= 0 {
		return len(messages)
	}

	userTurnsSeen := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser && hasProviderTextContent(messages[i]) {
			userTurnsSeen++
			if userTurnsSeen >= preserveTurns {
				return i
			}
		}
	}
	return 0
}

// hasProviderTextContent returns true if a message contains at least one text block.
func hasProviderTextContent(msg provider.Message) bool {
	for _, block := range msg.Content {
		if _, ok := block.(provider.TextBlock); ok {
			return true
		}
	}
	return false
}

// hasProviderToolResultContent returns true if a message contains at least one tool result block.
func hasProviderToolResultContent(msg provider.Message) bool {
	for _, block := range msg.Content {
		if _, ok := block.(provider.ToolResultBlock); ok {
			return true
		}
	}
	return false
}

// findProviderToolRoundBoundary walks backward through provider messages counting
// user messages that contain ToolResultBlock entries and returns the index at or
// after which all messages should be preserved.
// If preserveToolRounds <= 0, the tool-round boundary is disabled and 0 is returned.
func findProviderToolRoundBoundary(messages []provider.Message, preserveToolRounds int) int {
	if preserveToolRounds <= 0 {
		return 0
	}

	toolRoundsSeen := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser && hasProviderToolResultContent(messages[i]) {
			toolRoundsSeen++
			if toolRoundsSeen >= preserveToolRounds {
				return i
			}
		}
	}
	return 0
}

// compactProviderToolResults processes content blocks, replacing large tool results
// with summaries.
func compactProviderToolResults(content []provider.ContentBlock, cfg Config) ([]provider.ContentBlock, int, int) {
	newContent := make([]provider.ContentBlock, 0, len(content))
	totalFreed := 0
	count := 0

	for _, block := range content {
		tr, ok := block.(provider.ToolResultBlock)
		if !ok {
			newContent = append(newContent, block)
			continue
		}

		if isAlreadyCompacted(tr.Content) || len(tr.Content) < cfg.MinResultSize {
			newContent = append(newContent, block)
			continue
		}

		summary := summarizeToolResultWithConfig(tr.Content, cfg)
		freed := len(tr.Content) - len(summary)
		if freed <= 0 {
			newContent = append(newContent, block)
			continue
		}

		newContent = append(newContent, provider.ToolResultBlock{
			ToolUseID: tr.ToolUseID,
			Name:      tr.Name,
			Content:   summary,
			IsError:   tr.IsError,
		})

		totalFreed += freed
		count++
	}

	return newContent, totalFreed, count
}

// CompactProviderBatched applies compaction to provider history, deferring small
// mid-turn compactions when cfg.MinCompactBatch > 0 and force is false so prefix
// prompt caches are invalidated in batches rather than on every single tool round.
// Set force=true at user turn boundaries, when ShouldCompact triggers, or for small models.
func CompactProviderBatched(messages []provider.Message, currentTurn int, cfg Config, force bool) ProviderResult {
	result := CompactProvider(messages, currentTurn, cfg)
	if result.Compacted == 0 {
		return result
	}
	if !force && cfg.MinCompactBatch > 1 && result.Compacted < cfg.MinCompactBatch {
		return ProviderResult{Messages: messages}
	}
	return result
}

// CompactProviderIfNeeded applies compaction only when the estimated token usage
// exceeds the threshold. This is a convenience wrapper for the agent loop.
func CompactProviderIfNeeded(messages []provider.Message, currentTurn int, lastInputTokens int32, contextWindow int32, cfg Config) ([]provider.Message, bool) {
	// If MaxContextPercent is 0, always compact (used for small models)
	if cfg.MaxContextPercent > 0 {
		if contextWindow <= 0 || lastInputTokens <= 0 {
			return messages, false
		}
		usage := float64(lastInputTokens) / float64(contextWindow)
		if usage < cfg.MaxContextPercent {
			return messages, false
		}
	}

	result := CompactProvider(messages, currentTurn, cfg)
	if result.Compacted == 0 {
		return messages, false
	}

	// Log what happened (consumers can check the bool return)
	_ = fmt.Sprintf("compacted %d tool results, freed ~%d tokens", result.Compacted, result.TokensEstimate)
	return result.Messages, true
}
