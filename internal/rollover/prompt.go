package rollover

import (
	"fmt"
	"strings"

	"github.com/Modzybear/codecuttlectl/internal/session"
)

// BuildCloseoutPrompt constructs the instruction given to the model during the closeout turn.
func BuildCloseoutPrompt(allowDocWrites bool, forced bool, steeringHint string) string {
	var sb strings.Builder
	sb.WriteString("Your context window is rolling over to a fresh epoch. Call `save_checkpoint` to persist a comprehensive handoff so the fresh epoch can come fully up to speed immediately.\n\n")

	sb.WriteString("Required checkpoint components:\n")
	sb.WriteString("1. **summary**: Concise rundown of current progress, architecture decisions made, and exact state of the task.\n")
	sb.WriteString("2. **lessons_learned**: Hard-fought lessons, root causes discovered, gotchas, and dead ends not to repeat.\n")
	sb.WriteString("3. **recently_changed_files**: Files modified or created with what changed and why.\n")
	sb.WriteString("4. **key_files**: Priority files the fresh epoch should ingest to get fully up to speed, each with a clear purpose blurb.\n")
	sb.WriteString("5. **key_dirs**: Important directories to explore for full architectural grasp.\n")
	sb.WriteString("6. **open_questions**: Unresolved design questions, pending decisions, or risks to verify.\n")
	sb.WriteString("7. **resume_instructions**: Exact starting point, immediate next actions, and verification commands to pick up right where this epoch leaves off.\n\n")

	if allowDocWrites {
		sb.WriteString("Documentation update policy is active: Before calling `save_checkpoint`, update any affected project READMEs or documentation to reflect landed changes and list them in `docs_updated`.\n\n")
	}

	if forced {
		sb.WriteString("Notice: This rollover was triggered after significant context growth. Be sure to capture any in-progress uncommitted changes or active hypotheses in `summary` and `resume_instructions`.\n\n")
	}

	if trimmed := strings.TrimSpace(steeringHint); trimmed != "" {
		sb.WriteString(fmt.Sprintf("User steering note for next epoch:\n%s\n\n", trimmed))
	}

	sb.WriteString("Call `save_checkpoint` now with these details to finalize this epoch.")
	return sb.String()
}

