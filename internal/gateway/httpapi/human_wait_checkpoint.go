package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

const defaultHumanWaitGrace = 250 * time.Millisecond

func (c *RunCoordinator) humanWaitGrace() time.Duration {
	if c.srv.HumanWaitGrace > 0 {
		return c.srv.HumanWaitGrace
	}
	return defaultHumanWaitGrace
}

// parkHumanWait releases the worker of a Run waiting on a person. reason is why
// (a control.HumanWaitPark* value) and is kept with the request. An armed wait
// already wrote the exact checkpoint when it started and nothing has changed it
// since, so only an unarmed wait writes it now.
func (c *RunCoordinator) parkHumanWait(ctx context.Context, identity *control.IdentityContext, runID, kind, ref, toolName, reason string, armed *armedHumanWait) (bool, error) {
	var err error
	if armed == nil {
		err = kernel.SaveHumanWaitCheckpoint(ctx, kind, ref, toolName)
	}
	if err == nil {
		var parked bool
		parked, err = c.srv.Control.ParkHumanWaitRun(context.WithoutCancel(ctx), identity.TenantID, identity.PersonID, runID, kind, ref, reason)
		if err == nil && parked {
			kernel.MarkHumanWaitParked(ctx)
			armed.parked()
			return true, nil
		}
		if err == nil {
			return false, nil
		}
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{Type: "agent.thinking", Content: "Waiting for your answer. This call cannot be safely checkpointed, so its worker remains occupied; /stop cancels it.", Payload: map[string]interface{}{"phase": "waiting_user", "wait_kind": kind, "wait_ref": ref, "worker_retained": true}})
	log.Warn("human wait keeps cancellable worker because checkpoint parking failed", "run_id", runID, "kind", kind, "error", err)
	_, eventErr := c.srv.Control.AppendEvent(context.WithoutCancel(ctx), control.Event{RunID: runID, Type: "checkpoint.failed", Visibility: "task", Payload: mustJSON(map[string]interface{}{"outcome": "await_human", "wait_kind": kind, "wait_ref": ref, "error": err.Error(), "worker_retained": true})})
	if eventErr != nil {
		log.Warn("human wait checkpoint diagnostic could not be saved", "run_id", runID, "error", eventErr)
	}
	return false, err
}

func (c *RunCoordinator) reportHumanWaitControlFailure(ctx context.Context, runID, kind, ref string, err error) {
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{Type: "agent.thinking", Content: "The human decision cannot be consumed safely yet. Its worker remains occupied; /stop cancels it.", Payload: map[string]interface{}{"phase": "waiting_user", "wait_kind": kind, "wait_ref": ref, "worker_retained": true}})
	log.Warn("human wait retains its worker after a control-store failure", "run_id", runID, "kind", kind, "error", err)
	_, eventErr := c.srv.Control.AppendEvent(context.WithoutCancel(ctx), control.Event{RunID: runID, Type: "human_wait.control_failed", Visibility: "task", Payload: mustJSON(map[string]interface{}{"wait_kind": kind, "wait_ref": ref, "error": err.Error(), "worker_retained": true})})
	if eventErr != nil {
		log.Warn("human wait control diagnostic could not be saved", "run_id", runID, "error", eventErr)
	}
}

// The clarify body only records an unanswered question. Its pause has no
// external effect, and is deliberately distinct from success or rejection.
type humanWaitPause struct{ kernel.TurnPause }

func (*humanWaitPause) ToolErrorCode() string      { return "clarification_parked" }
func (*humanWaitPause) ToolErrorCategory() string  { return "human_wait" }
func (e *humanWaitPause) ModelSafeMessage() string { return e.Error() }
func (*humanWaitPause) ToolRecoveryHint() string {
	return "Nobody has answered yet. Do not retry this question; its exact pending answer resumes the work."
}
func (*humanWaitPause) ToolFailurePhase() string   { return "authorization" }
func (*humanWaitPause) ToolRetryability() string   { return "after_human" }
func (*humanWaitPause) ToolEffectState() string    { return "not_dispatched" }
func (*humanWaitPause) ToolStateChanged() bool     { return false }
func (*humanWaitPause) ToolAlternatives() []string { return nil }

// A checkpointed continuation belongs to its exact origin session. Endpoint
// metadata is routing only: the recorded account must still belong to this
// person, and execution authority remains the inherited admission snapshot.
func (d *Server) humanWaitContinuationRoute(ctx context.Context, identity *control.IdentityContext, source *control.Run, kind, ref, answerChannel string) (platform, userID, channel string) {
	platform, userID, channel = identity.Platform, identity.PlatformUserID, continuationChannel(identity.Platform, fallback(answerChannel, identity.Platform), source)
	if source == nil {
		return
	}
	checkpoint, err := d.Control.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, source.ID)
	if err != nil || checkpoint == nil || checkpoint.Outcome != "await_human" || checkpoint.WaitKind != kind || checkpoint.WaitRef != ref {
		return
	}
	payload, err := d.Control.RunStartedPayload(ctx, identity.TenantID, identity.PersonID, source.ID)
	var route struct {
		Platform string `json:"platform"`
		UserID   string `json:"platform_user_id"`
	}
	if err != nil || json.Unmarshal(payload, &route) != nil || route.Platform == "" || route.UserID == "" {
		log.Warn("human continuation cannot restore its origin route", "run_id", source.ID)
		return
	}
	origin, err := d.Control.ResolveAccount(ctx, identity.TenantID, route.Platform, route.UserID)
	if err != nil || origin == nil || origin.PersonID != identity.PersonID {
		log.Warn("human continuation origin account is unavailable", "run_id", source.ID)
		return
	}
	return origin.Platform, origin.PlatformUserID, source.Channel
}
