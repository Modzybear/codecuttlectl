package rollover

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

// CollectPinnedState gathers deterministic ground-truth directly from the runtime environment.
// It uses a short timeout for git commands and gracefully falls back to empty values if git is unavailable.
func CollectPinnedState(ctx context.Context, workDir string, originalObjective string, todos []todo.Item, pendingAsyncTasks []string) session.PinnedState {
	pinned := session.PinnedState{
		OriginalObjective: strings.TrimSpace(originalObjective),
		WorkDir:           strings.TrimSpace(workDir),
	}

	if len(todos) > 0 {
		pinned.Todos = make([]todo.Item, len(todos))
		copy(pinned.Todos, todos)
	}

	if len(pendingAsyncTasks) > 0 {
		pinned.PendingAsyncTasks = make([]string, len(pendingAsyncTasks))
		copy(pinned.PendingAsyncTasks, pendingAsyncTasks)
	}

	if workDir == "" {
		return pinned
	}

	gitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// 1. Current Branch
	if out, err := runGitCmd(gitCtx, workDir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		pinned.GitBranch = strings.TrimSpace(out)
	}

	// 2. HEAD commit
	if out, err := runGitCmd(gitCtx, workDir, "log", "-1", "--oneline"); err == nil {
		pinned.GitHeadCommit = strings.TrimSpace(out)
	}

	// 3. Status short (capped)
	if out, err := runGitCmd(gitCtx, workDir, "status", "--short"); err == nil {
		pinned.GitStatusShort = truncateLines(strings.TrimSpace(out), 50, 4000)
	}

	// 4. Diff stat (capped)
	if out, err := runGitCmd(gitCtx, workDir, "diff", "--stat"); err == nil {
		pinned.GitDiffStat = truncateLines(strings.TrimSpace(out), 50, 4000)
	}

	// 5. Recent commits on branch
	if out, err := runGitCmd(gitCtx, workDir, "log", "-n", "10", "--oneline"); err == nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		for _, line := range lines {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				pinned.RecentCommits = append(pinned.RecentCommits, trimmed)
			}
		}
	}

	return pinned
}

// ResolveOriginalObjective returns existing if non-empty, otherwise scans messages
// for the first user text message.
func ResolveOriginalObjective(existing string, messages []provider.Message) string {
	if trimmed := strings.TrimSpace(existing); trimmed != "" {
		return trimmed
	}

	for _, msg := range messages {
		if msg.Role != provider.RoleUser {
			continue
		}
		var textParts []string
		for _, block := range msg.Content {
			if tb, ok := block.(provider.TextBlock); ok && tb.Text != "" {
				textParts = append(textParts, tb.Text)
			}
		}
		if len(textParts) > 0 {
			combined := strings.TrimSpace(strings.Join(textParts, "\n"))
			if runes := []rune(combined); len(runes) > 2000 {
				combined = string(runes[:2000]) + "..."
			}
			return combined
		}
	}

	return ""
}

func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func truncateLines(s string, maxLines int, maxChars int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		omitted := len(lines) - maxLines
		lines = append(lines[:maxLines], fmt.Sprintf("... (%d more lines omitted)", omitted))
	}
	res := strings.Join(lines, "\n")
	if runes := []rune(res); len(runes) > maxChars {
		res = string(runes[:maxChars]) + "\n... (truncated)"
	}
	return res
}
