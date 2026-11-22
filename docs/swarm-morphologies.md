# Swarm Morphologies (Morphs)

## Overview

As `codecuttlectl` evolves beyond a single-model coding assistant, we are introducing **Swarm Morphologies** (or **Morphs**). A Morphology is a declarative configuration that defines a multi-agent system, detailing the models, providers, system prompts, skills, and the topologies through which they interact.

This replaces the hardcoded `Primary / Planning / Auxiliary` Bedrock-only Model Pool with a generic, extensible, provider-agnostic framework supporting **Bedrock, Google, OpenRouter, and Ollama**.

> **Status:** this document mixes shipped mechanics with long-term design. As of PR #70 (open), synchronous handoff and basic morphology parsing/routing work, but asynchronous backlog scheduling is partial and the features below (event triggers, provider fallbacks, full trace synthesis, distributed/remote agents) are conceptual, not all shipped. See the runtime-contracts ledger and the merge-gate notes for the current limitations.

## Motivation

Modern agentic workflows require specialization. A single frontier model (like Claude Opus or Gemini Pro) is often too expensive or slow for every sub-task, while a smaller model (like Haiku or local Llama 3) might lack the reasoning required for complex architectural decisions.

By defining a **Morphology**, users can orchestrate a swarm where:
1. A local `Ollama` model handles quick syntax checks or code formatting.
2. A fast `Google` model (Gemini Flash) drafts execution plans or performs deep web searches.
3. A frontier `Bedrock` model (Claude Opus) synthesizes the final output and performs delicate file edits.

## Core Concepts

### 1. Nodes (Agents) & Strict Tool Sandboxing
A swarm is composed of multiple discrete agents, referred to as **Nodes**. Each node is configured with:
*   **Provider:** `bedrock`, `google`, `openrouter`, or `ollama`.
*   **Model ID:** The specific model identifier for that provider.
*   **System Prompt:** A tailored prompt giving the node its persona and instructions.
*   **Workbench (Skills):** An allowlist of tool *names* exposed to the node. A "reviewer" node might only get `read_file` and `git`. Workbenches do NOT enforce subcommands, OS file permissions or network access: a node granted `git` or `bash_exec` is not a read-only OS sandbox, and native-tool authorization in the TUI path is still incomplete (an out-of-workbench `todo_manage` call must be gated consistently — see the runtime-contracts ledger).
*   **Max Concurrency:** The maximum number of parallel instances of this node for asynchronous tasks (default: 1). It is a scheduling bound for local resource use; it does not by itself prevent provider rate limiting or file conflicts.

### 2. Presentation Modes & Epistemic Transparency
A morphology can present itself to the user in different ways to solve the "trust calibration problem":
*   **Single Agent (Hidden Committee):** The user interacts with the system as if it were a single entity. The primary node coordinates with planners and reviewers silently. *Crucially*, to avoid the "Aggregation Paradox" (where majority voting destroys correct intermediate logic), the orchestrator is fed the **full reasoning traces** of the subordinate nodes, performing **Trace-Level Synthesis** rather than a simple vote.
*   **Transparent Swarm:** The TUI actively displays the swarm dynamics in real-time.
*   **Progressive Disclosure (Default):** A hybrid approach. The TUI streams the primary agent's consensus by default but provides interactive toggles (similar to our current `<thinking>` blocks) to expand and inspect specific agent critiques, chronological thought paths, or dissenting opinions on demand.

### 3. Topologies, Handoffs, and Asynchronous Delegation
Instead of brittle, static sequential pipelines, `codecuttlectl` utilizes dynamic routing:
*   **Synchronous Handoffs:** A node has conversational authority until it yields control. It can invoke a native `handoff` tool to dynamically transfer execution (and the full conversation state) to another specialized node based on real-time needs. The caller sleeps until the target finishes.
*   **Asynchronous Delegation (The Swarm Backlog):** For "fan-out" parallel work (e.g., scraping 5 different API docs simultaneously), nodes interact with the Swarm Backlog (a supercharged version of the `todo_manage` tool). A node can create tasks, flag them as `async: true`, and assign them to other nodes (e.g., `researcher`). The Orchestrator does not block; it continues talking to the user while background Goroutines (or remote Arm Nodes) execute the tasks, eventually injecting completion events back into the Orchestrator's context window.

