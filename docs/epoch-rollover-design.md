# Epoch Rollover Compaction Design

## Overview

Epoch Rollover Compaction enables continuous, ultra-long, temporally coherent agent sessions in `codecuttlectl` by automating a structured **closeout → checkpoint → fresh-context bootstrap** cycle under a single persistent session ID.

Instead of letting a single context window degrade past 50% capacity with accumulated prose, dead-end debugging hypotheses, and diluted attention, the harness detects context pressure, guides the agent through a structured closeout via a dedicated internal tool (`save_checkpoint`), archives the completed epoch as an immutable, fully timestamped JSON record, and boots the next epoch with a clean context seeded with both deterministic ground truth and agent-synthesized institutional knowledge.

## Status

- **Phase**: Design Specification (this document)
- **Depends on**:
  - Layer 1 Heuristic Compaction (`internal/compact`, batched tool-round compaction — ✅ implemented in PR #75)
  - Lossless provider session serialization (`internal/session`, `MarshalProviderHistory`/`UnmarshalProviderHistory` — ✅ implemented in PR #75)
- **Enables**:
  - Manual `/rollover` in TUI and REPL
  - Autonomous `--auto-rollover` continuous execution
  - Future Optic Lobe / FTS / vector indexing over immutable epoch archives (`search_session_history`)

---

## Motivation: Why Layer 1 Compaction Is Not Enough

`codecuttlectl` already has **Layer 1: Heuristic Tool-Result Compaction** (see [`docs/context-compaction.md`](context-compaction.md)), which replaces stale, large `tool_result` outputs outside the recent preserve window with head/tail summaries in cache-friendly batches.

Even with Layer 1 working optimally, ultra-long coding sessions still hit three fundamental limits:

1. **Trajectory rot**: Dead-end debugging hypotheses, reverted attempts, and outdated mental models remain in assistant messages and subtly anchor the model even after later turns proved them wrong.
2. **Uncompactable monotonic growth**: User messages, assistant prose, reasoning summaries, and tool invocation arguments are never removed by Layer 1. Over 80–150 turns, the non-tool-result history alone fills hundreds of thousands of tokens.
3. **Attention & instruction dilution**: Even on 1M-token frontier models, precision, tool discipline, and adherence to constraints degrade measurably once context utilization passes ~40–50%.

Experienced operators work around this manually by asking the agent to write a detailed handoff/compaction rundown (hard-fought lessons, key files/directories with blurbs, current state, and exact resume instructions), starting a brand-new session, pasting the handoff, letting the fresh agent read the priority files to get its bearings, and continuing seamlessly.

**Epoch Rollover Compaction automates this exact workflow inside the harness.**

---

## Layered Compaction Architecture

```
┌────────────────────────────────────────────────────────────────────────────┐
│ Layer 1: Within-Epoch Heuristic Tool Compaction (✅ Implemented)           │
│   • Summarizes stale tool_result blocks (head/tail + single-line fallback) │
│   • Preserves recent turns & tool rounds; batches updates for prefix cache │
│   • Keeps within-epoch cost and latency low                                │
├────────────────────────────────────────────────────────────────────────────┤
│ Layer 2: Across-Epoch Session Rollover (This Design)                       │
│   • Triggered manually (/rollover) or automatically (--auto-rollover)      │
│   • Two-watermark quiescent trigger (Soft 45% → Armed, Hard 65% → Forced)  │
│   • Structured closeout via internal tool `save_checkpoint`                │
│   • Clean-context bootstrap ("get your bearings" re-hydration phase)       │
│   • Single Session ID; closed epochs archived as immutable JSON files      │
├────────────────────────────────────────────────────────────────────────────┤
│ Layer 3: Historical Context Retrieval / Optic Lobe (Future)                │
│   • Indexes immutable closed epoch archives + Inkwell (FTS5 / pgvector)    │
│   • Exposes `search_session_history` tool to retrieve exact quotes,        │
│     decisions, or error traces from earlier epochs on demand               │
└────────────────────────────────────────────────────────────────────────────┘
```

---

## Core Design Principles

1. **Single logical session identity**: A multi-epoch run keeps one `SessionID` (`ses_<id>`). Cost totals, audit trail, Inkwell diagnostics, and task continuity stay unified across epochs.
2. **Anti-"telephone game" grounding**: Never rely solely on an LLM summarizing a previous LLM summary. Every epoch boundary pairs **harness-collected immutable Pinned State** (original user objective, live `Todos`, git branch/status/diff/commits) with the **agent-synthesized Checkpoint Payload**.
3. **Typed harness tool, not markdown scraping**: The closeout agent produces its checkpoint via an internal tool (`save_checkpoint`) with a strict JSON Schema — matching `todo_manage`, `tool_info`, and `handoff`. The harness validates completeness before committing the epoch.
4. **Full provenance & timestamps everywhere**: Every persisted message, tool call, and checkpoint carries UTC timestamps, epoch number, turn number, active node ID, and model ID so closed epochs are directly indexable later.
5. **Repo hygiene by default**: Machine-managed compaction state lives strictly in the Session Store (`~/.local/share/codecuttlectl/sessions/`), **never** polluting git working trees. Updating project READMEs/docs is an explicit, orthogonal policy (`--rollover-docs` or `/rollover --docs`).
6. **Crash-safe atomic transitions**: Rollover never mutates or discards the current epoch until the closeout checkpoint is validated and the archive file is atomically committed to disk. Pressing `Esc` or hitting an API error during closeout leaves the session intact in the current epoch.
7. **Orchestrator-exclusive ownership**: In swarm morphologies, only the orchestrator/primary node owns rollover policy and `save_checkpoint`. Async background workers (`flash_coder`) already run with fresh isolated contexts; synchronous `handoff` nodes (`deep_coder`) inherit the orchestrator's lean active epoch window.

---

## Data Model & Storage Layout

### 1. Timestamped & Provenance-Tagged `session.Message`

Currently, `session.InkEntry` is fully timestamped (`Timestamp`, `EndTime`, `DurationMs`, `Turn`, `Step`), while `session.Message` (`internal/session/message.go`) only stores `Role` and `Blocks`.

We extend `session.Message` with backward-compatible (`omitempty`) provenance fields:

```go
type Message struct {
	ID        string        `json:"id,omitempty"`         // Monotonic or UUID per message ("msg_0001")
	Epoch     int           `json:"epoch,omitempty"`      // 1-indexed epoch number (0 for legacy sessions)
	Turn      int           `json:"turn,omitempty"`       // Session-global turn number
	Timestamp time.Time     `json:"timestamp,omitempty"`  // UTC creation timestamp
	NodeID    string        `json:"node_id,omitempty"`    // Swarm node ID (e.g., "orchestrator", "deep_coder")
	Model     string        `json:"model,omitempty"`      // Effective provider/model ID that produced/received this
	Role      string        `json:"role"`                 // "user", "assistant"
	Blocks    []ContentItem `json:"blocks"`
}
```

And on `ContentItem`, optional block-level timestamps for fine-grained indexing:

```go
type ContentItem struct {
	// ... existing fields (Type, Text, Signature, ReasoningDetails, ToolUseID, Name, Input, Content, Status, ResultFor) ...
	Timestamp time.Time `json:"timestamp,omitempty"` // UTC timestamp when block was finalized/recorded
}
```

> **Backward compatibility**: All new fields use `omitempty`. Legacy session files load cleanly with `Epoch == 0`, treated as Epoch 1 with no prior checkpoints.

#### Preserving Provenance Across In-Memory History Re-Serialization
Because `tui/app.go:saveSession()` and `conversation/agent_provider.go:flushSessionProvider()` re-serialize `m.history` / `a.provHistory` via `session.MarshalProviderHistory()` on every save, timestamps and provenance **must** live on `provider.Message` as well (e.g., an optional `Meta *MessageMeta` or flat `ID`, `Epoch`, `Turn`, `Timestamp`, `NodeID`, `Model` fields):
- Messages are stamped with `Timestamp: time.Now().UTC()`, `Epoch`, `Turn`, `NodeID`, and `Model` when appended to history (and assigned a monotonic `ID` if unset).
- `CompactProvider()` and `SanitizeHistoryForProvider()` **must preserve** message metadata when copying/rewriting `provider.Message` structs so compaction or handoff sanitization never strips original timestamps.
- `saveSession()` and `flushSessionProvider()` **must preserve** `Epochs` and `OriginalObjective` from in-memory state (falling back to `existing` on disk) instead of overwriting them with zero values.

### 2. Epoch Checkpoint Schema

Added to `internal/session/types.go`:

```go
// EpochCheckpoint records the structured handoff at the close of an epoch.
type EpochCheckpoint struct {
	Epoch              int               `json:"epoch"`                // Completed epoch number (1, 2, ...)
	CreatedAt          time.Time         `json:"created_at"`           // UTC timestamp of rollover commit
	Trigger            RolloverTrigger   `json:"trigger"`              // "manual", "soft_threshold", "hard_threshold"
	ArchiveFile        string            `json:"archive_file,omitempty"` // Relative path e.g. "epochs/ses_a3f8c2d1/epoch-001.json"
	MessageCount       int               `json:"message_count"`        // Number of messages archived in this epoch
	TurnRange          [2]int            `json:"turn_range"`           // [firstTurn, lastTurn] inclusive
	TokensAtRollover   int32             `json:"tokens_at_rollover"`   // Last API call total tokens before closeout
	ContextWindow      int32             `json:"context_window"`       // Effective context window at rollover
	EstimatedCostUSD   float64           `json:"estimated_cost_usd"`   // Cumulative session cost at rollover
	DocsPolicyApplied  string            `json:"docs_policy_applied"`  // "none", "milestone", "always"
	Pinned             PinnedState       `json:"pinned"`               // Harness-collected deterministic ground truth
	Payload            CheckpointPayload `json:"payload"`              // Agent-synthesized handoff from save_checkpoint
}

type RolloverTrigger string

const (
	RolloverTriggerManual        RolloverTrigger = "manual"
	RolloverTriggerSoftThreshold RolloverTrigger = "soft_threshold"
	RolloverTriggerHardThreshold RolloverTrigger = "hard_threshold"
)

// PinnedState is collected deterministically by the harness and never rewritten
// by the LLM, preventing requirement drift ("telephone game") across many epochs.
type PinnedState struct {
	OriginalObjective string      `json:"original_objective"`          // Anchored from Turn 1 (or user-updated goal)
	Todos             []todo.Item `json:"todos,omitempty"`             // Authoritative task list snapshot
	WorkDir           string      `json:"work_dir"`                    // Active working directory
	GitBranch         string      `json:"git_branch,omitempty"`        // Current git branch
	GitHeadCommit     string      `json:"git_head_commit,omitempty"`   // Short SHA + subject of HEAD
	GitStatusShort    string      `json:"git_status_short,omitempty"`  // Output of `git status --short`
	GitDiffStat       string      `json:"git_diff_stat,omitempty"`     // Output of `git diff --stat`
	RecentCommits     []string    `json:"recent_commits,omitempty"`    // Up to 10 recent commits on branch
	PendingAsyncTasks []string    `json:"pending_async_tasks,omitempty"` // Any in-flight swarm tasks at rollover
}

// CheckpointPayload is produced by the orchestrator via the internal `save_checkpoint` tool.
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

type FileContextNote struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"` // Why this file matters / what changed / what to read
}

type DirContextNote struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"` // Area's role and why it is valuable context
}
```

### 3. `SessionState` & `SessionMeta` Extensions

```go
type SessionMeta struct {
	// ... existing fields ...
	CurrentEpoch int `json:"current_epoch,omitempty"` // 1-indexed active epoch (0 treated as 1)
}

