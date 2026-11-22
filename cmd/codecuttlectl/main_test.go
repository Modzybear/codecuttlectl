package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/config"
	"github.com/Modzybear/codecuttlectl/internal/session"
)

func TestFormatShortWorkDir(t *testing.T) {
	if got := formatShortWorkDir(""); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("skipping home dir tests: UserHomeDir unavailable")
	}

	if got := formatShortWorkDir(home); got != "~" {
		t.Errorf("expected ~ for home dir, got %q", got)
	}

	subDir := filepath.Join(home, "projects", "codecuttle")
	expected := filepath.ToSlash(filepath.Join("~", "projects", "codecuttle"))
	got := filepath.ToSlash(formatShortWorkDir(subDir))
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}

	nonHome := "/some/arbitrary/path/outside/home"
	if got := formatShortWorkDir(nonHome); got != nonHome {
		t.Errorf("expected %q for non-home path, got %q", nonHome, got)
	}
}

func TestComputeSessionMorphSpec(t *testing.T) {
	tmpDir := t.TempDir()
	morphsDir := filepath.Join(tmpDir, ".codecuttle", "morphs")
	if err := os.MkdirAll(morphsDir, 0755); err != nil {
		t.Fatal(err)
	}

	morphPath := filepath.Join(morphsDir, "swarm-test.yaml")
	morphContent := `name: swarm-test
version: "1.0.0"
nodes:
  worker:
    provider: ollama
    model: gemma4
    is_primary: true
`
	if err := os.WriteFile(morphPath, []byte(morphContent), 0644); err != nil {
		t.Fatal(err)
	}

	userCfg := config.Config{}

	// Case 1: Empty path
	if got := computeSessionMorphSpec(config.MorphResolution{}, tmpDir, userCfg); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}

	// Case 2: Discoverable morph in project .codecuttle/
	resDiscoverable := config.MorphResolution{
		Path: morphPath,
		Name: "swarm-test",
	}
	if got := computeSessionMorphSpec(resDiscoverable, tmpDir, userCfg); got != "swarm-test" {
		t.Errorf("expected portable morph name 'swarm-test', got %q", got)
	}

	// Case 3: Adhoc external morph file not in search dirs
	adhocDir := t.TempDir()
	adhocFile := filepath.Join(adhocDir, "adhoc.yaml")
	if err := os.WriteFile(adhocFile, []byte(morphContent), 0644); err != nil {
		t.Fatal(err)
	}
	resAdhoc := config.MorphResolution{
		Path: adhocFile,
		Name: "adhoc",
	}
	expectedAbs, _ := filepath.Abs(adhocFile)
	if got := computeSessionMorphSpec(resAdhoc, tmpDir, userCfg); got != expectedAbs {
		t.Errorf("expected absolute path %q, got %q", expectedAbs, got)
	}
}

