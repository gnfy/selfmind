package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// ExternalEffectTargetProvider is a trusted built-in tool's proof of which
// external resources this exact invocation can change. Target keys are opaque
// non-secret identifiers, not URLs or credentials, and must cover every
// possible effect; completeOnReturn is true only when a successful
// tool return proves that the remote operation itself is finished. External
// tools and commands without such a proof use the person-wide unknown target.
type ExternalEffectTargetProvider interface {
	ExternalEffectTargets(args map[string]interface{}) (keys []string, completeOnReturn bool, err error)
}

// ExternalEffectInFlightGrace bounds how long a call waits in place while
// another Run's call on a conflicting target is still in flight (or a watcher
// bound to it can still release it). Past the grace the Run parks as an
// external_resource_wait and gives its worker back.
const ExternalEffectInFlightGrace = 30 * time.Second

const (
	externalEffectPollStart = 200 * time.Millisecond
	externalEffectPollMax   = 2 * time.Second
)

// ExternalEffectClaimMiddleware sits immediately inside approval and before
// the tool body. Existing single-Run production behavior is unchanged; a Run
// admitted at parallel capacity must claim every external write target before
// dispatch.
//
// The claim settles when the call proves success: a foreground command that
// exits 0, or a synchronous tool that returns without error (an adapter that
// declares asynchronous completion keeps its claim). A failure, timeout,
// cancellation, external tool error, or crash after dispatch leaves it
// uncertain until a bound observation or the person's settlement releases it.
// A background start is in flight until the daemon observes its exit.
//
// Conflicts are resolved before dispatch: the Run's own unresolved effect is
// refused with instructions; another Run's in-flight effect is waited for in
// place up to ExternalEffectInFlightGrace and then parks the Run; another
// Run's left-uncertain effect needs the person's one-time approval of this
// exact call, or this Run's own later observation bound to that claim.
//
// Lock order: the in-place wait runs here, after approval and before the tool
// body. It holds no control.db transaction, and the waiting Run holds only its
// worker and its admitted roots, none of which the in-flight owner needs to
// return. Any exclusive execution lane must be taken after this claim, inside
// the dispatch it guards, never around this wait.
func ExternalEffectClaimMiddleware(store *control.Store) ResultMiddleware {
	return externalEffectClaimMiddleware(store, ExternalEffectInFlightGrace)
}

func externalEffectClaimMiddleware(store *control.Store, grace time.Duration) ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			scope, installed := currentExecutionScopeAny(args)
			name := stringArg(args, "_tool_name")
			if claimID := strings.TrimSpace(stringArg(args, "effect_claim_id")); claimID != "" && isExecTool(name) && name != "watch_external" {
				return observeBoundExternalEffect(store, scope, installed, args, claimID, next)
			}
			if !installed || !scope.ParallelWork || !externalEffectPossible(args) {
				return next(args)
			}
			if store == nil || scope.TenantID == "" || scope.PersonID == "" || scope.RunID == "" {
				return uninvokedExternalEffect(fmt.Errorf("external effect claim cannot be established for this run"))
			}
			callID := strings.TrimSpace(stringArg(args, "_tool_call_id"))
			if callID == "" {
				return uninvokedExternalEffect(fmt.Errorf("external effect call has no durable identity"))
			}
			ctx := contextFromArgs(args)
			effectID := kernel.ToolEffectID(scope.RunID, callID)
			targets, settleOnSuccess := externalEffectTargets(args, store)
			// Claims the person already approved this call to pass in the
			// approval layer's own ask.
			tolerated, _ := args[externalEffectToleratedClaimsArg].([]string)
			claim, err := acquireExternalEffectClaim(ctx, store, scope, args, control.ExternalEffectClaimRequest{
				TenantID: scope.TenantID, PersonID: scope.PersonID, RunID: scope.RunID,
				EffectID: effectID, TargetKeys: targets, ToleratedClaimIDs: append([]string(nil), tolerated...),
			}, grace)
			if err != nil {
				return uninvokedExternalEffect(err)
			}
			if claim.AlreadyKnown {
				return uninvokedExternalEffect(fmt.Errorf("external effect %s was already claimed; inspect its observed state before retrying", callID))
			}
			cleanupCtx := context.WithoutCancel(ctx)
			background, _ := args["background"].(bool)
			background = background && isExecTool(name)
			if err := store.RecordExternalEffectDispatch(cleanupCtx, control.ExternalEffectDispatch{
				TenantID: scope.TenantID, PersonID: scope.PersonID, RunID: scope.RunID, EffectID: effectID,
				ToolCallID: callID, ToolName: name, Summary: externalEffectSummary(name, args), Background: background,
			}); err != nil {
				_ = store.ObserveExternalEffectGroup(cleanupCtx, scope.TenantID, scope.PersonID,
					scope.RunID, effectID, "dispatch:not_started:"+callID)
				return uninvokedExternalEffect(fmt.Errorf("external effect could not record its dispatch: %w", err))
			}
			if err := store.MarkExternalEffectPossible(ctx, scope.TenantID, scope.RunID, effectID); err != nil {
				// The tool has not been called. A canceled request or failed
				// dispatch-boundary write must not strand a reserved target when
				// the store is still available for a separate cleanup write.
				_ = store.ObserveExternalEffectGroup(cleanupCtx, scope.TenantID, scope.PersonID,
					scope.RunID, effectID, "dispatch:not_started:"+callID)
				return uninvokedExternalEffect(fmt.Errorf("external effect could not cross the durable dispatch boundary: %w", err))
			}
			var observer *effectProcessObserver
			if background {
				observer = newEffectProcessObserver(store, scope, effectID, callID)
				args[effectProcessObserverArg] = observer
			}
			result, runErr := next(args)
			return finishExternalEffect(cleanupCtx, store, scope, name, effectID, callID, settleOnSuccess, observer, result, runErr)
		}
	}
}

