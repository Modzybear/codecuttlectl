package rollover

import (
	"fmt"
	"sync"

	"github.com/Modzybear/codecuttlectl/internal/session"
)

// Action represents the decision returned by Controller.Evaluate().
type Action string

const (
	ActionNone            Action = "none"
	ActionArm             Action = "arm"
	ActionTriggerCloseout Action = "trigger_closeout"
	ActionBudgetPaused    Action = "budget_paused"
)

// Observation captures the runtime context evaluated by the Controller.
type Observation struct {
	CurrentEpoch      int     // 1-indexed active epoch
	EpochTurns        int     // User turns completed/started in current epoch
	LastCallTokens    int32   // Total tokens (in + cache_read + cache_write) of last API call in current epoch
	ContextWindow     int32   // Effective model context window (<= 0 disables auto-rollover)
	CumulativeCostUSD float64 // Cumulative session cost in USD
	IsOrchestrator    bool    // Must be true to trigger rollover (swarm workers never roll over)
	PendingAsyncTasks int     // Number of in-flight async swarm tasks
	TurnIdle          bool    // True when model finished turn with zero pending tool calls
	GreenMilestone    bool    // True when a todo completed with no subsequent failing test/build
}

// Decision represents the action recommended by the Controller.
type Decision struct {
	Action         Action
	Trigger        session.RolloverTrigger
	AllowDocWrites bool
	ContextPct     float64
	Reason         string
}

// Controller coordinates the two-watermark state machine (Normal -> Armed -> Closeout -> Bootstrapping).
type Controller struct {
	mu sync.Mutex

	cfg                 Config
	state               State
	armedRounds         int
	snoozeTurns         int
	epochUsageSeen      bool
	budgetNotifiedEpoch int
}

// NewController creates a new Controller with normalized config defaults.
func NewController(cfg Config) *Controller {
	c := &Controller{
		state: StateNormal,
	}
	c.cfg = normalizeConfig(cfg)
	return c
}

// Config returns a copy of the current configuration.
func (c *Controller) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SetConfig updates the Controller's configuration and normalizes values.
func (c *Controller) SetConfig(cfg Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = normalizeConfig(cfg)
	if !c.cfg.AutoRollover && c.state == StateArmed {
		c.state = StateNormal
		c.armedRounds = 0
	}
}

// State returns the current lifecycle state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// SetState updates the lifecycle state directly.
func (c *Controller) SetState(s State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = s
}

// MarkUsageSeen marks that at least one fresh API usage report has been received in the current epoch.
func (c *Controller) MarkUsageSeen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epochUsageSeen = true
}

// OnUserTurnStarted is called when a new user message is submitted in the active epoch.
func (c *Controller) OnUserTurnStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snoozeTurns > 0 {
		c.snoozeTurns--
	}
}

// OnCloseoutCancelled handles when closeout is cancelled by user (e.g. Esc) or aborted on error.
func (c *Controller) OnCloseoutCancelled() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateNormal
	c.armedRounds = 0
	c.snoozeTurns = 1 // Snooze for 1 user turn to avoid instant re-trigger on the same turn
}

// OnEpochCommitted resets per-epoch runtime state when an epoch rollover commits.
func (c *Controller) OnEpochCommitted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateNormal
	c.armedRounds = 0
	c.snoozeTurns = 0
	c.epochUsageSeen = false
	c.budgetNotifiedEpoch = 0
}

