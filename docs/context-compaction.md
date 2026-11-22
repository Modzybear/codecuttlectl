# Context Compaction

## Overview

Context compaction replaces old, verbose tool results in the conversation history with concise summaries. This keeps the context window lean without losing the model's awareness of what was there — it can always re-read files or re-run commands if it needs the full content again.

**Key invariant**: The Inkwell and session file always retain full-fidelity content. Compaction only affects what's sent to the model in subsequent API calls.

## Problem

A typical coding session accumulates large tool results:
- `read_file` on a 1600-line file: ~8k tokens
- `grep` across a project: ~2-4k tokens
- `bash_exec` (build output): ~1-3k tokens
- `websearch` results: ~3-5k tokens

After 10+ turns with file reads, 100-200k tokens of stale tool results sit in the context window. The model is paying (in latency and cost) to re-process content it looked at 15 minutes ago and doesn't need byte-for-byte.

## Current Implementation: Layer 1 — Heuristic Tool-Result Compaction

### When It Triggers

Compaction distinguishes between **large cloud/frontier models** (`DefaultConfig`) and **small/local models** (`SmallModelConfig`, gated by `needsGroundingAssist()` — Ollama models or providers explicitly reporting `<= 262k` context):

- **Preserve boundary**: Computes `max(turnBoundary, toolRoundBoundary)` so both multi-turn chats and long single-turn tool-call chains (100+ tool rounds) compact stale tool outputs while preserving the recent working window.
- **Large models (prefix-cache friendly batching)**:
  - Default window preserves the last **7 user turns** and **12 tool rounds** (`MinResultSize: 1000` chars).
  - Mid-turn, `CompactProviderBatched` defers compaction until at least **`MinCompactBatch = 6`** newly compactable tool results have accumulated — invalidating prompt prefix caches in chunks rather than on every single tool round.
  - Compaction is forced immediately at new user turn boundaries (`streamStep == 0`) or when context usage reaches **50%** (`ShouldCompact`), which also tightens preservation to **3 user turns** and **5 tool rounds**.
- **Small / local models**:
  - Preserves **2 user turns** and **4 tool rounds** (`MinResultSize: 500` chars, `SummaryMaxLines: 6`), compacting eagerly on every step.

### What It Does

For tool results before the preserve boundary:
- Already-compacted summaries (detected via omission markers) are skipped — preventing re-compaction loops.
- Results smaller than `MinResultSize` are left alone.
- Multi-line results exceeding `SummaryMaxLines` are replaced with a head/tail summary:

```
line 1: package tui
line 2: 
line 3: import (
line 4:     "context"

[... 1592 lines omitted — use read_file with offset/limit to retrieve specific sections ...]

line 1597: func sanitizeModelText(text string) string {
line 1598:     cleaned := xmlTagPattern.ReplaceAllString(text, "")
line 1599:     for strings.Contains(cleaned, "\n\n\n") {
line 1600:         cleaned = strings.ReplaceAll(cleaned, "\n\n\n", "\n\n")
```

- Huge single-line or few-line results (e.g., minified JSON where line count `<= SummaryMaxLines` but length `>= MinResultSize`) fall back to UTF-8-safe character head/tail truncation with `[... N chars omitted — use read_file with offset/limit to retrieve specific sections ...]`.

The model sees: "I read this file, here's what the beginning and end look like, and I can re-read specific sections with offset/limit if needed."

### What It Preserves

- **Recent turns & tool rounds**: Last 7 user turns / 12 tool rounds on large models (tightened to 3 / 5 at ≥50% ctx); last 2 turns / 4 tool rounds on small models
- **Small results**: Anything under `MinResultSize` (1000 chars default, 500 small models) passes through unchanged
- **Inkwell**: Full content, always — governance and audit are never compromised

### Cache Impact

Compaction mutates the message history, which invalidates the Tier 3 (messages) prefix cache on the next call. By **batching** mid-turn compactions (`MinCompactBatch: 6`) and skipping already-compacted summaries, prefix caches remain warm across consecutive tool rounds instead of thrashing on every step:
- One cache write penalty per batch of 6+ stale tool results (amortized across the next 6+ cached calls)
- Tier 1 (tools) and Tier 2 (system prompt) caches are unaffected

## Configuration

```go
compact.DefaultConfig() // Large / frontier models:
compact.Config{
    MaxContextPercent:        0.50, // Tighten & force when >= 50% of context window used
    PreserveRecentTurns:      7,    // Preserve last 7 user turns (3 when >= 50%)
    PreserveRecentToolRounds: 12,   // Preserve last 12 tool rounds (5 when >= 50%)
    SummaryMaxLines:          8,    // Head + tail lines in summary
    MinResultSize:            1000, // Don't compact results under 1000 chars
    MinCompactBatch:          6,    // Batch mid-turn compactions to protect prefix cache
}
```

These defaults are currently hardcoded. Future: expose via CLI flags or config file.

## Architecture