// acquireExternalEffectClaim resolves every conflict before dispatch. It
// returns a granted (or already known) decision, or the error that stops the
// call without dispatching it.
func acquireExternalEffectClaim(ctx context.Context, store *control.Store, scope ExecutionScope, args map[string]interface{},
	request control.ExternalEffectClaimRequest, grace time.Duration) (control.ExternalEffectClaimDecision, error) {
	deadline := time.Now().Add(grace)
	poll := externalEffectPollStart
	approved := map[string]bool{}
	for _, id := range request.ToleratedClaimIDs {
		approved[id] = true
	}
	for {
		request.DeferWait = time.Now().Before(deadline)
		decision, err := store.ClaimExternalEffects(ctx, request)
		if err != nil {
			return decision, fmt.Errorf("external effect was not dispatched: %w", err)
		}
		switch {
		case decision.Granted || decision.AlreadyKnown:
			return decision, nil
		case decision.NeedsObservation:
			yieldQueuedHostExecution(args)
			return decision, ownUnresolvedEffectError(scope.RunID, decision.BlockingClaims)
		case decision.NeedsApproval:
			yieldQueuedHostExecution(args)
			fresh := false
			for _, claim := range decision.ApprovalClaims {
				fresh = fresh || !approved[claim.ID]
			}
			if !fresh {
				return decision, fmt.Errorf("external effect was not dispatched: an approved claim was not accepted by the claim gate")
			}
			if err := approveForeignExternalEffects(ctx, scope, args, decision.ApprovalClaims); err != nil {
				return decision, err
			}
			for _, claim := range decision.ApprovalClaims {
				if !approved[claim.ID] {
					approved[claim.ID] = true
					request.ToleratedClaimIDs = append(request.ToleratedClaimIDs, claim.ID)
				}
			}
			continue
		case !request.DeferWait:
			yieldQueuedHostExecution(args)
			// The durable wait row is committed: park the Run.
			return decision, newExternalResourcePause(decision)
		}
		yieldQueuedHostExecution(args)
		select {
		case <-ctx.Done():
			return decision, fmt.Errorf("external effect was not dispatched: the wait for run %s ended: %w", decision.BlockedByRun, ctx.Err())
		case <-time.After(poll):
		}
		if poll = poll * 3 / 2; poll > externalEffectPollMax {
			poll = externalEffectPollMax
		}
	}
}