// Evaluate determines whether to stay Normal, Arm rollover, trigger Closeout, or pause on budget.
func (c *Controller) Evaluate(obs Observation) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.cfg.AutoRollover || !obs.IsOrchestrator || obs.ContextWindow <= 0 || obs.LastCallTokens <= 0 || c.state == StateCloseout {
		return Decision{Action: ActionNone}
	}

	pct := float64(obs.LastCallTokens) / float64(obs.ContextWindow)

	// Anti-thrash cooldown: require usage seen in current epoch, minimum epoch turns, and snooze=0
	if !c.epochUsageSeen || obs.EpochTurns < c.cfg.MinEpochTurns || c.snoozeTurns > 0 {
		return Decision{Action: ActionNone, ContextPct: pct}
	}

	// Usage is below soft threshold
	if pct < c.cfg.SoftPct {
		if c.state == StateArmed {
			c.state = StateNormal
			c.armedRounds = 0
		}
		return Decision{Action: ActionNone, ContextPct: pct}
	}

	// Budget guard checks (evaluated once usage reaches or exceeds SoftPct)
	if c.cfg.MaxEpochs > 0 && obs.CurrentEpoch >= c.cfg.MaxEpochs {
		if c.budgetNotifiedEpoch != obs.CurrentEpoch {
			c.budgetNotifiedEpoch = obs.CurrentEpoch
			c.state = StateNormal
			c.armedRounds = 0
			return Decision{
				Action:     ActionBudgetPaused,
				ContextPct: pct,
				Reason:     fmt.Sprintf("Auto-rollover paused: reached --max-epochs=%d (use /rollover manually to continue).", c.cfg.MaxEpochs),
			}
		}
		return Decision{Action: ActionNone, ContextPct: pct}
	}

	if c.cfg.MaxCostUSD > 0 && obs.CumulativeCostUSD >= c.cfg.MaxCostUSD {
		if c.budgetNotifiedEpoch != obs.CurrentEpoch {
			c.budgetNotifiedEpoch = obs.CurrentEpoch
			c.state = StateNormal
			c.armedRounds = 0
			return Decision{
				Action:     ActionBudgetPaused,
				ContextPct: pct,
				Reason:     fmt.Sprintf("Auto-rollover paused: reached --max-cost=$%.2f (current $%.2f; use /rollover manually to continue).", c.cfg.MaxCostUSD, obs.CumulativeCostUSD),
			}
		}
		return Decision{Action: ActionNone, ContextPct: pct}
	}

	// 1. Hard watermark: force closeout immediately
	if pct >= c.cfg.HardPct {
		c.state = StateCloseout
		return Decision{
			Action:         ActionTriggerCloseout,
			Trigger:        session.RolloverTriggerHardThreshold,
			AllowDocWrites: c.allowDocs(obs.GreenMilestone),
			ContextPct:     pct,
			Reason:         fmt.Sprintf("Context at %.0f%% (hard threshold %.0f%%) — forcing epoch rollover", pct*100, c.cfg.HardPct*100),
		}
	}

	// 2. Soft watermark (SoftPct <= pct < HardPct): Arm and check for quiescent boundaries
	if c.state == StateNormal {
		c.state = StateArmed
		c.armedRounds = 0
	}

	// Quiescent boundary: turn idle or green task milestone with no pending async tasks
	if obs.PendingAsyncTasks == 0 && (obs.TurnIdle || obs.GreenMilestone) {
		c.state = StateCloseout
		reason := "turn idle"
		if obs.GreenMilestone {
			reason = "green task milestone"
		}
		return Decision{
			Action:         ActionTriggerCloseout,
			Trigger:        session.RolloverTriggerSoftThreshold,
			AllowDocWrites: c.allowDocs(obs.GreenMilestone),
			ContextPct:     pct,
			Reason:         fmt.Sprintf("Context at %.0f%% (%s) — rolling over cleanly", pct*100, reason),
		}
	}

	// If tool execution is active mid-turn, increment armed rounds counter
	if !obs.TurnIdle {
		c.armedRounds++
	}

	// Forced timeout after staying armed for too many tool rounds
	if c.armedRounds >= c.cfg.MaxArmedRounds {
		c.state = StateCloseout
		return Decision{
			Action:         ActionTriggerCloseout,
			Trigger:        session.RolloverTriggerHardThreshold,
			AllowDocWrites: c.allowDocs(obs.GreenMilestone),
			ContextPct:     pct,
			Reason:         fmt.Sprintf("Context at %.0f%% (armed for %d tool rounds) — rolling over", pct*100, c.armedRounds),
		}
	}

	return Decision{
		Action:     ActionArm,
		ContextPct: pct,
		Reason:     fmt.Sprintf("Context at %.0f%% — rollover armed, waiting for quiescent boundary", pct*100),
	}
}

// allowDocs determines if doc writes are authorized during closeout.
func (c *Controller) allowDocs(greenMilestone bool) bool {
	switch c.cfg.DocsPolicy {
	case DocsPolicyAlways:
		return true
	case DocsPolicyMilestone:
		return greenMilestone
	default:
		return false
	}
}

// ArmedSystemHint returns a transient system prompt hint to include during model calls while Armed.
func ArmedSystemHint(ctxPct float64) string {
	return fmt.Sprintf("[Session Context Notice: Context usage is at %.0f%%. Finish your current atomic unit of work cleanly (e.g., complete the current edit/test pass) so the harness can checkpoint and roll over to a fresh epoch.]", ctxPct*100)
}

// ValidateConfig verifies that the config values are structurally and logically sound.
func ValidateConfig(cfg Config) error {
	if cfg.SoftPct <= 0 || cfg.SoftPct >= 1.0 {
		return fmt.Errorf("soft threshold must be between 0.0 and 1.0, got %f", cfg.SoftPct)
	}
	if cfg.HardPct <= 0 || cfg.HardPct > 1.0 {
		return fmt.Errorf("hard threshold must be between 0.0 and 1.0, got %f", cfg.HardPct)
	}
	if cfg.HardPct <= cfg.SoftPct {
		return fmt.Errorf("hard threshold (%.2f) must be strictly greater than soft threshold (%.2f)", cfg.HardPct, cfg.SoftPct)
	}
	if cfg.MaxEpochs < 0 {
		return fmt.Errorf("max epochs cannot be negative, got %d", cfg.MaxEpochs)
	}
	if cfg.MaxCostUSD < 0 {
		return fmt.Errorf("max cost cannot be negative, got %f", cfg.MaxCostUSD)
	}
	switch cfg.DocsPolicy {
	case "", DocsPolicyNone, DocsPolicyMilestone, DocsPolicyAlways:
		return nil
	default:
		return fmt.Errorf("invalid docs policy %q", cfg.DocsPolicy)
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.SoftPct <= 0 {
		cfg.SoftPct = 0.45
	}
	if cfg.HardPct <= 0 {
		cfg.HardPct = 0.65
	}
	if cfg.MaxArmedRounds <= 0 {
		cfg.MaxArmedRounds = 8
	}
	if cfg.CloseoutMaxSteps <= 0 {
		cfg.CloseoutMaxSteps = 4
	}
	if cfg.MinEpochTurns <= 0 {
		cfg.MinEpochTurns = 2
	}
	if cfg.DocsPolicy == "" {
		cfg.DocsPolicy = DocsPolicyNone
	}
	return cfg
}
