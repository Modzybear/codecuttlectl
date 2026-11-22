# Sessions and the Inkwell

## Overview

Every `codecuttlectl` invocation creates a session. Sessions persist the full conversation state — history, tool execution diagnostics, task lists — enabling resume, audit, and the Inkwell reconciliation loop.

## Session Storage

**Location:** `~/.local/share/codecuttlectl/sessions/` (respects `$XDG_DATA_HOME`)

Each session is a single JSON file named `{session_id}.json`. Writes are atomic (write to `.tmp`, then rename) to prevent corruption from crashes or power loss.

### Session File Contents

```json
{
  "meta": {
    "id": "ses_a3f8c2d1",
    "alias": "auth-service-refactor",
    "pinned": true,
    "created_at": "2026-06-01T08:00:00Z",
    "updated_at": "2026-06-01T08:15:30Z",
    "title": "Fix Authentication Bug",
    "model": "us.anthropic.claude-opus-4-6-v1",
    "morph": "openrouter-expert-swarm",
    "tags": ["backend", "auth"],
    "region": "us-east-1",
    "work_dir": "/home/user/project",
    "current_epoch": 2,
    "stats": {
      "input_tokens": 12500,
      "output_tokens": 4200,
      "tool_calls": 15,
      "turns": 4
    }
  },
  "messages": [...],
  "todos": [...],
  "inkwell": [...]
}
```

### Session Identifiers, Aliases, and Pinning

- **Session IDs**: Standard auto-generated identifiers starting with `ses_` (e.g. `ses_a3f8c2d1`).
- **Session Aliases**: Friendly human-readable names assigned to sessions.
  - **Validation Rules**: Must match `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$` (1 to 64 alphanumeric characters, dots, underscores, or hyphens, starting with an alphanumeric character).
  - **Reserved Prefix**: Aliases cannot begin with the reserved prefix `ses_` (case-insensitive) to prevent collisions with raw session IDs.
  - **Uniqueness**: Aliases are globally unique across all sessions (case-insensitive match).
- **Session Pinning (`pinned`)**: Pinned sessions are highlighted in listings and **exempt from pruning**. When running `--prune-sessions`, pinned sessions are never deleted regardless of age.
- **Recency Preservation (`UpdateMeta`)**: Metadata modifications (such as pinning, renaming, or updating titles via `/pin`, `/alias`, `/title`) use atomic `UpdateMeta` callbacks that preserve the original `updated_at` timestamp so metadata tweaks do not artificially promote old sessions in recency-based lists.
- **Pinned-First Sorting**: Session listings sort all pinned sessions first, followed by unpinned sessions ordered by `updated_at` descending.

---

## Interactive Session Picker (`codecuttlectl -s`)

Running `codecuttlectl -s` (or `codecuttlectl --sessions`) launches a full-screen interactive Bubble Tea picker to browse, search, manage, and resume sessions with live metadata preview.

### Picker Keybindings

| Key | Action |
|:---|:---|
| `↑` / `↓` or `k` / `j` | Navigate up / down through sessions |
| `Enter` | Select and resume highlighted session |
| `n` | Start a new session |
| `/` | Enter search filter mode (fuzzy matches ID, alias, title, workdir) |
| `f` / `Tab` | Cycle filter scope (e.g., current directory vs all sessions) |
| `p` | Toggle pinned status (★) on selected session |
| `a` | Set or update friendly alias |
| `t` | Edit session title |
| `d` | Delete selected session (prompts for confirmation) |
| `q` / `Esc` / `Ctrl+C` | Quit picker |

*Note: In non-TTY environments (or when stdin/stdout is piped), `-s` / `--sessions` automatically falls back to `--list-sessions` table (or `--json`) output and exits cleanly.*

---

## CLI Session Management

```bash
# Interactive session picker
codecuttlectl -s
codecuttlectl --sessions

# Resume by ID or friendly alias (shorthand -r or --session)
codecuttlectl -r auth-service-refactor
codecuttlectl --session ses_a3f8c2d1

# Auto-restoring behavior:
# When resuming, codecuttlectl automatically switches work_dir to the session's
# original directory (if --workdir is omitted) and reloads its saved morphology.

# List recent sessions with Epoch, WorkDir, and Alias columns
codecuttlectl --list-sessions

# List only pinned sessions
codecuttlectl --list-sessions --pinned

# Search sessions by title, alias, or ID
codecuttlectl --list-sessions --search "auth refactor"

# Output session listings as JSON for automation
codecuttlectl --list-sessions --json

# Prune unpinned sessions older than N days (pinned sessions are preserved)
codecuttlectl --prune-sessions 30
```