### 4. Event Triggers (Continuous Background Processing)
Morphologies can define global event triggers that spawn asynchronous node tasks based on system activity.
To prevent trigger storms (e.g., firing a "Review Code" task on every single file write during a massive refactor), triggers support debouncing and logical grouping.
*   *Example:* Trigger the `reviewer` node on `event: "git_commit"` rather than raw file writes, ensuring the reviewer only critiques logical, completed chunks of work.

### 5. Resilience & Fallbacks
Multi-agent swarms face compounding probabilities of failure. Morphologies encode graceful degradation:
*   **Hierarchical Fallbacks:** If the primary provider (e.g., Anthropic) hits rate limits or 5xx errors, the node dynamically falls back to an alternative provider (e.g., Google).
*   **Intelligent Circuit Breakers:** If an external tool (e.g., the GitHub API) repeatedly times out, a circuit breaker trips to the `OPEN` state, preventing retry storms and immediately forcing the agent to attempt an alternative reasoning path or escalate to a Human-in-the-Loop (HITL) pause.
*   **Timeout & Heartbeat Policies:** Agents that stall without emitting streamed tokens or "heartbeat" progress signals are aggressively halted and retried or escalated.

## Morphology Configuration (YAML)

Morphologies are defined in a standardized declarative YAML format. This eliminates configuration drift across repositories and allows users to share topologies easily.

```yaml
name: "senior-dev-committee"
version: "1.0.0"
description: "A fast, cheap planner combined with a powerful executor."
presentation: "progressive_disclosure"

# Define the agents in the swarm
nodes:
  orchestrator:
    provider: "bedrock"
    model: "us.anthropic.claude-opus-4-6-v1"
    system_prompt: "You are the lead developer. You synthesize plans from the planner and execute them."
    workbench: ["*"] # Has access to all tools
    is_primary: true
    fallbacks:
      - provider: "google"
        model: "gemini-3.1-pro"
    
  planner:
    provider: "google"
    model: "gemini-3.1-pro"
    system_prompt: "You are a software architect. Draft a step-by-step plan for the user's request."
    workbench: ["read_file", "list_directory", "glob", "grep"] # Strictly read-only sandbox
    
  reviewer:
    provider: "ollama"
    model: "qwen2.5-coder:32b"
    system_prompt: "Review code diffs and suggest optimizations."
    workbench: ["git"]
    max_concurrency: 1 # Ensure we don't blow up local VRAM

# Define routing rules and allowed handoffs
topology:
  type: "handoff"
  rules:
    orchestrator: ["planner", "reviewer"]
    planner: ["orchestrator"]
    reviewer: ["orchestrator"]
  triggers:
    - event: "git_commit"
      assign_to: "reviewer"
      action: "Review the latest commit and suggest improvements."
```

When `topology.rules` is empty or absent, routing is legacy-permissive: any defined target node may be handed off to. Once rules are present, only the explicitly listed source→target edges are allowed and enforce a matching handoff path at runtime.

---

## Managing Local & Project Morphologies

`codecuttlectl` makes it seamless to define, discover, customize, and share swarm morphologies across global, user, and project-local scopes without modifying git-tracked files or maintaining fragile shell aliases.

### Directory Layout & Search Order

When referencing a morphology by name (e.g. `codecuttlectl --morph openrouter-expert-swarm` or via `default_morph` in configuration), `codecuttlectl` resolves the file by searching the following locations in order (checking `.yaml` and `.yml` extensions):

1. **Direct Path**: Absolute or relative file path (e.g. `./my-morph.yaml` or `/opt/morphs/team.yaml`).
2. **Project Local Morphs**: `<workdir>/.codecuttle/morphs/`
3. **Custom Configured Morphs Directory**: `morphs_dir` set in `~/.config/codecuttlectl/config.yaml`.
4. **User Morphologies**: `~/.config/codecuttlectl/morphs/` (or `$XDG_CONFIG_HOME/codecuttlectl/morphs/`).
5. **System Morphologies**: `/usr/local/share/codecuttlectl/morphs/` (installed by `make install`).

