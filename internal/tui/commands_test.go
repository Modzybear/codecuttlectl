package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Modzybear/codecuttlectl/internal/session"
)

func TestCommands_PinAliasTitle(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewFileStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	meta1 := session.SessionMeta{
		Model:   "test-model",
		WorkDir: "/test/dir",
		Title:   "Initial Title",
	}
	id1, err := store.Create(meta1)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	meta2 := session.SessionMeta{
		Model:   "test-model",
		WorkDir: "/test/dir",
		Alias:   "existing-alias",
	}
	_, err = store.Create(meta2)
	if err != nil {
		t.Fatalf("store.Create meta2: %v", err)
	}

	cfg := Config{
		Store:     store,
		SessionID: id1,
	}
	m := New(cfg)

	reg := NewDefaultCommandRegistry()

	// 1. /help contains /pin, /alias, /title
	helpCmd, ok := reg.Get("/help")
	if !ok {
		t.Fatal("/help not registered")
	}
	helpCmd.Run(&m, nil)
	if len(m.messages) == 0 {
		t.Fatal("expected help message")
	}
	helpText := m.messages[len(m.messages)-1].content
	if !strings.Contains(helpText, "`/pin`") || !strings.Contains(helpText, "`/alias") || !strings.Contains(helpText, "`/title") {
		t.Errorf("help text missing new commands:\n%s", helpText)
	}

	// 2. /pin toggle
	pinCmd, ok := reg.Get("/pin")
	if !ok {
		t.Fatal("/pin not registered")
	}
	pinCmd.Run(&m, nil)
	resMeta, err := store.ResolveMeta(id1)
	if err != nil || !resMeta.Pinned {
		t.Fatalf("expected session to be pinned, got err=%v, pinned=%v", err, resMeta.Pinned)
	}
	pinMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(pinMsg, "★ Session pinned") {
		t.Errorf("expected pin message to contain star, got: %s", pinMsg)
	}

	// /pin unpin
	pinCmd.Run(&m, nil)
	resMeta, err = store.ResolveMeta(id1)
	if err != nil || resMeta.Pinned {
		t.Fatalf("expected session to be unpinned, got err=%v, pinned=%v", err, resMeta.Pinned)
	}
	unpinMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(unpinMsg, "Session unpinned") {
		t.Errorf("expected unpin message, got: %s", unpinMsg)
	}

	// 3. /alias
	aliasCmd, ok := reg.Get("/alias")
	if !ok {
		t.Fatal("/alias not registered")
	}

	// /alias with no args (currently empty)
	aliasCmd.Run(&m, nil)
	aliasMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(aliasMsg, "No alias is currently set") {
		t.Errorf("expected 'No alias' message, got: %s", aliasMsg)
	}

	// /alias set valid
	aliasCmd.Run(&m, []string{"my-feature"})
	resMeta, err = store.ResolveMeta(id1)
	if err != nil || resMeta.Alias != "my-feature" {
		t.Fatalf("expected alias my-feature, got err=%v, alias=%s", err, resMeta.Alias)
	}

	// /alias with no args (now set)
	aliasCmd.Run(&m, nil)
	aliasMsg = m.messages[len(m.messages)-1].content
	if !strings.Contains(aliasMsg, "`my-feature`") {
		t.Errorf("expected alias shown, got: %s", aliasMsg)
	}

	// /alias invalid ses_ prefix
	aliasCmd.Run(&m, []string{"ses_invalid"})
	errMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(errMsg, "Invalid alias") {
		t.Errorf("expected invalid alias error, got: %s", errMsg)
	}

	// /alias duplicate rejection
	aliasCmd.Run(&m, []string{"existing-alias"})
	dupErrMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(dupErrMsg, "already in use") && !strings.Contains(dupErrMsg, "Error setting alias") {
		t.Errorf("expected duplicate alias error, got: %s", dupErrMsg)
	}

	// /alias --clear
	aliasCmd.Run(&m, []string{"--clear"})
	resMeta, err = store.ResolveMeta(id1)
	if err != nil || resMeta.Alias != "" {
		t.Fatalf("expected cleared alias, got err=%v, alias=%s", err, resMeta.Alias)
	}

	// /alias set and clear with 'clear'
	aliasCmd.Run(&m, []string{"another-alias"})
	aliasCmd.Run(&m, []string{"clear"})
	resMeta, err = store.ResolveMeta(id1)
	if err != nil || resMeta.Alias != "" {
		t.Fatalf("expected cleared alias with 'clear', got err=%v, alias=%s", err, resMeta.Alias)
	}

	// 4. /title
	titleCmd, ok := reg.Get("/title")
	if !ok {
		t.Fatal("/title not registered")
	}

	// /title with no args
	titleCmd.Run(&m, nil)
	titleMsg := m.messages[len(m.messages)-1].content
	if !strings.Contains(titleMsg, "Initial Title") {
		t.Errorf("expected Initial Title, got: %s", titleMsg)
	}

	// /title update
	titleCmd.Run(&m, []string{"Brand", "New", "Title"})
	resMeta, err = store.ResolveMeta(id1)
	if err != nil || resMeta.Title != "Brand New Title" {
		t.Fatalf("expected title 'Brand New Title', got err=%v, title=%s", err, resMeta.Title)
	}

	// 6. Test dispatching via text
	handled, _ := reg.Dispatch(&m, "/title Final Title Via Dispatch")
	if !handled {
		t.Error("expected /title to be dispatched")
	}
	resMeta, _ = store.ResolveMeta(id1)
	if resMeta.Title != "Final Title Via Dispatch" {
		t.Errorf("expected title from dispatch, got %s", resMeta.Title)
	}
	mNilStore := Model{}
	pinCmd.Run(&mNilStore, nil)
	if !mNilStore.messages[len(mNilStore.messages)-1].isError {
		t.Error("expected error notice when store is nil for /pin")
	}
	aliasCmd.Run(&mNilStore, nil)
	if !mNilStore.messages[len(mNilStore.messages)-1].isError {
		t.Error("expected error notice when store is nil for /alias")
	}
	titleCmd.Run(&mNilStore, nil)
	if !mNilStore.messages[len(mNilStore.messages)-1].isError {
		t.Error("expected error notice when store is nil for /title")
	}
}