type SessionState struct {
	Meta              SessionMeta       `json:"meta"`
	OriginalObjective string            `json:"original_objective,omitempty"` // Captured on Turn 1, immutable across epochs
	Messages          []Message         `json:"messages"`                     // Active epoch messages ONLY
	Epochs            []EpochCheckpoint `json:"epochs,omitempty"`             // Completed epoch summaries + archive pointers
	Todos             []todo.Item       `json:"todos"`
	Inkwell           []InkEntry        `json:"inkwell"`                      // Active epoch Inkwell (or archived per-epoch)
	Audit             AuditTrail        `json:"audit"`                        // Cumulative session-wide audit & token accounting
	DraftMessage      string            `json:"draft_message,omitempty"`
}
```

### 4. On-Disk Storage Layout (Active File + Immutable Epoch Archives)

```
~/.local/share/codecuttlectl/sessions/
├── ses_a3f8c2d1.json                 # Active session state: Meta, OriginalObjective, Epochs[], active epoch Messages & Inkwell, Todos, Audit
└── epochs/
    └── ses_a3f8c2d1/
        ├── epoch-001.json            # Immutable archive of Epoch 1: EpochArchive{SessionID, Epoch, ClosedAt, Checkpoint, Messages, Inkwell}
        ├── epoch-002.json            # Immutable archive of Epoch 2
        └── ...
