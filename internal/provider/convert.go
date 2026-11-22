// Package convert provides conversion utilities between provider-agnostic types
// and AWS Bedrock SDK types. This allows the existing codebase (TUI, agent) to
// continue using Bedrock SDK types internally while the provider layer uses
// agnostic types for cross-provider compatibility.
package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// MessagesToProvider converts Bedrock SDK messages to provider-agnostic messages.
func MessagesToProvider(msgs []types.Message) []Message {
	var result []Message
	for _, msg := range msgs {
		result = append(result, MessageToProvider(msg))
	}
	return result
}

// MessageToProvider converts a single Bedrock SDK message to a provider-agnostic message.
func MessageToProvider(msg types.Message) Message {
	var role Role
	switch msg.Role {
	case types.ConversationRoleUser:
		role = RoleUser
	case types.ConversationRoleAssistant:
		role = RoleAssistant
	default:
		role = Role(string(msg.Role))
	}

	var blocks []ContentBlock
	for _, block := range msg.Content {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			blocks = append(blocks, TextBlock{Text: b.Value})
		case *types.ContentBlockMemberReasoningContent:
			switch r := b.Value.(type) {
			case *types.ReasoningContentBlockMemberReasoningText:
				blocks = append(blocks, ReasoningBlock{
					Text:      aws.ToString(r.Value.Text),
					Signature: aws.ToString(r.Value.Signature),
				})
			case *types.ReasoningContentBlockMemberRedactedContent:
				blocks = append(blocks, ReasoningBlock{
					Text: "[redacted reasoning content]",
				})
			}
		case *types.ContentBlockMemberToolUse:
			var inputMap interface{}
			if b.Value.Input != nil {
				_ = b.Value.Input.UnmarshalSmithyDocument(&inputMap)
			}
			inputJSON, _ := json.Marshal(inputMap)
			blocks = append(blocks, ToolUseBlock{
				ToolUseID: aws.ToString(b.Value.ToolUseId),
				Name:      aws.ToString(b.Value.Name),
				Input:     inputJSON,
			})
		case *types.ContentBlockMemberToolResult:
			var content string
			for _, rc := range b.Value.Content {
				if text, ok := rc.(*types.ToolResultContentBlockMemberText); ok {
					content += text.Value
				}
			}
			blocks = append(blocks, ToolResultBlock{
				ToolUseID: aws.ToString(b.Value.ToolUseId),
				Content:   content,
				IsError:   b.Value.Status == types.ToolResultStatusError,
			})
		// Skip CachePoint blocks — they're Bedrock-specific
		case *types.ContentBlockMemberCachePoint:
			continue
		}
	}

	return Message{Role: role, Content: blocks}
}

// ToolDefsFromBedrock converts bedrock.ToolDefinition (internal) to provider.ToolDefinition.
// Since they have the same structure, this is a straightforward copy.
func ToolDefsFromBedrock(defs []struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}) []ToolDefinition {
	var result []ToolDefinition
	for _, d := range defs {
		result = append(result, ToolDefinition{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		})
	}
	return result
}

// SanitizeToolName cleans tool names to match pattern ^[a-zA-Z0-9_-]+$ required by OpenAI/Bedrock/Azure.
func SanitizeToolName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	res := sb.String()
	if res == "" {
		return "tool"
	}
	return res
}

