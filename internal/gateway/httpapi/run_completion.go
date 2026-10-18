package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

type runCompletionSessionKey struct{}

// One executing Run owns this synchronization, not the Agent or provider.
// result is only a receipt of a completed database transaction; it is not an
// independent writable business state.
type runCompletionSession struct {
	coordinator *RunCoordinator
	identity    *control.IdentityContext
	task        *control.Task
	run         *control.Run
	req         api.MessageRequest
	replay      runMaintenanceReplay
	mu          sync.Mutex
	waiters     map[string]chan error
	result      *api.MessageResponse
}

func completionSession(ctx context.Context) *runCompletionSession {
	s, _ := ctx.Value(runCompletionSessionKey{}).(*runCompletionSession)
	return s
}

func (s *runCompletionSession) flush(ctx context.Context) error {
	ch := kernel.EventChannelFromContext(ctx)
	if ch == nil {
		return fmt.Errorf("completion requires the Run event stream")
	}
	id := uuid.NewString()
	ack := make(chan error, 1)
	s.mu.Lock()
	s.waiters[id] = ack
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiters, id); s.mu.Unlock() }()
	encoded := kernel.EncodeAgentEvent(kernel.AgentEvent{Type: "run.flush", Payload: map[string]interface{}{"barrier_id": id}})
	select {
	case ch <- encoded:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *runCompletionSession) acknowledge(id string, err error) {
	s.mu.Lock()
	ack := s.waiters[id]
	s.mu.Unlock()
	if ack != nil {
		ack <- err
	}
}

func (s *runCompletionSession) CommittedTurn() (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result == nil {
		return "", "", false
	}
	return s.result.Outcome.Status, s.result.Content, true
}

func (s *runCompletionSession) response(usage llm.UsageStats) *api.MessageResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result == nil {
		return nil
	}
	out := *s.result
	out.Usage = usage
	out.Context = s.coordinator.srv.messageContextBudget(usage)
	return &out
}

func (s *runCompletionSession) CompleteRun(ctx context.Context, fields map[string]interface{}, patch []tools.PlanStep) (bool, error) {
	if kernel.DelegationNamespace(ctx) != "" {
		return false, nil
	}
	if err := s.flush(ctx); err != nil {
		return true, fmt.Errorf("%w: %v", errRunResultCommit, err)
	}
	c := s.coordinator
	// Deferred selection must still pass its existing exact-scope handoff path.
	proposal, err := c.latestWorkSelection(ctx, s.identity, s.task, s.run)
	if err != nil {
		return true, err
	}
	if proposal != nil && (proposal.Action != "resume" || s.run.ResumesRunID != proposal.RunID) {
		return false, nil
	}
	scope, _ := kernel.ToolInvocationScopeFromContext(ctx)
	if scope.RunID != s.run.ID || scope.PersonID != s.identity.PersonID {
		return true, fmt.Errorf("completion does not match the executing Run")
	}
	var outcome api.RunOutcome
	raw, err := json.Marshal(fields)
	if err != nil {
		return true, err
	}
	if err = json.Unmarshal(raw, &outcome); err != nil {
		return true, err
	}
	version, _ := kernel.CurrentRunExecutionPlan(ctx)
	input := control.RunCompletionInput{Status: outcome.Status, ExpectedPlanVersion: version, Assessments: outcome.Assessments}
	for _, step := range patch {
		input.PlanPatch = append(input.PlanPatch, control.RunPlanStepInput{
			StepID: step.StepID, Step: step.Step, Status: step.Status, SuccessCriteria: step.SuccessCriteria, VerificationRequired: step.VerificationRequired,
			CancellationDisposition: step.CancellationDisposition, CancellationReason: step.CancellationReason, UserTakeoverQuote: step.UserTakeoverQuote,
			ReplacementStepID: step.ReplacementStepID, ScopeChangeQuote: step.ScopeChangeQuote, ReusePriorVerification: step.ReusePriorVerification, ReuseReason: step.ReuseReason,
			WorkUnitID: step.WorkUnitID, WorkUnit: step.WorkUnit,
		})
	}
	candidate, err := c.srv.Control.PrepareRunCompletion(ctx, s.identity.TenantID, s.identity.PersonID, s.run.ID, input)
	if err != nil {
		return true, completionToolError(err)
	}
	outcome.Assessments = candidate.Conditions
	outcome.CompletionReason = completionReasonForStatus(outcome.Status)
	verdict, files := c.evidenceOutcome(ctx, s.identity.TenantID, s.run.ID)
	if len(candidate.ObservationChecks) > 0 {
		if verdict == nil {
			verdict = &api.VerificationOutcome{}
		}
		for _, check := range candidate.ObservationChecks {
			if check.Observation.Kind != "command" {
				continue
			}
			check.NetworkMayFetch = tools.CommandMayFetchDependencies(check.Command)
			found := false
			for i, old := range verdict.Checks {
				if old.Source == nil && old.ToolCallID == check.ToolCallID {
					verdict.Checks[i] = check
					found = true
					break
				}
			}
			if !found {
				verdict.Checks = append(verdict.Checks, check)
			}
		}
		if verdict.State != "blocked" {
			verdict.State, verdict.Summary = verification.StateWithMutations(candidate.Mutations, verdict.Checks)
		}
	}
	if verdict != nil && len(candidate.Conditions) > 0 {
		applyConditionProjection(verdict, candidate.Assessment, nil)
	}
	outcome.Verification, outcome.Files = verdict, files
	if outcome.Status == "done" && verificationRequiresResume(verdict, files) {
		return true, &completionFailure{error: fmt.Errorf("completion requires verification evidence: %s", verdict.Summary)}
	}
	body, _ := fields["final_response"].(string)
	body = withVerificationNotice(body, verdict, verificationClaimMismatches(outcome))
	body = withCompletionNotice(body, outcome)
	body = c.withExternalEffectFacts(ctx, s.identity.TenantID, s.identity.PersonID, s.run.ID, body, &outcome)
	handoff := control.Handoff{TaskID: s.task.ID, Summary: outcome.Summary, DoneItems: outcome.Done, NextSteps: outcome.NextSteps, ChangedFiles: outcome.Files, TestStatus: strings.Join(outcome.Tests, "\n"), Risks: outcome.Risks}
	storedRaw, _ := json.Marshal(outcome)
	var storedOutcome map[string]interface{}
	_ = json.Unmarshal(storedRaw, &storedOutcome)
	storedOutcome["final_response"] = body
	event := control.Event{Type: "run.finished", Visibility: "task", Channel: s.req.Channel, Payload: mustJSON(map[string]interface{}{"outcome": storedOutcome})}
	err = c.materializeRunFinalization(ctx, s.identity, s.task, s.run, taskStatusForFinalization(outcome, true), s.replay.WorkspaceID, s.replay.UserInput, s.req.Channel, body, outcome, s.replay.Attach, handoff, event, candidate)
	if err != nil {
		return true, completionToolError(err)
	}
	// No fallible operation below can invalidate the committed result.
	fields["final_response"] = body
	fields["completion_reason"] = outcome.CompletionReason
	savedRun := *s.run
	savedRun.Status = outcome.Status
	result := &api.MessageResponse{Identity: s.identity, Task: s.task, Run: &savedRun, Outcome: &outcome, Content: body, Turn: messageTurn(turnStatusForOutcome(outcome), taskStatusForFinalization(outcome, true), "idle", s.task.ID, s.run.ID, outcome.Summary)}
	s.mu.Lock()
	s.result = result
	s.mu.Unlock()
	if len(patch) > 0 {
		if receipt, ok := candidate.CommittedReceipt(); ok {
			var payload struct {
				AcceptedPlan *control.RunPlan `json:"accepted_plan"`
			}
			if json.Unmarshal(receipt.Payload, &payload) == nil && payload.AcceptedPlan != nil {
				plan := payload.AcceptedPlan
				raw, _ := json.Marshal(plan.Steps)
				var items []kernel.PlanItem
				_ = json.Unmarshal(raw, &items)
				kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{Type: "plan.updated", Plan: items, Payload: map[string]interface{}{"plan_version": plan.Version, "explanation": "Final progress"}})
			}
		}
	}

	return true, nil
}

