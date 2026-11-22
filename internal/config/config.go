package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Modzybear/codecuttlectl/internal/swarm"
)

// Config represents the user/system-wide codecuttlectl configuration.
type Config struct {
	PluginDir    string `yaml:"plugin_dir"`
	DefaultMorph string `yaml:"default_morph"`
	MorphsDir    string `yaml:"morphs_dir"`
}

// ConfigDir returns the XDG configuration directory for codecuttlectl.
func ConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "codecuttlectl")
	}
	return ExpandPath("~/.config/codecuttlectl")
}

// ConfigPath returns the path to the configuration file.
func ConfigPath() string {
	if env := os.Getenv("CODECUTTLE_CONFIG"); env != "" {
		return ExpandPath(env)
	}
	return filepath.Join(ConfigDir(), "config.yaml")
}

// UserMorphsDir returns the user-level morphologies directory.
func UserMorphsDir() string {
	return filepath.Join(ConfigDir(), "morphs")
}

var (
	systemMorphsDir  = "/usr/local/share/codecuttlectl/morphs"
	systemPluginsDir = "/usr/local/lib/codecuttlectl/plugins"
)

// SystemMorphsDir returns the system-wide morphologies directory.
func SystemMorphsDir() string {
	return systemMorphsDir
}

// UserPluginsDirs returns the ordered list of user-level plugin directories.
func UserPluginsDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "codecuttlectl", "plugins"))
	} else {
		dirs = append(dirs, ExpandPath("~/.local/share/codecuttlectl/plugins"))
	}
	dirs = append(dirs, ExpandPath("~/.local/lib/codecuttlectl/plugins"))
	return dirs
}

// SystemPluginsDir returns the system-wide plugin directory.
func SystemPluginsDir() string {
	return systemPluginsDir
}

// ExpandPath expands leading ~ or ~/ to the user's home directory.
func ExpandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
		if home := os.Getenv("HOME"); home != "" {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.Getenv("HOME")
		}
		if home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Load loads configuration from the default ConfigPath(). Missing file returns an empty Config.
func Load() (Config, error) {
	return LoadFrom(ConfigPath())
}

// LoadFrom loads configuration from the specified path. Missing file returns an empty Config.
// Malformed YAML returns an error wrapping the path.
func LoadFrom(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	expanded := ExpandPath(path)
	data, err := os.ReadFile(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, nil
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	return cfg, nil
}

// ResolvePluginDir resolves the effective plugin directory based on flag, env, config, or auto-detection.
func ResolvePluginDir(flagVal string, flagSet bool, cfg Config) string {
	if flagSet {
		if flagVal == "" || strings.EqualFold(flagVal, "none") {
			return ""
		}
		return ExpandPath(flagVal)
	}
	if envVal, ok := os.LookupEnv("CODECUTTLE_PLUGIN_DIR"); ok {
		if envVal == "" || strings.EqualFold(envVal, "none") {
			return ""
		}
		return ExpandPath(envVal)
	}
	if cfg.PluginDir != "" {
		if strings.EqualFold(cfg.PluginDir, "none") {
			return ""
		}
		return ExpandPath(cfg.PluginDir)
	}

	// Auto-detect first existing directory containing cuttlebone- executables
	candidates := append(UserPluginsDirs(), SystemPluginsDir())
	for _, dir := range candidates {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "cuttlebone-") {
				return dir
			}
		}
	}
	return ""
}

// MorphResolveOptions defines the inputs for morphology resolution.
type MorphResolveOptions struct {
	FlagValue               string
	FlagSet                 bool
	ExplicitModelOrProvider bool
	SessionMorph            string
	WorkDir                 string
	Config                  Config
}

// MorphResolution encapsulates the resolved morphology details and origin.
type MorphResolution struct {
	Path    string
	Name    string
	Source  string // "flag", "session", "env", "project", "config"
	Warning string
}

