package provider

import (
	"testing"
)

func TestSanitizeHistoryForProvider(t *testing.T) {
	// Case 1: Assistant with only reasoning gets downgraded to TextBlock
	msgs := []Message{
		{
			Role: RoleUser,
			Content: []ContentBlock{
				TextBlock{Text: "Please think about this"},
			},
		},
		{
			Role: RoleAssistant,
			Content: []ContentBlock{
				ReasoningBlock{Text: "This is pure internal reasoning text without a signature."},
			},
		},
	}

	sanitized := SanitizeHistoryForProvider(msgs, "google:gemini-3.1-flash")
	if len(sanitized) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(sanitized))
	}

	asstMsg := sanitized[1]
	if len(asstMsg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(asstMsg.Content))
	}

	if tb, ok := asstMsg.Content[0].(TextBlock); !ok || tb.Text != "This is pure internal reasoning text without a signature." {
		t.Errorf("expected reasoning to be converted to TextBlock, got: %#v", asstMsg.Content[0])
	}

	// Case 3: Target-aware signature preservation / stripping and orphan tool results dropping
	mixedMsgs := []Message{
		{
			Role: RoleAssistant,
			Content: []ContentBlock{
				ReasoningBlock{Text: "Thinking on Bedrock", Signature: "bedrock_sig_123"},
				ToolUseBlock{ToolUseID: "tool_1", Name: "read_file", ThoughtSignature: "gemini_thought_sig_456"},
			},
		},
		{
			Role: RoleUser,
			Content: []ContentBlock{
				ToolResultBlock{ToolUseID: "tool_1", Content: "file content"},
				ToolResultBlock{ToolUseID: "orphan_tool_99", Content: "orphan result"},
			},
		},
	}

	// 3a. Target is OpenRouter: Bedrock reasoning signature & Gemini thought signature should be stripped; orphan tool dropped
	sanitizedOpenRouter := SanitizeHistoryForProvider(mixedMsgs, "openrouter:deepseek/deepseek-v4-pro-0813")
	if len(sanitizedOpenRouter) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(sanitizedOpenRouter))
	}
	asstRB := sanitizedOpenRouter[0].Content[0].(ReasoningBlock)
	if asstRB.Signature != "" {
		t.Errorf("expected Bedrock signature stripped for OpenRouter, got: %q", asstRB.Signature)
	}
	asstTUB := sanitizedOpenRouter[0].Content[1].(ToolUseBlock)
	if asstTUB.ThoughtSignature != "" {
		t.Errorf("expected Gemini thought signature stripped for OpenRouter, got: %q", asstTUB.ThoughtSignature)
	}
	userContent := sanitizedOpenRouter[1].Content
	if len(userContent) != 1 {
		t.Fatalf("expected 1 tool result (orphan dropped), got %d", len(userContent))
	}
	if trb := userContent[0].(ToolResultBlock); trb.ToolUseID != "tool_1" {
		t.Errorf("expected tool_1 result preserved, got: %s", trb.ToolUseID)
	}

	// 3b. Target is Bedrock: Bedrock signature preserved
	sanitizedBedrock := SanitizeHistoryForProvider(mixedMsgs, "bedrock:us.anthropic.claude-opus-4-6-v1")
	bedrockRB := sanitizedBedrock[0].Content[0].(ReasoningBlock)
	if bedrockRB.Signature != "bedrock_sig_123" {
		t.Errorf("expected Bedrock signature preserved for Bedrock target, got: %q", bedrockRB.Signature)
	}

	// 3c. Target is Google: Gemini thought signature preserved
	sanitizedGoogle := SanitizeHistoryForProvider(mixedMsgs, "google:gemini-3.1-flash")
	googleTUB := sanitizedGoogle[0].Content[1].(ToolUseBlock)
	if googleTUB.ThoughtSignature != "gemini_thought_sig_456" {
		t.Errorf("expected Gemini thought signature preserved for Google target, got: %q", googleTUB.ThoughtSignature)
	}
}

