package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

type keyPress string

func (k keyPress) String() string    { return string(k) }
func (k keyPress) Keystroke() string { return string(k) }
func (k keyPress) Key() tea.Key {
	return tea.Key{
		Text: string(k),
	}
}

func sendKey(m tea.Model, key string) (tea.Model, tea.Cmd) {
	return m.Update(keyPress(key))
}

func TestSessionPicker_NavigationAndResume(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewFileStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	id1, _ := store.Create(session.SessionMeta{Title: "Session One", WorkDir: "/work"})
	time.Sleep(10 * time.Millisecond)
	id2, _ := store.Create(session.SessionMeta{Title: "Session Two", WorkDir: "/work"})

	m := NewSessionPickerModel(store, "/work")
	if len(m.sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(m.sessions))
	}

	// Initial cursor is at index 0 (id2, most recent)
	if m.cursorIndex != 0 {
		t.Errorf("expected cursor 0, got %d", m.cursorIndex)
	}

	// Move down with 'j'
	mModel, _ := sendKey(m, "j")
	m = mModel.(SessionPickerModel)
	if m.cursorIndex != 1 {
		t.Errorf("expected cursor 1 after 'j', got %d", m.cursorIndex)
	}

	// Move up with 'k'
	mModel, _ = sendKey(m, "k")
	m = mModel.(SessionPickerModel)
	if m.cursorIndex != 0 {
		t.Errorf("expected cursor 0 after 'k', got %d", m.cursorIndex)
	}

	// Press Enter to resume
	mModel, cmd := sendKey(m, "enter")
	m = mModel.(SessionPickerModel)
	if cmd == nil {
		t.Fatal("expected non-nil cmd (tea.Quit)")
	}
	res := m.Result()
	if res.Action != PickerActionResume || res.SessionID != id2 {
		t.Errorf("expected Resume session %s, got action=%v id=%s", id2, res.Action, res.SessionID)
	}
	_ = id1
}

func TestSessionPicker_NewAndQuit(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	store.Create(session.SessionMeta{Title: "Session A"})

	// Test 'n'
	m := NewSessionPickerModel(store, "/work")
	mModel, _ := sendKey(m, "n")
	m = mModel.(SessionPickerModel)
	if m.Result().Action != PickerActionNew {
		t.Errorf("expected PickerActionNew, got %v", m.Result().Action)
	}

	// Test 'q'
	m2 := NewSessionPickerModel(store, "/work")
	mModel2, _ := sendKey(m2, "q")
	m2 = mModel2.(SessionPickerModel)
	if m2.Result().Action != PickerActionQuit {
		t.Errorf("expected PickerActionQuit, got %v", m2.Result().Action)
	}
}

func TestSessionPicker_SearchFilter(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	store.Create(session.SessionMeta{Title: "Frontend React App"})
	store.Create(session.SessionMeta{Title: "Backend Go API"})

	m := NewSessionPickerModel(store, "/work")
	if len(m.sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(m.sessions))
	}

	// Press '/' to start search
	mModel, _ := sendKey(m, "/")
	m = mModel.(SessionPickerModel)
	if m.inputMode != modeSearch {
		t.Fatalf("expected modeSearch, got %v", m.inputMode)
	}

	// Type "React"
	for _, ch := range "React" {
		mModel, _ = sendKey(m, string(ch))
		m = mModel.(SessionPickerModel)
	}

	if len(m.sessions) != 1 || m.sessions[0].Title != "Frontend React App" {
		t.Fatalf("expected 1 session matching React, got %d", len(m.sessions))
	}

	// Press Enter to submit search
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)
	if m.inputMode != modeNormal {
		t.Fatalf("expected modeNormal after submit, got %v", m.inputMode)
	}
	if len(m.sessions) != 1 {
		t.Fatalf("expected 1 filtered session, got %d", len(m.sessions))
	}

	// Press '/' then Esc to clear search
	mModel, _ = sendKey(m, "/")
	m = mModel.(SessionPickerModel)
	mModel, _ = sendKey(m, "esc")
	m = mModel.(SessionPickerModel)
	if len(m.sessions) != 2 {
		t.Fatalf("expected 2 sessions after clearing search, got %d", len(m.sessions))
	}
}