// finishExternalEffect applies the lifecycle after the tool body returns.
func finishExternalEffect(ctx context.Context, store *control.Store, scope ExecutionScope, name, effectID, callID string,
	settleOnSuccess bool, observer *effectProcessObserver, result kernel.ToolDispatchResult, runErr error) (kernel.ToolDispatchResult, error) {
	observation := ""
	notStarted := result.Invoked != nil && !*result.Invoked || result.Process != nil && !result.Process.Started
	switch {
	case notStarted:
		observation = "dispatch:not_started:" + callID
	case observer != nil && observer.attached.Load() && runErr == nil:
		// The process is running; its observed exit settles or keeps the claim.
		return result, nil
	case runErr == nil && settleOnSuccess && callProvedSuccess(name, result):
		observation = "tool:completed:" + callID
	}
	if observation != "" {
		if err := store.ObserveExternalEffectGroup(ctx, scope.TenantID, scope.PersonID, scope.RunID, effectID, observation); err != nil {
			return result, errors.Join(runErr, fmt.Errorf("external effect observation was not saved: %w", err))
		}
		return result, runErr
	}
	outcome, exitCode := externalEffectOutcome(name, result, runErr, settleOnSuccess)
	if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: scope.TenantID,
		PersonID: scope.PersonID, RunID: scope.RunID, EffectID: effectID, ToolCallID: callID,
		Outcome: outcome, ExitCode: exitCode}); err != nil {
		log.Warn("external effect return was not recorded", "run_id", scope.RunID, "effect_id", effectID, "error", err)
	}
	claimID := ""
	if claims, err := store.ExternalEffectClaimsForEffect(ctx, scope.TenantID, scope.PersonID, scope.RunID, effectID); err == nil && len(claims) > 0 {
		claimID = claims[0].ID
	}
	note := uncertainEffectNote(claimID, outcome)
	if runErr != nil {
		return result, &externalEffectNoteError{cause: runErr, note: note}
	}
	result.Output = strings.TrimRight(result.Output, "\n") + "\n\n" + note
	return result, nil
}

// callProvedSuccess: a process must report a known exit status 0; a
// synchronous tool proves success by returning without error. An exec tool
// without process facts proves nothing.
func callProvedSuccess(name string, result kernel.ToolDispatchResult) bool {
	if result.Process != nil {
		return result.Process.Started && result.Process.ExitCode != nil && *result.Process.ExitCode == 0
	}
	return !isExecTool(name)
}

func externalEffectOutcome(name string, result kernel.ToolDispatchResult, runErr error, settleOnSuccess bool) (string, *int) {
	detail := ""
	if runErr != nil {
		detail = truncateRunes(toSingleLine(RedactSensitive(firstLine(runErr.Error()))), 120)
	}
	if result.Process != nil {
		if code := result.Process.ExitCode; code != nil {
			exit := *code
			return fmt.Sprintf("exit %d", exit), &exit
		}
		if detail != "" {
			return "no exit status: " + detail, nil
		}
		return "no exit status", nil
	}
	switch {
	case runErr != nil:
		return "error: " + detail, nil
	case !settleOnSuccess:
		return "returned; its adapter does not prove completion", nil
	case isExecTool(name):
		return "returned without process facts", nil
	}
	return "returned without proof of success", nil
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}

// externalEffectSummary is the bounded, redacted description stored with the
// dispatch record and shown when the claim is named later.
func externalEffectSummary(name string, args map[string]interface{}) string {
	summary := ""
	if isExecTool(name) {
		summary = execCommandPayload(name, args)
	} else {
		encoded, _ := json.Marshal(publicToolArgs(args))
		summary = name + " " + string(encoded)
	}
	return truncateRunes(toSingleLine(RedactSensitive(summary)), 160)
}

// uncertainEffectNote is appended to a call that left its claim uncertain, so
// the model learns at once that the claim holds and how it is released.
func uncertainEffectNote(claimID, outcome string) string {
	if claimID == "" {
		claimID = "<see /effects>"
	}
	return fmt.Sprintf("External effect claim %s is now unresolved: this call ended with %s, which does not prove the effect's result. "+
		"Before another external effect in this work, release it with a successful read-only observation bound to the claim: "+
		"terminal with effect_claim_id=%q running a proven read-only command or an owner-approved observation script, "+
		"or watch_external with effect_claim_id=%q. You judge whether the observation shows the result. "+
		"Otherwise the person can run /effects settle %s [reason]. Do not repeat the effect blindly.",
		claimID, outcome, claimID, claimID, claimID)
}

