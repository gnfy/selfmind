package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"selfmind/internal/verification"
)

// Adoption records Main's explicit association, not a new execution. Source
// identity, original cursor and observed process facts are re-read on every
// projection. Historical unversioned ordinary output never gains this ability.
type observationAdoption struct {
	Version      int    `json:"version"`
	SourceCursor int64  `json:"source_cursor"`
	SourceCallID string `json:"source_call_id"`
	StepID       string `json:"step_id"`
	Criterion    string `json:"criterion"`
}

func observationCheckTx(ctx context.Context, tx *sql.Tx, runID string, adoption observationAdoption) (verification.Check, error) {
	var raw string
	if adoption.Version != 1 && adoption.Version != 2 {
		return verification.Check{}, fmt.Errorf("unsupported observation adoption")
	}
	if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND cursor=? AND type='evidence.recorded'`, runID, adoption.SourceCursor).Scan(&raw); err != nil {
		return verification.Check{}, err
	}
	var payload struct {
		Evidence workUnitEvidence `json:"evidence"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return verification.Check{}, fmt.Errorf("observation source is unreadable")
	}
	fact := payload.Evidence
	if fact.Version != 1 || fact.ToolCallID != adoption.SourceCallID || fact.Invoked == nil || !*fact.Invoked || fact.FinishedAt < fact.StartedAt || fact.StartedAt <= 0 {
		return verification.Check{}, fmt.Errorf("observation has no supported runtime provenance")
	}
	binding := &verification.Binding{Version: 3, StepID: adoption.StepID, Criterion: adoption.Criterion, Target: adoption.StepID}
	check := verification.Check{ToolCallID: fact.ToolCallID, Binding: binding, Status: fact.Status, StartedAt: fact.StartedAt, FinishedAt: fact.FinishedAt, Invoked: fact.Invoked, EffectState: fact.EffectState}
	switch {
	case fact.Kind == "command" && fact.ToolName == "terminal" && fact.Command != nil && fact.Process != nil && fact.Process.Started && fact.Process.ExitCode != nil:
		check.Command, check.CWD, check.Kind = fact.Command.Command, fact.Command.CWD, fact.Command.Kind
		check.ExitCode = *fact.Process.ExitCode
		check.ProcessStarted = &fact.Process.Started
		check.NetworkDecision, check.NetworkPermission = fact.Process.NetworkDecision, fact.Process.NetworkPermission
		check.Observation = &verification.Observation{Kind: "command", CWD: check.CWD}
		check = verification.WithDispatchFacts(check, fact.Process.ExitCode)
	case fact.Kind == "observation" && (fact.ToolName == "read_file" || fact.ToolName == "search_files") && fact.Observation != nil && fact.Observation.Kind == "file" && fact.Observation.Target != "" && fact.Status == "succeeded":
		check.Observation = fact.Observation
		// A condition can rely on several separately scoped reads. New
		// associations keep its logical identity separate from physical inputs;
		// old recorded associations retain their original target semantics.
		if adoption.Version == 1 {
			binding.Target = fact.Observation.Target
		}
		binding.LocalDependencies = fact.Observation.LocalDependencies
		binding.ResolvedLocalDependencies = fact.Observation.ResolvedLocalDependencies
		binding.UnresolvedLocalDependencies = fact.Observation.UnresolvedLocalDependencies
		check.CWD = fact.Observation.CWD
	default:
		return verification.Check{}, fmt.Errorf("reference %q (%s) lacks a confirmed command result or file read; use the exact tool_call_id of a successful read_file/search_files item, not a batch or mutation receipt", adoption.SourceCallID, fact.ToolName)
	}
	if fact.EffectState == "unknown" || fact.EffectState == "not_dispatched" {
		return verification.Check{}, fmt.Errorf("unconfirmed observation cannot support a condition")
	}
	return check, nil
}

func adoptCompletionObservationsTx(ctx context.Context, tx *sql.Tx, tenant, person, runID string, plan *RunPlan, findings []verification.Assessment) error {
	if plan == nil {
		return nil
	}
	steps := map[string]RunPlanStep{}
	unitID := ""
	for _, step := range plan.Steps {
		if step.WorkUnitID != "" {
			unitID = step.WorkUnitID
		}
		step.WorkUnitID = unitID
		steps[step.StepID] = step
	}
	for _, finding := range findings {
		step, exists := steps[finding.StepID]
		if !exists || len(finding.EvidenceRefs) == 0 {
			continue
		}
		if len(finding.EvidenceRefs) > 8 {
			return &conditionAssessmentInputError{fmt.Errorf("a finding needs at most 8 observation references")}
		}
		for _, ref := range finding.EvidenceRefs {
			var cursor int64
			var kind string
			err := tx.QueryRowContext(ctx, `SELECT cursor,COALESCE(json_extract(payload_json,'$.evidence.kind'),'') FROM task_events WHERE run_id=? AND type='evidence.recorded' AND json_extract(payload_json,'$.evidence.tool_call_id')=? ORDER BY cursor DESC LIMIT 1`, runID, ref).Scan(&cursor, &kind)
			if errors.Is(err, sql.ErrNoRows) || kind == "verification" {
				continue
			}
			if err != nil {
				return err
			}
			start, end, err := workUnitEvidenceWindowTx(ctx, tx, runID, step.WorkUnitID)
			if err != nil {
				return err
			}
			if cursor <= start || cursor > end {
				return &conditionAssessmentInputError{fmt.Errorf("observation %q is outside this step's frozen evidence window", ref)}
			}
			adoption := observationAdoption{Version: 2, SourceCursor: cursor, SourceCallID: ref, StepID: step.StepID, Criterion: step.SuccessCriteria}
			check, err := observationCheckTx(ctx, tx, runID, adoption)
			if err != nil {
				return &conditionAssessmentInputError{err}
			}
			if step.VerificationRequired && finding.Result == "met" && (check.Observation.Kind == "file" || check.Status != "succeeded" || check.ExitCode != 0) {
				return &conditionAssessmentInputError{fmt.Errorf("step %q requires a successful executed check; this observation cannot prove it", step.StepID)}
			}
			key := fmt.Sprintf("observation:%s:%d:%s", runID, cursor, step.StepID)
			var existing string
			err = tx.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND idempotency_key=?`, runID, key).Scan(&existing)
			raw, _ := json.Marshal(adoption)
			if err == nil {
				if existing != string(raw) {
					return &conditionAssessmentInputError{fmt.Errorf("observation %q is already bound under another criterion", ref)}
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var thread, channel string
			if err = tx.QueryRowContext(ctx, `SELECT thread_id,channel FROM runs WHERE tenant_id=? AND person_id=? AND id=?`, tenant, person, runID).Scan(&thread, &channel); err != nil {
				return err
			}
			if _, err = insertScopedEventsTx(ctx, tx, tenant, person, runID, thread, channel, []Event{{Type: "evidence.adopted", Payload: raw, IdempotencyKey: key}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// CurrentRunObservationChecks reads accepted associations through the same
// window projection used by Plan and completion. It never adopts on read.
func (s *Store) CurrentRunObservationChecks(ctx context.Context, tenant, runID string) ([]verification.Check, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := latestRunPlanTx(ctx, tx, normalizeTenant(tenant), runID)
	if err != nil || plan == nil {
		return nil, err
	}
	checks, _, _, err := conditionEvidenceTx(ctx, tx, normalizeTenant(tenant), runID, plan)
	if err != nil {
		return nil, err
	}
	var adopted []verification.Check
	for _, check := range checks {
		if check.Observation != nil {
			adopted = append(adopted, check)
		}
	}
	return adopted, tx.Commit()
}