```
                    ┌─────────────────────────────┐
                    │     Context Window (1M)      │
                    │                              │
                    │  Tools [cached, 12k]         │
                    │  System [cached, 6k]         │
                    │  Messages:                   │
                    │    Turn 1: [COMPACTED]       │  ← Summary only
                    │    Turn 2: [COMPACTED]       │  ← Summary only
                    │    ...                       │
                    │    Turn N-1: [FULL]          │  ← Preserved
                    │    Turn N: [FULL]            │  ← Preserved (current)
                    └─────────────────────────────┘
                                 │
                    ┌────────────┴────────────┐
                    │                         │
            ┌───────▼───────┐        ┌───────▼───────┐
            │  Session File │        │    Inkwell    │
            │  (FULL always)│        │ (FULL always) │
            └───────────────┘        └───────────────┘
```

## Future Enhancements

### Layer 2: Epoch Rollover Compaction (Continuous Ultra-Long Sessions)

Heuristic tool-result compaction shrinks stale tool outputs within an epoch, but cannot eliminate trajectory rot, dead-end debugging prose, or attention dilution past ~45–50% context fill. **Epoch Rollover Compaction** automates a structured `closeout → save_checkpoint → fresh-context bootstrap` cycle under a single persistent session ID, archiving closed epochs as immutable, fully timestamped JSON files (`epochs/{id}/epoch-NNN.json`) and seeding the new epoch with deterministic pinned state + agent-synthesized lessons, key files/directories, and resume instructions.

**Full design**: [`docs/epoch-rollover-design.md`](epoch-rollover-design.md)

### Phase 2: LLM-Generated Tool Summaries

Replace the heuristic head/tail truncation with model-generated summaries. A Converse API call (same model, `max_tokens=200`) produces richer summaries:

```
[read_file: internal/tui/app.go — 1614 lines, Bubble Tea TUI]
Key structures: Model{} with viewport, input, spinner, streaming state.
Token tracking: totalInputTokens, lastCallInputTokens (for ctx%).
Status bar renders at line 1226 with cache%, ctx%, cost.
launchStream() at line 745 calls maybeCompact() then ConverseStream.
Tool execution in executePendingTools() at line 895.
```

This costs ~$0.08 per summarization (Opus 4.6) — expensive, so it's triggered conditionally:
- Only for results >5000 chars that are >3 turns stale
- Capped by a per-session budget (default $1.00)
- Falls back to heuristic compaction when budget is exhausted or summarization is slow

**Full design**: [`docs/llm-compaction-design.md`](docs/llm-compaction-design.md)

### Phase 3: Optic Lobe Integration (pgvector)

When compacting, store the full content in the Optic Lobe with:
- **pgvector embedding** of the content (for semantic retrieval)
- **Metadata**: file path, tool name, turn number, timestamp
- **Raw text** in PostgreSQL for exact retrieval

The compacted summary in the context window gains a retrieval reference:

```
[read_file: internal/tui/app.go — compacted, optic_ref=ol_a3f8c2d1]
TUI model with Bubble Tea. Key: renderStatusBar (line 1226), 
launchStream (line 745), executePendingTools (line 895).
```

A new built-in `recall` tool lets the model pull content back:

```json
{"type": "object", "properties": {
  "ref": {"type": "string", "description": "Optic Lobe reference ID"},
  "query": {"type": "string", "description": "What to retrieve (semantic search)"}
}}
```

### Phase 4: Cross-Session Retrieval

Once the Optic Lobe is populated across sessions, the model can retrieve relevant context from *previous sessions*:

- "Last time we worked on the status bar, we changed X at line Y"
- "The caching doc says Z about cache invalidation"
- Error patterns: "This build error was fixed in session ses_xyz by doing W"

This is where compaction, the Inkwell, and the Optic Lobe fully converge:

| Layer | What it stores | How it's accessed |
|-------|---------------|-------------------|
| **Context window** | Summaries + recent full content | Directly by the model |
| **Inkwell (session file)** | Full I/O for all tool calls this session | Resume, audit, governance |
| **Optic Lobe (pgvector)** | Embedded content from all sessions | `recall` tool, semantic search |
| **Apache AGE (graph)** | Causal relationships (error→fix, file→change) | Graph traversal queries |

### Phase 5: Adaptive Compaction

Instead of a fixed threshold, the system learns which content is worth keeping:
- Content that was re-read → increase its preservation priority
- Content from files that were later edited → higher relevance
- Content that was never referenced again → compact aggressively
- Error outputs that led to fixes → keep the error, compact the rest

This feeds back from Inkwell patterns: if the model re-reads a file within 3 turns of its last read, that file's content has high "temporal locality" and should be preserved longer.

## Design Principles

1. **Never lose data** — Compaction is a context window optimization, not a deletion. Full content lives in Inkwell and session files forever.
2. **Transparent to the model** — The omission marker tells the model exactly what was removed and how to get it back (`read_file with offset/limit`).
3. **Conservative by default** — 50% threshold, preserve 7 recent turns / 12 tool rounds, 1000-char minimum. Better to over-preserve than to lose something the model needs.
4. **Cache-friendly batching** — Compaction batches mid-turn updates (`MinCompactBatch: 6`) so prefix caches stay warm across consecutive tool rounds instead of thrashing on every step.
5. **Composable with future systems** — Summaries become metadata for vector retrieval. The architecture layers cleanly without rewrites.