// externalEffectNoteError appends the claim note to a failed call's error
// while keeping every typed fact of the original failure.
type externalEffectNoteError struct {
	cause error
	note  string
}

func (e *externalEffectNoteError) Error() string { return e.cause.Error() + "\n" + e.note }
func (e *externalEffectNoteError) Unwrap() error { return e.cause }
func (e *externalEffectNoteError) ModelSafeMessage() string {
	var typed interface{ ModelSafeMessage() string }
	if errors.As(e.cause, &typed) {
		if message := strings.TrimSpace(typed.ModelSafeMessage()); message != "" {
			return message + "\n" + e.note
		}
	}
	return e.Error()
}
func (e *externalEffectNoteError) ToolErrorCode() string {
	var typed interface{ ToolErrorCode() string }
	if errors.As(e.cause, &typed) {
		return typed.ToolErrorCode()
	}
	return ""
}
func (e *externalEffectNoteError) ToolErrorCategory() string {
	var typed interface{ ToolErrorCategory() string }
	if errors.As(e.cause, &typed) {
		return typed.ToolErrorCategory()
	}
	return ""
}
func (e *externalEffectNoteError) ToolRecoveryHint() string {
	var typed interface{ ToolRecoveryHint() string }
	if errors.As(e.cause, &typed) {
		return typed.ToolRecoveryHint()
	}
	return ""
}

// firstClaimPerEffect keeps one row per effect group: every target of an
// effect is released together, so one claim id names the group.
func firstClaimPerEffect(claims []control.ExternalEffectClaim) []control.ExternalEffectClaim {
	seen := map[string]bool{}
	var out []control.ExternalEffectClaim
	for _, claim := range claims {
		key := claim.RunID + "\x00" + claim.EffectID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, claim)
	}
	return out
}

// ownUnresolvedEffectError refuses a call while the Run's own chain holds an
// unresolved effect on a conflicting target, and says exactly how to release
// it.
func ownUnresolvedEffectError(runID string, blockers []control.ExternalEffectClaim) error {
	var own []control.ExternalEffectClaim
	for _, claim := range firstClaimPerEffect(blockers) {
		if claim.SameChain {
			own = append(own, claim)
		}
	}
	if len(own) == 0 {
		own = firstClaimPerEffect(blockers)
	}
	if len(own) == 0 {
		return newStableToolRecoveryError(errors.New("external effect remains unresolved"), "external_effect_unresolved", "stale_precondition",
			"An earlier external effect of this work is unresolved. This call was not dispatched. Use /effects to find the claim.",
			"Bind a successful read-only observation to the claim or ask the person to settle it.",
			"admission", "after_observation", "not_dispatched", false, "read_only_observation", "human_settlement")
	}
	var lines []string
	inFlight := false
	for _, claim := range own {
		lines = append(lines, claim.Description())
		inFlight = inFlight || claim.Disposition == control.ExternalClaimInFlight
	}
	first := own[0].ID
	if inFlight {
		message := fmt.Sprintf("This call was not dispatched: an earlier call of this work is still in flight on a conflicting external target (%s). "+
			"A background process settles its claim when it exits 0. Wait for it, or stop it and observe the result, before another external effect.",
			strings.Join(lines, "; "))
		return newStableToolRecoveryError(errors.New(message), "external_effect_in_flight", "stale_precondition", message,
			"Do not start another external effect until the in-flight call finishes or is stopped and observed.",
			"admission", "after_observation", "not_dispatched", false, "wait_for_process", "read_only_observation")
	}
	message := fmt.Sprintf("This call was not dispatched: an earlier external effect of this work is unresolved (%s). "+
		"Release it first with a read-only observation made now and bound to the claim: terminal with effect_claim_id=%q running a proven read-only command "+
		"or an owner-approved observation script, or watch_external with effect_claim_id=%q. A successful bound observation releases the claim; "+
		"you judge whether it shows the effect's result. Otherwise ask the person to run /effects settle %s [reason].",
		strings.Join(lines, "; "), first, first, first)
	return newStableToolRecoveryError(errors.New(message), "external_effect_unresolved", "stale_precondition", message,
		"Do not replay the earlier effect. Observe its result read-only with effect_claim_id, or report the unresolved effect and ask the person to settle it.",
		"admission", "after_observation", "not_dispatched", false, "read_only_observation", "human_settlement")
}

