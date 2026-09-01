package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	RunRecoveryModeContinue   = "continue"
	RunRecoveryModeVerifyOnly = "verify_only"
)

// AutomaticRunRecoveryDecision is the control plane's complete answer to
// whether one interrupted Run may create a daemon-owned child. The gateway
// consumes this projection; it does not reconstruct eligibility from prose.
type AutomaticRunRecoveryDecision struct {
	Eligible         bool
	Mode             string
	Cause            string
	Reason           string
	UncertainEffects []ToolLedgerEntry
}

// AutomaticRunRecoveryDecisionForRun fails closed. Historical Runs, Runs
// already claimed by a child, specialist-owned interactions, recovery children,
// and Runs with a known external side effect never enter generic auto-resume.
func (s *Store) AutomaticRunRecoveryDecisionForRun(ctx context.Context, tenantID, runID string) (AutomaticRunRecoveryDecision, error) {
	decision := AutomaticRunRecoveryDecision{}
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" {
		return decision, nil
	}
	tenantID = normalizeTenant(tenantID)
	var status string
	var contractVersion int
	if err := s.db.QueryRowContext(ctx, `SELECT status, recovery_contract_version FROM task_runs WHERE tenant_id=? AND id=?`,
		tenantID, runID).Scan(&status, &contractVersion); err != nil {
		if err == sql.ErrNoRows {
			return decision, nil
		}
		return decision, err
	}
	if contractVersion < RunRecoveryContractVersion {
		decision.Reason = "historical_recovery_contract"
		return decision, nil
	}
	if status != "interrupted" {
		decision.Reason = "run_not_interrupted"
		return decision, nil
	}

	var childCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_runs WHERE tenant_id=? AND parent_run_id=?`, tenantID, runID).Scan(&childCount); err != nil {
		return decision, err
	}
	if childCount > 0 {
		decision.Reason = "parent_already_claimed"
		return decision, nil
	}

	var terminalPayload string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(payload_json, '{}') FROM task_events
		WHERE run_id=? AND type='run.interrupted' ORDER BY cursor DESC LIMIT 1`, runID).Scan(&terminalPayload); err != nil {
		if err == sql.ErrNoRows {
			decision.Reason = "missing_interruption_outcome"
			return decision, nil
		}
		return decision, err
	}
	var terminal struct {
		Outcome struct {
			CompletionReason string `json:"completion_reason"`
			Resumable        bool   `json:"resumable"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(terminalPayload), &terminal); err != nil {
		decision.Reason = "invalid_interruption_outcome"
		return decision, nil
	}
	decision.Cause = strings.TrimSpace(terminal.Outcome.CompletionReason)
	if !terminal.Outcome.Resumable || (decision.Cause != "daemon_recovery" && decision.Cause != "provider_or_transport_error") {
		decision.Reason = "interruption_not_auto_recoverable"
		return decision, nil
	}

	var startedPayload string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(payload_json, '{}') FROM task_events
		WHERE run_id=? AND type='run.started' ORDER BY cursor ASC LIMIT 1`, runID).Scan(&startedPayload)
	var started struct {
		Origin string `json:"origin"`
	}
	_ = json.Unmarshal([]byte(startedPayload), &started)
	switch strings.TrimSpace(started.Origin) {
	case "recovery":
		decision.Reason = "automatic_recovery_already_attempted"
		return decision, nil
	case "approval", "watch":
		decision.Reason = "specialist_origin"
		return decision, nil
	}

	// A row-specific mechanism remains the only owner whenever this Run
	// participated in an approval, clarification, or durable watcher.
	for _, check := range []struct {
		query  string
		reason string
	}{
		{`SELECT COUNT(*) FROM approval_requests WHERE tenant_id=? AND run_id=?`, "approval_recovery_owns_run"},
		{`SELECT COUNT(*) FROM clarify_requests WHERE tenant_id=? AND run_id=?`, "clarification_recovery_owns_run"},
		{`SELECT COUNT(*) FROM external_watches WHERE tenant_id=? AND run_id=?`, "watcher_recovery_owns_run"},
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, check.query, tenantID, runID).Scan(&count); err != nil {
			return decision, fmt.Errorf("classify specialist recovery: %w", err)
		}
		if count > 0 {
			decision.Reason = check.reason
			return decision, nil
		}
	}

	uncertain, err := s.ListUncertainToolEntries(ctx, tenantID, runID, 100)
	if err != nil {
		return decision, err
	}
	if len(uncertain) > 0 {
		decision.Eligible = true
		decision.Mode = RunRecoveryModeVerifyOnly
		decision.Reason = "uncertain_effect_requires_observation"
		decision.UncertainEffects = uncertain
		return decision, nil
	}

	// Known external mutations are not uncertain, but a new model turn could
	// still repeat them with a new call id. Automatic continuation is therefore
	// limited to Runs that never dispatched such an effect. Lifecycle projection
	// calls are control-plane bookkeeping and do not count as external effects.
	var sideEffects int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger
		WHERE tenant_id=? AND run_id=? AND retry_class<>'read_only'
		  AND tool_name NOT IN ('update_plan', 'finish_run')
		  AND status IN ('started', 'dispatched', 'completed', 'failed')`, tenantID, runID).Scan(&sideEffects); err != nil {
		return decision, err
	}
	if sideEffects > 0 {
		decision.Reason = "known_effect_requires_user_resume"
		return decision, nil
	}

	decision.Eligible = true
	decision.Mode = RunRecoveryModeContinue
	decision.Reason = "safe_pre_effect_interruption"
	return decision, nil
}