### In-Session TUI Slash Commands

Within active sessions, users can manage session metadata on the fly:

- `/pin` — Toggles pinned status for the active session. Pinned sessions are marked with `★` and protected from `--prune-sessions`.
- `/alias <name>` — Assigns a friendly alias to the active session (e.g. `/alias backend-auth`).
- `/alias --clear` — Clears the alias from the active session.
- `/alias` — Displays the currently assigned alias.
- `/title <text>` — Overrides or updates the active session title (e.g. `/title Refactor Auth Middleware`).
- `/title` — Displays the current session title.

## The Inkwell

Named after the cuttlefish ink defense mechanism. The Inkwell captures diagnostic data from every tool execution within a session.

### InkEntry Structure

```json
{
  "timestamp": "2026-06-01T08:10:15Z",
  "turn": 2,
  "tool_name": "bash_exec",
  "tool_use_id": "tooluse_abc123",
  "input": {"command": "go build ./..."},
  "output": "internal/foo.go:15: undefined: bar",
  "duration_ms": 1350,
  "is_error": true,
  "error_type": "compile"
}
```

### Error Classification

The Inkwell classifies errors by language and type:

| Class | Examples |
|-------|---------|
| `compile` | Build failures, linker errors |
| `syntax` | Missing brackets, unexpected tokens |
| `type` | Type mismatches, interface violations |
| `import` | Missing packages, undefined names |
| `not_found` | File/directory doesn't exist |
| `permission` | Access denied |
| `timeout` | Command timed out |
| `network` | Connection refused, DNS failure |
| `runtime` | Panics, exceptions, general failures |

Language-specific parsers extract file paths, line numbers, and structured error messages from Go, Python, TypeScript, and Rust compiler output.

## Reconciliation Loop

The reconciler analyzes recent Inkwell entries before each model call and dynamically injects corrective guidance into the system prompt.

### Behavior

| Condition | Response |
|-----------|----------|
| Single error | Inject targeted correction (file, line, suggestion) |
| Same tool fails 3+ times | Loop detection: "STOP, change strategy" |
| 5+ consecutive failures | Escalation: "Report failure, propose alternatives" |
| No recent errors | No injection (zero overhead) |

### How It Works

1. After each tool execution, the result is analyzed by `inkwell.Classify()`
2. Before the next `Converse()` call, `effectiveSystemPrompt()` runs:
   - Base system prompt
   - Reconciler advice (if errors detected)
   - Matching skills (from plugin registry)
3. The model sees the corrective guidance and adjusts its approach

### Design Principles

- **Zero cost when things work** — The reconciler does nothing if the last entry wasn't an error
- **Proportional response** — Single errors get gentle nudges; loops get firm redirects
- **Model-aware** — Guidance is in natural language the model understands, not code
- **Non-blocking** — The reconciler never blocks execution; it only modifies the next prompt

## Future: PostgreSQL (Optic Lobe)

The current `FileStore` implementation is designed behind a `Store` interface. A future PostgreSQL implementation will enable:

- **pgvector**: Semantic similarity search on historical errors (find similar past failures and their solutions)
- **Apache AGE**: Graph traversal of execution dependencies (trace causation chains)
- **Cross-session learning**: Apply lessons from past sessions to new ones
- **Fleet-scale telemetry**: Aggregate diagnostics across multiple agents

The interface provides full lifecycle operations:

```go
type Store interface {
    Create(meta SessionMeta) (string, error)
    Save(id string, state *SessionState) error
    Load(id string) (*SessionState, error)
    List(limit int) ([]SessionMeta, error)
    Delete(id string) error
    Prune(maxAge time.Duration) (int, error)
    ResolveMeta(idOrAlias string) (SessionMeta, error)
    UpdateMeta(id string, fn func(*SessionMeta) error) error
    SaveEpochArchive(id string, archive *EpochArchive) (string, error)
    ListEpochArchives(id string) ([]int, error)
}
```