func TestSessionPicker_ScopeCycling(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	store.Create(session.SessionMeta{Title: "Pinned elsewhere", Pinned: true, WorkDir: "/other"})
	store.Create(session.SessionMeta{Title: "Unpinned here", Pinned: false, WorkDir: "/mywork"})
	store.Create(session.SessionMeta{Title: "Unpinned elsewhere", Pinned: false, WorkDir: "/other"})

	m := NewSessionPickerModel(store, "/mywork")

	// ScopeAll: 3 sessions
	if len(m.sessions) != 3 {
		t.Fatalf("expected 3 sessions in ScopeAll, got %d", len(m.sessions))
	}

	// Press 'f' -> ScopePinned
	mModel, _ := sendKey(m, "f")
	m = mModel.(SessionPickerModel)
	if m.scope != ScopePinned || len(m.sessions) != 1 || m.sessions[0].Title != "Pinned elsewhere" {
		t.Fatalf("expected 1 pinned session in ScopePinned, got %d (scope=%v)", len(m.sessions), m.scope)
	}

	// Press 'f' -> ScopeHere
	mModel, _ = sendKey(m, "f")
	m = mModel.(SessionPickerModel)
	if m.scope != ScopeHere || len(m.sessions) != 1 || m.sessions[0].Title != "Unpinned here" {
		t.Fatalf("expected 1 session in ScopeHere, got %d (scope=%v)", len(m.sessions), m.scope)
	}

	// Press 'f' -> ScopeAll
	mModel, _ = sendKey(m, "f")
	m = mModel.(SessionPickerModel)
	if m.scope != ScopeAll || len(m.sessions) != 3 {
		t.Fatalf("expected 3 sessions back in ScopeAll, got %d (scope=%v)", len(m.sessions), m.scope)
	}
}

func TestSessionPicker_PinToggle(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	id, _ := store.Create(session.SessionMeta{Title: "Toggle Pin", Pinned: false})

	m := NewSessionPickerModel(store, "/work")

	// Press 'p' to pin
	mModel, _ := sendKey(m, "p")
	m = mModel.(SessionPickerModel)
	meta, err := store.ResolveMeta(id)
	if err != nil || !meta.Pinned {
		t.Fatalf("expected pinned true in store, got %v (err=%v)", meta.Pinned, err)
	}

	// Press 'p' again to unpin
	mModel, _ = sendKey(m, "p")
	m = mModel.(SessionPickerModel)
	meta, err = store.ResolveMeta(id)
	if err != nil || meta.Pinned {
		t.Fatalf("expected pinned false in store, got %v (err=%v)", meta.Pinned, err)
	}
}

func TestSessionPicker_AliasEditAndValidation(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	id1, _ := store.Create(session.SessionMeta{Title: "Session 1"})
	store.Create(session.SessionMeta{Title: "Session 2", Alias: "existing-alias"})

	m := NewSessionPickerModel(store, "/work")
	// Target session 1
	for i, s := range m.sessions {
		if s.ID == id1 {
			m.cursorIndex = i
			break
		}
	}

	// Press 'a' to set alias
	mModel, _ := sendKey(m, "a")
	m = mModel.(SessionPickerModel)
	if m.inputMode != modeAlias {
		t.Fatalf("expected modeAlias, got %v", m.inputMode)
	}

	// Type "my-alias"
	for _, ch := range "my-alias" {
		mModel, _ = sendKey(m, string(ch))
		m = mModel.(SessionPickerModel)
	}
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)

	meta1, _ := store.ResolveMeta(id1)
	if meta1.Alias != "my-alias" {
		t.Fatalf("expected alias 'my-alias', got %s", meta1.Alias)
	}

	// Try invalid alias prefix ses_
	mModel, _ = sendKey(m, "a")
	m = mModel.(SessionPickerModel)
	m.textInput = "ses_foo"
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)
	if !m.flashIsErr || !strings.Contains(m.flashMsg, "Invalid alias") {
		t.Fatalf("expected invalid alias error flash, got isErr=%v msg=%s", m.flashIsErr, m.flashMsg)
	}

	// Try duplicate alias
	mModel, _ = sendKey(m, "a")
	m = mModel.(SessionPickerModel)
	m.textInput = "existing-alias"
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)
	if !m.flashIsErr || (!strings.Contains(m.flashMsg, "already in use") && !strings.Contains(m.flashMsg, "Error saving alias")) {
		t.Fatalf("expected duplicate alias error flash, got isErr=%v msg=%s", m.flashIsErr, m.flashMsg)
	}

	// Clear alias with empty input
	mModel, _ = sendKey(m, "a")
	m = mModel.(SessionPickerModel)
	m.textInput = ""
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)
	meta1, _ = store.ResolveMeta(id1)
	if meta1.Alias != "" {
		t.Fatalf("expected cleared alias, got %s", meta1.Alias)
	}
}