// BuildBootstrapMessage renders the deterministic seed user message for the fresh epoch.
func BuildBootstrapMessage(nextEpoch int, prevEpoch int, pinned session.PinnedState, payload session.CheckpointPayload, steeringHint string) string {
	var sb strings.Builder

	if prevEpoch > 0 {
		sb.WriteString(fmt.Sprintf("# Session Rollover — Epoch %d (Resumed from Epoch %d)\n\n", nextEpoch, prevEpoch))
	} else {
		sb.WriteString(fmt.Sprintf("# Session Rollover — Epoch %d\n\n", nextEpoch))
	}

	// 1. Pinned Ground Truth
	sb.WriteString("## Pinned Ground Truth (Harness-Verified)\n")
	if pinned.OriginalObjective != "" {
		sb.WriteString(fmt.Sprintf("- **Original Objective**: %s\n", pinned.OriginalObjective))
	}
	if pinned.WorkDir != "" {
		sb.WriteString(fmt.Sprintf("- **Working Directory**: `%s`\n", pinned.WorkDir))
	}
	if pinned.GitBranch != "" || pinned.GitHeadCommit != "" {
		head := pinned.GitHeadCommit
		if head == "" {
			head = "unknown"
		}
		sb.WriteString(fmt.Sprintf("- **Git Branch / HEAD**: `%s` (%s)\n", pinned.GitBranch, head))
	}

	if pinned.GitStatusShort != "" || pinned.GitDiffStat != "" {
		sb.WriteString("- **Uncommitted Changes**:\n```\n")
		if pinned.GitStatusShort != "" {
			sb.WriteString(pinned.GitStatusShort)
			sb.WriteString("\n")
		}
		if pinned.GitDiffStat != "" {
			sb.WriteString(pinned.GitDiffStat)
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
	}

	if len(pinned.RecentCommits) > 0 {
		sb.WriteString("- **Recent Commits on Branch**:\n")
		for _, c := range pinned.RecentCommits {
			sb.WriteString(fmt.Sprintf("  - `%s`\n", c))
		}
	}

	if len(pinned.Todos) > 0 {
		sb.WriteString("- **Active Task List**:\n")
		for _, item := range pinned.Todos {
			sb.WriteString(fmt.Sprintf("  - [%s] %s\n", item.Status, item.Content))
		}
	}

	if len(pinned.PendingAsyncTasks) > 0 {
		sb.WriteString("- **Pending Background Tasks**:\n")
		for _, t := range pinned.PendingAsyncTasks {
			sb.WriteString(fmt.Sprintf("  - %s\n", t))
		}
	}

	sb.WriteString("\n")

	// 2. Handoff from previous epoch
	if prevEpoch > 0 {
		sb.WriteString(fmt.Sprintf("## Handoff from Epoch %d\n", prevEpoch))
	} else {
		sb.WriteString("## Handoff\n")
	}

	if payload.Summary != "" {
		sb.WriteString("### Current State\n")
		sb.WriteString(payload.Summary)
		sb.WriteString("\n\n")
	}

	if len(payload.LessonsLearned) > 0 {
		sb.WriteString("### Hard-Fought Lessons & Pitfalls\n")
		for _, lesson := range payload.LessonsLearned {
			sb.WriteString(fmt.Sprintf("- %s\n", lesson))
		}
		sb.WriteString("\n")
	}

	if len(payload.RecentlyChangedFiles) > 0 {
		sb.WriteString("### Recently Changed Files\n")
		for _, file := range payload.RecentlyChangedFiles {
			sb.WriteString(fmt.Sprintf("- `%s` — %s\n", file.Path, file.Purpose))
		}
		sb.WriteString("\n")
	}

	if len(payload.KeyFiles) > 0 {
		sb.WriteString("### Priority Files to Ingest (Get Your Bearings)\n")
		for _, file := range payload.KeyFiles {
			sb.WriteString(fmt.Sprintf("- `%s` — %s\n", file.Path, file.Purpose))
		}
		sb.WriteString("\n")
	}

	if len(payload.KeyDirs) > 0 {
		sb.WriteString("### Important Directories\n")
		for _, dir := range payload.KeyDirs {
			sb.WriteString(fmt.Sprintf("- `%s` — %s\n", dir.Path, dir.Purpose))
		}
		sb.WriteString("\n")
	}

	if len(payload.OpenQuestions) > 0 {
		sb.WriteString("### Open Questions & Risks\n")
		for _, q := range payload.OpenQuestions {
			sb.WriteString(fmt.Sprintf("- %s\n", q))
		}
		sb.WriteString("\n")
	}

	if len(payload.DocsUpdated) > 0 {
		sb.WriteString("### Project Docs Updated During Closeout\n")
		for _, d := range payload.DocsUpdated {
			sb.WriteString(fmt.Sprintf("- `%s`\n", d))
		}
		sb.WriteString("\n")
	}

	if payload.ResumeInstructions != "" {
		sb.WriteString("### Resume Instructions\n")
		sb.WriteString(payload.ResumeInstructions)
		sb.WriteString("\n\n")
	}

	if trimmed := strings.TrimSpace(steeringHint); trimmed != "" {
		sb.WriteString("### User Steering Note\n")
		sb.WriteString(trimmed)
		sb.WriteString("\n\n")
	}

	sb.WriteString("---\n")
	sb.WriteString("**Orientation Protocol**: Before making edits, spend 1–3 tool rounds reading the listed priority files (and checking current git diff if uncommitted changes exist) to verify ground truth on disk, then immediately resume execution from the Resume Instructions above.")

	return sb.String()
}

// FormatEpochsTable formats a list of completed epoch checkpoints into a readable GFM Markdown summary table.
func FormatEpochsTable(currentEpoch int, epochs []session.EpochCheckpoint) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Session Epochs (Current: Epoch %d)\n\n", currentEpoch))

	if len(epochs) == 0 {
		sb.WriteString("No prior epochs archived. This session is currently in its initial epoch (Epoch 1).\n\n")
		sb.WriteString("Use `/rollover` to manually checkpoint and start Epoch 2.")
		return sb.String()
	}

	sb.WriteString("| Epoch | Archived At (UTC) | Trigger | Tokens @ Roll | Cost USD | Summary |\n")
	sb.WriteString("| :--- | :--- | :--- | ---: | ---: | :--- |\n")

	for _, ep := range epochs {
		dateStr := ep.CreatedAt.UTC().Format("2006-01-02 15:04:05")
		summaryPreview := strings.ReplaceAll(ep.Payload.Summary, "\n", " ")
		summaryPreview = strings.ReplaceAll(summaryPreview, "|", "\\|")
		if runes := []rune(summaryPreview); len(runes) > 35 {
			summaryPreview = string(runes[:32]) + "..."
		}

		sb.WriteString(fmt.Sprintf("| Epoch %d | %s | %s | %d | $%.4f | %s |\n",
			ep.Epoch,
			dateStr,
			string(ep.Trigger),
			ep.TokensAtRollover,
			ep.EstimatedCostUSD,
			summaryPreview,
		))
	}

	return strings.TrimRight(sb.String(), "\n")
}