// SanitizeHistoryForProvider cleans and transcodes conversation messages when switching
// across different LLM backends (e.g. Bedrock Claude -> Google Gemini / OpenRouter),
// preventing HTTP 400 INVALID_ARGUMENT errors from foreign block formats or missing signatures.
func SanitizeHistoryForProvider(msgs []Message, targetProvider string) []Message {
	if len(msgs) == 0 {
		return nil
	}

	targetLower := strings.ToLower(targetProvider)
	isBedrockTarget := strings.HasPrefix(targetLower, "bedrock")
	isGoogleTarget := strings.HasPrefix(targetLower, "google")
	isOpenRouterTarget := strings.HasPrefix(targetLower, "openrouter:")
	isOpenRouterGeminiTarget := strings.HasPrefix(targetLower, "openrouter:google/")

	// Pre-scan all ToolResultBlock IDs present in msgs so we know which
	// ToolUseBlocks represent completed historical calls vs in-flight calls.
	completedToolResults := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == RoleUser {
			for _, b := range m.Content {
				if trb, ok := b.(ToolResultBlock); ok && trb.ToolUseID != "" {
					completedToolResults[trb.ToolUseID] = true
				}
			}
		}
	}

	knownToolCalls := make(map[string]bool)
	transcodedToolCalls := make(map[string]string) // ToolUseID -> sanitized tool name
	var sanitized []Message

	for _, msg := range msgs {
		// Pre-scan tool_use IDs and Gemini signature availability in this assistant message.
		msgToolUseIDs := make(map[string]bool)
		hasGeminiToolSig := false
		if msg.Role == RoleAssistant {
			for _, b := range msg.Content {
				switch blk := b.(type) {
				case ToolUseBlock:
					if blk.ToolUseID != "" && blk.Name != "" {
						msgToolUseIDs[blk.ToolUseID] = true
						if blk.ThoughtSignature != "" && isGoogleTarget {
							hasGeminiToolSig = true
						}
					}
				}
			}
			if isOpenRouterGeminiTarget {
				for _, b := range msg.Content {
					if rb, ok := b.(ReasoningBlock); ok {
						for _, d := range rb.ReasoningDetails {
							if d.Type == "reasoning.encrypted" &&
								strings.HasPrefix(strings.ToLower(d.Format), "google-gemini") &&
								(d.ID == "" || msgToolUseIDs[d.ID]) {
								hasGeminiToolSig = true
								break
							}
						}
					}
				}
			}
		}

		var cleanBlocks []ContentBlock
		var textBlocks []string
		var reasoningText []string

		for _, b := range msg.Content {
			switch block := b.(type) {
			case TextBlock:
				if block.Text != "" {
					cleanBlocks = append(cleanBlocks, block)
					textBlocks = append(textBlocks, block.Text)
				}
			case ReasoningBlock:
				rb := block
				// If transitioning to a non-Bedrock provider, strip Bedrock-specific signatures
				// that foreign backends reject or misunderstand.
				if !isBedrockTarget && rb.Signature != "" {
					rb.Signature = ""
				}
				// Handle OpenRouter reasoning_details: strip for non-OpenRouter targets,
				// or filter by vendor format compatibility and matching tool_use ID.
				if len(rb.ReasoningDetails) > 0 {
					if !isOpenRouterTarget {
						rb.ReasoningDetails = nil
					} else {
						rb.ReasoningDetails = filterReasoningDetailsForOpenRouter(rb.ReasoningDetails, targetProvider, msgToolUseIDs)
					}
				}
				// Extract human-readable reasoning text before checking whether Bedrock requires a signature.
				if rb.Text != "" {
					reasoningText = append(reasoningText, rb.Text)
				} else {
					for _, d := range rb.ReasoningDetails {
						if d.Text != "" {
							reasoningText = append(reasoningText, d.Text)
						} else if d.Summary != "" {
							reasoningText = append(reasoningText, d.Summary)
						}
					}
				}
				// Bedrock rejects unsigned ReasoningBlocks; omit them from cleanBlocks
				// (reasoningText is still preserved above so reasoning-only turns downgrade to TextBlock).
				if isBedrockTarget && rb.Signature == "" {
					continue
				}
				// Drop empty ReasoningBlock (no text, no signature, no reasoning_details).
				if rb.Text == "" && rb.Signature == "" && len(rb.ReasoningDetails) == 0 {
					continue
				}
				cleanBlocks = append(cleanBlocks, rb)
			case ToolUseBlock:
				if block.ToolUseID != "" && block.Name != "" {
					tub := block
					tub.Name = SanitizeToolName(tub.Name)
					// If transitioning to a non-Google provider, strip Gemini thought signatures
					if !isGoogleTarget && tub.ThoughtSignature != "" {
						tub.ThoughtSignature = ""
					}
					// Google Gemini and OpenRouter Gemini reject completed foreign ToolUseBlocks
					// that lack a Gemini thought signature / reasoning.encrypted detail (Issue #62).
					// Transcode unsigned completed foreign tool calls into plain text transcripts.
					if (isGoogleTarget || isOpenRouterGeminiTarget) && !hasGeminiToolSig && completedToolResults[tub.ToolUseID] {
						inputStr := strings.TrimSpace(string(tub.Input))
						text := fmt.Sprintf("[Prior tool call: %s(%s)]", tub.Name, inputStr)
						cleanBlocks = append(cleanBlocks, TextBlock{Text: text})
						textBlocks = append(textBlocks, text)
						transcodedToolCalls[tub.ToolUseID] = tub.Name
						continue
					}
					knownToolCalls[block.ToolUseID] = true
					cleanBlocks = append(cleanBlocks, tub)
				}
			case ToolResultBlock:
				if block.ToolUseID != "" {
					trb := block
					if trb.Name != "" {
						trb.Name = SanitizeToolName(trb.Name)
					}
					if toolName, transcoded := transcodedToolCalls[trb.ToolUseID]; transcoded {
						name := trb.Name
						if name == "" {
							name = toolName
						}
						status := "ok"
						if trb.IsError {
							status = "error"
						}
						text := fmt.Sprintf("[Prior tool result (%s, %s): %s]", name, status, trb.Content)
						cleanBlocks = append(cleanBlocks, TextBlock{Text: text})
						textBlocks = append(textBlocks, text)
						continue
					}
					if knownToolCalls[block.ToolUseID] {
						cleanBlocks = append(cleanBlocks, trb)
					}
				}
			}
		}

		// Assistant turn validation: if empty content & no tool calls, downgrade reasoning to text if available
		if msg.Role == RoleAssistant {
			hasToolUse := false
			hasText := false
			for _, cb := range cleanBlocks {
				switch cb.(type) {
				case ToolUseBlock:
					hasToolUse = true
				case TextBlock:
					hasText = true
				}
			}

			if !hasToolUse && !hasText {
				if len(reasoningText) > 0 {
					// Convert reasoning to standard text block
					cleanBlocks = []ContentBlock{TextBlock{Text: strings.Join(reasoningText, "\n")}}
				} else {
					// Drop empty or encrypted-only assistant turns with no text or tool calls
					// that upstream providers (e.g. Google/OpenRouter) reject with 400.
					continue
				}
			}
		}

		if len(cleanBlocks) > 0 {
			outMsg := msg
			outMsg.Content = cleanBlocks
			sanitized = append(sanitized, outMsg)
		}
	}

	return sanitized
}

