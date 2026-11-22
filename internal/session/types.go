// Package session manages persistent session state for codecuttlectl conversations.
// Sessions capture the full conversation history, tool execution diagnostics (Inkwell),
// and task state (todos), enabling resume, audit, and the Phase 3 reconciliation loop.
package session

import (
	"encoding/json"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/todo"
)

// SessionMeta is the lightweight metadata shown in session listings.
// It is also the header of a full SessionState file.
type SessionMeta struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Title        string    `json:"title"`                  // Agent-generated after first turn
	Model        string    `json:"model"`                  // Bedrock model ID used
	Region       string    `json:"region"`                 // AWS region
	WorkDir      string    `json:"work_dir"`               // Working directory at session start
	CurrentEpoch int       `json:"current_epoch,omitempty"` // 1-indexed active epoch (0 treated as 1 for legacy sessions)
	Stats        Stats     `json:"stats"`
}

// EffectiveEpoch returns the 1-indexed epoch number, defaulting to 1 for legacy sessions.
func (m SessionMeta) EffectiveEpoch() int {
	if m.CurrentEpoch <= 0 {
		return 1
	}
	return m.CurrentEpoch
}

// Stats tracks aggregate metrics for a session.
type Stats struct {
	InputTokens           int32   `json:"input_tokens"`
	OutputTokens          int32   `json:"output_tokens"`
	CacheReadInputTokens  int32   `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int32   `json:"cache_write_input_tokens"`
	LastCallContextTokens int32   `json:"last_call_context_tokens,omitempty"`
	ToolCalls             int     `json:"tool_calls"`
	Turns                 int     `json:"turns"`
	EstimatedCostUSD      float64 `json:"estimated_cost_usd"`
}

// SessionState is the full persisted state of a session.
// This is what gets serialized to disk and loaded on resume.
type SessionState struct {
	Meta              SessionMeta       `json:"meta"`
	OriginalObjective string            `json:"original_objective,omitempty"` // Captured on Turn 1; anchored across all epochs
	Messages          []Message         `json:"messages"`                     // Active epoch conversation history
	Epochs            []EpochCheckpoint `json:"epochs,omitempty"`             // Completed epoch summaries & archive pointers
	Todos             []todo.Item       `json:"todos"`                        // Current task state
	Inkwell           []InkEntry        `json:"inkwell"`                      // Tool execution diagnostics
	Audit             AuditTrail        `json:"audit"`                        // Session-level governance/audit info
	DraftMessage      string            `json:"draft_message,omitempty"`      // Content being actively typed
}

// RolloverTrigger classifies what caused an epoch to roll over.
type RolloverTrigger string

const (
	RolloverTriggerManual        RolloverTrigger = "manual"
	RolloverTriggerSoftThreshold RolloverTrigger = "soft_threshold"
	RolloverTriggerHardThreshold RolloverTrigger = "hard_threshold"
)

// EpochCheckpoint records the structured handoff and deterministic ground-truth
// snapshot captured at the close of an epoch.
type EpochCheckpoint struct {
	Epoch             int               `json:"epoch"`                          // Completed epoch number (1, 2, ...)
	CreatedAt         time.Time         `json:"created_at"`                     // UTC timestamp of rollover commit
	Trigger           RolloverTrigger   `json:"trigger"`                        // "manual", "soft_threshold", "hard_threshold"
	ArchiveFile       string            `json:"archive_file,omitempty"`         // Relative path e.g. "epochs/ses_abc12345/epoch-001.json"
	MessageCount      int               `json:"message_count"`                  // Number of messages archived in this epoch
	TurnRange         [2]int            `json:"turn_range"`                     // [firstTurn, lastTurn] inclusive
	TokensAtRollover  int32             `json:"tokens_at_rollover"`             // Last API call total tokens before closeout
	ContextWindow     int32             `json:"context_window"`                 // Effective context window at rollover
	EstimatedCostUSD  float64           `json:"estimated_cost_usd"`             // Cumulative session cost at rollover
	DocsPolicyApplied string            `json:"docs_policy_applied,omitempty"`  // "none", "milestone", "always"
	Pinned            PinnedState       `json:"pinned"`                         // Harness-collected deterministic ground truth
	Payload           CheckpointPayload `json:"payload"`                        // Agent-synthesized handoff from save_checkpoint
}

// PinnedState captures deterministic ground-truth gathered directly by the harness
// (never rewritten by the LLM) to prevent requirement drift across epochs.
type PinnedState struct {
	OriginalObjective string      `json:"original_objective"`            // Anchored from Turn 1 (or user-updated goal)
	Todos             []todo.Item `json:"todos,omitempty"`               // Authoritative task list snapshot
	WorkDir           string      `json:"work_dir"`                      // Active working directory
	GitBranch         string      `json:"git_branch,omitempty"`          // Current git branch
	GitHeadCommit     string      `json:"git_head_commit,omitempty"`     // Short SHA + subject of HEAD
	GitStatusShort    string      `json:"git_status_short,omitempty"`    // Output of `git status --short`
	GitDiffStat       string      `json:"git_diff_stat,omitempty"`       // Output of `git diff --stat`
	RecentCommits     []string    `json:"recent_commits,omitempty"`      // Up to 10 recent commits on branch
	PendingAsyncTasks []string    `json:"pending_async_tasks,omitempty"` // In-flight swarm tasks at rollover
}

// CheckpointPayload is synthesized by the agent via the internal save_checkpoint tool.
type CheckpointPayload struct {
	Summary              string            `json:"summary"`
	LessonsLearned       []string          `json:"lessons_learned"`
	RecentlyChangedFiles []FileContextNote `json:"recently_changed_files,omitempty"`
	KeyFiles             []FileContextNote `json:"key_files"`
	KeyDirs              []DirContextNote  `json:"key_dirs,omitempty"`
	OpenQuestions        []string          `json:"open_questions,omitempty"`
	ResumeInstructions   string            `json:"resume_instructions"`
	DocsUpdated          []string          `json:"docs_updated,omitempty"`
}

// FileContextNote annotates a priority file with its purpose for the next epoch.
type FileContextNote struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

// DirContextNote annotates an important directory with its purpose and architectural role.
type DirContextNote struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

// EpochArchive is the immutable, self-contained JSON document persisted when an epoch closes.
type EpochArchive struct {
	SessionID  string          `json:"session_id"`
	Epoch      int             `json:"epoch"`
	ClosedAt   time.Time       `json:"closed_at"`
	Checkpoint EpochCheckpoint `json:"checkpoint"`
	Messages   []Message       `json:"messages"`
	Inkwell    []InkEntry      `json:"inkwell"`
}

// AuditTrail captures session-level governance and accounting metadata.
// This is the authoritative record for liability determination, cost attribution,
// security monitoring, and compliance reviews. Persisted in the session file and
// emitted as structured log events.
type AuditTrail struct {
	// Who
	AuthMethod   string `json:"auth_method,omitempty"`   // "iam_role", "pat", "coder_oauth", "env"
	AuthIdentity string `json:"auth_identity,omitempty"` // Role ARN, username, etc.

	// What model
	ModelID      string `json:"model_id"`
	ModelVersion string `json:"model_version,omitempty"`

	// Token accounting (for cost attribution)
	TotalInputTokens      int64 `json:"total_input_tokens"`
	TotalOutputTokens     int64 `json:"total_output_tokens"`
	TotalCacheReadTokens  int64 `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens int64 `json:"total_cache_write_tokens"`

	// Safety
	ToolDisciplineBlocks    int `json:"tool_discipline_blocks"`
	ToolDisciplineOverrides int `json:"tool_discipline_overrides"`
	DestructiveOpsAttempted int `json:"destructive_ops_attempted"`
	DestructiveOpsApproved  int `json:"destructive_ops_approved"`
	DestructiveOpsDenied    int `json:"destructive_ops_denied"`

	// Timing
	FirstToolCallAt *time.Time `json:"first_tool_call_at,omitempty"`
	LastToolCallAt  *time.Time `json:"last_tool_call_at,omitempty"`
	WallClockMs     int64      `json:"wall_clock_ms"`
	SessionStartAt  time.Time  `json:"session_start_at"`
}

