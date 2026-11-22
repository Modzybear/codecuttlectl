package rollover

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

// SaveCheckpointToolName is the reserved tool name used by the orchestrator during closeout.
const SaveCheckpointToolName = "save_checkpoint"

// SaveCheckpointToolDef returns the provider.ToolDefinition for save_checkpoint.
func SaveCheckpointToolDef() provider.ToolDefinition {
	schema := `{
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
          "path": { "type": "string", "description": "Relative file path." },
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
          "path": { "type": "string", "description": "Relative file path." },
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
          "path": { "type": "string", "description": "Relative directory path." },
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
      "description": "List of project README/doc files updated during closeout."
    }
  }
}`

	return provider.ToolDefinition{
		Name:        SaveCheckpointToolName,
		Description: "Persist a structured epoch checkpoint before rolling over to a fresh context window. Injected exclusively during closeout.",
		InputSchema: json.RawMessage(schema),
	}
}

// ParseAndValidateCheckpoint unmarshals and strictly validates a save_checkpoint payload.
func ParseAndValidateCheckpoint(raw json.RawMessage) (session.CheckpointPayload, error) {
	var payload session.CheckpointPayload
	if len(raw) == 0 {
		return payload, fmt.Errorf("empty save_checkpoint arguments")
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("parsing save_checkpoint arguments: %w", err)
	}

	payload.Summary = strings.TrimSpace(payload.Summary)
	payload.ResumeInstructions = strings.TrimSpace(payload.ResumeInstructions)
	payload.LessonsLearned = cleanStringList(payload.LessonsLearned)
	payload.OpenQuestions = cleanStringList(payload.OpenQuestions)
	payload.DocsUpdated = cleanStringList(payload.DocsUpdated)
	payload.RecentlyChangedFiles = cleanFileNotes(payload.RecentlyChangedFiles)
	payload.KeyFiles = cleanFileNotes(payload.KeyFiles)
	payload.KeyDirs = cleanDirNotes(payload.KeyDirs)

	var validationErrors []string

	if payload.Summary == "" {
		validationErrors = append(validationErrors, "missing required field 'summary'")
	}
	if payload.ResumeInstructions == "" {
		validationErrors = append(validationErrors, "missing required field 'resume_instructions'")
	}
	if len(payload.KeyFiles) == 0 {
		validationErrors = append(validationErrors, "field 'key_files' must contain at least 1 file with path and purpose")
	} else {
		for i, kf := range payload.KeyFiles {
			if kf.Path == "" || kf.Purpose == "" {
				validationErrors = append(validationErrors, fmt.Sprintf("key_files[%d] must have non-empty path and purpose (got path=%q, purpose=%q)", i, kf.Path, kf.Purpose))
			}
		}
	}

	for i, rcf := range payload.RecentlyChangedFiles {
		if rcf.Path == "" || rcf.Purpose == "" {
			validationErrors = append(validationErrors, fmt.Sprintf("recently_changed_files[%d] must have non-empty path and purpose (got path=%q, purpose=%q)", i, rcf.Path, rcf.Purpose))
		}
	}

	for i, kd := range payload.KeyDirs {
		if kd.Path == "" || kd.Purpose == "" {
			validationErrors = append(validationErrors, fmt.Sprintf("key_dirs[%d] must have non-empty path and purpose (got path=%q, purpose=%q)", i, kd.Path, kd.Purpose))
		}
	}

	if len(validationErrors) > 0 {
		return payload, fmt.Errorf("invalid checkpoint payload:\n - %s", strings.Join(validationErrors, "\n - "))
	}

	return payload, nil
}

// IsCloseoutToolAllowed determines whether a tool can be invoked during the closeout phase.
func IsCloseoutToolAllowed(toolName string, allowDocWrites bool) bool {
	if toolName == SaveCheckpointToolName {
		return true
	}

	// Always forbidden during closeout
	switch toolName {
	case "handoff", "todo_manage", "scaffold_plugin", "reload_plugins":
		return false
	}

	// Read-only inspection tools are always allowed
	switch toolName {
	case "read_file", "grep", "glob", "list_directory", "tool_info", "get_skill":
		return true
	}

	// File edits allowed only if doc writes are permitted
	if (toolName == "edit_file" || toolName == "write_file") && allowDocWrites {
		return true
	}

	return false
}

func cleanStringList(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	var out []string
	for _, s := range list {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func cleanFileNotes(notes []session.FileContextNote) []session.FileContextNote {
	if len(notes) == 0 {
		return nil
	}
	var out []session.FileContextNote
	for _, n := range notes {
		n.Path = strings.TrimSpace(n.Path)
		n.Purpose = strings.TrimSpace(n.Purpose)
		if n.Path != "" || n.Purpose != "" {
			out = append(out, n)
		}
	}
	return out
}

func cleanDirNotes(notes []session.DirContextNote) []session.DirContextNote {
	if len(notes) == 0 {
		return nil
	}
	var out []session.DirContextNote
	for _, n := range notes {
		n.Path = strings.TrimSpace(n.Path)
		n.Purpose = strings.TrimSpace(n.Purpose)
		if n.Path != "" || n.Purpose != "" {
			out = append(out, n)
		}
	}
	return out
}
