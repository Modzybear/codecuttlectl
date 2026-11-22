// Package provider defines the provider-agnostic LLM interface and types.
// All LLM backends (Bedrock, Ollama, etc.) implement the Provider interface.
package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Provider is the interface that all LLM backends must implement.
type Provider interface {
	// ID returns a unique identifier for this provider instance (e.g., "bedrock:opus-4-6", "ollama:gemma4")
	ID() string

	// Name returns a human-friendly display name (e.g., "opus-4-6", "gemma4:31b")
	Name() string

	// Converse sends messages and returns a complete response.
	Converse(ctx context.Context, req Request) (*Response, error)

	// ConverseStream sends messages and streams the response via channel.
	// The channel is closed when the stream completes or an error occurs.
	ConverseStream(ctx context.Context, req Request) <-chan StreamEvent
}

// Request is provider-agnostic conversation input.
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolDefinition
	Config   InferenceConfig
}

// InferenceConfig holds generation parameters.
type InferenceConfig struct {
	MaxTokens      int
	Temperature    *float64
	EnableThinking bool
	ThinkingBudget int
}

// Message is a provider-agnostic message.
type Message struct {
	Role    Role
	Content []ContentBlock
}

// Role represents the message author role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ContentBlock is the interface for message content variants.
type ContentBlock interface {
	contentBlock()
}

// TextBlock represents plain text content.
type TextBlock struct {
	Text string
}

func (TextBlock) contentBlock() {}

// ReasoningDetail represents a structured reasoning item returned by OpenRouter
// (e.g. encrypted Gemini thought signatures, reasoning.text, reasoning.summary)
// that must be preserved and echoed back in assistant messages.
type ReasoningDetail struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Format    string `json:"format,omitempty"`
	Index     *int   `json:"index,omitempty"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Data      string `json:"data,omitempty"`
}

// ReasoningBlock represents thinking/reasoning content from the model.
type ReasoningBlock struct {
	Text             string
	Signature        string
	ReasoningDetails []ReasoningDetail
}

func (ReasoningBlock) contentBlock() {}

// ToolUseBlock represents a tool call from the model.
type ToolUseBlock struct {
	ToolUseID string
	Name      string
	Input     json.RawMessage
	// ThoughtSignature is an opaque signature from Gemini 3 models that must be
	// passed back in conversation history for function calling to work correctly.
	// Only the first function call in a parallel set will have this populated.
	ThoughtSignature string
}

func (ToolUseBlock) contentBlock() {}

// ToolResultBlock represents the result of a tool execution.
type ToolResultBlock struct {
	Name      string
	ToolUseID string
	Content   string
	IsError   bool
}

func (ToolResultBlock) contentBlock() {}

// ToolDefinition describes a tool the model can invoke.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for the tool's input
}

// Response holds the result of a complete (non-streaming) call.
type Response struct {
	Content          string
	Reasoning        string
	ReasoningDetails []ReasoningDetail
	ToolUses         []ToolUseRequest
	StopReason       string
	Usage            Usage
}

// ToolUseRequest represents the model requesting to use a tool.
type ToolUseRequest struct {
	ToolUseID        string
	Name             string
	Input            json.RawMessage
	ThoughtSignature string
}

// Usage reports token consumption.
type Usage struct {
	InputTokens      int32
	OutputTokens     int32
	CacheReadTokens  int32
	CacheWriteTokens int32
}

// --- Stream Events ---

// StreamEvent is the interface for all streaming events.
type StreamEvent interface {
	streamEvent()
}

// TextDeltaEvent is emitted when the model produces text.
type TextDeltaEvent struct {
	Text string
}

func (TextDeltaEvent) streamEvent() {}

// ReasoningDeltaEvent is emitted when the model streams reasoning/thinking text.
type ReasoningDeltaEvent struct {
	Text string
}

func (ReasoningDeltaEvent) streamEvent() {}

// ReasoningSignatureEvent carries the verification signature or structured
// reasoning_details for multi-turn reasoning continuity.
type ReasoningSignatureEvent struct {
	Signature string
	Details   []ReasoningDetail
}

func (ReasoningSignatureEvent) streamEvent() {}

// ToolUseStartEvent is emitted when the model begins a tool call.
type ToolUseStartEvent struct {
	ToolUseID string
	Name      string
	// ThoughtSignature is the opaque Gemini 3 thought signature attached to this
	// function call. Must be preserved and sent back in conversation history.
	ThoughtSignature string
}

func (ToolUseStartEvent) streamEvent() {}

// ToolInputDeltaEvent is emitted as tool input JSON is streamed.
type ToolInputDeltaEvent struct {
	Delta string
}

func (ToolInputDeltaEvent) streamEvent() {}

// ToolUseStopEvent is emitted when a tool call block is complete.
type ToolUseStopEvent struct{}

func (ToolUseStopEvent) streamEvent() {}

// MessageStopEvent is emitted when the entire message is complete.
type MessageStopEvent struct {
	StopReason string
}

func (MessageStopEvent) streamEvent() {}

// UsageEvent reports token consumption during streaming.
type UsageEvent struct {
	InputTokens      int32
	OutputTokens     int32
	CacheReadTokens  int32
	CacheWriteTokens int32
	// InputTokensUnknown marks usage snapshots with no reported input count.
	// The zero value preserves existing adapters' authoritative count semantics.
	InputTokensUnknown bool
}

func (UsageEvent) streamEvent() {}

// RetryEvent reports an adapter-owned retry wait, not response content.
type RetryEvent struct {
	Attempt    int
	MaxRetries int
	Delay      time.Duration
	Reason     string // Optional reason (empty defaults to rate-limit 429 for backwards compat)
}

func (RetryEvent) streamEvent() {}

// StreamErrorEvent reports an error during streaming.
type StreamErrorEvent struct {
	Err error
}

func (StreamErrorEvent) streamEvent() {}

// ContextWindowProvider is an optional interface that providers can implement
// to report their context window size. If not implemented, a default is assumed.
type ContextWindowProvider interface {
	// ContextWindow returns the maximum context window in tokens.
	ContextWindow() int32
}

// CostEstimator is an optional interface that providers can implement
// to estimate the dollar cost of the session based on token usage.
type CostEstimator interface {
	EstimateCost(usage Usage) float64
}

// EffectiveID returns the active underlying provider ID for a Provider.
// If p is a *FallbackProvider with a non-empty ActiveID(), it returns ActiveID();
// otherwise it falls back to p.ID() (or "" if p is nil).
func EffectiveID(p Provider) string {
	if p == nil {
		return ""
	}
	if fp, ok := p.(*FallbackProvider); ok {
		if active := fp.ActiveID(); active != "" {
			return active
		}
	}
	return p.ID()
}

// Helper functions for building messages

// BuildUserTextMessage creates a new message with the given text for the user.
func BuildUserTextMessage(text string) Message {
	return Message{
		Role: RoleUser,
		Content: []ContentBlock{
			TextBlock{Text: text},
		},
	}
}

// BuildAssistantMessage creates a new message for the assistant with the given blocks.
func BuildAssistantMessage(blocks []ContentBlock) Message {
	return Message{
		Role:    RoleAssistant,
		Content: blocks,
	}
}

// BuildToolResultMessage creates a new user message containing tool results.
func BuildToolResultMessage(results []ToolResultBlock) Message {
	var blocks []ContentBlock
	for _, r := range results {
		blocks = append(blocks, r)
	}
	return Message{
		Role:    RoleUser,
		Content: blocks,
	}
}
