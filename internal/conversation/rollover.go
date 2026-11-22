package conversation

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/Modzybear/codecuttlectl/internal/provider"
	"github.com/Modzybear/codecuttlectl/internal/rollover"
	"github.com/Modzybear/codecuttlectl/internal/session"
	"github.com/Modzybear/codecuttlectl/internal/todo"
)

// CurrentEpoch returns the active 1-indexed epoch number for this agent session.
func (a *Agent) CurrentEpoch() int {
	if a.currentEpoch <= 0 {
		return 1
	}
	return a.currentEpoch
}

// Epochs returns a copy of completed epoch checkpoints recorded for this session.
func (a *Agent) Epochs() []session.EpochCheckpoint {
	if len(a.epochs) == 0 {
		return nil
	}
	cp := make([]session.EpochCheckpoint, len(a.epochs))
	copy(cp, a.epochs)
	return cp
}

// stampProviderMessage attaches epoch, turn, timestamp, node, and model provenance to msg.
func (a *Agent) stampProviderMessage(msg provider.Message) provider.Message {
	a.msgSeq++
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("e%d_msg_%04d", a.CurrentEpoch(), a.msgSeq)
	}
	if msg.Epoch == 0 {
		msg.Epoch = a.CurrentEpoch()
	}
	if msg.Turn == 0 {
		msg.Turn = a.turn
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}
	if msg.NodeID == "" {
		msg.NodeID = a.activeNode
	}
	if msg.Model == "" && a.provider != nil {
		msg.Model = provider.EffectiveID(a.provider)
	}
	return msg
}

// Rollover executes a manual epoch closeout → save_checkpoint → atomic archive →
// fresh-context bootstrap cycle on provider-based conversations.
// If closeout fails or context is cancelled, the current epoch history is restored
// untouched and an error is returned.
func (a *Agent) Rollover(ctx context.Context, docsPolicy rollover.DocsPolicy, steeringHint string) (*session.EpochCheckpoint, error) {
	allowDocWrites := docsPolicy == rollover.DocsPolicyAlways
	return a.rolloverInternal(ctx, session.RolloverTriggerManual, docsPolicy, allowDocWrites, steeringHint)
}

func (a *Agent) isOrchestratorActive() bool {
	if a.morph == nil || a.activeNode == "" {
		return true
	}
	node, ok := a.morph.Nodes[a.activeNode]
	if !ok {
		return true
	}
	return node.IsPrimary
}

func (a *Agent) recordProviderCost(usage provider.Usage) float64 {
	if usage.CostUSD > 0 {
		a.cumulativeCostUSD += usage.CostUSD
		return usage.CostUSD
	}
	if est, ok := a.provider.(provider.CostEstimator); ok {
		c := est.EstimateCost(usage)
		a.cumulativeCostUSD += c
		return c
	}
	return 0
}

func (a *Agent) armedSystemHint() string {
	if a.rolloverCtrl != nil && a.rolloverCtrl.State() == rollover.StateArmed && a.armedCtxPct > 0 {
		return rollover.ArmedSystemHint(a.armedCtxPct)
	}
	return ""
}

// maybeAutoRollover evaluates automatic epoch rollover for headless/REPL provider loops.
// Returns true if an epoch rollover was committed (caller should reset its step counter).
func (a *Agent) maybeAutoRollover(ctx context.Context, turnIdle bool, greenMilestone bool) bool {
	if a.rolloverCtrl == nil || !a.rolloverCtrl.Config().AutoRollover {
		return false
	}

	var cw int32
	if cwp, ok := a.provider.(provider.ContextWindowProvider); ok {
		cw = cwp.ContextWindow()
	}

	obs := rollover.Observation{
		CurrentEpoch:      a.CurrentEpoch(),
		EpochTurns:        a.epochTurns,
		LastCallTokens:    a.lastCallTokens,
		ContextWindow:     cw,
		CumulativeCostUSD: a.cumulativeCostUSD,
		IsOrchestrator:    a.isOrchestratorActive(),
		TurnIdle:          turnIdle,
		GreenMilestone:    greenMilestone,
	}

	dec := a.rolloverCtrl.Evaluate(obs)
	switch dec.Action {
	case rollover.ActionArm:
		a.armedCtxPct = dec.ContextPct
		return false
	case rollover.ActionBudgetPaused:
		a.armedCtxPct = 0
		if a.verbose {
			log.Printf("[rollover] %s", dec.Reason)
		}
		return false
	case rollover.ActionTriggerCloseout:
		a.armedCtxPct = 0
		if a.verbose {
			log.Printf("[rollover] %s", dec.Reason)
		}
		_, err := a.rolloverInternal(ctx, dec.Trigger, a.rolloverCtrl.Config().DocsPolicy, dec.AllowDocWrites, "")
		if err != nil {
			if a.verbose {
				log.Printf("[rollover] closeout failed: %v", err)
			}
			a.rolloverCtrl.OnCloseoutCancelled()
			return false
		}
		return true
	default:
		if a.rolloverCtrl.State() == rollover.StateNormal {
			a.armedCtxPct = 0
		}
		return false
	}
}

