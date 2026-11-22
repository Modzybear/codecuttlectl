package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

// PickerAction represents the user's final selection action in the session picker.
type PickerAction int

const (
	PickerActionQuit PickerAction = iota
	PickerActionResume
	PickerActionNew
)

// SessionPickerResult captures the outcome of running the interactive session picker.
type SessionPickerResult struct {
	Action    PickerAction
	SessionID string
}

// PickerScope specifies which sessions to display.
type PickerScope int

const (
	ScopeAll PickerScope = iota
	ScopePinned
	ScopeHere
)

type pickerMode int

const (
	modeNormal pickerMode = iota
	modeSearch
	modeAlias
	modeTitle
	modeConfirmDelete
)

// SessionPickerModel is the Bubble Tea model for the interactive session picker.
type SessionPickerModel struct {
	store       session.Store
	workDir     string
	allSessions []session.SessionMeta
	sessions    []session.SessionMeta

	cursorIndex  int
	scrollOffset int

	scope       PickerScope
	searchQuery string

	inputMode  pickerMode
	textInput  string
	flashMsg   string
	flashIsErr bool

	width    int
	height   int
	result   SessionPickerResult
	quitting bool
}

// NewSessionPickerModel creates an initialized SessionPickerModel.
func NewSessionPickerModel(store session.Store, workDir string) SessionPickerModel {
	if workDir != "" {
		if absWD, err := filepath.Abs(workDir); err == nil {
			workDir = absWD
		}
	}
	m := SessionPickerModel{
		store:   store,
		workDir: workDir,
		scope:   ScopeAll,
		width:   100,
		height:  30,
		result:  SessionPickerResult{Action: PickerActionQuit},
	}
	m.reloadSessions("")
	return m
}

// RunSessionPicker launches the interactive full-screen session picker TUI.
func RunSessionPicker(store session.Store, workDir string) (SessionPickerResult, error) {
	m := NewSessionPickerModel(store, workDir)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return SessionPickerResult{Action: PickerActionQuit}, err
	}
	if sm, ok := finalModel.(SessionPickerModel); ok {
		return sm.Result(), nil
	}
	return SessionPickerResult{Action: PickerActionQuit}, nil
}

// Result returns the outcome of the picker.
func (m SessionPickerModel) Result() SessionPickerResult {
	return m.result
}

// Init implements tea.Model.
func (m SessionPickerModel) Init() tea.Cmd {
	return nil
}

func (m *SessionPickerModel) reloadSessions(preferredID string) {
	if m.store == nil {
		m.allSessions = nil
		m.sessions = nil
		m.cursorIndex = 0
		return
	}

	all, err := m.store.List(0)
	if err != nil {
		m.allSessions = nil
		m.sessions = nil
		m.flashMsg = fmt.Sprintf("Error loading sessions: %v", err)
		m.flashIsErr = true
		return
	}
	m.allSessions = all

	var filter session.ListFilter
	switch m.scope {
	case ScopePinned:
		filter.PinnedOnly = true
	case ScopeHere:
		filter.WorkDir = m.workDir
	}
	filter.Query = m.searchQuery

	m.sessions = session.FilterSessions(m.allSessions, filter)

	if preferredID != "" {
		found := false
		for i, s := range m.sessions {
			if s.ID == preferredID {
				m.cursorIndex = i
				found = true
				break
			}
		}
		if !found {
			if m.cursorIndex >= len(m.sessions) {
				m.cursorIndex = max(0, len(m.sessions)-1)
			}
		}
	} else {
		if m.cursorIndex >= len(m.sessions) {
			m.cursorIndex = max(0, len(m.sessions)-1)
		}
	}
	m.adjustScroll()
}

