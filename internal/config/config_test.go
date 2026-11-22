package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXDGAndExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir not available")
	}

	t.Run("ExpandPath", func(t *testing.T) {
		if got := ExpandPath("~"); got != home {
			t.Errorf("expected %q, got %q", home, got)
		}
		if got := ExpandPath("~/foo/bar"); got != filepath.Join(home, "foo/bar") {
			t.Errorf("expected %q, got %q", filepath.Join(home, "foo/bar"), got)
		}
		if got := ExpandPath("/absolute/path"); got != "/absolute/path" {
			t.Errorf("expected unchanged path, got %q", got)
		}
		if got := ExpandPath(""); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("ConfigDir and ConfigPath default", func(t *testing.T) {
		origXDG := os.Getenv("XDG_CONFIG_HOME")
		origCfg := os.Getenv("CODECUTTLE_CONFIG")
		defer func() {
			os.Setenv("XDG_CONFIG_HOME", origXDG)
			os.Setenv("CODECUTTLE_CONFIG", origCfg)
		}()

		os.Unsetenv("XDG_CONFIG_HOME")
		os.Unsetenv("CODECUTTLE_CONFIG")

		expectedDir := filepath.Join(home, ".config", "codecuttlectl")
		if got := ConfigDir(); got != expectedDir {
			t.Errorf("expected %q, got %q", expectedDir, got)
		}
		expectedPath := filepath.Join(expectedDir, "config.yaml")
		if got := ConfigPath(); got != expectedPath {
			t.Errorf("expected %q, got %q", expectedPath, got)
		}
		if got := UserMorphsDir(); got != filepath.Join(expectedDir, "morphs") {
			t.Errorf("expected %q, got %q", filepath.Join(expectedDir, "morphs"), got)
		}
		if got := SystemMorphsDir(); got != "/usr/local/share/codecuttlectl/morphs" {
			t.Errorf("expected system morphs dir, got %q", got)
		}
		if got := SystemPluginsDir(); got != "/usr/local/lib/codecuttlectl/plugins" {
			t.Errorf("expected system plugins dir, got %q", got)
		}
	})

	t.Run("ConfigDir and ConfigPath with env overrides", func(t *testing.T) {
		tempDir := t.TempDir()
		origXDG := os.Getenv("XDG_CONFIG_HOME")
		origCfg := os.Getenv("CODECUTTLE_CONFIG")
		defer func() {
			os.Setenv("XDG_CONFIG_HOME", origXDG)
			os.Setenv("CODECUTTLE_CONFIG", origCfg)
		}()

		os.Setenv("XDG_CONFIG_HOME", tempDir)
		if got := ConfigDir(); got != filepath.Join(tempDir, "codecuttlectl") {
			t.Errorf("expected %q, got %q", filepath.Join(tempDir, "codecuttlectl"), got)
		}

		customCfg := filepath.Join(tempDir, "custom.yaml")
		os.Setenv("CODECUTTLE_CONFIG", customCfg)
		if got := ConfigPath(); got != customCfg {
			t.Errorf("expected %q, got %q", customCfg, got)
		}
	})

	t.Run("UserPluginsDirs with XDG_DATA_HOME", func(t *testing.T) {
		tempDir := t.TempDir()
		origXDG := os.Getenv("XDG_DATA_HOME")
		defer os.Setenv("XDG_DATA_HOME", origXDG)

		os.Setenv("XDG_DATA_HOME", tempDir)
		dirs := UserPluginsDirs()
		if len(dirs) != 2 {
			t.Fatalf("expected 2 dirs, got %d", len(dirs))
		}
		if dirs[0] != filepath.Join(tempDir, "codecuttlectl", "plugins") {
			t.Errorf("expected %q, got %q", filepath.Join(tempDir, "codecuttlectl", "plugins"), dirs[0])
		}
	})
}

func TestLoadConfig(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("missing file returns empty config without error", func(t *testing.T) {
		cfg, err := LoadFrom(filepath.Join(tempDir, "nonexistent.yaml"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != (Config{}) {
			t.Fatalf("expected empty config, got %+v", cfg)
		}
	})

	t.Run("empty string path returns empty config without error", func(t *testing.T) {
		cfg, err := LoadFrom("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != (Config{}) {
			t.Fatalf("expected empty config, got %+v", cfg)
		}
	})

	t.Run("valid yaml", func(t *testing.T) {
		cfgPath := filepath.Join(tempDir, "valid.yaml")
		content := `
plugin_dir: /custom/plugins
default_morph: expert-swarm
morphs_dir: ~/my-morphs
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadFrom(cfgPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.PluginDir != "/custom/plugins" || cfg.DefaultMorph != "expert-swarm" || cfg.MorphsDir != "~/my-morphs" {
			t.Errorf("unexpected loaded values: %+v", cfg)
		}
	})

	t.Run("malformed yaml wraps path in error", func(t *testing.T) {
		cfgPath := filepath.Join(tempDir, "invalid.yaml")
		content := `
plugin_dir: [unclosed list
`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadFrom(cfgPath)
		if err == nil {
			t.Fatalf("expected error on malformed YAML")
		}
		if !strings.Contains(err.Error(), cfgPath) {
			t.Errorf("expected error to contain path %q, got %q", cfgPath, err.Error())
		}
	})
}

func TestResolvePluginDir(t *testing.T) {
	tempDir := t.TempDir()
	origEnv := os.Getenv("CODECUTTLE_PLUGIN_DIR")
	defer os.Setenv("CODECUTTLE_PLUGIN_DIR", origEnv)

	t.Run("flagSet overrides everything", func(t *testing.T) {
		os.Setenv("CODECUTTLE_PLUGIN_DIR", "/env/plugins")
		cfg := Config{PluginDir: "/cfg/plugins"}

		// Explicit path
		got := ResolvePluginDir("/cli/plugins", true, cfg)
		if got != "/cli/plugins" {
			t.Errorf("expected /cli/plugins, got %q", got)
		}

		// Opt-out with "none"
		got = ResolvePluginDir("none", true, cfg)
		if got != "" {
			t.Errorf("expected empty string for none opt-out, got %q", got)
		}

		// Empty string flag
		got = ResolvePluginDir("", true, cfg)
		if got != "" {
			t.Errorf("expected empty string for empty flag, got %q", got)
		}
	})

	t.Run("env overrides config", func(t *testing.T) {
		cfg := Config{PluginDir: "/cfg/plugins"}

		os.Setenv("CODECUTTLE_PLUGIN_DIR", "/env/plugins")
		got := ResolvePluginDir("", false, cfg)
		if got != "/env/plugins" {
			t.Errorf("expected /env/plugins, got %q", got)
		}

		os.Setenv("CODECUTTLE_PLUGIN_DIR", "none")
		got = ResolvePluginDir("", false, cfg)
		if got != "" {
			t.Errorf("expected empty string for env none, got %q", got)
		}
	})

	t.Run("config value used when no flag or env", func(t *testing.T) {
		os.Unsetenv("CODECUTTLE_PLUGIN_DIR")
		cfg := Config{PluginDir: "/cfg/plugins"}
		got := ResolvePluginDir("", false, cfg)
		if got != "/cfg/plugins" {
			t.Errorf("expected /cfg/plugins, got %q", got)
		}

		cfgNone := Config{PluginDir: "none"}
		got = ResolvePluginDir("", false, cfgNone)
		if got != "" {
			t.Errorf("expected empty string for cfg none, got %q", got)
		}
	})

	t.Run("auto-detect candidate directory with cuttlebone prefix", func(t *testing.T) {
		os.Unsetenv("CODECUTTLE_PLUGIN_DIR")
		cfg := Config{}

		origXDGData := os.Getenv("XDG_DATA_HOME")
		defer os.Setenv("XDG_DATA_HOME", origXDGData)

		mockDataHome := filepath.Join(tempDir, "data")
		pluginsDir := filepath.Join(mockDataHome, "codecuttlectl", "plugins")
		if err := os.MkdirAll(pluginsDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginsDir, "cuttlebone-test"), []byte("#!/bin/sh"), 0755); err != nil {
			t.Fatal(err)
		}

		os.Setenv("XDG_DATA_HOME", mockDataHome)
		got := ResolvePluginDir("", false, cfg)
		if got != pluginsDir {
			t.Errorf("expected auto-detected %q, got %q", pluginsDir, got)
		}
	})
}

func sampleMorphYAML(name string) string {
	return `
name: "` + name + `"
version: "1.0.0"
description: "Sample morph"
nodes:
  lead:
    provider: "bedrock"
    model: "claude-v1"
    system_prompt: "Lead node"
    workbench: ["*"]
    is_primary: true
`
}

func TestLookupMorphPath(t *testing.T) {
	tempDir := t.TempDir()
	workDir := filepath.Join(tempDir, "work")
	customMorphsDir := filepath.Join(tempDir, "custom_morphs")
	projectMorphsDir := filepath.Join(workDir, ".codecuttle", "morphs")
	if err := os.MkdirAll(projectMorphsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customMorphsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a project-level morph and a custom morph
	projFile := filepath.Join(projectMorphsDir, "proj-swarm.yaml")
	if err := os.WriteFile(projFile, []byte(sampleMorphYAML("proj-swarm")), 0644); err != nil {
		t.Fatal(err)
	}
	custFile := filepath.Join(customMorphsDir, "custom-swarm.yml")
	if err := os.WriteFile(custFile, []byte(sampleMorphYAML("custom-swarm")), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{MorphsDir: customMorphsDir}

	t.Run("named lookup in project morphs", func(t *testing.T) {
		p, name, err := LookupMorphPath("proj-swarm", workDir, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != projFile || name != "proj-swarm" {
			t.Errorf("got path=%q name=%q, expected path=%q name=proj-swarm", p, name, projFile)
		}
	})

	t.Run("named lookup in custom morphs with .yaml suffix stripped", func(t *testing.T) {
		p, name, err := LookupMorphPath("custom-swarm.yaml", workDir, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != custFile || name != "custom-swarm" {
			t.Errorf("got path=%q name=%q, expected path=%q name=custom-swarm", p, name, custFile)
		}
	})

	t.Run("direct file path lookup", func(t *testing.T) {
		relPath := filepath.Join(".", "custom_morphs", "custom-swarm.yml")
		p, name, err := LookupMorphPath(relPath, tempDir, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != custFile || name != "custom-swarm" {
			t.Errorf("got path=%q name=%q", p, name)
		}
	})

	t.Run("rejects path traversal in named lookup", func(t *testing.T) {
		_, _, err := LookupMorphPath("foo..bar", workDir, cfg)
		if err == nil || !strings.Contains(err.Error(), "path traversal not allowed") {
			t.Errorf("expected path traversal error, got %v", err)
		}
	})

	t.Run("not found returns searched directories", func(t *testing.T) {
		_, _, err := LookupMorphPath("nonexistent", workDir, cfg)
		if err == nil {
			t.Fatalf("expected error for nonexistent morph")
		}
		if !strings.Contains(err.Error(), "searched:") || !strings.Contains(err.Error(), projectMorphsDir) {
			t.Errorf("expected search directories in error message, got: %v", err)
		}
	})
}

func TestResolveMorphPrecedence(t *testing.T) {
	tempDir := t.TempDir()
	workDir := filepath.Join(tempDir, "workspace")
	customMorphsDir := filepath.Join(tempDir, "custom_morphs")
	projectMorphsDir := filepath.Join(workDir, ".codecuttle", "morphs")
	if err := os.MkdirAll(projectMorphsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customMorphsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create sample morphs
	flagMorph := filepath.Join(customMorphsDir, "flag-morph.yaml")
	sessionMorph := filepath.Join(customMorphsDir, "session-morph.yaml")
	envMorph := filepath.Join(customMorphsDir, "env-morph.yaml")
	projectMorph := filepath.Join(workDir, ".codecuttle", "morph.yaml")
	configMorph := filepath.Join(customMorphsDir, "config-morph.yaml")

	for path, name := range map[string]string{
		flagMorph:    "flag-morph",
		sessionMorph: "session-morph",
		envMorph:     "env-morph",
		projectMorph: "project-morph",
		configMorph:  "config-morph",
	} {
		if err := os.WriteFile(path, []byte(sampleMorphYAML(name)), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Config{
		MorphsDir:    customMorphsDir,
		DefaultMorph: "config-morph",
	}

	origEnv := os.Getenv("CODECUTTLE_MORPH")
	defer os.Setenv("CODECUTTLE_MORPH", origEnv)
	os.Unsetenv("CODECUTTLE_MORPH")

	// 1. Flag overrides everything
	res, err := ResolveMorph(MorphResolveOptions{
		FlagValue:    "flag-morph",
		FlagSet:      true,
		SessionMorph: "session-morph",
		WorkDir:      workDir,
		Config:       cfg,
	})
	if err != nil || res.Source != "flag" || res.Name != "flag-morph" {
		t.Errorf("expected flag morph, got %+v (err: %v)", res, err)
	}

	// 1b. Flag "none" disables morph resolution
	res, err = ResolveMorph(MorphResolveOptions{
		FlagValue:    "none",
		FlagSet:      true,
		SessionMorph: "session-morph",
		WorkDir:      workDir,
		Config:       cfg,
	})
	if err != nil || res.Path != "" {
		t.Errorf("expected empty resolution for flag 'none', got %+v", res)
	}

	// 2. Explicit model/provider disables ambient morph
	res, err = ResolveMorph(MorphResolveOptions{
		ExplicitModelOrProvider: true,
		SessionMorph:            "session-morph",
		WorkDir:                 workDir,
		Config:                  cfg,
	})
	if err != nil || res.Path != "" {
		t.Errorf("expected empty resolution for explicit model/provider, got %+v", res)
	}

	// 3. Session morph when found
	res, err = ResolveMorph(MorphResolveOptions{
		SessionMorph: "session-morph",
		WorkDir:      workDir,
		Config:       cfg,
	})
	if err != nil || res.Source != "session" || res.Name != "session-morph" {
		t.Errorf("expected session morph, got %+v (err: %v)", res, err)
	}

	// 3b. Missing session morph produces warning and falls through to env/project/config
	os.Setenv("CODECUTTLE_MORPH", "env-morph")
	res, err = ResolveMorph(MorphResolveOptions{
		SessionMorph: "nonexistent-session-morph",
		WorkDir:      workDir,
		Config:       cfg,
	})
	if err != nil || res.Source != "env" || res.Name != "env-morph" {
		t.Errorf("expected env morph fallback, got %+v (err: %v)", res, err)
	}
	if !strings.Contains(res.Warning, "session morphology \"nonexistent-session-morph\" not found") {
		t.Errorf("expected session morph warning, got %q", res.Warning)
	}
	os.Unsetenv("CODECUTTLE_MORPH")

	// 4. Env morph
	os.Setenv("CODECUTTLE_MORPH", "env-morph")
	res, err = ResolveMorph(MorphResolveOptions{
		WorkDir: workDir,
		Config:  cfg,
	})
	if err != nil || res.Source != "env" || res.Name != "env-morph" {
		t.Errorf("expected env morph, got %+v (err: %v)", res, err)
	}

	// 4b. Env "none" disables ambient morph
	os.Setenv("CODECUTTLE_MORPH", "none")
	res, err = ResolveMorph(MorphResolveOptions{
		WorkDir: workDir,
		Config:  cfg,
	})
	if err != nil || res.Path != "" {
		t.Errorf("expected empty resolution for env 'none', got %+v", res)
	}
	os.Unsetenv("CODECUTTLE_MORPH")

	// 5. Project morph (.codecuttle/morph.yaml)
	res, err = ResolveMorph(MorphResolveOptions{
		WorkDir: workDir,
		Config:  cfg,
	})
	if err != nil || res.Source != "project" || res.Name != "morph" {
		t.Errorf("expected project morph, got %+v (err: %v)", res, err)
	}

	// 6. Config default_morph (when no project morph)
	res, err = ResolveMorph(MorphResolveOptions{
		WorkDir: tempDir, // tempDir does not have .codecuttle/morph.yaml
		Config:  cfg,
	})
	if err != nil || res.Source != "config" || res.Name != "config-morph" {
		t.Errorf("expected config morph, got %+v (err: %v)", res, err)
	}

	// 6b. Config default_morph "none"
	res, err = ResolveMorph(MorphResolveOptions{
		WorkDir: tempDir,
		Config:  Config{DefaultMorph: "none"},
	})
	if err != nil || res.Path != "" {
		t.Errorf("expected empty resolution for config 'none', got %+v", res)
	}
}

func TestDiscoverMorphs(t *testing.T) {
	tempDir := t.TempDir()
	workDir := filepath.Join(tempDir, "workspace")
	customMorphsDir := filepath.Join(tempDir, "custom_morphs")
	userDir := filepath.Join(tempDir, "user_morphs")

	origXDG := os.Getenv("XDG_CONFIG_HOME")
	defer os.Setenv("XDG_CONFIG_HOME", origXDG)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "xdg_config"))

	// Ensure directories exist
	projDir := filepath.Join(workDir, ".codecuttle", "morphs")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customMorphsDir, 0755); err != nil {
		t.Fatal(err)
	}
	realUserDir := UserMorphsDir()
	if err := os.MkdirAll(realUserDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = userDir

	// 1. Project morph .codecuttle/morph.yaml
	projMorphFile := filepath.Join(workDir, ".codecuttle", "morph.yaml")
	if err := os.WriteFile(projMorphFile, []byte(sampleMorphYAML("project-main")), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Project morph in .codecuttle/morphs/shared.yaml
	if err := os.WriteFile(filepath.Join(projDir, "shared.yaml"), []byte(sampleMorphYAML("shared-project")), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Custom morph in customMorphsDir/shared.yaml (should be SHADOWED by project shared)
	if err := os.WriteFile(filepath.Join(customMorphsDir, "shared.yaml"), []byte(sampleMorphYAML("shared-custom")), 0644); err != nil {
		t.Fatal(err)
	}

	// 4. Custom morph in customMorphsDir/unique-custom.yaml
	if err := os.WriteFile(filepath.Join(customMorphsDir, "unique-custom.yaml"), []byte(sampleMorphYAML("unique-custom")), 0644); err != nil {
		t.Fatal(err)
	}

	// 5. User morph in realUserDir/user-morph.yaml
	if err := os.WriteFile(filepath.Join(realUserDir, "user-morph.yaml"), []byte(sampleMorphYAML("user-morph")), 0644); err != nil {
		t.Fatal(err)
	}

	// 6. User morph in realUserDir/unique-custom.yaml (should be SHADOWED by custom unique-custom)
	if err := os.WriteFile(filepath.Join(realUserDir, "unique-custom.yaml"), []byte(sampleMorphYAML("unique-user")), 0644); err != nil {
		t.Fatal(err)
	}

	// Isolate system morphs dir from host /usr/local/share/codecuttlectl/morphs
	origSysMorphs := systemMorphsDir
	systemMorphsDir = filepath.Join(t.TempDir(), "system-morphs")
	t.Cleanup(func() { systemMorphsDir = origSysMorphs })

	cfg := Config{MorphsDir: customMorphsDir}
	discovered, err := DiscoverMorphs(workDir, cfg)
	if err != nil {
		t.Fatalf("unexpected error discovering morphs: %v", err)
	}

	type summary struct {
		Name     string
		Scope    string
		Shadowed bool
	}
	var summaries []summary
	for _, d := range discovered {
		summaries = append(summaries, summary{
			Name:     d.Name,
			Scope:    d.Scope,
			Shadowed: d.Shadowed,
		})
		if d.NodeCount != 1 || d.PrimaryNode != "lead" {
			t.Errorf("morph %s:%s unexpected inspection result: nodeCount=%d primary=%s", d.Scope, d.Name, d.NodeCount, d.PrimaryNode)
		}
	}

	expected := []summary{
		{Name: "morph", Scope: "project", Shadowed: false},
		{Name: "shared", Scope: "project", Shadowed: false},
		{Name: "shared", Scope: "custom", Shadowed: true},
		{Name: "unique-custom", Scope: "custom", Shadowed: false},
		{Name: "unique-custom", Scope: "user", Shadowed: true},
		{Name: "user-morph", Scope: "user", Shadowed: false},
	}

	if len(summaries) != len(expected) {
		t.Fatalf("expected %d morphs, got %d:\n%+v", len(expected), len(summaries), summaries)
	}
	for i, exp := range expected {
		if summaries[i] != exp {
			t.Errorf("at index %d: expected %+v, got %+v", i, exp, summaries[i])
		}
	}
}