### Full Resolution Precedence

When running `codecuttlectl`, the active morphology is selected using a strict 6-tier precedence hierarchy:

```
┌─────────────────────────────────────────────────────────────┐
│ 1. CLI Flag: --morph <name|path> (or --morph none)          │
├─────────────────────────────────────────────────────────────┤
│ 2. Single-Model CLI Override: --provider / --model          │
├─────────────────────────────────────────────────────────────┤
│ 3. Resumed Session: Saved morph attribute in session meta   │
├─────────────────────────────────────────────────────────────┤
│ 4. Environment Variable: CODECUTTLE_MORPH                   │
├─────────────────────────────────────────────────────────────┤
│ 5. Project Default: <workdir>/.codecuttle/morph.yaml        │
├─────────────────────────────────────────────────────────────┤
│ 6. User Configuration: default_morph in config.yaml         │
└─────────────────────────────────────────────────────────────┘
```

- **Explicit Flags**: `--morph <name>` or `--morph <path>` takes top priority. Passing `--morph none` disables any ambient default morphology and runs in standard single-agent mode.
- **Single-Model Fallback**: If an explicit `--provider` or `--model` is supplied on the CLI and `--morph` is omitted, `codecuttlectl` respects the single-model override and will not activate an ambient background morphology.
- **Resumed Sessions**: Resuming a session (`codecuttlectl -r <alias|id>`) automatically inspects the session metadata and restores the exact morphology originally used.
- **Environment & Project Overrides**: `CODECUTTLE_MORPH` and repository-level `.codecuttle/morph.yaml` files allow teams to standardize agent setups per project.
- **User Config Default**: `~/.config/codecuttlectl/config.yaml` provides a persistent user-level default when running outside a project repository.

### Customizing Morphologies Locally

To customize or tweak a system-installed morphology without dirtying the git repository or requiring root permissions, copy it into your user morphs directory:

```bash
# Ensure user morphs directory exists
mkdir -p ~/.config/codecuttlectl/morphs

# Copy system starter template
cp /usr/local/share/codecuttlectl/morphs/openrouter-expert-swarm.yaml ~/.config/codecuttlectl/morphs/

# Edit agent models, prompts, or tools
$EDITOR ~/.config/codecuttlectl/morphs/openrouter-expert-swarm.yaml
```

Because `~/.config/codecuttlectl/morphs/` is searched before `/usr/local/share/codecuttlectl/morphs/`, your customized template will automatically shadow the system template when referenced by name.

### Listing Morphologies (`--list-morphs`)

To inspect all available morphologies across all search locations:

```bash
codecuttlectl --list-morphs
```

Example output:
```
SCOPE     NAME                     NODES  PRIMARY       STATUS            PATH
------------------------------------------------------------------------------------------
user      openrouter-expert-swarm      3  orchestrator  active (default)  /home/user/.config/codecuttlectl/morphs/openrouter-expert-swarm.yaml
system    openrouter-expert-swarm      3  orchestrator  (shadowed)        /usr/local/share/codecuttlectl/morphs/openrouter-expert-swarm.yaml
system    google-squad                 3  orchestrator  -                 /usr/local/share/codecuttlectl/morphs/google-squad.yaml
```

- **Shadowing Indicator**: When multiple files share the same base name across different scopes, higher-precedence directories take effect and lower-precedence files are marked as `(shadowed)`.
- **JSON Output**: Use `codecuttlectl --list-morphs --json` for programmatic inspection in scripts and tooling.
- **Bundled Starter Templates vs Test Fixtures**: Production starter templates reside in `morphs/` and are installed to `/usr/local/share/codecuttlectl/morphs/`. Test fixtures used exclusively for unit and integration testing remain isolated in `testdata/`.

## Migration from PR #25 (`ModelPool`)

PR #25 introduced the concept of multi-model routing via a Bedrock-specific `ModelPool` (`Primary`, `Auxiliary`, `Planning`).

### The Transition Plan:

1. **Generalize the Pool Interface (`internal/provider/pool.go`):**
   * Deprecate the Bedrock-specific pool.
   * Create a new `swarm.Morphology` that parses the YAML schema and stores a mapping of `NodeID -> provider.Provider`.
   * Implement the `handoff` context passing mechanism.