func TestSessionPicker_TitleEdit(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	id, _ := store.Create(session.SessionMeta{Title: "Old Title"})

	m := NewSessionPickerModel(store, "/work")
	mModel, _ := sendKey(m, "t")
	m = mModel.(SessionPickerModel)
	if m.inputMode != modeTitle {
		t.Fatalf("expected modeTitle, got %v", m.inputMode)
	}

	m.textInput = "Updated Title"
	mModel, _ = sendKey(m, "enter")
	m = mModel.(SessionPickerModel)

	meta, _ := store.ResolveMeta(id)
	if meta.Title != "Updated Title" {
		t.Fatalf("expected 'Updated Title', got %s", meta.Title)
	}
}

func TestSessionPicker_DeleteBehavior(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))
	idPinned, _ := store.Create(session.SessionMeta{Title: "Pinned Session", Pinned: true})
	idUnpinned, _ := store.Create(session.SessionMeta{Title: "Unpinned Session", Pinned: false})

	m := NewSessionPickerModel(store, "/work")

	// Select pinned session
	for i, s := range m.sessions {
		if s.ID == idPinned {
			m.cursorIndex = i
			break
		}
	}

	// Press 'd' on pinned session -> blocked
	mModel, _ := sendKey(m, "d")
	m = mModel.(SessionPickerModel)
	if !m.flashIsErr || !strings.Contains(m.flashMsg, "unpin before deleting") {
		t.Fatalf("expected pinned warning, got isErr=%v msg=%s", m.flashIsErr, m.flashMsg)
	}
	if _, err := store.ResolveMeta(idPinned); err != nil {
		t.Fatal("pinned session should not have been deleted")
	}

	// Select unpinned session
	for i, s := range m.sessions {
		if s.ID == idUnpinned {
			m.cursorIndex = i
			break
		}
	}

	// Press 'd' then cancel with 'n'
	mModel, _ = sendKey(m, "d")
	m = mModel.(SessionPickerModel)
	if m.inputMode != modeConfirmDelete {
		t.Fatalf("expected modeConfirmDelete, got %v", m.inputMode)
	}
	mModel, _ = sendKey(m, "n")
	m = mModel.(SessionPickerModel)
	if _, err := store.ResolveMeta(idUnpinned); err != nil {
		t.Fatal("session should not have been deleted after cancel")
	}

	// Press 'd' then confirm with 'y'
	mModel, _ = sendKey(m, "d")
	m = mModel.(SessionPickerModel)
	mModel, _ = sendKey(m, "y")
	m = mModel.(SessionPickerModel)
	if _, err := store.ResolveMeta(idUnpinned); err == nil {
		t.Fatal("session should have been deleted after confirming 'y'")
	}
}

func TestSessionPicker_EmptyStoreRendering(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewFileStore(filepath.Join(dir, "sessions"))

	m := NewSessionPickerModel(store, "/work")
	view := m.View()
	content := view.Content
	if !strings.Contains(content, "No sessions found") {
		t.Errorf("expected 'No sessions found' in view, got:\n%s", content)
	}

	// Keys should not crash on empty store
	keys := []string{"enter", "p", "a", "t", "d", "j", "k"}
	for _, k := range keys {
		mModel, _ := sendKey(m, k)
		m = mModel.(SessionPickerModel)
	}
}