// LookupMorphPath resolves a morphology spec to a concrete file path and canonical name.
func LookupMorphPath(spec, workDir string, cfg Config) (string, string, error) {
	if spec == "" {
		return "", "", errors.New("empty morphology specification")
	}

	isPath := strings.ContainsAny(spec, "/\\") || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "~")

	// If not an explicit path indicator, check if the file directly exists on disk
	if !isPath {
		checkPath := spec
		if !filepath.IsAbs(checkPath) && workDir != "" {
			joined := filepath.Join(workDir, checkPath)
			if info, err := os.Stat(joined); err == nil && !info.IsDir() {
				checkPath = joined
				isPath = true
			}
		}
		if !isPath {
			if info, err := os.Stat(checkPath); err == nil && !info.IsDir() {
				isPath = true
			}
		}
		if isPath {
			spec = checkPath
		}
	}

	if isPath {
		expanded := ExpandPath(spec)
		target := expanded
		if !filepath.IsAbs(target) && workDir != "" {
			joined := filepath.Join(workDir, target)
			if info, err := os.Stat(joined); err == nil && !info.IsDir() {
				target = joined
			}
		}
		info, err := os.Stat(target)
		if err != nil || info.IsDir() {
			return "", "", fmt.Errorf("morphology file not found: %s", spec)
		}
		base := filepath.Base(target)
		ext := filepath.Ext(base)
		canonical := strings.TrimSuffix(base, ext)
		return target, canonical, nil
	}

	// Named lookup: reject path traversal
	if strings.Contains(spec, "..") {
		return "", "", fmt.Errorf("invalid morph name %q: path traversal not allowed", spec)
	}

	name := strings.TrimSuffix(strings.TrimSuffix(spec, ".yaml"), ".yml")
	var searchDirs []string
	if workDir != "" {
		searchDirs = append(searchDirs, filepath.Join(workDir, ".codecuttle", "morphs"))
	}
	if cfg.MorphsDir != "" {
		searchDirs = append(searchDirs, ExpandPath(cfg.MorphsDir))
	}
	searchDirs = append(searchDirs, UserMorphsDir())
	searchDirs = append(searchDirs, SystemMorphsDir())

	for _, dir := range searchDirs {
		for _, ext := range []string{".yaml", ".yml"} {
			cand := filepath.Join(dir, name+ext)
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				return cand, name, nil
			}
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("morphology %q not found; searched:", spec))
	for _, dir := range searchDirs {
		b.WriteString(fmt.Sprintf("\n  - %s", dir))
	}
	return "", "", errors.New(b.String())
}

// ResolveMorph resolves which morphology to use following the 6-tier precedence hierarchy.
func ResolveMorph(opts MorphResolveOptions) (MorphResolution, error) {
	// 1. Explicit CLI flag (-morph)
	if opts.FlagSet {
		if opts.FlagValue == "" || strings.EqualFold(opts.FlagValue, "none") {
			return MorphResolution{}, nil
		}
		p, name, err := LookupMorphPath(opts.FlagValue, opts.WorkDir, opts.Config)
		if err != nil {
			return MorphResolution{}, err
		}
		return MorphResolution{Path: p, Name: name, Source: "flag"}, nil
	}

	// 2. Explicit model or provider override disables ambient morph
	if opts.ExplicitModelOrProvider {
		return MorphResolution{}, nil
	}

	var warning string

	// 3. Resumed session morph
	if opts.SessionMorph != "" {
		if strings.EqualFold(opts.SessionMorph, "none") {
			// Session explicitly had no morph
			return MorphResolution{}, nil
		}
		p, name, err := LookupMorphPath(opts.SessionMorph, opts.WorkDir, opts.Config)
		if err == nil {
			return MorphResolution{Path: p, Name: name, Source: "session"}, nil
		}
		warning = fmt.Sprintf("session morphology %q not found (%v); falling back to default", opts.SessionMorph, err)
	}

	// 4. Environment variable CODECUTTLE_MORPH
	if envVal, ok := os.LookupEnv("CODECUTTLE_MORPH"); ok {
		if envVal == "" || strings.EqualFold(envVal, "none") {
			return MorphResolution{Warning: warning}, nil
		}
		p, name, err := LookupMorphPath(envVal, opts.WorkDir, opts.Config)
		if err != nil {
			return MorphResolution{}, err
		}
		return MorphResolution{Path: p, Name: name, Source: "env", Warning: warning}, nil
	}

	// 5. Project-local .codecuttle/morph.yaml or .yml
	if opts.WorkDir != "" {
		for _, ext := range []string{".yaml", ".yml"} {
			projMorph := filepath.Join(opts.WorkDir, ".codecuttle", "morph"+ext)
			if info, err := os.Stat(projMorph); err == nil && !info.IsDir() {
				return MorphResolution{Path: projMorph, Name: "morph", Source: "project", Warning: warning}, nil
			}
		}
	}

	// 6. User configuration default_morph
	if opts.Config.DefaultMorph != "" {
		if strings.EqualFold(opts.Config.DefaultMorph, "none") {
			return MorphResolution{Warning: warning}, nil
		}
		p, name, err := LookupMorphPath(opts.Config.DefaultMorph, opts.WorkDir, opts.Config)
		if err != nil {
			return MorphResolution{}, err
		}
		return MorphResolution{Path: p, Name: name, Source: "config", Warning: warning}, nil
	}

	return MorphResolution{Warning: warning}, nil
}