```

```go
// EpochArchive is the immutable, self-contained JSON document written when an epoch closes.
type EpochArchive struct {
	SessionID  string          `json:"session_id"`
	Epoch      int             `json:"epoch"`
	ClosedAt   time.Time       `json:"closed_at"`
	Checkpoint EpochCheckpoint `json:"checkpoint"`
	Messages   []Message       `json:"messages"`
	Inkwell    []InkEntry      `json:"inkwell"`
}
```

**Why this layout is optimal:**
1. **O(active epoch) save speed**: Every tool call triggers `saveSession()`. Keeping only the active epoch's `Messages` and `Inkwell` in `{id}.json` keeps writes fast and bounded even in Epoch 20.
2. **Immutable indexing units**: Once `epochs/ses_a3f8c2d1/epoch-001.json` is renamed into place, it never changes. A future indexer (SQLite FTS5, pgvector, or Optic Lobe) can ingest each closed archive file exactly once, keyed by `(session_id, epoch, msg.id)`.
3. **Full backwards- AND forward-compatibility (`FileStore` invariant)**:
   - Because `FileStore.List()` and `FileStore.Prune()` already skip subdirectories (`if entry.IsDir() { continue }`), storing closed archives under `epochs/{id}/epoch-NNN.json` guarantees that **both** new and older `codecuttlectl` binaries can never mistake an epoch archive for a top-level session file or accidentally prune active archives.
   - `FileStore.Delete(id)` and `FileStore.Prune(maxAge)` cascade-remove `epochs/{id}/` (`os.RemoveAll`) when removing the parent `{id}.json`.
4. **Inkwell archival vs. cumulative accounting**:
   - On rollover commit, `EpochArchive.Inkwell` captures Epoch N's `InkEntry` slice, and the active `a.inkwell` / `state.Inkwell` resets to empty for Epoch N+1.
   - Meanwhile, `SessionMeta.Stats` (`InputTokens`, `OutputTokens`, `CacheReadInputTokens`, `CacheWriteInputTokens`, `ToolCalls`, `Turns`, `EstimatedCostUSD`) and `AuditTrail` remain strictly **cumulative** across all epochs of the session (never reset or double-counted).
5. **Atomic commit sequence**:
   1. Ensure `epochs/{id}/` exists (`0700`), write `epochs/{id}/epoch-NNN.json.tmp` → `os.Rename` to `epochs/{id}/epoch-NNN.json`
   2. Write updated `{id}.json.tmp` (with `CurrentEpoch = N+1`, `Epochs` appended, `Messages` reset to bootstrap seed, `Inkwell` cleared) → `os.Rename` to `{id}.json`
   - If a crash occurs after step 1 and before step 2, `{id}.json` still cleanly points to Epoch N with all messages intact; an unreferenced archive file is harmlessly overwritten on retry.

### 5. `session.Store` Interface Additions

Extend `internal/session/store.go` with epoch archive methods (implemented in `FileStore`):

```go
type Store interface {
	// ... existing Create, Save, Load, List, Delete, Prune ...

	// SaveEpochArchive atomically persists an immutable closed epoch archive
	// and returns its relative archive path (e.g. "epochs/ses_abc12345/epoch-001.json").
	SaveEpochArchive(id string, archive *EpochArchive) (string, error)

	// LoadEpochArchive retrieves a specific closed epoch archive by session ID and epoch number.
	LoadEpochArchive(id string, epoch int) (*EpochArchive, error)

	// ListEpochArchives returns the sorted epoch numbers archived for a session.
	ListEpochArchives(id string) ([]int, error)
}
```

---

## Internal Tool Contract: `save_checkpoint`

Following the exact harness pattern of `todo_manage`, `tool_info`, and `handoff`:

### Visibility & Scoping
- **Injected ONLY during the Closeout phase** (never present in normal tool definitions).
- **Available ONLY to the orchestrator / primary node** (sub-agents / swarm workers never receive `save_checkpoint`).
- Handled directly inside the harness (TUI and `conversation.Agent`), never dispatched as a Cuttlebone plugin binary.

### JSON Schema

```json
{
  "type": "object",
  "required": ["summary", "lessons_learned", "key_files", "resume_instructions"],
  "properties": {
    "summary": {
      "type": "string",
      "description": "Concise rundown of current progress, architecture decisions made, and state of the task."
    },
    "lessons_learned": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Hard-fought lessons, root causes discovered, gotchas, and dead ends not to repeat."
    },
    "recently_changed_files": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["path", "purpose"],
        "properties": {
          "path": { "type": "string" },
          "purpose": { "type": "string", "description": "Summary of changes made and why." }
        }
      },
      "description": "Files modified, created, or deleted during this epoch."
    },
    "key_files": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["path", "purpose"],
        "properties": {
          "path": { "type": "string" },
          "purpose": { "type": "string", "description": "What this file does and why the fresh epoch must read it." }
        }
      },
      "description": "Priority files the fresh epoch should ingest to get fully up to speed."
    },
    "key_dirs": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["path", "purpose"],
        "properties": {
          "path": { "type": "string" },
          "purpose": { "type": "string", "description": "Area's purpose and why exploring it provides valuable context." }
        }
      },
      "description": "Important directories to explore for full architectural grasp."
    },
    "open_questions": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Unresolved design questions, pending decisions, or risks to verify."
    },
    "resume_instructions": {
      "type": "string",
      "description": "Exact starting point, immediate next actions, and verification commands to pick up right where this epoch leaves off."
    },
    "docs_updated": {
      "type": "array",
      "items": { "type": "string" },
      "description": "List of project README/doc files updated during closeout (when docs policy allowed doc updates)."
    }
  }
}
```

### Harness Validation
When the model invokes `save_checkpoint`:
1. Unmarshal into `CheckpointPayload`.
2. Validate non-empty `Summary`, `ResumeInstructions`, and at least 1 `KeyFiles` entry with non-empty `Path` & `Purpose`.
3. If validation fails (e.g. model left `resume_instructions` blank), return a `ToolResultBlock` with `IsError: true` detailing the missing fields so the model corrects its call within the closeout step budget.
4. Once valid, the harness records the validated payload, returns `"Checkpoint validated and accepted. Closing epoch."`, and transitions immediately to **Commit → Bootstrap** without waiting for another assistant turn.

---

## Rollover Lifecycle & Two-Watermark State Machine

To avoid interrupting atomic multi-file edits while still guaranteeing context never runs away, rollover uses a **two-watermark state machine**:

```
                     ctx >= SoftPct (45%)
     ┌──────────┐ ─────────────────────────► ┌──────────────┐
     │  NORMAL  │                            │    ARMED     │
     └──────────┘ ◄───────────────────────── └──────────────┘
          ▲         (after rollover +               │
          │          bootstrap completes)           │ Quiescent boundary hit:
          │                                         │  • Turn idle (no tool calls)
          │                                         │  • Todo completed + green test
          │                                         │  • Manual /rollover
          │                                         │  OR HardPct (65%) / Armed timeout
          │                                         ▼
     ┌──────────────┐                        ┌──────────────┐
     │ BOOTSTRAPPING│ ◄─── atomic commit ─── │   CLOSEOUT   │
     └──────────────┘                        └──────────────┘
          │                                         │
          │ (model reads key files &                │ (API error / Esc cancel)
          │  resumes task)                          ▼
          └──────────────────────────────► [Stay in current epoch safely]