func externalClaimIDs(claims []control.ExternalEffectClaim) []string {
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.ID)
	}
	return ids
}

// foreignEffectApprovalReason names other Runs' unresolved effects in a
// person-facing approval: their runs, redacted call summaries, and states.
func foreignEffectApprovalReason(claims []control.ExternalEffectClaim) string {
	var lines []string
	unknown := false
	for _, claim := range claims {
		unknown = unknown || claim.TargetKey == control.UnknownExternalTarget
	}
	claims = firstClaimPerEffect(claims)
	for _, claim := range claims {
		lines = append(lines, claim.Description())
	}
	scope := "on a potentially conflicting target; the earlier effect's target scope is known"
	if unknown {
		scope = "with unknown target scope; the runtime cannot prove this call is independent"
	}
	reason := "another run left an external effect unresolved " + scope + " (" + strings.Join(lines, "; ") +
		"); approving lets only this call proceed once, stores no permission, and leaves that claim unresolved until its result is observed or settled"
	if len(claims) > 0 {
		reason += ". Review /effects; only if you know the original outcome, use /effects settle " + claims[0].ID + " [reason]"
	}
	return reason
}

// approveForeignExternalEffects asks the person once whether this exact call
// may proceed while another Run's effect stays uncertain. The approval stores
// no grant and settles nothing; a rejection is final for the call.
func approveForeignExternalEffects(ctx context.Context, scope ExecutionScope, args map[string]interface{}, claims []control.ExternalEffectClaim) error {
	// Scope facts need every target row, before one approval per effect is
	// selected. A named row must not hide its group's unknown target.
	effectReason := foreignEffectApprovalReason(claims)
	claims = firstClaimPerEffect(claims)
	ids := externalClaimIDs(claims)
	lines := make([]string, 0, len(claims))
	for _, claim := range claims {
		lines = append(lines, claim.Description())
	}
	toolName := stringArg(args, "_tool_name")
	fingerprint := foreignEffectAuthorizationFingerprint(toolName, args, scope, ids)
	if scope.ResumeAuthorizations != nil && fingerprint != "" {
		_, _, _, claimed, err := scope.ResumeAuthorizations.ClaimApprovalResumeAuthorization(ctx, scope.TenantID, scope.PersonID, scope.RunID, fingerprint)
		if err != nil {
			return fmt.Errorf("claim parked approval authorization: %w", err)
		}
		if claimed {
			return nil
		}
	}
	// This ask releases the exact call, so an exec call's ask states the
	// boundary it runs under as the approval funnel's own ask does: the
	// boundary facts lead the reason every surface truncates, and the
	// containment summary travels. Only an exec call has a boundary to state;
	// another tool's default assessment would claim "no network, no
	// credentials" for a call nothing has assessed.
	reason := effectReason
	containmentSummary := ""
	if isExecTool(toolName) {
		containment := assessExecContainment(toolName, args)
		reason = prependApprovalReason(approvalBoundaryFact(args, containment), reason)
		containmentSummary = containment.Summary()
	}
	if scope.Approval == nil {
		message := "This call was not dispatched: it conflicts with an unresolved external effect of another run (" + strings.Join(lines, "; ") +
			") and needs the person's one-time approval, but no approval channel is available."
		return newStableToolRecoveryError(errors.New(message), "external_effect_approval_unavailable", "human_wait", message,
			"Report the unresolved effect; the person can settle it with /effects settle, or approve when a channel is available.",
			"authorization", "after_human", "not_dispatched", false, "human_settlement")
	}
	decision, err := scope.Approval(ctx, ToolApprovalRequest{
		TenantID: scope.TenantID, PersonID: scope.PersonID, TaskID: scope.TaskID, RunID: scope.RunID, Channel: scope.Channel,
		ToolName: toolName, Reason: reason, Args: approvalDisplayArgs(args),
		Environment: scope.EnvironmentSnapshotID, Cwd: approvalDisplayCwd(scope),
		Containment:    containmentSummary,
		DecisionPolicy: ApprovalDecisionPolicyOnceOnly, AuthorizationFingerprint: fingerprint,
		ExternalEffectClaimIDs: ids,
	})
	if err != nil {
		return err
	}
	if !decision.Approved {
		if decision.Outcome == ApprovalOutcomeTimedOut {
			return &runPauseError{
				cause:  fmt.Errorf("approval timed out with no answer: this call conflicts with another run's unresolved external effect (%s); do not retry a variant, finish waiting_user", strings.Join(ids, ", ")),
				reason: "waiting_user", message: "Work is paused because this action still needs your approval.", needApproval: true,
			}
		}
		text := "operation rejected: the person declined to proceed while another run's external effect is unresolved (" + strings.Join(ids, ", ") + ")"
		if strings.TrimSpace(decision.Reason) != "" {
			text += ": " + decision.Reason
		}
		return rejectOperation(rejectionCodeApproval, text)
	}
	if decision.Scope != "" || strings.TrimSpace(decision.GrantKey) != "" {
		return rejectOperation(rejectionCodeScope, "operation rejected: this request can only be approved once")
	}
	return nil
}