func completionReasonForStatus(status string) string {
	if status == "done" {
		return "completed"
	}
	return status
}

type completionFailure struct {
	error
	input   bool
	changed bool
}

func (e *completionFailure) ToolErrorCode() string {
	if e.changed {
		return "completion_context_changed"
	}
	if e.input {
		return "condition_assessment_invalid"
	}
	return "completion_precondition"
}
func (*completionFailure) ToolFailurePhase() string   { return "preparation" }
func (*completionFailure) ToolEffectState() string    { return "not_dispatched" }
func (*completionFailure) ToolStateChanged() bool     { return false }
func (*completionFailure) ToolAlternatives() []string { return nil }
func (*completionFailure) ToolErrorCategory() string  { return "stale_precondition" }
func (e *completionFailure) ModelSafeMessage() string { return tools.RedactSensitive(e.Error()) }
func (e *completionFailure) Unwrap() error            { return e.error }
func (e *completionFailure) ToolRetryability() string {
	if e.input {
		return "corrected_input"
	}
	return "after_state_change"
}
func (*completionFailure) ToolRecoveryHint() string {
	return "Review the accepted Plan and evidence. Correct input without rerunning checks; observe again only when evidence is missing or stale."
}

func completionToolError(err error) error {
	if input, ok := err.(interface{ CurrentPlanStepIDs() []string }); ok && len(input.CurrentPlanStepIDs()) > 0 {
		return &completionFailure{error: err, input: true}
	}
	if precondition, ok := err.(interface{ PlanCancellationPrecondition() bool }); ok && precondition.PlanCancellationPrecondition() {
		return &completionFailure{error: err, input: true}
	}
	if precondition, ok := err.(interface{ PlanVerificationPrecondition() bool }); ok && precondition.PlanVerificationPrecondition() {
		return &completionFailure{error: err}
	}
	if input, ok := err.(interface{ ConditionAssessmentInput() bool }); ok && input.ConditionAssessmentInput() {
		return &completionFailure{error: err, input: true}
	}
	if changed, ok := err.(interface{ CompletionContextChanged() bool }); ok && changed.CompletionContextChanged() {
		return &completionFailure{error: err, changed: true}
	}
	if precondition, ok := err.(interface{ CompletionPrecondition() bool }); ok && precondition.CompletionPrecondition() {
		return &completionFailure{error: err}
	}
	return &completionCommitError{err}
}

type completionCommitError struct{ error }

func (e *completionCommitError) Unwrap() error { return e.error }
func (e *completionCommitError) ToolRunError() error {
	if errors.Is(e.error, control.ErrRunCancellationRequested) {
		return context.Canceled
	}
	return fmt.Errorf("%w: %v", errRunResultCommit, e.error)
}
