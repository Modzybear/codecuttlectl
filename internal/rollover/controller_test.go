package rollover

import (
	"strings"
	"testing"

	"github.com/Modzybear/codecuttlectl/internal/session"
)

func TestController_SoftArmingAndQuiescentTrigger(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	cfg.SoftPct = 0.45
	cfg.HardPct = 0.65
	cfg.MinEpochTurns = 2

	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	baseObs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     2,
		ContextWindow:  100_000,
		IsOrchestrator: true,
	}

	// 1. Below soft threshold (40%) -> ActionNone, state remains Normal
	baseObs.LastCallTokens = 40_000
	baseObs.TurnIdle = false
	dec := ctrl.Evaluate(baseObs)
	if dec.Action != ActionNone || ctrl.State() != StateNormal {
		t.Fatalf("expected ActionNone at 40%%, got action=%q state=%q", dec.Action, ctrl.State())
	}

	// 2. Above soft threshold (48%) mid-turn (TurnIdle=false) -> ActionArm, state transitions to Armed
	baseObs.LastCallTokens = 48_000
	dec = ctrl.Evaluate(baseObs)
	if dec.Action != ActionArm || ctrl.State() != StateArmed {
		t.Fatalf("expected ActionArm at 48%% mid-turn, got action=%q state=%q", dec.Action, ctrl.State())
	}

	// 3. Quiescent boundary hit (TurnIdle=true) at 48% -> triggers soft closeout cleanly
	baseObs.TurnIdle = true
	dec = ctrl.Evaluate(baseObs)
	if dec.Action != ActionTriggerCloseout || dec.Trigger != session.RolloverTriggerSoftThreshold || ctrl.State() != StateCloseout {
		t.Fatalf("expected soft ActionTriggerCloseout on turn idle, got action=%q trigger=%q state=%q", dec.Action, dec.Trigger, ctrl.State())
	}
}

func TestController_GreenMilestoneTrigger(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	cfg.DocsPolicy = DocsPolicyMilestone
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     3,
		LastCallTokens: 50_000,
		ContextWindow:  100_000,
		IsOrchestrator: true,
		TurnIdle:       false,
		GreenMilestone: true,
	}

	dec := ctrl.Evaluate(obs)
	if dec.Action != ActionTriggerCloseout || dec.Trigger != session.RolloverTriggerSoftThreshold {
		t.Fatalf("expected soft closeout on GreenMilestone, got %+v", dec)
	}
	if !dec.AllowDocWrites {
		t.Fatalf("expected AllowDocWrites=true for DocsPolicyMilestone on GreenMilestone, got false")
	}
}

func TestController_PendingAsyncTasksDeferSoftTrigger(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:      1,
		EpochTurns:        3,
		LastCallTokens:    50_000, // 50% (between 45% and 65%)
		ContextWindow:     100_000,
		IsOrchestrator:    true,
		TurnIdle:          true,
		PendingAsyncTasks: 1, // in-flight async task
	}

	// Should NOT trigger soft closeout while async tasks are in flight
	dec := ctrl.Evaluate(obs)
	if dec.Action != ActionArm {
		t.Fatalf("expected ActionArm while async task pending, got %+v", dec)
	}

	// Hard threshold (70%) triggers closeout even with pending async tasks
	obs.LastCallTokens = 70_000
	dec = ctrl.Evaluate(obs)
	if dec.Action != ActionTriggerCloseout || dec.Trigger != session.RolloverTriggerHardThreshold {
		t.Fatalf("expected hard closeout at 70%% despite pending async task, got %+v", dec)
	}
}

func TestController_MaxArmedRoundsTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	cfg.MaxArmedRounds = 3
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     3,
		LastCallTokens: 50_000,
		ContextWindow:  100_000,
		IsOrchestrator: true,
		TurnIdle:       false,
	}

	// Rounds 1, 2 stay armed
	for r := 1; r <= 2; r++ {
		dec := ctrl.Evaluate(obs)
		if dec.Action != ActionArm {
			t.Fatalf("round %d: expected ActionArm, got %+v", r, dec)
		}
	}

	// Round 3 hits MaxArmedRounds -> forces closeout with HardThreshold
	dec := ctrl.Evaluate(obs)
	if dec.Action != ActionTriggerCloseout || dec.Trigger != session.RolloverTriggerHardThreshold {
		t.Fatalf("round 3: expected forced ActionTriggerCloseout on MaxArmedRounds, got %+v", dec)
	}
}

func TestController_AntiThrashCooldownAndSnooze(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	cfg.MinEpochTurns = 2
	ctrl := NewController(cfg)

	obs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     1,
		LastCallTokens: 50_000,
		ContextWindow:  100_000,
		IsOrchestrator: true,
		TurnIdle:       true,
	}

	// 1. No usage seen yet in this epoch -> ActionNone
	if dec := ctrl.Evaluate(obs); dec.Action != ActionNone {
		t.Fatalf("expected ActionNone before usage seen, got %+v", dec)
	}

	ctrl.MarkUsageSeen()

	// 2. EpochTurns (1) < MinEpochTurns (2) -> ActionNone
	if dec := ctrl.Evaluate(obs); dec.Action != ActionNone {
		t.Fatalf("expected ActionNone when EpochTurns < MinEpochTurns, got %+v", dec)
	}

	// 3. EpochTurns reaches 2 -> Triggers closeout
	obs.EpochTurns = 2
	if dec := ctrl.Evaluate(obs); dec.Action != ActionTriggerCloseout {
		t.Fatalf("expected ActionTriggerCloseout when tenure reached, got %+v", dec)
	}

	// 4. User cancels closeout (Esc) -> snoozes for 1 user turn
	ctrl.OnCloseoutCancelled()
	if dec := ctrl.Evaluate(obs); dec.Action != ActionNone {
		t.Fatalf("expected ActionNone immediately after cancel snooze, got %+v", dec)
	}

	// Next user turn started -> decrements snooze
	ctrl.OnUserTurnStarted()
	if dec := ctrl.Evaluate(obs); dec.Action != ActionTriggerCloseout {
		t.Fatalf("expected ActionTriggerCloseout after snooze consumed on next turn, got %+v", dec)
	}
}