func foreignEffectAuthorizationFingerprint(toolName string, args map[string]interface{}, scope ExecutionScope, claimIDs []string) string {
	base := approvalResumeFingerprint(toolName, args, scope, assessExecContainment(toolName, args).Summary())
	if base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(base + "\x00external-effect-claims\x00" + strings.Join(claimIDs, ",")))
	return fmt.Sprintf("resume:v1:effects:%x", sum[:])
}

// The durable wait row was committed by ClaimExternalEffects before this
// boundary parks the Run. The daemon later resumes its exact parent; no tool
// call is replayed by the wakeup itself.
type externalResourcePause struct {
	blockedByRun string
	// ownWatched holds the claims the Run waits for when every one is an
	// effect of this work's own chain that a bound watcher still observes:
	// no other Run's call is involved.
	ownWatched []control.ExternalEffectClaim
	// decideAfter holds other Runs' left-uncertain claims the call has not
	// passed. The wait does not resolve them: once it ends, the work is
	// blocked until the person settles them or approves the call once.
	decideAfter []control.ExternalEffectClaim
}

func newExternalResourcePause(decision control.ExternalEffectClaimDecision) externalResourcePause {
	pause := externalResourcePause{blockedByRun: decision.BlockedByRun, decideAfter: firstClaimPerEffect(decision.DecideAfterWait)}
	var waited []control.ExternalEffectClaim
	for _, claim := range firstClaimPerEffect(decision.BlockingClaims) {
		switch {
		case claim.Disposition != control.ExternalClaimInFlight && claim.Disposition != control.ExternalClaimWatched:
			// Not waited for: another Run's left-uncertain claim is decided
			// after the wait (decideAfter).
		case !claim.SameChain || claim.Disposition != control.ExternalClaimWatched:
			return pause
		default:
			waited = append(waited, claim)
		}
	}
	pause.ownWatched = waited
	return pause
}

func describeClaims(claims []control.ExternalEffectClaim) string {
	lines := make([]string, 0, len(claims))
	for _, claim := range claims {
		lines = append(lines, claim.Description())
	}
	return strings.Join(lines, "; ")
}

func (p externalResourcePause) Error() string {
	text := fmt.Sprintf("external target is occupied by an in-flight effect of run %s", p.blockedByRun)
	if len(p.ownWatched) > 0 {
		text = fmt.Sprintf("external target is held by an earlier effect of this work that a watcher bound to it still observes (%s)",
			describeClaims(p.ownWatched))
	}
	if len(p.decideAfter) > 0 {
		text += fmt.Sprintf("; another run's effect on it was left unresolved (%s) and still needs the person's decision after the wait",
			describeClaims(p.decideAfter))
	}
	return text + "; this call was not dispatched"
}