func (a *Agent) rolloverInternal(ctx context.Context, trigger session.RolloverTrigger, docsPolicy rollover.DocsPolicy, allowDocWrites bool, steeringHint string) (*session.EpochCheckpoint, error) {
	if a.provider == nil {
		return nil, fmt.Errorf("epoch rollover requires a provider-based agent")
	}
	if len(a.provHistory) == 0 {
		return nil, fmt.Errorf("nothing to roll over: conversation history is empty")
	}

	prevEpoch := a.CurrentEpoch()
	nextEpoch := prevEpoch + 1
	maxSteps := rollover.DefaultConfig().CloseoutMaxSteps
	if a.rolloverCtrl != nil && a.rolloverCtrl.Config().CloseoutMaxSteps > 0 {
		maxSteps = a.rolloverCtrl.Config().CloseoutMaxSteps
	}
	if allowDocWrites && maxSteps < 8 {
		maxSteps = 8
	}

	preHistory := append([]provider.Message(nil), a.provHistory...)
	restoreOnErr := func(err error) (*session.EpochCheckpoint, error) {
		a.provHistory = preHistory
		if a.rolloverCtrl != nil {
			a.rolloverCtrl.OnCloseoutCancelled()
		}
		return nil, err
	}

	closeoutTools := a.closeoutProviderToolDefs(allowDocWrites)
	forced := trigger == session.RolloverTriggerHardThreshold
	closeoutPrompt := rollover.BuildCloseoutPrompt(allowDocWrites, forced, steeringHint)
	a.provHistory = append(a.provHistory, a.stampProviderMessage(provider.BuildUserTextMessage(closeoutPrompt)))

	var validatedPayload *session.CheckpointPayload

	for step := 0; step < maxSteps; step++ {
		req := provider.Request{
			System:   a.effectiveSystemPrompt(),
			Messages: a.provHistory,
			Tools:    closeoutTools,
		}

		resp, err := a.provider.Converse(ctx, req)
		if err != nil {
			return restoreOnErr(fmt.Errorf("rollover closeout failed: %w", err))
		}
		a.recordProviderUsage(resp.Usage, step)

		var assistBlocks []provider.ContentBlock
		if resp.Reasoning != "" || len(resp.ReasoningDetails) > 0 {
			assistBlocks = append(assistBlocks, provider.ReasoningBlock{
				Text:             resp.Reasoning,
				ReasoningDetails: resp.ReasoningDetails,
			})
		}
		if resp.Content != "" {
			assistBlocks = append(assistBlocks, provider.TextBlock{Text: resp.Content})
		}
		for _, tu := range resp.ToolUses {
			assistBlocks = append(assistBlocks, provider.ToolUseBlock{
				ToolUseID:        tu.ToolUseID,
				Name:             tu.Name,
				Input:            tu.Input,
				ThoughtSignature: tu.ThoughtSignature,
			})
		}
		if len(assistBlocks) > 0 {
			a.provHistory = append(a.provHistory, a.stampProviderMessage(provider.BuildAssistantMessage(assistBlocks)))
		}

		if len(resp.ToolUses) == 0 {
			reminder := "Please call the `save_checkpoint` tool now with summary, lessons_learned, key_files, and resume_instructions to finalize this epoch rollover."
			a.provHistory = append(a.provHistory, a.stampProviderMessage(provider.BuildUserTextMessage(reminder)))
			continue
		}

		var resultBlocks []provider.ContentBlock
		for _, tu := range resp.ToolUses {
			if tu.Name == rollover.SaveCheckpointToolName {
				payload, valErr := rollover.ParseAndValidateCheckpoint(tu.Input)
				if valErr != nil {
					resultBlocks = append(resultBlocks, provider.ToolResultBlock{
						ToolUseID: tu.ToolUseID,
						Name:      tu.Name,
						Content:   fmt.Sprintf("Error: %s\nPlease fix the missing/invalid fields and call save_checkpoint again.", valErr.Error()),
						IsError:   true,
					})
					continue
				}
				cp := payload
				validatedPayload = &cp
				resultBlocks = append(resultBlocks, provider.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Name:      tu.Name,
					Content:   "Checkpoint validated and accepted. Closing epoch.",
					IsError:   false,
				})
				continue
			}

			if !rollover.IsCloseoutToolAllowed(tu.Name, allowDocWrites) {
				resultBlocks = append(resultBlocks, provider.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Name:      tu.Name,
					Content:   fmt.Sprintf("Error: Tool %q is not permitted during rollover closeout. Finish inspecting and call save_checkpoint.", tu.Name),
					IsError:   true,
				})
				continue
			}

			output, status := a.executeTool(ctx, tu.Name, tu.Input, false)
			resultBlocks = append(resultBlocks, provider.ToolResultBlock{
				ToolUseID: tu.ToolUseID,
				Name:      tu.Name,
				Content:   output,
				IsError:   status == types.ToolResultStatusError,
			})
		}

		if validatedPayload != nil {
			break
		}

		a.provHistory = append(a.provHistory, a.stampProviderMessage(provider.Message{
			Role:    provider.RoleUser,
			Content: resultBlocks,
		}))
	}

	if validatedPayload == nil {
		return restoreOnErr(fmt.Errorf("rollover aborted: model did not produce a valid save_checkpoint within %d steps", maxSteps))
	}

	// Commit epoch atomically: archive Epoch N -> advance to Epoch N+1 -> save active state.
	a.originalObjective = rollover.ResolveOriginalObjective(a.originalObjective, preHistory)
	pinned := rollover.CollectPinnedState(ctx, a.workDir, a.originalObjective, a.todos.Items(), nil)

	serializedPrev, err := session.MarshalProviderHistory(preHistory)
	if err != nil {
		return restoreOnErr(fmt.Errorf("marshaling Epoch %d history: %w", prevEpoch, err))
	}

	now := time.Now().UTC()
	var contextWindow int32
	if cwp, ok := a.provider.(provider.ContextWindowProvider); ok {
		contextWindow = cwp.ContextWindow()
	}

	checkpoint := session.EpochCheckpoint{
		Epoch:             prevEpoch,
		CreatedAt:         now,
		Trigger:           trigger,
		MessageCount:      len(serializedPrev),
		TurnRange:         [2]int{1, max(a.turn, 1)},
		TokensAtRollover:  a.lastCallTokens,
		ContextWindow:     contextWindow,
		EstimatedCostUSD:  a.cumulativeCostUSD,
		DocsPolicyApplied: string(docsPolicy),
		Pinned:            pinned,
		Payload:           *validatedPayload,
	}

	if a.store != nil && a.sessionID != "" {
		archive := &session.EpochArchive{
			SessionID:  a.sessionID,
			Epoch:      prevEpoch,
			ClosedAt:   now,
			Checkpoint: checkpoint,
			Messages:   serializedPrev,
			Inkwell:    append([]session.InkEntry(nil), a.inkwell...),
		}
		relPath, err := a.store.SaveEpochArchive(a.sessionID, archive)
		if err != nil {
			return restoreOnErr(fmt.Errorf("archiving Epoch %d: %w", prevEpoch, err))
		}
		checkpoint.ArchiveFile = relPath
	}

	// Advance active epoch, archive Inkwell count cumulatively, and seed Epoch N+1.
	a.archivedToolCalls += len(a.inkwell)
	a.inkwell = nil
	a.epochs = append(a.epochs, checkpoint)
	a.currentEpoch = nextEpoch
	a.epochTurns = 0
	a.msgSeq = 0
	a.lastCallTokens = 0
	a.lastCallCacheReadTokens = 0
	a.armedCtxPct = 0
	if a.rolloverCtrl != nil {
		a.rolloverCtrl.OnEpochCommitted()
	}

	bootstrapText := rollover.BuildBootstrapMessage(nextEpoch, prevEpoch, pinned, *validatedPayload, steeringHint)
	a.provHistory = []provider.Message{
		a.stampProviderMessage(provider.BuildUserTextMessage(bootstrapText)),
	}
	a.dirty = true
	a.flushSessionProvider()

	cpCopy := checkpoint
	return &cpCopy, nil
}

func (a *Agent) hasCompletedTodo() bool {
	if a.todos == nil {
		return false
	}
	for _, item := range a.todos.Items() {
		if item.Status == todo.StatusCompleted {
			return true
		}
	}
	return false
}

func (a *Agent) closeoutProviderToolDefs(allowDocWrites bool) []provider.ToolDefinition {
	var filtered []provider.ToolDefinition
	for _, td := range a.allProviderToolDefs() {
		if rollover.IsCloseoutToolAllowed(td.Name, allowDocWrites) {
			filtered = append(filtered, td)
		}
	}
	filtered = append(filtered, rollover.SaveCheckpointToolDef())
	return filtered
}