// DiscoveredMorph represents metadata about a morphology found in the search paths.
type DiscoveredMorph struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"` // "project", "custom", "user", "system"
	Path        string `json:"path"`
	Shadowed    bool   `json:"shadowed"`
	NodeCount   int    `json:"node_count"`
	PrimaryNode string `json:"primary_node"`
	ParseErr    string `json:"parse_err,omitempty"`
}

// DiscoverMorphs scans all morphology directories (and project morphs) in precedence order,
// marking duplicates with Shadowed: true.
func DiscoverMorphs(workDir string, cfg Config) ([]DiscoveredMorph, error) {
	type searchTarget struct {
		scope string
		dir   string
	}

	var targets []searchTarget
	if workDir != "" {
		targets = append(targets, searchTarget{scope: "project", dir: filepath.Join(workDir, ".codecuttle", "morphs")})
	}
	if cfg.MorphsDir != "" {
		targets = append(targets, searchTarget{scope: "custom", dir: ExpandPath(cfg.MorphsDir)})
	}
	targets = append(targets, searchTarget{scope: "user", dir: UserMorphsDir()})
	targets = append(targets, searchTarget{scope: "system", dir: SystemMorphsDir()})

	var results []DiscoveredMorph
	seenNames := make(map[string]bool)

	// Check .codecuttle/morph.yaml / .yml project-level morph first
	if workDir != "" {
		for _, ext := range []string{".yaml", ".yml"} {
			projectFile := filepath.Join(workDir, ".codecuttle", "morph"+ext)
			if info, err := os.Stat(projectFile); err == nil && !info.IsDir() {
				dm := DiscoveredMorph{
					Name:     "morph",
					Scope:    "project",
					Path:     projectFile,
					Shadowed: seenNames["morph"],
				}
				seenNames["morph"] = true
				if m, err := swarm.LoadMorphology(projectFile); err != nil {
					dm.ParseErr = err.Error()
				} else {
					dm.NodeCount = len(m.Nodes)
					for nodeID, node := range m.Nodes {
						if node.IsPrimary {
							dm.PrimaryNode = nodeID
							break
						}
					}
				}
				results = append(results, dm)
				break // only take one of .yaml / .yml
			}
		}
	}

	for _, target := range targets {
		info, err := os.Stat(target.dir)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(target.dir)
		if err != nil {
			continue
		}
		// Sort entries for deterministic output
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})

		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext != ".yaml" && ext != ".yml" {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ext)
			fullPath := filepath.Join(target.dir, e.Name())

			shadowed := seenNames[name]
			seenNames[name] = true

			dm := DiscoveredMorph{
				Name:     name,
				Scope:    target.scope,
				Path:     fullPath,
				Shadowed: shadowed,
			}

			if m, err := swarm.LoadMorphology(fullPath); err != nil {
				dm.ParseErr = err.Error()
			} else {
				dm.NodeCount = len(m.Nodes)
				for nodeID, node := range m.Nodes {
					if node.IsPrimary {
						dm.PrimaryNode = nodeID
						break
					}
				}
			}
			results = append(results, dm)
		}
	}

	return results, nil
}
