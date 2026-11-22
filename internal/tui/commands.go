package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

// Command describes a TUI slash command.
type Command struct {
	Name        string
	Usage       string
	Description string
	Run         func(m *Model, args []string) tea.Cmd
}

// CommandRegistry manages built-in TUI slash commands.
type CommandRegistry struct {
	order    []string
	commands map[string]Command
}

// NewDefaultCommandRegistry creates a CommandRegistry populated with built-in commands.
func NewDefaultCommandRegistry() *CommandRegistry {
	reg := &CommandRegistry{
		commands: make(map[string]Command),
	}

	reg.Register(Command{
		Name:        "/rollover",
		Usage:       "/rollover [--docs|--no-docs] [hint...]",
		Description: "Checkpoint the current epoch via save_checkpoint and roll over to a fresh context window",
		Run: func(m *Model, args []string) tea.Cmd {
			docsOverride, hint, err := ParseRolloverCommandArgs(args)
			if err != nil {
				m.appendSystemNotice(fmt.Sprintf("Error: %v\nUsage: /rollover [--docs|--no-docs] [hint...]", err), true)
				return nil
			}
			return m.startManualRollover(docsOverride, hint)
		},
	})

	reg.Register(Command{
		Name:        "/epochs",
		Usage:       "/epochs",
		Description: "List completed epoch checkpoints for this session",
		Run: func(m *Model, _ []string) tea.Cmd {
			m.appendSystemNotice(rollover.FormatEpochsTable(m.effectiveEpoch(), m.epochs), false)
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/auto-rollover",
		Usage:       "/auto-rollover [on|off|status]",
		Description: "Toggle or inspect automatic epoch rollover settings",
		Run: func(m *Model, args []string) tea.Cmd {
			if len(args) == 0 || strings.EqualFold(args[0], "status") {
				m.appendSystemNotice(m.formatAutoRolloverStatus(), false)
				return nil
			}
			switch strings.ToLower(args[0]) {
			case "on", "true", "1":
				m.rolloverCfg.AutoRollover = true
				if m.rolloverCtrl != nil {
					m.rolloverCtrl.SetConfig(m.rolloverCfg)
				}
				m.appendSystemNotice("Auto-rollover enabled.\n\n"+m.formatAutoRolloverStatus(), false)
			case "off", "false", "0":
				m.rolloverCfg.AutoRollover = false
				if m.rolloverCtrl != nil {
					m.rolloverCtrl.SetConfig(m.rolloverCfg)
					m.rolloverState = m.rolloverCtrl.State()
				} else if m.rolloverState == rollover.StateArmed {
					m.rolloverState = rollover.StateNormal
				}
				m.appendSystemNotice("Auto-rollover disabled.\n\n"+m.formatAutoRolloverStatus(), false)
			default:
				m.appendSystemNotice("Usage: /auto-rollover [on|off|status]", true)
			}
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/reset",
		Usage:       "/reset",
		Description: "Clear active conversation history and task list",
		Run: func(m *Model, _ []string) tea.Cmd {
			m.history = nil
			m.messages = nil
			m.todos = todo.NewList()
			m.originalObjective = ""
			m.lastCallInputTokens = 0
			m.lastCallCacheReadInputTokens = 0
			m.lastCallCacheWriteInputTokens = 0
			m.rolloverState = rollover.StateNormal
			m.rolloverPreHistory = nil
			m.viewport.SetContent("")
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/plugins",
		Usage:       "/plugins",
		Description: "List loaded Cuttlebone tool plugins",
		Run: func(m *Model, _ []string) tea.Cmd {
			var names []string
			if m.pluginMgr != nil {
				names = m.pluginMgr.PluginNames()
			}
			m.appendSystemNotice("Loaded plugins: "+strings.Join(names, ", "), false)
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/pin",
		Usage:       "/pin",
		Description: "Toggle pin status for the active session",
		Run: func(m *Model, _ []string) tea.Cmd {
			if m.store == nil || m.sessionID == "" {
				m.appendSystemNotice("Error: No active session store or session ID to pin session.", true)
				return nil
			}
			var pinned bool
			err := m.store.UpdateMeta(m.sessionID, func(meta *session.SessionMeta) error {
				meta.Pinned = !meta.Pinned
				pinned = meta.Pinned
				return nil
			})
			if err != nil {
				m.appendSystemNotice(fmt.Sprintf("Error updating pin status: %v", err), true)
				return nil
			}
			if pinned {
				m.appendSystemNotice(fmt.Sprintf("★ Session pinned (`%s`).", m.sessionID), false)
			} else {
				m.appendSystemNotice(fmt.Sprintf("Session unpinned (`%s`).", m.sessionID), false)
			}
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/alias",
		Usage:       "/alias [name|--clear]",
		Description: "View, set, or clear a friendly alias for the active session",
		Run: func(m *Model, args []string) tea.Cmd {
			if m.store == nil || m.sessionID == "" {
				m.appendSystemNotice("Error: No active session store or session ID to update alias.", true)
				return nil
			}
			if len(args) == 0 {
				meta, err := m.store.ResolveMeta(m.sessionID)
				if err != nil {
					m.appendSystemNotice(fmt.Sprintf("Error retrieving session metadata: %v", err), true)
					return nil
				}
				if meta.Alias != "" {
					m.appendSystemNotice(fmt.Sprintf("Current session alias: `%s`\n\nUsage: `/alias <name>` or `/alias --clear`", meta.Alias), false)
				} else {
					m.appendSystemNotice("No alias is currently set for this session.\n\nUsage: `/alias <name>` or `/alias --clear`", false)
				}
				return nil
			}
			arg := strings.TrimSpace(args[0])
			if arg == "--clear" || strings.EqualFold(arg, "clear") {
				err := m.store.UpdateMeta(m.sessionID, func(meta *session.SessionMeta) error {
					meta.Alias = ""
					return nil
				})
				if err != nil {
					m.appendSystemNotice(fmt.Sprintf("Error clearing session alias: %v", err), true)
					return nil
				}
				m.appendSystemNotice(fmt.Sprintf("Cleared alias for session `%s`.", m.sessionID), false)
				return nil
			}
			if err := session.ValidateAlias(arg); err != nil {
				m.appendSystemNotice(fmt.Sprintf("Invalid alias `%s`: %v", arg, err), true)
				return nil
			}
			err := m.store.UpdateMeta(m.sessionID, func(meta *session.SessionMeta) error {
				meta.Alias = arg
				return nil
			})
			if err != nil {
				m.appendSystemNotice(fmt.Sprintf("Error setting alias `%s`: %v", arg, err), true)
				return nil
			}
			m.appendSystemNotice(fmt.Sprintf("Set session alias to `%s`.", arg), false)
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/title",
		Usage:       "/title <text>",
		Description: "View or set the title for the active session",
		Run: func(m *Model, args []string) tea.Cmd {
			if m.store == nil || m.sessionID == "" {
				m.appendSystemNotice("Error: No active session store or session ID to update title.", true)
				return nil
			}
			if len(args) == 0 {
				meta, err := m.store.ResolveMeta(m.sessionID)
				if err != nil {
					m.appendSystemNotice(fmt.Sprintf("Error retrieving session metadata: %v", err), true)
					return nil
				}
				if meta.Title != "" {
					m.appendSystemNotice(fmt.Sprintf("Current session title: %s\n\nUsage: `/title <text>`", meta.Title), false)
				} else {
					m.appendSystemNotice("No title is currently set for this session.\n\nUsage: `/title <text>`", false)
				}
				return nil
			}
			titleText := strings.TrimSpace(strings.Join(args, " "))
			err := m.store.UpdateMeta(m.sessionID, func(meta *session.SessionMeta) error {
				meta.Title = titleText
				return nil
			})
			if err != nil {
				m.appendSystemNotice(fmt.Sprintf("Error setting session title: %v", err), true)
				return nil
			}
			m.appendSystemNotice(fmt.Sprintf("Set session title to: %s", titleText), false)
			return nil
		},
	})

	reg.Register(Command{
		Name:        "/help",
		Usage:       "/help",
		Description: "Show available slash commands",
		Run: func(m *Model, _ []string) tea.Cmd {
			m.appendSystemNotice(reg.HelpText(), false)
			return nil
		},
	})

	return reg
}

// Register adds a command to the registry.
func (r *CommandRegistry) Register(cmd Command) {
	key := strings.ToLower(strings.TrimSpace(cmd.Name))
	if _, exists := r.commands[key]; !exists {
		r.order = append(r.order, key)
	}
	r.commands[key] = cmd
}

// Get returns the command for a given name if registered.
func (r *CommandRegistry) Get(name string) (Command, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	cmd, ok := r.commands[key]
	return cmd, ok
}

// Dispatch parses text as a slash command and executes it if registered.
// Returns (true, cmd) if text was handled as a command, or (false, nil) for normal chat input.
func (r *CommandRegistry) Dispatch(m *Model, text string) (bool, tea.Cmd) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return false, nil
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false, nil
	}

	name := strings.ToLower(fields[0])
	cmd, ok := r.commands[name]
	if !ok {
		return false, nil
	}

	return true, cmd.Run(m, fields[1:])
}

// HelpText formats all registered slash commands in clean Markdown.
func (r *CommandRegistry) HelpText() string {
	var sb strings.Builder
	sb.WriteString("### Available Slash Commands\n\n")
	for _, key := range r.order {
		cmd := r.commands[key]
		sb.WriteString(fmt.Sprintf("- `%s` — %s\n", cmd.Usage, cmd.Description))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ParseRolloverCommandArgs parses optional --docs / --no-docs flags and any trailing steering hint.
func ParseRolloverCommandArgs(args []string) (*rollover.DocsPolicy, string, error) {
	var docsOverride *rollover.DocsPolicy
	var hintParts []string

	for _, arg := range args {
		if len(hintParts) == 0 && strings.HasPrefix(arg, "--") {
			switch arg {
			case "--docs":
				p := rollover.DocsPolicyAlways
				docsOverride = &p
				continue
			case "--no-docs":
				p := rollover.DocsPolicyNone
				docsOverride = &p
				continue
			default:
				return nil, "", fmt.Errorf("unknown flag %q", arg)
			}
		}
		hintParts = append(hintParts, arg)
	}

	return docsOverride, strings.TrimSpace(strings.Join(hintParts, " ")), nil
}