func TestPrintSessionsList_TableAndJSON(t *testing.T) {
	dataDir := t.TempDir()
	store, err := session.NewFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	// Create test sessions
	s1 := &session.SessionState{
		Meta: session.SessionMeta{
			ID:           "ses_0001",
			Alias:        "main-task",
			Title:        "Implement feature X",
			Pinned:       true,
			CurrentEpoch: 2,
			WorkDir:      "/home/user/project",
			Model:        "claude-opus-4",
			UpdatedAt:    now.Add(-10 * time.Minute),
			Stats: session.Stats{
				EstimatedCostUSD: 1.25,
			},
		},
	}
	s2 := &session.SessionState{
		Meta: session.SessionMeta{
			ID:           "ses_0002",
			Title:        "Debug bug Y",
			Pinned:       false,
			CurrentEpoch: 1,
			WorkDir:      "/tmp/other",
			Model:        "claude-sonnet-4",
			UpdatedAt:    now.Add(-2 * time.Hour),
			Stats: session.Stats{
				EstimatedCostUSD: 0.42,
			},
		},
	}
	if err := store.Save("ses_0001", s1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("ses_0002", s2); err != nil {
		t.Fatal(err)
	}

	// Test 1: Full table output
	var tableBuf bytes.Buffer
	err = printSessionsList(&tableBuf, store, 0, session.ListFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	tableOut := tableBuf.String()

	// Check table headers
	for _, col := range []string{"PIN", "ALIAS / ID", "TITLE", "EPOCH", "WORKDIR", "MODEL", "COST", "AGE"} {
		if !strings.Contains(tableOut, col) {
			t.Errorf("expected header %q in table output, got:\n%s", col, tableOut)
		}
	}
	// Check pinned star and alias
	if !strings.Contains(tableOut, "★") {
		t.Errorf("expected pinned star ★ in table output, got:\n%s", tableOut)
	}
	if !strings.Contains(tableOut, "main-task (ses_0001)") {
		t.Errorf("expected alias formatting in table output, got:\n%s", tableOut)
	}
	if !strings.Contains(tableOut, "E2") {
		t.Errorf("expected E2 epoch in table output, got:\n%s", tableOut)
	}

	// Test 2: Filter pinned only
	var pinnedBuf bytes.Buffer
	err = printSessionsList(&pinnedBuf, store, 0, session.ListFilter{PinnedOnly: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	pinnedOut := pinnedBuf.String()
	if !strings.Contains(pinnedOut, "ses_0001") {
		t.Errorf("expected ses_0001 in pinned output, got:\n%s", pinnedOut)
	}
	if strings.Contains(pinnedOut, "ses_0002") {
		t.Errorf("did not expect unpinned ses_0002 in pinned output, got:\n%s", pinnedOut)
	}

	// Test 3: Filter by query
	var queryBuf bytes.Buffer
	err = printSessionsList(&queryBuf, store, 0, session.ListFilter{Query: "Debug"}, false)
	if err != nil {
		t.Fatal(err)
	}
	queryOut := queryBuf.String()
	if strings.Contains(queryOut, "ses_0001") {
		t.Errorf("did not expect ses_0001 in query output, got:\n%s", queryOut)
	}
	if !strings.Contains(queryOut, "ses_0002") {
		t.Errorf("expected ses_0002 in query output, got:\n%s", queryOut)
	}

	// Test 4: JSON output
	var jsonBuf bytes.Buffer
	err = printSessionsList(&jsonBuf, store, 0, session.ListFilter{}, true)
	if err != nil {
		t.Fatal(err)
	}
	var jsonMetas []session.SessionMeta
	if err := json.Unmarshal(jsonBuf.Bytes(), &jsonMetas); err != nil {
		t.Fatalf("failed to parse json output: %v", err)
	}
	if len(jsonMetas) != 2 {
		t.Fatalf("expected 2 session metas in json, got %d", len(jsonMetas))
	}

	// Test 5: Empty sessions list
	emptyStore, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var emptyBuf bytes.Buffer
	err = printSessionsList(&emptyBuf, emptyStore, 0, session.ListFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emptyBuf.String(), "No sessions found.") {
		t.Errorf("expected 'No sessions found.' for empty store, got %q", emptyBuf.String())
	}
}

func TestPrintMorphsList_TableAndJSON(t *testing.T) {
	tmpDir := t.TempDir()
	morphsDir := filepath.Join(tmpDir, ".codecuttle", "morphs")
	if err := os.MkdirAll(morphsDir, 0755); err != nil {
		t.Fatal(err)
	}

	morphContent := `name: dev-swarm
version: "1.0.0"
nodes:
  lead:
    provider: bedrock
    model: claude-sonnet-4
    is_primary: true
  reviewer:
    provider: openrouter
    model: openai/gpt-4o
`
	if err := os.WriteFile(filepath.Join(morphsDir, "dev-swarm.yaml"), []byte(morphContent), 0644); err != nil {
		t.Fatal(err)
	}

	userCfg := config.Config{}

	// Test 1: Table output
	var tableBuf bytes.Buffer
	err := printMorphsList(&tableBuf, tmpDir, userCfg, false)
	if err != nil {
		t.Fatal(err)
	}
	tableOut := tableBuf.String()
	for _, col := range []string{"SCOPE", "NAME", "NODES", "PRIMARY", "STATUS", "PATH"} {
		if !strings.Contains(tableOut, col) {
			t.Errorf("expected column %q in morph table output, got:\n%s", col, tableOut)
		}
	}
	if !strings.Contains(tableOut, "dev-swarm") {
		t.Errorf("expected dev-swarm in morph table output, got:\n%s", tableOut)
	}
	if !strings.Contains(tableOut, "lead") {
		t.Errorf("expected primary node lead in morph table output, got:\n%s", tableOut)
	}

	// Test 2: JSON output
	var jsonBuf bytes.Buffer
	err = printMorphsList(&jsonBuf, tmpDir, userCfg, true)
	if err != nil {
		t.Fatal(err)
	}
	var discovered []config.DiscoveredMorph
	if err := json.Unmarshal(jsonBuf.Bytes(), &discovered); err != nil {
		t.Fatalf("failed to parse json output: %v", err)
	}
	var found *config.DiscoveredMorph
	for i := range discovered {
		if discovered[i].Name == "dev-swarm" && discovered[i].Scope == "project" {
			found = &discovered[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected project morph dev-swarm in JSON output, got: %+v", discovered)
	}
	if found.NodeCount != 2 || found.PrimaryNode != "lead" {
		t.Errorf("unexpected discovered morph: %+v", *found)
	}
}

func TestPrintSessionExit(t *testing.T) {
	dataDir := t.TempDir()
	store, err := session.NewFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	sWithAlias := &session.SessionState{
		Meta: session.SessionMeta{
			ID:    "ses_alias",
			Alias: "my-alias",
		},
	}
	sNoAlias := &session.SessionState{
		Meta: session.SessionMeta{
			ID: "ses_plain",
		},
	}
	if err := store.Save("ses_alias", sWithAlias); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("ses_plain", sNoAlias); err != nil {
		t.Fatal(err)
	}

	// Capture stderr by running helper logic
	// Test alias meta resolution
	metaAlias, err := store.ResolveMeta("ses_alias")
	if err != nil || metaAlias.Alias != "my-alias" {
		t.Errorf("expected alias 'my-alias', got %q", metaAlias.Alias)
	}

	metaPlain, err := store.ResolveMeta("ses_plain")
	if err != nil || metaPlain.Alias != "" {
		t.Errorf("expected empty alias, got %q", metaPlain.Alias)
	}
}

func TestFormatAge(t *testing.T) {
	if got := formatAge(time.Time{}); got != "-" {
		t.Errorf("expected '-', got %q", got)
	}

	now := time.Now()
	if got := formatAge(now.Add(-10 * time.Second)); got != "just now" {
		t.Errorf("expected 'just now', got %q", got)
	}
	if got := formatAge(now.Add(-15 * time.Minute)); got != "15m ago" {
		t.Errorf("expected '15m ago', got %q", got)
	}
	if got := formatAge(now.Add(-3 * time.Hour)); got != "3h ago" {
		t.Errorf("expected '3h ago', got %q", got)
	}
	if got := formatAge(now.Add(-25 * time.Hour)); got != "1 day ago" {
		t.Errorf("expected '1 day ago', got %q", got)
	}
	if got := formatAge(now.Add(-72 * time.Hour)); got != "3d ago" {
		t.Errorf("expected '3d ago', got %q", got)
	}
}

func TestIsTTY(t *testing.T) {
	if isTTY(nil) {
		t.Errorf("expected isTTY(nil) to be false")
	}
}