2. **Strict Workbench Loading (`internal/pluginhost`):**
   * Update the plugin manager to instantiate isolated tool registries per node, rather than a global tool array injected into every system prompt.

3. **CLI Updates:**
   * Introduce a `--morph <path.yaml>` flag. 
   * When `--morph` is used, the system overrides standard `--model` and `--provider` flags, initializing the full swarm defined in the file.
   * *Note: The existing `--aux-model` and `--plan-model` flags are considered transitional convenience flags and will be superseded by morphology files.*

4. **Agent Orchestration (`internal/conversation/agent.go`):**
   * The `Agent` struct will be updated to handle the active `Morphology`.
   * It will support executing a `handoff` tool, suspending the current active node, and resuming the loop with the target node's provider and system prompt.

## Phase 2: Asynchronous Delegation (The Swarm Backlog)

While Phase 1 (Parser, Sandboxing, Synchronous Handoff) enables dynamic multi-agent conversation, **Phase 2** unlocks true parallel execution. The goal is to allow the primary Orchestrator agent to assign multi-step or time-consuming tasks (like "scrape these 5 API docs" or "draft an implementation plan") to specialized background agents without blocking its own conversation loop with the user.

### Architecture

1.  **Event Dispatcher Interface:**
    To keep the core `conversation` and `tui` packages cleanly decoupled, inter-thread communication utilizes a `swarm.EventDispatcher` interface (`Dispatch(msg any)`). The TUI implements this by wrapping Bubble Tea's `tea.Program.Send(msg)`.

2.  **The Swarm Manager (`internal/swarm/manager.go`):**
    A central component responsible for orchestrating background agents. It holds a queue of pending tasks (added to the `todo_manage` tool with `async: true` and an `assignee`). It implements a Worker Pool / Semaphore system that respects the `MaxConcurrency` value defined for each Node in the morphology (e.g., ensuring we don't spawn 50 parallel Ollama models and exhaust VRAM).

3.  **Headless Agents & Strict Sandboxing:**
    When a worker picks up a task, it spins up an isolated background agent.
    *   **Context Pre-seeding:** The headless agent is seeded with a compacted summary of the current session and the specific task description.
    *   **Safety Policy:** Headless agents are instantiated with `AutoApprove: false` and a strict policy that denies any destructive operations. They rely entirely on the `workbench` defined in the morphology (e.g., restricted to `read_file`, `webfetch`). If an agent attempts a destructive command, it fails safely and records the error.
    *   **Final Synthesis:** The agent's prompt instructs it to synthesize its findings into a concise summary when finished, rather than dumping raw tool outputs.

4.  **TUI Integration and Context Injection (Closing the Loop):**
    Background tasks must safely bring their findings back into the Orchestrator's context.
    *   The background worker emits a `TaskCompletedMsg` containing the final summary.
    *   **Race Condition Mitigation:** If the background task finishes while the Orchestrator is actively streaming a response to the user, injecting text directly into the history would cause slice corruption or index panics. The system queues the result in a `pendingAsyncResults` slice.
    *   When the active stream finishes, the TUI safely injects the results into the history as synthetic `System` messages (e.g., `[Background Task Complete] Node 'researcher' finished: <Summary>`).

5.  **File State Safety & Shared Context:**
    The project directory acts as the shared state boundary (the "Modular Monolith" approach). To prevent data corruption if the Orchestrator and a background agent edit the same file simultaneously, tools like `edit_file` and `write_file` enforce thread-safe atomic writes.

    **Note:** atomic, single-operation writes prevent torn files but do not prevent *logical* edit conflicts (two agents racing on the same file can still overwrite each other's changes). Concurrent workers must be serialized by the coordinator or by task-level file ownership; per-file atomicity alone is not transaction safety.

## Future Enhancements
* **The SAGA Pattern:** Allowing agents to revert orphaned states (e.g., executing idempotent compensating tools) if a multi-step workflow fails catastrophically midway through.
* **Morphology Registry:** A central repository where users can download community-created morphologies via a command like `codecuttlectl morph install react-specialist`.