```

### State Definitions

1. **`Normal` (`ctxPct < SoftThreshold`, default `0.45`)**
   - Standard execution. Layer 1 batched heuristic compaction runs normally.

2. **`Armed` (`SoftThreshold <= ctxPct < HardThreshold`, default `0.45–0.65`)**
   - Only entered when `--auto-rollover` is enabled and cooldown has elapsed.
   - Does **not** interrupt an active multi-file edit.
   - **Gentle wrap-up hint**: On the next model call while `Armed`, the harness appends a single transient system-prompt note (not persisted in history):
     > `[Session Context Notice: Context usage is at 48%. Finish your current atomic unit of work cleanly (e.g., complete the current edit/test pass) so the harness can checkpoint and roll over to a fresh epoch.]`
   - **Quiescent boundary triggers** (any one transitions `Armed → Closeout`):
     - **Turn idle**: Model finishes with `StopReason == "end_turn"` / `"stop"` and zero pending tool calls.
     - **Green task milestone**: A `todo_manage` update transitions a task to `completed` *and* no subsequent build/test `bash_exec` failed (exit code `0` or no errors since last edit).
     - **Manual trigger**: User invokes `/rollover`.

3. **`Forced` (`ctxPct >= HardThreshold`, default `0.65`, OR `Armed` for `> MaxArmedRounds`, default `8` tool rounds)**
   - Prevents runaway context if the agent is stuck in a deep debugging loop that never reaches a clean turn end.
   - Transitions cleanly **after the current tool execution batch finishes** (never killing a running shell command or leaving an unmatched `tool_use` without a `tool_result`).
   - Closeout prompt explicitly instructs the model to document the exact in-progress uncommitted state (paired with `PinnedState.GitDiffStat` and `GitStatusShort`).

4. **`Closeout`**
   - Runs inside the **old epoch's context** with its **own dedicated step budget** (`CloseoutMaxSteps`, default `4` for `docs=none`, `8` when doc updates are enabled), independent of the active turn's `maxSteps` counter so a late-turn rollover never starves closeout.
   - Upon transitioning into `Bootstrapping` for Epoch N+1, the active turn's `step` counter resets to `0` so the fresh epoch gets a full `maxSteps` allowance.
   - Tool policy during Closeout:
     - `save_checkpoint` is **enabled** (required).
     - Read-only inspection tools (`read_file`, `grep`, `glob`, `list_directory`, `git` read-only) are allowed so the agent can verify exact paths/symbols.
     - Write tools (`edit_file`, `write_file`) are **blocked** when `docsPolicy == "none"`, and **permitted only for documentation files** (or with normal approval) when `docsPolicy` triggers doc updates.
     - `handoff` and async `todo_manage` delegation are disabled during Closeout.
   - **Closeout Prompt Template**:
     > Your context window is rolling over to a fresh epoch. Call `save_checkpoint` to persist a comprehensive handoff so the fresh epoch can come fully up to speed immediately. Include:
     > 1. Current progress, architecture decisions, and exact state of the task.
     > 2. All valuable context and hard-fought lessons (gotchas, dead ends not to repeat).
     > 3. Recently changed files with what changed and why.
     > 4. Priority files to ingest and important directories to explore, each with a concise blurb on its purpose and why it is valuable context.
     > 5. Current open questions/risks and exact resume instructions to pick up right where this epoch leaves off.
     > *(When docs policy is active:)* Before calling `save_checkpoint`, update any affected project READMEs or docs to reflect completed work and list them in `docs_updated`.
   - **Fallback if step budget expires without `save_checkpoint`**: Force one final closeout call with `tool_choice` / explicit instruction requiring `save_checkpoint`; if that still fails (e.g., provider outage) or user presses `Esc`, abort rollover cleanly and remain in the current epoch with a warning banner.

5. **`Commit` (Atomic Epoch Boundary)**
   - Harness collects fresh `PinnedState` (`git branch`, `git status --short`, `git diff --stat`, `git log -n 10 --oneline`, `Todos`, `OriginalObjective`).
   - Constructs `EpochCheckpoint` and `EpochArchive`.
   - Atomically persists `epochs/{id}/epoch-NNN.json` then updates `{id}.json` with `CurrentEpoch = N + 1`.
   - **Clean provider state reset**: Clears `m.history` / `a.provHistory`, resets `lastCallInputTokens`, `reasoningDetails`, `reasoningSignature`, and any stale provider-specific signatures so Epoch N+1 starts with a 100% clean wire payload (zero stale OpenRouter `reasoning_details` or Gemini `ThoughtSignature`s crossing epoch boundaries).

6. **`Bootstrapping` (Fresh Context "Get Your Bearings" Phase)**
   - Seeds the new epoch's `Messages` with a single deterministic **Epoch Bootstrap User Message** rendered from `PinnedState` + `CheckpointPayload` + any user steering hint passed via `/rollover <hint>`:
     ```markdown
     # Session Rollover — Epoch 2 (Resumed from Epoch 1)

     ## Pinned Ground Truth (Harness-Verified)
     - **Original Objective**: <OriginalObjective>
     - **Working Directory**: <WorkDir>
     - **Git Branch / HEAD**: <GitBranch> (<GitHeadCommit>)
     - **Uncommitted Changes**:
       <GitStatusShort / GitDiffStat>
     - **Active Task List**:
       - [completed] ...
       - [in_progress] ...
       - [pending] ...

     ## Handoff from Epoch 1
     ### Current State
     <Summary>

     ### Hard-Fought Lessons & Pitfalls
     - ...

     ### Recently Changed Files
     - `path/to/file.go` — ...

     ### Priority Files to Ingest (Get Your Bearings)
     - `path/to/key.go` — ...

     ### Important Directories
     - `internal/foo/` — ...

     ### Open Questions
     - ...

     ### Resume Instructions
     <ResumeInstructions>

     ---
     **Orientation Protocol**: Before making edits, spend 1–3 tool rounds reading the listed priority files (and checking current git diff if uncommitted changes exist) to verify ground truth on disk, then immediately resume execution from the Resume Instructions above.
     ```
   - **Execution behavior after seeding**:
     - If **auto-rollover** triggered mid-task (or in `--continuous` / headless mode): automatically launches the bootstrap turn so the agent orients itself on disk and keeps working without human intervention.
     - If **manual `/rollover`** triggered while idle at the prompt: either auto-runs a brief orientation pass (default when active `in_progress`/`pending` todos exist) or waits at the prompt ready for your next instruction with the seeded context loaded (configurable; e.g., `/rollover` while idle with no pending todos seeds and waits, or `/rollover --resume` starts orienting immediately).

### Anti-Thrash Cooldown & Loop Safety
Even when `--max-epochs=0` (unlimited) and `--max-cost=0` (unlimited):
- **Minimum epoch tenure**: Auto-rollover will not arm until the new epoch has completed at least `MinEpochTurns = 2` (or `MinEpochSteps = 4`) **and** at least one fresh API `UsageEvent` from the new epoch confirms `ctxPct >= SoftThreshold`.
- This guarantees a misconfigured tiny context window or stale token counter can never spin an infinite loop of empty epochs.
- If `--max-epochs > 0` and `CurrentEpoch >= MaxEpochs`, or `--max-cost > 0` and `EstimatedCostUSD >= MaxCostUSD`: auto-rollover pauses cleanly at the next boundary and displays an actionable notice (`Rollover budget reached (max-epochs=5); use /rollover manually or raise limit`).

---

## Repo Hygiene & Documentation Policy (`--rollover-docs`)

To keep automated rollovers from polluting focused bugfix PRs while still supporting documentation updates on long feature branches, session checkpoints and project doc updates are strictly decoupled:

| Policy (`--rollover-docs`) | Session Checkpoint | Project READMEs / Docs | Best For |
|---|---|---|---|
| **`none`** *(default)* | Always saved to Session Store (`epochs/{id}/epoch-NNN.json`) | Never touched during closeout | Bugfix PRs, surgical changes, clean git history |
| **`milestone`** | Always saved to Session Store | Updated only when closeout triggers on a **green milestone** (completed todo + passing tests) | Multi-epoch feature branches |
| **`always`** | Always saved to Session Store | Always prompts closeout agent to update affected READMEs/docs | Exploratory / long-lived architecture work (matches manual workflow) |

Manual overrides always win: `/rollover --docs` forces doc updates for that specific rollover regardless of the session's default policy; `/rollover --no-docs` skips doc updates.

---

## Swarm Morphology Interaction

In a multi-agent swarm (`orchestrator`, `deep_coder`, `flash_coder`):

1. **Orchestrator-only ownership**: Only the primary/orchestrator node monitors rollover thresholds, holds `RolloverState`, and receives the `save_checkpoint` tool during closeout.
2. **Async background tasks (`todo_manage` with `async: true`)**:
   - Background workers (`flash_coder`) already execute in isolated, fresh context windows seeded from a compact summary — they never need rollover.
   - If async background tasks are still running when rollover triggers:
     - During `Armed`, prefer waiting for in-flight async tasks to finish if `ctxPct < HardThreshold`.
     - If forced at `HardThreshold` while async tasks are still running, record their task IDs/descriptions in `PinnedState.PendingAsyncTasks`; when their completion events arrive later in Epoch N+1, they inject cleanly into Epoch N+1's history just as they do today.
3. **Synchronous `handoff` (`deep_coder` / `flash_coder`)**:
   - Synchronous handoffs share the orchestrator's active message history (sanitized via `provider.EffectiveID`).
   - Rollover never triggers while a non-orchestrator node holds control; if context crosses a threshold during a synchronous handoff, it arms immediately upon handoff return to `orchestrator`. Rolling over on `orchestrator` automatically keeps subsequent synchronous handoffs lean.

---

## CLI Flags, Slash Commands & TUI UX

### CLI Flags (`cmd/codecuttlectl/main.go`)

| Flag | Type | Default | Description |
|---|---|---|---|
| `--auto-rollover` | `bool` | `false` | Automatically checkpoint and roll over to a fresh epoch when context usage reaches the rollover threshold |
| `--rollover-soft-pct` | `float64` | `0.45` | Soft context utilization threshold (`0.0–1.0`) to arm rollover and wait for a quiescent boundary |
| `--rollover-hard-pct` | `float64` | `0.65` | Hard context utilization threshold (`0.0–1.0`) to force closeout after the current tool round |
| `--rollover-docs` | `string` | `"none"` | Project doc update policy on rollover: `none`, `milestone`, or `always` |
| `--max-epochs` | `int` | `0` | Maximum epochs allowed for automatic rollover (`0` = unlimited) |
| `--max-cost` | `float64` | `0` | Maximum cumulative session cost in USD before pausing automatic rollover (`0` = unlimited) |

### Unified Slash Command Registry (`internal/tui/commands.go`)

Replace the ad-hoc `if text == "/reset"` / `if text == "/plugins"` checks in `internal/tui/app.go` with a structured command registry (`CommandRegistry`) backed by runtime actions — keeping slash commands clean in the TUI today and trivially callable from a future Vim/Neovim mux/RPC integration:

| Command | Behavior |
|---|---|
| `/rollover [--docs\|--no-docs] [hint...]` | Immediately initiates a manual epoch rollover (optional doc update override and steering note for the fresh epoch) |
| `/epochs` | Displays a compact table of completed epochs (epoch #, timestamp, trigger, turns, tokens at rollover, summary blurb) |
| `/auto-rollover [on\|off\|status]` | Toggles or inspects automatic rollover settings (`soft%`, `hard%`, `docs`, `max-epochs`, `max-cost`) live mid-session |
| `/reset` | Existing behavior (clears conversation history) |
| `/plugins` | Existing behavior (lists loaded plugins) |
| `/help` | Lists all available slash commands and usage |

### TUI Visual UX

1. **Status Bar**:
   - Displays epoch indicator alongside context percentage when `CurrentEpoch > 1` (or when `--auto-rollover` is enabled), e.g.: `E2 · ctx 14% · auto-rollover`.
   - When `Armed`, subtly highlights context indicator: `E1 · ctx 48% (rollover armed)`.
2. **Transition Banners**:
   - Entering Closeout:
     `↻ Context at 48% — rolling over Epoch 1 → Epoch 2 (checkpointing... Esc to cancel)`
   - Entering Bootstrap:
     `↻ Epoch 2 started — orienting on 5 priority files from Epoch 1 checkpoint`
3. **Scrollback Continuity**:
   - TUI viewport keeps a clean visual divider (`────────── Epoch 2 (checkpointed at 48% ctx) ──────────`) above the new epoch's messages while preserving scrollback readability during the active TUI session.
4. **REPL (`--no-tui`) & Headless (`--message`)**:
   - REPL supports the exact same slash commands and prints concise stderr/stdout status lines on rollover.
   - Headless (`--message` with `--auto-rollover`) executes closeout → bootstrap transparently within `Agent.TurnProvider` / `Agent.StreamTurnProvider`, bounded by `--max-epochs` and `--max-cost` if set.

---

## Edge Cases & Failure Modes

| Scenario | Handling |
|---|---|
| **User presses `Esc` during Closeout** | Cancels the closeout stream, resets state back to `Normal` (with a 1-turn manual snooze so auto-rollover doesn't immediately re-fire on the same turn), preserves Epoch N intact. |
| **Provider returns 429 / 5xx / `MALFORMED_RESPONSE` during Closeout** | Standard provider retry logic runs first; if closeout still fails terminally, abort rollover safely, keep Epoch N intact, and surface error banner. |
| **OpenRouter `reasoning_details` / Gemini `ThoughtSignature`** | Never carried across epoch boundaries. Epoch N+1 starts with a fresh user bootstrap message, eliminating accumulated encrypted signature bloat and cross-turn signature mismatches. |
| **Provider / model switch across epochs** | `SessionMeta.Model` records latest model; each `session.Message` records its own `Model`; `Bootstrap` message is pure text so any target provider (Bedrock, OpenRouter, Google, Ollama) ingests it cleanly. |
| **Session resume (`--session ses_xxx`)** | Loads `{id}.json` directly into `CurrentEpoch` with active epoch `Messages` and `Epochs[]` metadata. Prior epochs remain accessible via `/epochs` and `LoadEpochArchive`. |
| **Context window unknown (`0`)** | `ShouldArmRollover` returns `false` when `contextWindow <= 0` (cannot compute percentage reliably); manual `/rollover` still works unconditionally. |
| **Non-git working directory** | `PinnedState` populates `WorkDir`, `OriginalObjective`, and `Todos`, leaving `Git*` fields empty without error. |
| **Tiny / single-turn session manual `/rollover`** | Works cleanly even with 1 turn; `save_checkpoint` summarizes whatever context exists and opens Epoch 2. |

---

## Phased Implementation Plan

### Phase 1: Foundation, Timestamps, Slash Commands & Manual `/rollover`
1. **Message timestamps & provenance (`internal/session/message.go`, `internal/session/types.go`)**:
   - Add `ID`, `Epoch`, `Turn`, `Timestamp`, `NodeID`, `Model` to `session.Message` and `Timestamp` to `ContentItem`.
   - Add `EpochCheckpoint`, `PinnedState`, `CheckpointPayload`, `EpochArchive`, `OriginalObjective`, and `CurrentEpoch` to `session/types.go`.
   - Implement `SaveEpochArchive`, `LoadEpochArchive`, `ListEpochArchives`, and cascade delete/prune in `internal/session/filestore.go`.
2. **Rollover package (`internal/rollover/`)**:
   - `types.go`: `Config`, `State` (`Normal`, `Armed`, `Closeout`, `Bootstrapping`), `DocsPolicy`.
   - `pinned.go`: Deterministic `PinnedState` collector (git branch, status, diff stat, recent commits, todos, original objective).
   - `tool.go`: `save_checkpoint` tool schema (`provider.ToolDefinition`), parser, and validator.
   - `prompt.go`: Closeout prompt builder (respecting `DocsPolicy`) and Bootstrap seed message renderer.
3. **Slash Command Registry & Manual `/rollover` (`internal/tui/commands.go`, `internal/tui/app.go`)**:
   - Migrate `/reset` and `/plugins` into `CommandRegistry`; add `/rollover`, `/epochs`, `/auto-rollover`, `/help`.
   - Wire closeout execution, `save_checkpoint` interception, atomic epoch commit, and bootstrap seeding in TUI and `conversation.Agent`.
4. **Tests**:
   - Unit tests for `session` timestamps, epoch archive atomic write/load/prune, legacy session backward compatibility.
   - Unit tests for `rollover` schema validation, pinned state collection (git + non-git), bootstrap rendering.
   - End-to-end TUI + Agent manual `/rollover` tests (including `Esc` cancellation rollback and session resume into Epoch 2).

### Phase 2: Autonomous `--auto-rollover`, Two-Watermark State Machine & Budgets
1. **State machine (`internal/rollover/controller.go`)**:
   - Implement `Normal → Armed → Closeout → Bootstrapping` transitions with `SoftPct` (`0.45`), `HardPct` (`0.65`), `MaxArmedRounds`, quiescent boundary detection (turn idle / green todo completion), anti-thrash cooldown, and `--max-epochs` / `--max-cost` budget guards.
2. **CLI flags & TUI banners (`cmd/codecuttlectl/main.go`, `internal/tui/app.go`, `internal/conversation/agent_provider.go`)**:
   - Wire `--auto-rollover`, `--rollover-soft-pct`, `--rollover-hard-pct`, `--rollover-docs`, `--max-epochs`, `--max-cost`.
   - Add TUI status bar epoch badge (`E2`), armed indicator, rollover transition banners, and scrollback divider.
   - Enforce orchestrator-only ownership in swarm morphologies and handle pending async tasks cleanly.
3. **Tests**:
   - State-machine table tests (soft arming, quiescent trigger, hard force, cooldown anti-thrash, budget stop, swarm non-orchestrator guard).
   - Crash-recovery tests simulating failure between archive write and active session save.
   - TUI visual/state tests for banners, epoch dividers, and `/auto-rollover` live toggles.

### Phase 3: Historical Retrieval & Indexing (Future Integration with Optic Lobe)
1. **Epoch Indexer (`internal/index/` or Optic Lobe)**:
   - Incrementally index immutable `epochs/{id}/epoch-NNN.json` files (messages, tool calls, timestamps, checkpoints) into SQLite FTS5 and/or pgvector.
2. **Internal retrieval tool (`search_session_history`)**:
   - Allow an agent in Epoch N to query prior epochs by keyword, time range, file path, or semantic similarity and pull exact historical snippets on demand without bloating the active window.

---

## Target File Map

| Path | Responsibility |
|---|---|
| `internal/provider/provider.go` | Provenance metadata fields on `provider.Message` (`ID`, `Epoch`, `Turn`, `Timestamp`, `NodeID`, `Model`) |
| `internal/provider/convert.go` & `internal/compact/compact_provider.go` | Preserve `provider.Message` provenance metadata through sanitization and compaction |
| `internal/session/types.go` | `EpochCheckpoint`, `PinnedState`, `CheckpointPayload`, `EpochArchive`, `CurrentEpoch`, `OriginalObjective` |
| `internal/session/message.go` | Timestamp & provenance fields on `Message` / `ContentItem` |
| `internal/session/store.go` | `SaveEpochArchive`, `LoadEpochArchive`, `ListEpochArchives` on `Store` interface |
| `internal/session/filestore.go` | Atomic `epochs/{id}/epoch-NNN.json` persistence and cascade delete/prune |
| `internal/rollover/types.go` | `Config`, `State`, `DocsPolicy`, `RolloverTrigger` |
| `internal/rollover/controller.go` | Two-watermark state machine, quiescent detection, cooldown & budget guards |
| `internal/rollover/tool.go` | `save_checkpoint` tool definition, JSON unmarshaling, validation |
| `internal/rollover/pinned.go` | Deterministic git + todos + objective snapshot collector |
| `internal/rollover/prompt.go` | Closeout prompt & Bootstrap seed renderer |
| `internal/tui/commands.go` | Unified `/` command registry (`/rollover`, `/epochs`, `/auto-rollover`, `/reset`, `/plugins`, `/help`) |
| `internal/tui/app.go` | Closeout/bootstrap orchestration, status bar epoch badge, banners, scrollback divider |
| `internal/conversation/agent_provider.go` | Headless/REPL closeout/bootstrap loop & `save_checkpoint` handler |
| `cmd/codecuttlectl/main.go` | CLI flags (`--auto-rollover`, `--rollover-soft-pct`, `--rollover-hard-pct`, `--rollover-docs`, `--max-epochs`, `--max-cost`) |

---

## Verification Strategy

Before merging each implementation phase:
1. `go test -race ./...` — Full unit and race-detector suite including new `internal/rollover`, `internal/session`, `internal/tui`, and `internal/conversation` tests.
2. `go vet ./...` and `go build ./...` — Static analysis and full binary compilation.
3. **End-to-end rollover verification**:
   - Manual `/rollover` in TUI with mock streaming provider verifying `save_checkpoint` validation retry, atomic archive write, clean Epoch 2 bootstrap, and lossless `--session` resume.
   - Simulated multi-epoch `--auto-rollover` run verifying soft-threshold arming, quiescent trigger, hard-threshold fallback, anti-thrash cooldown, and `--max-epochs`/`--max-cost` stopping behavior.
   - Swarm test verifying `save_checkpoint` is never exposed to worker nodes and synchronous handoffs inherit the rolled-over history cleanly.