// filterReasoningDetailsForOpenRouter keeps only ReasoningDetails whose Format is
// compatible with the target OpenRouter vendor (e.g. "openrouter:google/...") and,
// for reasoning.encrypted items with a non-empty ID, verifies a matching ToolUseBlock
// exists in the same assistant message.
func filterReasoningDetailsForOpenRouter(details []ReasoningDetail, targetProvider string, msgToolUseIDs map[string]bool) []ReasoningDetail {
	modelPath := strings.TrimPrefix(targetProvider, "openrouter:")
	vendor := ""
	if idx := strings.IndexByte(modelPath, '/'); idx > 0 {
		vendor = strings.ToLower(modelPath[:idx])
	}

	var kept []ReasoningDetail
	for _, d := range details {
		if !reasoningFormatMatchesVendor(d.Format, vendor) {
			continue
		}
		if d.Type == "reasoning.encrypted" && d.ID != "" && !msgToolUseIDs[d.ID] {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// reasoningFormatMatchesVendor checks whether a reasoning_detail format string
// is compatible with the target OpenRouter vendor prefix (e.g. "google", "anthropic", "openai").
func reasoningFormatMatchesVendor(format, vendor string) bool {
	f := strings.ToLower(strings.TrimSpace(format))
	if f == "" {
		return true
	}
	switch {
	case strings.HasPrefix(f, "google-"):
		return vendor == "google"
	case strings.HasPrefix(f, "anthropic-"):
		return vendor == "anthropic"
	case strings.HasPrefix(f, "openai-"):
		return vendor == "openai"
	case strings.HasPrefix(f, "xai-"):
		return vendor == "x-ai" || vendor == "xai"
	default:
		// Unknown/generic format: allow only if the vendor prefix appears in the format
		// or no vendor could be parsed.
		if vendor == "" {
			return true
		}
		return strings.HasPrefix(f, vendor+"-") || strings.HasPrefix(f, vendor+"_") || f == vendor
	}
}
