// codecuttlectl is the Codecuttle meta-harness CLI agent.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Modzybear/codecuttlectl/internal/audit"
	"github.com/Modzybear/codecuttlectl/internal/bedrock"
	"github.com/Modzybear/codecuttlectl/internal/config"
	"github.com/Modzybear/codecuttlectl/internal/conversation"
	"github.com/Modzybear/codecuttlectl/internal/keyring"
	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/prompt"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	bedrockprov "github.com/Modzybear/codecuttlectl/internal/provider/bedrock"
	googleprov "github.com/Modzybear/codecuttlectl/internal/provider/google"
	"github.com/Modzybear/codecuttlectl/internal/provider/ollama"
	"github.com/Modzybear/codecuttlectl/internal/provider/openrouter"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/swarm"
	"github.com/Modzybear/codecuttlectl/internal/tui"
)

func main() {
	var (
		modelID              = flag.String("model", "us.anthropic.claude-opus-4-6-v1", "Bedrock model ID or alias (opus, sonnet, haiku, or model name when --provider is set)")
		auxModel             = flag.String("aux-model", "", "Auxiliary model for summaries/titles (transitional, will be superseded by --morph)")
		planModel            = flag.String("plan-model", "", "Planning model for mid-tier tasks (transitional, will be superseded by --morph)")
		region               = flag.String("region", "", "AWS region (default: AWS_REGION env or us-west-2)")
		profile              = flag.String("profile", "", "AWS profile name")
		providerF            = flag.String("provider", "", "LLM provider: 'bedrock' (default), 'google', 'ollama', or 'openrouter'")
		ollamaURL            = flag.String("ollama-url", "", "Ollama server URL (default: http://localhost:11434)")
		openrouterURL        = flag.String("openrouter-url", "", "OpenRouter server URL (default: https://openrouter.ai/api/v1)")
		openrouterZDR        = flag.Bool("openrouter-zdr", true, "Enable Zero Data Retention (ZDR) on OpenRouter requests (default true, use --openrouter-zdr=false to disable)")
		openrouterFallbacks  = flag.String("openrouter-fallbacks", "", "Comma-separated list of fallback models for OpenRouter")
		googleCacheThreshold = flag.Int("google-cache-threshold", 32000, "Token threshold to trigger Google Context Caching API (default 32000)")
		morphPath            = flag.String("morph", "", "Swarm morphology name, path, or 'none' (overrides model/provider flags)")
		workDir              = flag.String("workdir", "", "Working directory (default: current directory)")
		pluginDir            = flag.String("plugin-dir", "", "Directory containing Cuttlebone plugin binaries, or 'none' to disable auto-discovery")
		verbose              = flag.Bool("verbose", false, "Enable verbose/debug output")
		maxSteps             = flag.Int("max-steps", 0, "Maximum tool-use iterations per turn (0 = unlimited)")
		thinking             = flag.Bool("thinking", false, "Enable extended thinking/reasoning (model must support it)")
		oneShot              = flag.String("message", "", "Single message mode: send this message and exit (no TUI)")
		noTUI                = flag.Bool("no-tui", false, "Disable TUI, use plain REPL mode")

		// Session management
		sessionID      = flag.String("session", "", "Resume an existing session by ID or alias")
		resumeFlag     = flag.String("r", "", "Resume an existing session by ID or alias (shorthand for --session)")
		sessionsShort  = flag.Bool("s", false, "Open interactive session picker (falls back to --list-sessions in non-TTY)")
		sessionsLong   = flag.Bool("sessions", false, "Open interactive session picker (falls back to --list-sessions in non-TTY)")
		listSessions   = flag.Bool("list-sessions", false, "Show recent sessions and exit")
		listPinned     = flag.Bool("pinned", false, "Filter --list-sessions to pinned sessions only")
		searchSessions = flag.String("search", "", "Filter --list-sessions by substring (ID, alias, title, workdir, model, morph, tags)")
		jsonOutput     = flag.Bool("json", false, "Output machine-readable JSON (for --list-sessions and --list-morphs)")
		listMorphs     = flag.Bool("list-morphs", false, "List discoverable swarm morphologies across project, user, and system directories")
		listModels     = flag.Bool("list-models", false, "List available models for the selected provider and exit")
		sessionLimit   = flag.Int("session-limit", 20, "Number of sessions to show in list")
		pruneSessions  = flag.Int("prune-sessions", 0, "Delete sessions older than N days and exit")

		// Safety
		autoApprove = flag.Bool("auto-approve", false, "Skip confirmation prompts for destructive operations (use in automated pipelines)")
		auditLog    = flag.Bool("audit-log", false, "Emit structured JSON audit events to stderr")

		// Epoch Rollover Compaction
		autoRollover    = flag.Bool("auto-rollover", false, "Automatically checkpoint and roll over to a fresh epoch when context reaches threshold")
		rolloverSoftPct = flag.Float64("rollover-soft-pct", 0.45, "Soft context utilization threshold (0.0-1.0) to arm rollover and wait for a quiescent boundary")
		rolloverHardPct = flag.Float64("rollover-hard-pct", 0.65, "Hard context utilization threshold (0.0-1.0) to force closeout after the current tool round")
		rolloverDocs    = flag.String("rollover-docs", "none", "Project doc update policy on rollover: 'none', 'milestone', or 'always'")
		maxEpochs       = flag.Int("max-epochs", 0, "Maximum epochs allowed for automatic rollover (0 = unlimited)")
		maxCost         = flag.Float64("max-cost", 0, "Maximum cumulative session cost in USD before pausing automatic rollover (0 = unlimited)")
	)
	flag.Parse()

	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	userCfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", cfgErr)
		os.Exit(1)
	}

	docsPolicy, err := rollover.ParseDocsPolicy(*rolloverDocs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	rolloverCfg := rollover.DefaultConfig()
	rolloverCfg.AutoRollover = *autoRollover
	rolloverCfg.SoftPct = *rolloverSoftPct
	rolloverCfg.HardPct = *rolloverHardPct
	rolloverCfg.DocsPolicy = docsPolicy
	rolloverCfg.MaxEpochs = *maxEpochs
	rolloverCfg.MaxCostUSD = *maxCost
	if err := rollover.ValidateConfig(rolloverCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid rollover configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize audit logger
	auditLogger := audit.NewLogger(os.Stderr, *auditLog)

	// Resolve working directory
	if *workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
			os.Exit(1)
		}
		*workDir = wd
	} else {
		expanded := config.ExpandPath(*workDir)
		if absWD, err := filepath.Abs(expanded); err == nil {
			*workDir = absWD
		} else {
			*workDir = expanded
		}
	}

	// Resolve region from env if not specified
	if *region == "" {
		*region = os.Getenv("AWS_REGION")
		if *region == "" {
			*region = os.Getenv("AWS_DEFAULT_REGION")
		}
	}

	// Resolve profile from env if not specified
	if *profile == "" {
		*profile = os.Getenv("AWS_PROFILE")
	}

	// Initialize session store
	store, err := session.NewFileStore(session.DefaultDataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing session store: %v\n", err)
		os.Exit(1)
	}

	// Early-exit commands: --list-morphs
	if *listMorphs {
		runListMorphs(*workDir, userCfg, *jsonOutput)
		return
	}

	// Early-exit commands: --list-sessions
	if *listSessions {
		runListSessions(store, *sessionLimit, session.ListFilter{PinnedOnly: *listPinned, Query: *searchSessions}, *jsonOutput)
		return
	}

	// Early-exit commands: --list-models
	if *listModels {
		runListModels(*providerF, *modelID)
		return
	}

	// Early-exit commands: --prune-sessions
	if *pruneSessions > 0 {
		runPruneSessions(store, *pruneSessions)
		return
	}

	// Early-exit / session picker: -s / --sessions
	if *sessionsShort || *sessionsLong {
		if isTTY(os.Stdin) && isTTY(os.Stdout) {
			res, err := tui.RunSessionPicker(store, *workDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error running session picker: %v\n", err)
				os.Exit(1)
			}
			switch res.Action {
			case tui.PickerActionQuit:
				return
			case tui.PickerActionResume:
				*sessionID = res.SessionID
			case tui.PickerActionNew:
				*sessionID = ""
			}
		} else {
			runListSessions(store, *sessionLimit, session.ListFilter{PinnedOnly: *listPinned, Query: *searchSessions}, *jsonOutput)
			return
		}
	}

	// Normalize -r / --session
	if *sessionID == "" && *resumeFlag != "" {
		*sessionID = *resumeFlag
	}

	var resumedSessionMorph string
	if *sessionID != "" {
		meta, err := store.ResolveMeta(*sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving session %q: %v\n", *sessionID, err)
			os.Exit(1)
		}
		*sessionID = meta.ID
		resumedSessionMorph = meta.Morph
		if !explicitFlags["workdir"] && meta.WorkDir != "" {
			if info, statErr := os.Stat(meta.WorkDir); statErr == nil && info.IsDir() {
				*workDir = meta.WorkDir
				_ = os.Chdir(meta.WorkDir)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: session workdir %q not found; staying in %s\n", meta.WorkDir, *workDir)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Initialize LLM provider
	var llmProvider provider.Provider
	var bedrockClient *bedrock.Client // Non-nil only for Bedrock (needed for Bedrock-specific features)
	var genericPool provider.Pool
	var morph *swarm.Morphology
	var sessionMorphSpec string
	providerName := *providerF

	morphRes, err := config.ResolveMorph(config.MorphResolveOptions{
		FlagValue:               *morphPath,
		FlagSet:                 explicitFlags["morph"],
		ExplicitModelOrProvider: explicitFlags["provider"] || explicitFlags["model"],
		SessionMorph:            resumedSessionMorph,
		WorkDir:                 *workDir,
		Config:                  userCfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving morphology: %v\n", err)
		os.Exit(1)
	}
	if morphRes.Warning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", morphRes.Warning)
	}

	if morphRes.Path != "" {
		var loadErr error
		morph, loadErr = swarm.LoadMorphology(morphRes.Path)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Error loading morphology: %v\n", loadErr)
			os.Exit(1)
		}
		sessionMorphSpec = computeSessionMorphSpec(morphRes, *workDir, userCfg)

		factory := func(ctx context.Context, nodeConfig swarm.Node) (provider.Provider, error) {
			provName := nodeConfig.Provider
			modID := nodeConfig.Model
			switch provName {
			case "ollama":
				return ollama.New(ollama.Config{
					BaseURL: *ollamaURL,
					Model:   modID,
				}), nil
			case "openrouter":
				if err := keyring.EnsureOpenRouterAPIKey(); err != nil {
					return nil, fmt.Errorf("failed to ensure OpenRouter API key: %w", err)
				}
				var fallbacks []string
				if *openrouterFallbacks != "" {
					for _, f := range strings.Split(*openrouterFallbacks, ",") {
						fallbacks = append(fallbacks, strings.TrimSpace(f))
					}
				}
				return openrouter.New(openrouter.Config{
					BaseURL:    *openrouterURL,
					Model:      modID,
					Fallbacks:  fallbacks,
					EnforceZDR: *openrouterZDR,
					APIKey:     os.Getenv("OPENROUTER_API_KEY"),
				}), nil
			case "google":
				if err := keyring.EnsureGeminiAPIKey(); err != nil {
					return nil, fmt.Errorf("failed to ensure Gemini API key: %w", err)
				}
				client, err := googleprov.New(ctx, googleprov.Config{
					Model:          modID,
					CacheThreshold: *googleCacheThreshold,
				})
				return client, err
			case "bedrock":
				resolved := bedrock.ResolveModelID(modID)
				c, err := bedrock.NewClient(ctx, bedrock.Config{
					Region:  *region,
					Profile: *profile,
					ModelID: resolved,
				})
				if err != nil {
					return nil, err
				}
				return bedrockprov.New(c), nil
			default:
				return nil, fmt.Errorf("unsupported provider: %s", provName)
			}
		}

		pool, err := swarm.NewPool(ctx, morph, factory)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error initializing swarm pool: %v\n", err)
			os.Exit(1)
		}

		genericPool = pool
		llmProvider = pool.Primary()

		if *verbose {
			fmt.Fprintf(os.Stderr, "[swarm] morphology loaded: %s (%s)\n", morph.Name, morph.Presentation)
			fmt.Fprintf(os.Stderr, "[swarm] primary node=%s\n", pool.Info("primary").DisplayName)
		}
	} else {
		// Auto-detect provider from model name prefix (e.g., "ollama:gemma4")
		if providerName == "" && strings.HasPrefix(*modelID, "ollama:") {
			providerName = "ollama"
			*modelID = strings.TrimPrefix(*modelID, "ollama:")
		} else if providerName == "" && strings.HasPrefix(*modelID, "openrouter:") {
			providerName = "openrouter"
			*modelID = strings.TrimPrefix(*modelID, "openrouter:")
		}
		if providerName == "" {
			providerName = "bedrock"
		}

		switch providerName {
		case "ollama":
			ollamaClient := ollama.New(ollama.Config{
				BaseURL: *ollamaURL,
				Model:   *modelID,
			})
			llmProvider = ollamaClient
			if *verbose {
				fmt.Fprintf(os.Stderr, "[provider] ollama model=%s url=%s\n", *modelID, ollamaClient.ID())
			}

		case "openrouter":
			if err := keyring.EnsureOpenRouterAPIKey(); err != nil {
				fmt.Fprintf(os.Stderr, "Error ensuring OpenRouter API key: %v\n", err)
				os.Exit(1)
			}
			var fallbacks []string
			if *openrouterFallbacks != "" {
				for _, f := range strings.Split(*openrouterFallbacks, ",") {
					fallbacks = append(fallbacks, strings.TrimSpace(f))
				}
			}
			orClient := openrouter.New(openrouter.Config{
				BaseURL:    *openrouterURL,
				Model:      *modelID,
				Fallbacks:  fallbacks,
				EnforceZDR: *openrouterZDR,
				APIKey:     os.Getenv("OPENROUTER_API_KEY"),
			})
			llmProvider = orClient
			if *verbose {
				fmt.Fprintf(os.Stderr, "[provider] openrouter model=%s fallbacks=%v zdr=%v\n", *modelID, fallbacks, *openrouterZDR)
			}

		case "google":
			if err := keyring.EnsureGeminiAPIKey(); err != nil {
				fmt.Fprintf(os.Stderr, "Error ensuring Gemini API key: %v\n", err)
				os.Exit(1)
			}
			googleClient, err := googleprov.New(ctx, googleprov.Config{
				Model:          *modelID,
				CacheThreshold: *googleCacheThreshold,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error initializing Google client: %v\n", err)
				os.Exit(1)
			}
			llmProvider = googleClient
			// Ensure cache cleanup on shutdown
			defer googleClient.Close(context.Background())
			if *verbose {
				fmt.Fprintf(os.Stderr, "[provider] google model=%s cache-threshold=%d\n", *modelID, *googleCacheThreshold)
			}

		case "bedrock":
			resolvedModel := bedrock.ResolveModelID(*modelID)
			pool, err := bedrock.NewPool(ctx, bedrock.PoolConfig{
				Region:    *region,
				Profile:   *profile,
				Primary:   resolvedModel,
				Auxiliary: *auxModel,
				Planning:  *planModel,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error initializing Bedrock model pool: %v\n", err)
				os.Exit(1)
			}
			genericPool = bedrockprov.NewPool(pool)
			bedrockClient = pool.Primary()
			llmProvider = nil // TUI/Agent still use bedrockClient directly for now
			if *verbose {
				fmt.Fprintf(os.Stderr, "[pool] primary=%s auxiliary=%s planning=%s\n",
					genericPool.Info(string(bedrock.RolePrimary)).DisplayName,
					genericPool.Info(string(bedrock.RoleAuxiliary)).DisplayName,
					genericPool.Info(string(bedrock.RolePlanning)).DisplayName,
				)
			}

		default:
			fmt.Fprintf(os.Stderr, "Unknown provider: %s (supported: bedrock, google, ollama, openrouter)\n", providerName)
			os.Exit(1)
		}
	}

	// Initialize prompt manager
	promptMgr, err := prompt.NewManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing prompt manager: %v\n", err)
		os.Exit(1)
	}

	// Initialize plugin manager and discover plugins
	pluginMgr := pluginhost.NewManager(*verbose)
	defer pluginMgr.Shutdown()

	resolvedPluginDir := config.ResolvePluginDir(*pluginDir, explicitFlags["plugin-dir"], userCfg)
	if resolvedPluginDir != "" {
		if err := pluginMgr.DiscoverPlugins(ctx, resolvedPluginDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error discovering plugins: %v\n", err)
			os.Exit(1)
		}
	}

	// Build system prompt
	var promptTools []prompt.ToolDef
	for _, def := range pluginMgr.Definitions() {
		promptTools = append(promptTools, prompt.ToolDef{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  prompt.SchemaToToolParams(def.InputSchema),
		})
	}
	for _, def := range conversation.BuiltinToolDefs(morph) {
		promptTools = append(promptTools, prompt.ToolDef{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  prompt.SchemaToToolParams(def.InputSchema),
		})
	}
	modelDisplayID := *modelID
	if bedrockClient != nil {
		modelDisplayID = bedrockClient.ModelID()
	} else if llmProvider != nil {
		modelDisplayID = llmProvider.Name()
	}
	// Render the primary system prompt without swarm nodes (REPL mode is single-agent)
	systemPrompt, err := promptMgr.RenderSystem(*workDir, modelDisplayID, providerName, promptTools, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error rendering system prompt: %v\n", err)
		os.Exit(1)
	}
	// Append LLM context hints from plugins
	if hints := pluginMgr.LLMHints(); hints != "" {
		systemPrompt += "\n\n## Additional Tool Guidance\n" + hints
	}

	// One-shot mode: no TUI, just print result to stdout
	if *oneShot != "" {
		runOneShot(ctx, bedrockClient, genericPool, llmProvider, pluginMgr, promptMgr, store, *sessionID, systemPrompt, *workDir, *maxSteps, *verbose, *autoApprove, auditLogger, *oneShot, morph, sessionMorphSpec, &rolloverCfg)
		return
	}

	// Interactive mode
	if *noTUI {
		// Initialize the Agent for REPL
		agent, err := conversation.NewAgent(conversation.Config{
			Client:         bedrockClient,
			Pool:           genericPool,
			Provider:       llmProvider,
			Morph:          morph,
			MorphName:      sessionMorphSpec,
			PromptMgr:      promptMgr,
			PluginMgr:      pluginMgr,
			WorkDir:        *workDir,
			MaxSteps:       *maxSteps,
			Verbose:        *verbose,
			AutoApprove:    *autoApprove,
			AuditLogger:    auditLogger,
			Store:          store,
			SessionID:      *sessionID,
			RolloverConfig: &rolloverCfg,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error initializing agent: %v\n", err)
			os.Exit(1)
		}
		if morph == nil {
			agent.SetSystemPrompt(systemPrompt)
		}
		runPlainREPL(ctx, agent, store, *sessionID, *workDir, *verbose, *autoApprove, auditLogger)
		return
	}

	// Initialize the Agent for TUI
	agent, err := conversation.NewAgent(conversation.Config{
		Client:         bedrockClient,
		Pool:           genericPool,
		Provider:       llmProvider,
		Morph:          morph,
		MorphName:      sessionMorphSpec,
		PromptMgr:      promptMgr,
		PluginMgr:      pluginMgr,
		WorkDir:        *workDir,
		MaxSteps:       *maxSteps,
		Verbose:        *verbose,
		AutoApprove:    *autoApprove,
		AuditLogger:    auditLogger,
		Store:          store,
		SessionID:      *sessionID,
		RolloverConfig: &rolloverCfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing agent: %v\n", err)
		os.Exit(1)
	}
	tuiSystem := systemPrompt
	if morph != nil {
		tuiSystem = agent.SystemPrompt()
	} else {
		agent.SetSystemPrompt(systemPrompt)
	}
	engine := conversation.NewEngine(agent)

	// Full-screen TUI mode
	model := tui.New(tui.Config{
		Client:         bedrockClient,
		Pool:           genericPool,
		Provider:       llmProvider,
		PluginMgr:      pluginMgr,
		System:         tuiSystem,
		WorkDir:        *workDir,
		Verbose:        *verbose,
		EnableThinking: *thinking,
		AutoApprove:    *autoApprove,
		Store:          store,
		SessionID:      *sessionID,
		Morph:          morph,
		MorphName:      sessionMorphSpec,
		Agent:          agent,
		Engine:         engine,
		MaxSteps:       *maxSteps,
		RolloverConfig: &rolloverCfg,
	})

	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}

	// Print session resume info on exit
	if fm, ok := finalModel.(tui.Model); ok {
		printSessionExit(store, fm.SessionID())
	}
}

// runOneShot executes a single message and exits (non-TUI, for scripting).
func runOneShot(ctx context.Context, client *bedrock.Client, pool provider.Pool, llmProvider provider.Provider, pluginMgr *pluginhost.Manager, promptMgr *prompt.Manager, store session.Store, sessionID, system, workDir string, maxSteps int, verbose, autoApprove bool, auditLogger *audit.Logger, message string, morph *swarm.Morphology, morphName string, rolloverCfg *rollover.Config) {
	agent, err := conversation.NewAgent(conversation.Config{
		Client:         client,
		Pool:           pool,
		Provider:       llmProvider,
		Morph:          morph,
		MorphName:      morphName,
		PromptMgr:      promptMgr,
		PluginMgr:      pluginMgr,
		WorkDir:        workDir,
		MaxSteps:       maxSteps,
		Verbose:        verbose,
		AutoApprove:    autoApprove,
		AuditLogger:    auditLogger,
		Store:          store,
		SessionID:      sessionID,
		RolloverConfig: rolloverCfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing agent: %v\n", err)
		os.Exit(1)
	}
	if morph == nil {
		agent.SetSystemPrompt(system)
	}
	engine := conversation.NewEngine(agent)

	// Create a new session if not resuming
	if sessionID == "" {
		modelID := ""
		region := ""
		if client != nil {
			modelID = client.ModelID()
			region = client.Region()
		} else if llmProvider != nil {
			modelID = llmProvider.ID()
		}
		id, err := agent.InitSession(modelID, region, workDir)
		if err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: session creation failed: %v\n", err)
		}
		if verbose && id != "" {
			fmt.Fprintf(os.Stderr, "[session] %s\n", id)
		}
	}

	response, err := engine.Agent().Turn(ctx, message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Generate title after first turn
	if agent.SessionID() != "" {
		title := agent.GenerateTitle(ctx)
		if title != "" && store != nil {
			state, loadErr := store.Load(agent.SessionID())
			if loadErr == nil {
				state.Meta.Title = title
				store.Save(agent.SessionID(), state)
			}
		}
	}

	fmt.Println(response)
	printSessionExit(store, agent.SessionID())
}

func runPlainREPL(ctx context.Context, agent *conversation.Agent, store session.Store, sessionID, workDir string, verbose, autoApprove bool, auditLogger *audit.Logger) {
	// In plain REPL mode, we can prompt the user for approval via stdin
	approvalFunc := func(toolName, command, reason, risk string) bool {
		fmt.Fprintf(os.Stderr, "\n⚠️  DESTRUCTIVE OPERATION DETECTED (risk: %s)\n", risk)
		fmt.Fprintf(os.Stderr, "   Tool: %s\n", toolName)
		fmt.Fprintf(os.Stderr, "   Command: %s\n", command)
		fmt.Fprintf(os.Stderr, "   Reason: %s\n", reason)
		fmt.Fprintf(os.Stderr, "\n   Allow this operation? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		return answer == "y" || answer == "yes"
	}

	// We already have an agent, just set the approval func
	agent.SetApprovalFunc(approvalFunc)

	// Create a new session if not resuming
	if sessionID == "" {
		id, _ := agent.InitSession("", "", workDir) // IDs are handled internally if needed
		if id != "" {
			fmt.Printf("Session: %s\n", id)
		}
	} else {
		fmt.Printf("Resuming session: %s\n", sessionID)
	}

	fmt.Println("codecuttlectl - Codecuttle Meta-Harness Agent (plain mode)")
	fmt.Println("Type your message and press Enter. Type 'exit' or Ctrl+C to quit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	firstTurn := true
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		if strings.HasPrefix(input, "/") {
			fields := strings.Fields(input)
			switch strings.ToLower(fields[0]) {
			case "/help":
				fmt.Println("Available commands:")
				fmt.Println("  /rollover [--docs|--no-docs] [hint...]   Checkpoint current epoch and start a fresh context window")
				fmt.Println("  /epochs                                  List completed epoch checkpoints")
				fmt.Println("  /help                                    Show this help")
				fmt.Println("  exit | quit                              Exit session")
				continue
			case "/epochs":
				fmt.Println(rollover.FormatEpochsTable(agent.CurrentEpoch(), agent.Epochs()))
				continue
			case "/rollover":
				docsOverride, hint, err := tui.ParseRolloverCommandArgs(fields[1:])
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\nUsage: /rollover [--docs|--no-docs] [hint...]\n", err)
					continue
				}
				policy := rollover.DocsPolicyNone
				if docsOverride != nil {
					policy = *docsOverride
				}
				fmt.Fprintf(os.Stderr, "↻ Rolling over Epoch %d → Epoch %d...\n", agent.CurrentEpoch(), agent.CurrentEpoch()+1)
				cp, err := agent.Rollover(ctx, policy, hint)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Rollover failed: %v\n", err)
					continue
				}
				fmt.Printf("✓ Epoch %d archived (%s). Now in Epoch %d (%d priority files).\n", cp.Epoch, cp.ArchiveFile, agent.CurrentEpoch(), len(cp.Payload.KeyFiles))
				continue
			}
		}

		response, err := agent.StreamTurn(ctx, input, func(ev conversation.StreamEvent) {
			switch ev.Type {
			case "text":
				fmt.Print(ev.Text)
			case "tool_start":
				fmt.Fprintf(os.Stderr, "\n  [tool] %s\n", ev.ToolName)
			case "tool_done":
				fmt.Fprintf(os.Stderr, "  [done] %s\n", ev.ToolName)
			case "done":
				fmt.Println()
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
			continue
		}
		_ = response // already printed via streaming

		// Generate title after first turn
		if firstTurn && agent.SessionID() != "" {
			firstTurn = false
			go func() {
				title := agent.GenerateTitle(context.Background())
				if title != "" && store != nil {
					state, loadErr := store.Load(agent.SessionID())
					if loadErr == nil {
						state.Meta.Title = title
						store.Save(agent.SessionID(), state)
					}
				}
			}()
		}

		fmt.Println()
	}

	printSessionExit(store, agent.SessionID())
}

// isTTY reports whether the given file is an interactive terminal character device.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// formatShortWorkDir replaces $HOME with ~ in a directory path for compact display.
func formatShortWorkDir(dir string) string {
	if dir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(filepath.Separator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// computeSessionMorphSpec determines the canonical morphology identifier for session persistence.
// If the morphology resolves to a named search path in the project, user, or system directory,
// it preserves the portable short name; otherwise it returns the absolute file path.
func computeSessionMorphSpec(morphRes config.MorphResolution, workDir string, userCfg config.Config) string {
	if morphRes.Path == "" {
		return ""
	}
	targetAbs, err := filepath.Abs(morphRes.Path)
	if err != nil {
		targetAbs = morphRes.Path
	}
	if morphRes.Name != "" {
		if lookupPath, _, err := config.LookupMorphPath(morphRes.Name, workDir, userCfg); err == nil {
			lookupAbs, err := filepath.Abs(lookupPath)
			if err == nil && filepath.Clean(lookupAbs) == filepath.Clean(targetAbs) {
				return morphRes.Name
			}
		}
	}
	return targetAbs
}

// runListMorphs lists discoverable swarm morphologies.
func runListMorphs(workDir string, userCfg config.Config, jsonOutput bool) {
	if err := printMorphsList(os.Stdout, workDir, userCfg, jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "Error listing morphologies: %v\n", err)
		os.Exit(1)
	}
}

// printMorphsList outputs the morphologies list as JSON or formatted table to w.
func printMorphsList(w io.Writer, workDir string, userCfg config.Config, jsonOutput bool) error {
	morphs, err := config.DiscoverMorphs(workDir, userCfg)
	if err != nil {
		return err
	}
	if jsonOutput {
		if morphs == nil {
			morphs = []config.DiscoveredMorph{}
		}
		data, err := json.MarshalIndent(morphs, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(data))
		return nil
	}

	if len(morphs) == 0 {
		fmt.Fprintln(w, "No morphologies found.")
		return nil
	}

	activeRes, _ := config.ResolveMorph(config.MorphResolveOptions{WorkDir: workDir, Config: userCfg})

	nameWidth := 16
	for _, m := range morphs {
		if len(m.Name) > nameWidth {
			nameWidth = len(m.Name)
		}
	}
	rowFmt := fmt.Sprintf("%%-8s  %%-%ds  %%5s  %%-12s  %%-16s  %%s\n", nameWidth)
	fmt.Fprintf(w, rowFmt, "SCOPE", "NAME", "NODES", "PRIMARY", "STATUS", "PATH")
	fmt.Fprintln(w, strings.Repeat("-", 74+nameWidth))

	for _, m := range morphs {
		nodeStr := fmt.Sprintf("%d", m.NodeCount)
		primaryStr := m.PrimaryNode
		status := "-"
		if m.ParseErr != "" {
			status = "invalid"
			nodeStr = "-"
			primaryStr = "-"
		} else if m.Shadowed {
			status = "(shadowed)"
		} else if activeRes.Path != "" && filepath.Clean(m.Path) == filepath.Clean(activeRes.Path) {
			status = "active (default)"
		}

		fmt.Fprintf(w, rowFmt,
			m.Scope, m.Name, nodeStr, primaryStr, status, m.Path)
	}
	return nil
}

// runListSessions prints recent sessions in a table format or JSON.
func runListSessions(store session.Store, limit int, filter session.ListFilter, jsonOutput bool) {
	if err := printSessionsList(os.Stdout, store, limit, filter, jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "Error listing sessions: %v\n", err)
		os.Exit(1)
	}
}

// printSessionsList outputs sessions filtered by ListFilter to w.
func printSessionsList(w io.Writer, store session.Store, limit int, filter session.ListFilter, jsonOutput bool) error {
	metas, err := store.List(0)
	if err != nil {
		return err
	}

	metas = session.FilterSessions(metas, filter)
	if limit > 0 && len(metas) > limit {
		metas = metas[:limit]
	}

	if jsonOutput {
		if metas == nil {
			metas = []session.SessionMeta{}
		}
		data, err := json.MarshalIndent(metas, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(data))
		return nil
	}

	if len(metas) == 0 {
		fmt.Fprintln(w, "No sessions found.")
		return nil
	}

	fmt.Fprintf(w, "%-3s  %-24s  %-24s  %-5s  %-20s  %-18s  %7s  %s\n",
		"PIN", "ALIAS / ID", "TITLE", "EPOCH", "WORKDIR", "MODEL", "COST", "AGE")
	fmt.Fprintln(w, strings.Repeat("-", 120))

	for _, m := range metas {
		pin := " "
		if m.Pinned {
			pin = "★"
		}
		idStr := m.ID
		if m.Alias != "" {
			idStr = fmt.Sprintf("%s (%s)", m.Alias, m.ID)
		}
		idStr = truncateDisplay(idStr, 24)
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		title = truncateDisplay(title, 24)
		epochStr := fmt.Sprintf("E%d", m.EffectiveEpoch())
		wd := truncateDisplay(formatShortWorkDir(m.WorkDir), 20)
		model := truncateDisplay(m.Model, 18)
		cost := fmt.Sprintf("$%.2f", m.Stats.EstimatedCostUSD)
		age := formatAge(m.UpdatedAt)

		fmt.Fprintf(w, "%-3s  %-24s  %-24s  %-5s  %-20s  %-18s  %7s  %s\n",
			pin, idStr, title, epochStr, wd, model, cost, age)
	}
	return nil
}

// truncateDisplay truncates a string by UTF-8 rune count (never splitting multi-byte runes).
func truncateDisplay(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 2 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-2]) + ".."
}

// runPruneSessions removes old sessions, preserving pinned sessions.
func runPruneSessions(store session.Store, days int) {
	maxAge := time.Duration(days) * 24 * time.Hour
	deleted, err := store.Prune(maxAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error pruning sessions: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Pruned %d session(s) older than %d days (pinned sessions preserved).\n", deleted, days)
}

// runListModels lists available models for the given provider.
func runListModels(providerName, currentModel string) {
	// Resolve provider name
	if providerName == "" {
		if strings.HasPrefix(currentModel, "ollama:") {
			providerName = "ollama"
		} else {
			providerName = "bedrock"
		}
	}

	switch providerName {
	case "google":
		runListGoogleModels()
	case "ollama":
		fmt.Println("Ollama models — use 'ollama list' to see locally available models.")
	case "openrouter":
		fmt.Println("OpenRouter models — visit https://openrouter.ai/models for a full list.")
	case "bedrock":
		fmt.Println("AWS Bedrock models (common Anthropic models):")
		fmt.Println("  us.anthropic.claude-opus-4-6-v1          (Claude Opus 4)")
		fmt.Println("  us.anthropic.claude-sonnet-4-20250514-v1:0 (Claude Sonnet 4)")
		fmt.Println("  us.anthropic.claude-3-7-sonnet-20250219-v1:0 (Claude 3.7 Sonnet)")
		fmt.Println("  us.anthropic.claude-3-5-haiku-20241022-v1:0  (Claude 3.5 Haiku)")
		fmt.Println("\n  Use AWS CLI: aws bedrock list-foundation-models --region us-west-2")
	default:
		fmt.Fprintf(os.Stderr, "Unknown provider: %s\n", providerName)
		os.Exit(1)
	}
}

func runListGoogleModels() {
	if err := keyring.EnsureGeminiAPIKey(); err != nil {
		fmt.Fprintf(os.Stderr, "Error ensuring Gemini API key: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := googleprov.NewClientForListing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to Google AI: %v\n", err)
		os.Exit(1)
	}

	models, err := googleprov.ListModels(ctx, client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing models: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Available Google AI models:")
	fmt.Println()
	for _, m := range models {
		fmt.Printf("  %s\n", m)
	}

	// Show aliases
	aliases := googleprov.ModelAliases()
	if len(aliases) > 0 {
		fmt.Println()
		fmt.Println("Aliases (shortcuts):")
		for alias, canonical := range aliases {
			fmt.Printf("  %s -> %s\n", alias, canonical)
		}
	}
}

// formatAge formats a timestamp as a human-readable relative age.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// printSessionExit prints session resume info to stderr on exit.
func printSessionExit(store session.Store, sessionID string) {
	if sessionID == "" {
		return
	}
	var alias string
	if store != nil {
		if meta, err := store.ResolveMeta(sessionID); err == nil && meta.Alias != "" {
			alias = meta.Alias
		}
	}
	if alias != "" {
		fmt.Fprintf(os.Stderr, "\nResume with: codecuttlectl -r %s (id: %s)\n", alias, sessionID)
	} else {
		fmt.Fprintf(os.Stderr, "\nResume with: codecuttlectl -r %s\n", sessionID)
	}
}