func TestController_BudgetGuards(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	cfg.MaxEpochs = 2
	cfg.MaxCostUSD = 1.00
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:      2, // Reached MaxEpochs (2)
		EpochTurns:        3,
		LastCallTokens:    50_000,
		ContextWindow:     100_000,
		CumulativeCostUSD: 0.50,
		IsOrchestrator:    true,
		TurnIdle:          true,
	}

	// First evaluation at MaxEpochs returns ActionBudgetPaused
	dec := ctrl.Evaluate(obs)
	if dec.Action != ActionBudgetPaused || !strings.Contains(dec.Reason, "--max-epochs=2") {
		t.Fatalf("expected ActionBudgetPaused for max-epochs, got %+v", dec)
	}

	// Subsequent evaluations in same epoch stay quiet (ActionNone)
	if dec2 := ctrl.Evaluate(obs); dec2.Action != ActionNone {
		t.Fatalf("expected ActionNone after first budget notification in same epoch, got %+v", dec2)
	}

	// Reset for cost test
	cfg.MaxEpochs = 0
	ctrl.SetConfig(cfg)
	ctrl.OnEpochCommitted()
	ctrl.MarkUsageSeen()

	obs.CurrentEpoch = 1
	obs.CumulativeCostUSD = 1.50 // Exceeds MaxCostUSD ($1.00)

	decCost := ctrl.Evaluate(obs)
	if decCost.Action != ActionBudgetPaused || !strings.Contains(decCost.Reason, "--max-cost=$1.00") {
		t.Fatalf("expected ActionBudgetPaused for max-cost, got %+v", decCost)
	}
}

func TestController_SwarmNonOrchestratorAlwaysNone(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     5,
		LastCallTokens: 90_000, // 90%
		ContextWindow:  100_000,
		IsOrchestrator: false, // Worker node (e.g. deep_coder)
		TurnIdle:       true,
	}

	dec := ctrl.Evaluate(obs)
	if dec.Action != ActionNone {
		t.Fatalf("expected ActionNone for non-orchestrator worker node, got %+v", dec)
	}
}

func TestController_DocsPolicyVariations(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoRollover = true
	ctrl := NewController(cfg)
	ctrl.MarkUsageSeen()

	obs := Observation{
		CurrentEpoch:   1,
		EpochTurns:     3,
		LastCallTokens: 50_000,
		ContextWindow:  100_000,
		IsOrchestrator: true,
		TurnIdle:       true,
		GreenMilestone: false,
	}

	// None policy -> AllowDocWrites=false
	cfg.DocsPolicy = DocsPolicyNone
	ctrl.SetConfig(cfg)
	if dec := ctrl.Evaluate(obs); !dec.Action.isCloseout() || dec.AllowDocWrites {
		t.Errorf("DocsPolicyNone: expected AllowDocWrites=false, got %v", dec.AllowDocWrites)
	}

	// Always policy -> AllowDocWrites=true
	cfg.DocsPolicy = DocsPolicyAlways
	ctrl.SetConfig(cfg)
	ctrl.SetState(StateNormal)
	if dec := ctrl.Evaluate(obs); !dec.Action.isCloseout() || !dec.AllowDocWrites {
		t.Errorf("DocsPolicyAlways: expected AllowDocWrites=true, got %v", dec.AllowDocWrites)
	}

	// Milestone policy without green milestone -> AllowDocWrites=false
	cfg.DocsPolicy = DocsPolicyMilestone
	ctrl.SetConfig(cfg)
	ctrl.SetState(StateNormal)
	if dec := ctrl.Evaluate(obs); !dec.Action.isCloseout() || dec.AllowDocWrites {
		t.Errorf("DocsPolicyMilestone without green: expected AllowDocWrites=false, got %v", dec.AllowDocWrites)
	}

	// Milestone policy with green milestone -> AllowDocWrites=true
	obs.GreenMilestone = true
	ctrl.SetState(StateNormal)
	if dec := ctrl.Evaluate(obs); !dec.Action.isCloseout() || !dec.AllowDocWrites {
		t.Errorf("DocsPolicyMilestone with green: expected AllowDocWrites=true, got %v", dec.AllowDocWrites)
	}
}

func TestValidateConfig(t *testing.T) {
	valid := DefaultConfig()
	if err := ValidateConfig(valid); err != nil {
		t.Fatalf("expected valid DefaultConfig, got error: %v", err)
	}

	// Invalid SoftPct
	badSoft := valid
	badSoft.SoftPct = 0
	if err := ValidateConfig(badSoft); err == nil {
		t.Error("expected error for SoftPct <= 0")
	}

	// HardPct <= SoftPct
	badHard := valid
	badHard.HardPct = 0.40
	badHard.SoftPct = 0.50
	if err := ValidateConfig(badHard); err == nil {
		t.Error("expected error for HardPct <= SoftPct")
	}

	// Invalid DocsPolicy
	badDocs := valid
	badDocs.DocsPolicy = "invalid"
	if err := ValidateConfig(badDocs); err == nil {
		t.Error("expected error for invalid DocsPolicy")
	}
}

func (a Action) isCloseout() bool {
	return a == ActionTriggerCloseout
}