// InkEntry records a single tool execution event for diagnostic analysis.
// Named after the cuttlefish ink defense mechanism — the system's local cache
// where defensive diagnostic "ink" is gathered and analyzed for the
// Inkwell reconciliation loop, governance auditing, and liability tracing.
//
// Design principle: NEVER truncate Input or Output in InkEntry. Display layers
// may truncate for UX, but the Inkwell is the authoritative record and must
// contain full, unmodified I/O for audit and replay.
type InkEntry struct {
	// Identity & Timing
	Timestamp time.Time `json:"timestamp"`
	EndTime   time.Time `json:"end_time,omitempty"` // When execution completed
	Turn      int       `json:"turn"`
	Step      int       `json:"step,omitempty"` // Step within turn (0-indexed)

	// Tool Execution
	ToolName   string          `json:"tool_name"`
	ToolUseID  string          `json:"tool_use_id"`
	Input      json.RawMessage `json:"input"`               // Full input JSON (NEVER truncated)
	Output     string          `json:"output"`              // Full output (NEVER truncated)
	Stderr     string          `json:"stderr,omitempty"`    // Separated stderr when available
	ExitCode   *int            `json:"exit_code,omitempty"` // For bash_exec results
	DurationMs int64           `json:"duration_ms"`

	// Error Classification
	IsError   bool   `json:"is_error"`
	ErrorType string `json:"error_type,omitempty"` // "compile", "runtime", "permission", "not_found", etc.

	// Context — what led to this tool call
	ReasoningContext string `json:"reasoning_context,omitempty"` // Model's reasoning/text before this call
	UserIntent       string `json:"user_intent,omitempty"`       // User message that initiated this turn

	// Plugin metadata
	PluginVersion string `json:"plugin_version,omitempty"`

	// Governance / Safety
	WasBlocked       bool   `json:"was_blocked,omitempty"`    // Tool discipline blocked execution
	WasOverridden    bool   `json:"was_overridden,omitempty"` // Allowed through after N blocks
	BlockReason      string `json:"block_reason,omitempty"`
	RequiredApproval bool   `json:"required_approval,omitempty"` // User was prompted to approve
	ApprovalDecision string `json:"approval_decision,omitempty"` // "approved", "denied", "auto_approved"
}