func TestSanitizeHistoryForProvider_ReasoningDetails(t *testing.T) {
	idx0 := 0
	msgs := []Message{
		{
			Role: RoleAssistant,
			Content: []ContentBlock{
				ReasoningBlock{
					ReasoningDetails: []ReasoningDetail{
						{Type: "reasoning.encrypted", ID: "call_1", Format: "google-gemini-v1", Index: &idx0, Data: "enc_g1"},
						{Type: "reasoning.encrypted", ID: "call_missing", Format: "google-gemini-v1", Index: &idx0, Data: "enc_orphan"},
						{Type: "reasoning.encrypted", ID: "call_1", Format: "anthropic-claude-v1", Index: &idx0, Data: "enc_a1"},
						{Type: "reasoning.text", Format: "google-gemini-v1", Index: &idx0, Text: "thought"},
					},
				},
				ToolUseBlock{ToolUseID: "call_1", Name: "read_file", Input: []byte(`{"path":"a.go"}`)},
			},
		},
		{
			Role: RoleUser,
			Content: []ContentBlock{
				ToolResultBlock{ToolUseID: "call_1", Name: "read_file", Content: "ok"},
			},
		},
	}

	// 1. OpenRouter Google target: keeps google-gemini-v1 details with matching tool_use ID (and reasoning.text), drops orphan and anthropic details.
	googleOR := SanitizeHistoryForProvider(msgs, "openrouter:google/gemini-4-neon")
	if len(googleOR) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(googleOR))
	}
	rb, ok := googleOR[0].Content[0].(ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock preserved for openrouter:google, got %T", googleOR[0].Content[0])
	}
	if len(rb.ReasoningDetails) != 2 {
		t.Fatalf("expected 2 compatible google reasoning details, got %+v", rb.ReasoningDetails)
	}
	if rb.ReasoningDetails[0].ID != "call_1" || rb.ReasoningDetails[0].Data != "enc_g1" {
		t.Errorf("unexpected first detail: %+v", rb.ReasoningDetails[0])
	}
	if rb.ReasoningDetails[1].Type != "reasoning.text" {
		t.Errorf("unexpected second detail: %+v", rb.ReasoningDetails[1])
	}

	// 2. OpenRouter Anthropic target: keeps only anthropic-claude-v1 detail.
	anthOR := SanitizeHistoryForProvider(msgs, "openrouter:anthropic/claude-opus-4-6")
	rbAnth := anthOR[0].Content[0].(ReasoningBlock)
	if len(rbAnth.ReasoningDetails) != 1 || rbAnth.ReasoningDetails[0].Format != "anthropic-claude-v1" {
		t.Fatalf("expected only anthropic reasoning detail, got %+v", rbAnth.ReasoningDetails)
	}

	// 3. OpenRouter OpenAI target: drops all incompatible details AND drops empty ReasoningBlock.
	openaiOR := SanitizeHistoryForProvider(msgs, "openrouter:openai/gpt-5")
	if len(openaiOR[0].Content) != 1 {
		t.Fatalf("expected empty ReasoningBlock to be dropped, got %+v", openaiOR[0].Content)
	}
	if _, ok := openaiOR[0].Content[0].(ToolUseBlock); !ok {
		t.Fatalf("expected only ToolUseBlock remaining, got %T", openaiOR[0].Content[0])
	}

	// 4. Non-OpenRouter target (Bedrock): strips ReasoningDetails and drops empty ReasoningBlock.
	bedrockRes := SanitizeHistoryForProvider(msgs, "bedrock:us.anthropic.claude-opus-4-6-v1")
	if len(bedrockRes[0].Content) != 1 {
		t.Fatalf("expected ReasoningDetails stripped and empty ReasoningBlock dropped for Bedrock, got %+v", bedrockRes[0].Content)
	}
}

func TestEffectiveID(t *testing.T) {
	if got := EffectiveID(nil); got != "" {
		t.Fatalf("EffectiveID(nil) = %q, want empty", got)
	}
	p1 := &fakeProvider{id: "openrouter:google/gemini-4-neon"}
	p2 := &fakeProvider{id: "openrouter:google/gemini-4-argon"}
	if got := EffectiveID(p1); got != "openrouter:google/gemini-4-neon" {
		t.Fatalf("EffectiveID(direct) = %q", got)
	}
	fp := NewFallbackProvider("orchestrator", p1, p2)
	if got := EffectiveID(fp); got != "openrouter:google/gemini-4-neon" {
		t.Fatalf("EffectiveID(fallback) = %q, want primary active ID", got)
	}
	// When all candidates are cooling down, ActiveID() is empty and EffectiveID falls back to logical ID.
	fp.markCooldown(p1.ID(), nowFunc())
	fp.markCooldown(p2.ID(), nowFunc())
	if got := EffectiveID(fp); got != "orchestrator" {
		t.Fatalf("EffectiveID(all cooling) = %q, want logical ID", got)
	}
}