func (p externalResourcePause) ToolRunPause() (string, string, bool) {
	waiting := "This run is waiting for an external target that another run's call is still using. The attempted tool call was not dispatched; "
	resumes := "SelfMind resumes this work once the target is released."
	if len(p.ownWatched) > 0 {
		waiting = "This run is waiting for its own earlier external effect, which a watcher bound to it still observes. The attempted tool call was not dispatched; "
		resumes = "SelfMind resumes this work once that watcher releases the target. " +
			"If you already know the effect's result, settle it with /effects settle " + p.ownWatched[0].ID + " [reason]."
	}
	if len(p.decideAfter) > 0 {
		// The wait alone does not let the work continue.
		resumes = "once the target is released, this work still needs your decision on another run's unresolved effect (" +
			describeClaims(p.decideAfter) + "): settle it with /effects settle " + p.decideAfter[0].ID +
			" [reason] and the work continues by itself, or resume it and approve the conflicting call once."
	}
	return "external_resource_wait", waiting + resumes, false
}
func (externalResourcePause) ToolRunPauseStatus() string { return "waiting_external" }
func (externalResourcePause) ToolErrorCode() string      { return "external_resource_wait" }
func (externalResourcePause) ToolErrorCategory() string  { return "external_wait" }
func (p externalResourcePause) ModelSafeMessage() string { return p.Error() }
func (p externalResourcePause) ToolRecoveryHint() string {
	if len(p.decideAfter) > 0 {
		return "The wait ends when the claims it waits for are released. Another run's unresolved effect still holds the target then, " +
			"so the daemon blocks this work with a notice; it continues by itself once the person settles that effect with /effects settle " +
			p.decideAfter[0].ID + ", or the person resumes it and approves the conflicting call once."
	}
	if len(p.ownWatched) > 0 {
		return "The daemon resumes this exact work once the watcher's success releases the claim, or once the person settles it with /effects settle " +
			p.ownWatched[0].ID + ". If the watcher ends without releasing it, this work is blocked until its effect is observed read-only or settled."
	}
	return "The daemon resumes this exact work after every conflicting claim is released: the in-flight call succeeds, a bound observation releases it, or the person settles it. If it is left unresolved instead, this work is blocked until the person settles it or resumes the run."
}
func (externalResourcePause) ToolFailurePhase() string { return "admission" }
func (externalResourcePause) ToolRetryability() string { return "after_observation" }
func (externalResourcePause) ToolEffectState() string  { return "not_dispatched" }
func (externalResourcePause) ToolStateChanged() bool   { return false }

func uninvokedExternalEffect(err error) (kernel.ToolDispatchResult, error) {
	invoked := false
	return kernel.ToolDispatchResult{Invoked: &invoked}, err
}

// effectProcessObserverArg carries the claim gate's exit observer to a tool
// that may start a background process.
const effectProcessObserverArg = "_effect_process_observer"

// effectProcessObserver hands the exit of a background process back to the
// claim lifecycle: exit 0 settles the effect, anything else leaves it
// uncertain. A tool that starts the process attaches it; an unattached
// observer means no exit can be observed.
type effectProcessObserver struct {
	attached atomic.Bool
	store    *control.Store
	scope    ExecutionScope
	effectID string
	callID   string
}

func newEffectProcessObserver(store *control.Store, scope ExecutionScope, effectID, callID string) *effectProcessObserver {
	return &effectProcessObserver{store: store, scope: scope, effectID: effectID, callID: callID}
}

// exitHook marks the observer attached and returns the callback the process
// registry runs after it reaps the process.
func (o *effectProcessObserver) exitHook() func(processID string, exitCode int, waitErr error) {
	if o == nil {
		return nil
	}
	o.attached.Store(true)
	return o.processExited
}