func (m *SessionPickerModel) adjustScroll() {
	visibleHeight := m.height - 10
	if visibleHeight < 3 {
		visibleHeight = 3
	}

	if m.cursorIndex < 0 {
		m.cursorIndex = 0
	}
	if m.cursorIndex >= len(m.sessions) {
		m.cursorIndex = max(0, len(m.sessions)-1)
	}

	if m.cursorIndex < m.scrollOffset {
		m.scrollOffset = m.cursorIndex
	}
	if m.cursorIndex >= m.scrollOffset+visibleHeight {
		m.scrollOffset = m.cursorIndex - visibleHeight + 1
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// Update implements tea.Model.
func (m SessionPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.adjustScroll()
		return m, nil

	case tea.KeyMsg:
		keyStr := msg.String()

		// Global quit
		if keyStr == "ctrl+c" {
			m.result = SessionPickerResult{Action: PickerActionQuit}
			m.quitting = true
			return m, tea.Quit
		}

		switch m.inputMode {
		case modeConfirmDelete:
			return m.handleConfirmDeleteKey(keyStr)
		case modeSearch:
			return m.handleSearchKey(msg, keyStr)
		case modeAlias:
			return m.handleAliasKey(msg, keyStr)
		case modeTitle:
			return m.handleTitleKey(msg, keyStr)
		default:
			return m.handleNormalKey(keyStr)
		}
	}

	return m, nil
}

func (m SessionPickerModel) handleNormalKey(keyStr string) (tea.Model, tea.Cmd) {
	switch keyStr {
	case "q":
		m.result = SessionPickerResult{Action: PickerActionQuit}
		m.quitting = true
		return m, tea.Quit

	case "enter":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		m.result = SessionPickerResult{Action: PickerActionResume, SessionID: selected.ID}
		m.quitting = true
		return m, tea.Quit

	case "n":
		m.result = SessionPickerResult{Action: PickerActionNew}
		m.quitting = true
		return m, tea.Quit

	case "up", "k":
		if m.cursorIndex > 0 {
			m.cursorIndex--
			m.adjustScroll()
		}
		return m, nil

	case "down", "j":
		if m.cursorIndex < len(m.sessions)-1 {
			m.cursorIndex++
			m.adjustScroll()
		}
		return m, nil

	case "pgup", "pageup":
		m.cursorIndex = max(0, m.cursorIndex-5)
		m.adjustScroll()
		return m, nil

	case "pgdown", "pagedown":
		m.cursorIndex = min(len(m.sessions)-1, m.cursorIndex+5)
		m.adjustScroll()
		return m, nil

	case "/":
		m.inputMode = modeSearch
		m.textInput = m.searchQuery
		m.flashMsg = ""
		return m, nil

	case "f", "tab":
		m.scope = (m.scope + 1) % 3
		m.reloadSessions("")
		m.flashMsg = ""
		return m, nil

	case "p":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		var newPinned bool
		err := m.store.UpdateMeta(selected.ID, func(meta *session.SessionMeta) error {
			meta.Pinned = !meta.Pinned
			newPinned = meta.Pinned
			return nil
		})
		if err != nil {
			m.flashMsg = fmt.Sprintf("Error toggling pin: %v", err)
			m.flashIsErr = true
		} else {
			if newPinned {
				m.flashMsg = fmt.Sprintf("★ Session %s pinned", selected.ID)
			} else {
				m.flashMsg = fmt.Sprintf("Session %s unpinned", selected.ID)
			}
			m.flashIsErr = false
			m.reloadSessions(selected.ID)
		}
		return m, nil

	case "a":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		m.inputMode = modeAlias
		m.textInput = selected.Alias
		m.flashMsg = ""
		return m, nil

	case "t":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		m.inputMode = modeTitle
		m.textInput = selected.Title
		m.flashMsg = ""
		return m, nil

	case "d":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		if selected.Pinned {
			m.flashMsg = "Pinned session — press 'p' to unpin before deleting"
			m.flashIsErr = true
		} else {
			m.inputMode = modeConfirmDelete
			m.flashMsg = ""
		}
		return m, nil
	}

	return m, nil
}

func (m SessionPickerModel) handleConfirmDeleteKey(keyStr string) (tea.Model, tea.Cmd) {
	if strings.ToLower(keyStr) == "y" {
		if len(m.sessions) > 0 && m.cursorIndex < len(m.sessions) {
			selected := m.sessions[m.cursorIndex]
			err := m.store.Delete(selected.ID)
			if err != nil {
				m.flashMsg = fmt.Sprintf("Delete failed: %v", err)
				m.flashIsErr = true
			} else {
				m.flashMsg = fmt.Sprintf("Deleted session %s", selected.ID)
				m.flashIsErr = false
			}
			m.reloadSessions("")
		}
		m.inputMode = modeNormal
		return m, nil
	}

	m.inputMode = modeNormal
	m.flashMsg = "Delete cancelled"
	m.flashIsErr = false
	return m, nil
}

func (m SessionPickerModel) handleSearchKey(msg tea.KeyMsg, keyStr string) (tea.Model, tea.Cmd) {
	switch keyStr {
	case "esc", "escape":
		m.inputMode = modeNormal
		m.textInput = ""
		m.searchQuery = ""
		m.reloadSessions("")
		return m, nil

	case "enter":
		m.searchQuery = m.textInput
		m.inputMode = modeNormal
		m.reloadSessions("")
		return m, nil

	case "backspace":
		if len(m.textInput) > 0 {
			r := []rune(m.textInput)
			m.textInput = string(r[:len(r)-1])
			m.searchQuery = m.textInput
			m.reloadSessions("")
		}
		return m, nil

	case "ctrl+u":
		m.textInput = ""
		m.searchQuery = ""
		m.reloadSessions("")
		return m, nil

	case "space":
		m.textInput += " "
		m.searchQuery = m.textInput
		m.reloadSessions("")
		return m, nil

	default:
		if char := extractKeyChar(msg, keyStr); char != "" {
			m.textInput += char
			m.searchQuery = m.textInput
			m.reloadSessions("")
		}
		return m, nil
	}
}

func (m SessionPickerModel) handleAliasKey(msg tea.KeyMsg, keyStr string) (tea.Model, tea.Cmd) {
	switch keyStr {
	case "esc", "escape":
		m.inputMode = modeNormal
		return m, nil

	case "enter":
		if len(m.sessions) == 0 {
			m.inputMode = modeNormal
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		newAlias := strings.TrimSpace(m.textInput)
		if newAlias == "" {
			err := m.store.UpdateMeta(selected.ID, func(meta *session.SessionMeta) error {
				meta.Alias = ""
				return nil
			})
			if err != nil {
				m.flashMsg = fmt.Sprintf("Error clearing alias: %v", err)
				m.flashIsErr = true
			} else {
				m.flashMsg = "Alias cleared"
				m.flashIsErr = false
				m.inputMode = modeNormal
				m.reloadSessions(selected.ID)
			}
			return m, nil
		}

		if err := session.ValidateAlias(newAlias); err != nil {
			m.flashMsg = fmt.Sprintf("Invalid alias: %v", err)
			m.flashIsErr = true
			return m, nil
		}

		err := m.store.UpdateMeta(selected.ID, func(meta *session.SessionMeta) error {
			meta.Alias = newAlias
			return nil
		})
		if err != nil {
			m.flashMsg = fmt.Sprintf("Error saving alias: %v", err)
			m.flashIsErr = true
			return m, nil
		}

		m.flashMsg = fmt.Sprintf("Alias set to '%s'", newAlias)
		m.flashIsErr = false
		m.inputMode = modeNormal
		m.reloadSessions(selected.ID)
		return m, nil

	case "backspace":
		if len(m.textInput) > 0 {
			r := []rune(m.textInput)
			m.textInput = string(r[:len(r)-1])
		}
		return m, nil

	case "ctrl+u":
		m.textInput = ""
		return m, nil

	case "space":
		m.textInput += " "
		return m, nil

	default:
		if char := extractKeyChar(msg, keyStr); char != "" {
			m.textInput += char
		}
		return m, nil
	}
}

func (m SessionPickerModel) handleTitleKey(msg tea.KeyMsg, keyStr string) (tea.Model, tea.Cmd) {
	switch keyStr {
	case "esc", "escape":
		m.inputMode = modeNormal
		return m, nil

	case "enter":
		if len(m.sessions) == 0 {
			m.inputMode = modeNormal
			return m, nil
		}
		selected := m.sessions[m.cursorIndex]
		newTitle := strings.TrimSpace(m.textInput)
		err := m.store.UpdateMeta(selected.ID, func(meta *session.SessionMeta) error {
			meta.Title = newTitle
			return nil
		})
		if err != nil {
			m.flashMsg = fmt.Sprintf("Error saving title: %v", err)
			m.flashIsErr = true
			return m, nil
		}

		m.flashMsg = fmt.Sprintf("Title updated to '%s'", newTitle)
		m.flashIsErr = false
		m.inputMode = modeNormal
		m.reloadSessions(selected.ID)
		return m, nil

	case "backspace":
		if len(m.textInput) > 0 {
			r := []rune(m.textInput)
			m.textInput = string(r[:len(r)-1])
		}
		return m, nil

	case "ctrl+u":
		m.textInput = ""
		return m, nil

	case "space":
		m.textInput += " "
		return m, nil

	default:
		if char := extractKeyChar(msg, keyStr); char != "" {
			m.textInput += char
		}
		return m, nil
	}
}

func extractKeyChar(msg tea.KeyMsg, keyStr string) string {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		if text := kp.Key().Text; text != "" {
			return text
		}
	}
	if len(keyStr) == 1 {
		return keyStr
	}
	return ""
}

// View implements tea.Model.
func (m SessionPickerModel) View() tea.View {
	v := tea.NewView(m.renderView())
	v.AltScreen = true
	return v
}

func (m SessionPickerModel) renderView() string {
	var b strings.Builder

	// Header
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorForestGreen)
	b.WriteString(titleStyle.Render("codecuttlectl sessions"))
	b.WriteString("  ")

	// Scope badges
	badgeActive := lipgloss.NewStyle().Bold(true).Foreground(colorSkyBlue).Background(colorSurface2).Padding(0, 1)
	badgeInactive := lipgloss.NewStyle().Foreground(colorDim).Padding(0, 1)

	if m.scope == ScopeAll {
		b.WriteString(badgeActive.Render("All"))
	} else {
		b.WriteString(badgeInactive.Render("All"))
	}
	b.WriteString(" ")
	if m.scope == ScopePinned {
		b.WriteString(badgeActive.Render("★ Pinned"))
	} else {
		b.WriteString(badgeInactive.Render("★ Pinned"))
	}
	b.WriteString(" ")
	if m.scope == ScopeHere {
		b.WriteString(badgeActive.Render("Here"))
	} else {
		b.WriteString(badgeInactive.Render("Here"))
	}

	if m.searchQuery != "" {
		b.WriteString("  ")
		searchBadge := lipgloss.NewStyle().Foreground(colorHoney).Render(fmt.Sprintf("Filter: %q", m.searchQuery))
		b.WriteString(searchBadge)
	}
	b.WriteString("\n\n")

	// Table or empty message
	visibleHeight := m.height - 10
	if visibleHeight < 3 {
		visibleHeight = 3
	}

	if len(m.sessions) == 0 {
		emptyStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)
		b.WriteString(emptyStyle.Render("  No sessions found matching current filter."))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Foreground(colorSkyBlue).Render("  [n] New session   [f] Change scope   [/] Search filter   [q] Quit\n"))
		for i := 0; i < visibleHeight-3; i++ {
			b.WriteString("\n")
		}
	} else {
		end := min(len(m.sessions), m.scrollOffset+visibleHeight)
		for i := m.scrollOffset; i < end; i++ {
			s := m.sessions[i]
			isCursor := i == m.cursorIndex

			// Row parts
			cursorStr := "  "
			if isCursor {
				cursorStr = lipgloss.NewStyle().Foreground(colorSkyBlue).Bold(true).Render("> ")
			}

			pinStr := "  "
			if s.Pinned {
				pinStr = lipgloss.NewStyle().Foreground(colorHoney).Bold(true).Render("★ ")
			}

			// Alias / ID column
			var nameCol string
			if s.Alias != "" {
				nameCol = lipgloss.NewStyle().Foreground(colorSkyBlue).Bold(true).Render(fmt.Sprintf("%-18s", truncate(s.Alias, 18)))
			} else {
				nameCol = lipgloss.NewStyle().Foreground(colorBright).Render(fmt.Sprintf("%-18s", truncate(s.ID, 18)))
			}

			// Title
			titleText := s.Title
			if titleText == "" {
				titleText = "(untitled)"
			}
			titleCol := lipgloss.NewStyle().Foreground(colorBright).Render(fmt.Sprintf("%-26s", truncate(titleText, 26)))

			// Epoch badge
			epochCol := lipgloss.NewStyle().Foreground(colorHoney).Render(fmt.Sprintf("E%-2d", s.EffectiveEpoch()))

			// Short WorkDir
			dirCol := lipgloss.NewStyle().Foreground(colorDim).Render(fmt.Sprintf("%-18s", truncate(shortenWorkDir(s.WorkDir), 18)))

			// Model
			modelCol := lipgloss.NewStyle().Foreground(colorDim).Render(fmt.Sprintf("%-16s", truncate(s.Model, 16)))

			// Cost
			costCol := lipgloss.NewStyle().Foreground(colorForestGreen).Render(fmt.Sprintf("$%-6.3f", s.Stats.EstimatedCostUSD))

			// Relative Age
			ageCol := lipgloss.NewStyle().Foreground(colorDim).Render(fmt.Sprintf("%-10s", formatRelativeTime(s.UpdatedAt)))

			rowStr := fmt.Sprintf("%s%s%s %s %s %s %s %s %s", cursorStr, pinStr, nameCol, titleCol, epochCol, dirCol, modelCol, costCol, ageCol)
			if isCursor {
				rowStr = lipgloss.NewStyle().Background(colorSurface2).Render(rowStr)
			}
			b.WriteString(rowStr)
			b.WriteString("\n")
		}

		// Fill remaining lines to keep height stable
		renderedRows := end - m.scrollOffset
		for i := 0; i < visibleHeight-renderedRows; i++ {
			b.WriteString("\n")
		}
	}

	// Separator
	sepStyle := lipgloss.NewStyle().Foreground(colorSurface2)
	sepWidth := max(60, m.width-4)
	b.WriteString(sepStyle.Render(strings.Repeat("─", sepWidth)))
	b.WriteString("\n")

	// Footer Preview Pane
	if len(m.sessions) > 0 && m.cursorIndex >= 0 && m.cursorIndex < len(m.sessions) {
		sel := m.sessions[m.cursorIndex]
		aliasPart := ""
		if sel.Alias != "" {
			aliasPart = fmt.Sprintf(" │ Alias: %s", lipgloss.NewStyle().Foreground(colorSkyBlue).Bold(true).Render(sel.Alias))
		}
		morphPart := ""
		if sel.Morph != "" {
			morphPart = fmt.Sprintf(" │ Morph: %s", lipgloss.NewStyle().Foreground(colorHoney).Render(sel.Morph))
		}
		modelPart := fmt.Sprintf(" │ Model: %s", lipgloss.NewStyle().Foreground(colorDim).Render(sel.Model))

		b.WriteString(fmt.Sprintf(" ID: %s%s%s%s\n",
			lipgloss.NewStyle().Bold(true).Render(sel.ID),
			aliasPart,
			morphPart,
			modelPart,
		))

		b.WriteString(fmt.Sprintf(" Dir: %s\n", lipgloss.NewStyle().Foreground(colorDim).Render(sel.WorkDir)))

		b.WriteString(fmt.Sprintf(" Epoch: %s │ Turns: %d │ Tool Calls: %d │ Tokens: in %d, out %d │ Cost: %s\n",
			lipgloss.NewStyle().Foreground(colorHoney).Render(fmt.Sprintf("E%d", sel.EffectiveEpoch())),
			sel.Stats.Turns,
			sel.Stats.ToolCalls,
			sel.Stats.InputTokens,
			sel.Stats.OutputTokens,
			lipgloss.NewStyle().Foreground(colorForestGreen).Render(fmt.Sprintf("$%.4f", sel.Stats.EstimatedCostUSD)),
		))
	} else {
		b.WriteString("\n\n\n")
	}

	// Status / Input Mode Bar
	b.WriteString(" ")
	switch m.inputMode {
	case modeConfirmDelete:
		if len(m.sessions) > 0 && m.cursorIndex < len(m.sessions) {
			b.WriteString(lipgloss.NewStyle().Foreground(colorRed).Bold(true).Render(
				fmt.Sprintf("Delete session %s? [y/N]: ", m.sessions[m.cursorIndex].ID)))
		}
	case modeSearch:
		b.WriteString(lipgloss.NewStyle().Foreground(colorSkyBlue).Bold(true).Render(
			fmt.Sprintf("Search: %s█  [Enter: apply | Esc: clear]", m.textInput)))
	case modeAlias:
		b.WriteString(lipgloss.NewStyle().Foreground(colorHoney).Bold(true).Render(
			fmt.Sprintf("Set alias (empty clears): %s█  [Enter: save | Esc: cancel]", m.textInput)))
	case modeTitle:
		b.WriteString(lipgloss.NewStyle().Foreground(colorHoney).Bold(true).Render(
			fmt.Sprintf("Set title: %s█  [Enter: save | Esc: cancel]", m.textInput)))
	default:
		if m.flashMsg != "" {
			if m.flashIsErr {
				b.WriteString(lipgloss.NewStyle().Foreground(colorRed).Bold(true).Render(m.flashMsg))
			} else {
				b.WriteString(lipgloss.NewStyle().Foreground(colorForestGreen).Bold(true).Render(m.flashMsg))
			}
		} else {
			hintStyle := lipgloss.NewStyle().Foreground(colorDim)
			keyStyle := lipgloss.NewStyle().Foreground(colorBright).Bold(true)
			b.WriteString(fmt.Sprintf("%s Resume  %s New  %s Search  %s Scope  %s Pin  %s Alias  %s Title  %s Delete  %s Quit",
				keyStyle.Render("[Enter]"),
				keyStyle.Render("[n]"),
				keyStyle.Render("[/]"),
				keyStyle.Render("[f]"),
				keyStyle.Render("[p]"),
				keyStyle.Render("[a]"),
				keyStyle.Render("[t]"),
				keyStyle.Render("[d]"),
				keyStyle.Render("[q]"),
			))
			_ = hintStyle
		}
	}
	b.WriteString("\n")

	return b.String()
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}

func shortenWorkDir(workDir string) string {
	if workDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if workDir == home {
			return "~"
		}
		if strings.HasPrefix(workDir, home+string(filepath.Separator)) {
			return "~" + strings.TrimPrefix(workDir, home)
		}
	}
	return workDir
}

func formatRelativeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	if d < 30*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}