func (o *effectProcessObserver) processExited(processID string, exitCode int, waitErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if waitErr == nil && exitCode == 0 {
		ref := fmt.Sprintf("process:exited:0:%s:%s", processID, o.callID)
		if err := o.store.ObserveExternalEffectGroup(ctx, o.scope.TenantID, o.scope.PersonID, o.scope.RunID, o.effectID, ref); err != nil {
			log.Warn("background effect exit was not saved", "run_id", o.scope.RunID, "effect_id", o.effectID, "error", err)
		}
		return
	}
	outcome := "background process ended without an exit status"
	var exit *int
	if exitCode >= 0 {
		code := exitCode
		exit = &code
		outcome = fmt.Sprintf("background process exit %d", exitCode)
	}
	if err := o.store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: o.scope.TenantID,
		PersonID: o.scope.PersonID, RunID: o.scope.RunID, EffectID: o.effectID, ToolCallID: o.callID,
		Outcome: outcome, ExitCode: exit}); err != nil {
		log.Warn("background effect exit was not recorded", "run_id", o.scope.RunID, "effect_id", o.effectID, "error", err)
	}
}

// externalEffectPossible reports whether a call may change an external
// system and so must claim its targets before dispatch. The call's execution
// policy decides by origin first: a call without a policy may change
// anything, a tool from an external server cannot classify itself, and a
// built-in is classified from its static proof and this call's facts.
func externalEffectPossible(args map[string]interface{}) bool {
	policy, known := args[toolExecutionPolicyArg].(toolExecutionPolicy)
	switch {
	case !known:
		return true
	case policy.Origin != ToolSchemaOriginBuiltin:
		return externalToolEffectPossible(policy)
	}
	return builtinEffectPossible(policy, args)
}

// externalToolEffectPossible classifies a call of a tool from an external
// server, such as an MCP tool. Its own description and schema never prove it
// read-only: only an operator or person read-only marking carried by the
// call's execution policy lets it skip the claim.
func externalToolEffectPossible(policy toolExecutionPolicy) bool {
	return policy.ReadOnlyMarkedBy == ""
}

// builtinEffectPossible classifies a built-in call from its static read-only
// proof, its category, and this call's execution facts.
func builtinEffectPossible(policy toolExecutionPolicy, args map[string]interface{}) bool {
	name := stringArg(args, "_tool_name")
	if isExecTool(name) {
		if name == "watch_external" || name == "verify" {
			// Static proof restricts this built-in to one read-only check.
			return false
		}
		if observationOnlyExec(name, args) || isRemoteObservationCommand(execCommandPayload(name, args)) {
			// A deterministic read command or an exact owner-approved
			// observation script can inspect a held target without acquiring
			// another write claim. This is needed for evidence before release.
			return false
		}
		if effectiveSandboxModeArg(args) == SandboxHost && stringArg(args, effectiveSandboxReasonArg) != "exec sandbox disabled by configuration" {
			return true
		}
		sharedNetwork, _ := args["_network_shared"].(bool)
		credentialRead, _ := args[credentialReadArgKey].(bool)
		return sharedNetwork || credentialRead
	}
	if policy.ReadOnly {
		return false
	}
	if policy.Category == "network" {
		return true
	}
	for _, class := range policy.OperationClasses {
		if class == OpClassNetwork {
			return true
		}
	}
	return false
}

// externalEffectTargets returns the claim's targets and whether a successful
// return settles it. A foreground command and a synchronous tool settle on
// success; a trusted adapter may declare asynchronous completion, and an
// owner-reviewed effect profile that names an observation command keeps its
// claim for that observation.
func externalEffectTargets(args map[string]interface{}, store *control.Store) ([]string, bool) {
	policy, known := args[toolExecutionPolicyArg].(toolExecutionPolicy)
	if !known || policy.Origin != ToolSchemaOriginBuiltin {
		return []string{control.UnknownExternalTarget}, true
	}
	if targets, observed, ok := registeredEffectScript(args, store); ok {
		return targets, !observed
	}
	registry, ok := args["_registry"].(*Registry)
	if !ok || registry == nil {
		return []string{control.UnknownExternalTarget}, true
	}
	tool, ok := registry.Get(stringArg(args, "_tool_name"))
	if !ok {
		return []string{control.UnknownExternalTarget}, true
	}
	provider, ok := tool.(ExternalEffectTargetProvider)
	if !ok {
		return []string{control.UnknownExternalTarget}, true
	}
	keys, completeOnReturn, err := provider.ExternalEffectTargets(args)
	if err != nil || len(keys) == 0 {
		return []string{control.UnknownExternalTarget}, false
	}
	return keys, completeOnReturn
}
