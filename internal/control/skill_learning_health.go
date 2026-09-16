package control

import (
	"context"
	"time"
)

// SkillLearningHealth describes the current bounded selection window, not a
// replay of historical readiness decisions or an automatic publication verdict.
type SkillLearningHealth struct {
	WindowLimit, Observations, ProceduralSuccesses, VerifiedSuccesses int
	MaxIndependentSuccesses, Activations                              int
	OldestAt, NewestAt                                                time.Time
	Reasons, Versions, Jobs                                           map[string]int
}

// Keep diagnostics and the actual candidate gate on the same predicate.
func skillEvidenceReadinessReason(cohort SkillEvidenceDigest, anchor WorkflowObservation) string {
	if cohort.TargetSkillKey == "" {
		if independentWorkflowRuns(cohort.SuccessObservations) >= 3 {
			return "ready"
		}
		return "insufficient_independent_successes"
	}
	if cohort.TargetSkillName != "" && cohort.TargetActiveContent != "" &&
		cohort.ParentVersionHash == anchor.VersionHash && digestHasVerifiedRepairIncidentForObservation(cohort, anchor.ID) &&
		SkillRepairCandidateEvidenceReady(cohort) {
		return "ready"
	}
	if reason := repairEvidenceSkipReason(cohort, anchor.ID); reason != "" {
		return reason
	}
	return "no_eligible_verified_repair"
}

func (s *Store) SkillLearningHealthForWorkspace(ctx context.Context, tenantID, personID, workspaceID string, curatorVersion int) (SkillLearningHealth, error) {
	h := SkillLearningHealth{WindowLimit: workflowCohortWindow, Reasons: map[string]int{}, Versions: map[string]int{}, Jobs: map[string]int{}}
	tenantID = normalizeTenant(tenantID)
	source, err := s.loadWorkflowCohortSource(ctx, WorkflowObservation{IdentityTenantID: tenantID, PersonID: personID, WorkspaceID: workspaceID})
	if err != nil {
		return h, err
	}
	h.Observations = len(source.candidates)
	for _, anchor := range source.candidates {
		if h.NewestAt.IsZero() || anchor.CreatedAt.After(h.NewestAt) {
			h.NewestAt = anchor.CreatedAt
		}
		if h.OldestAt.IsZero() || anchor.CreatedAt.Before(h.OldestAt) {
			h.OldestAt = anchor.CreatedAt
		}
		if workflowOriginExcludedFromCuration(source.origins[anchor.RunID]) {
			h.Reasons["continuation_observation_excluded"]++
			continue
		}
		if anchor.EvidenceRole == "success_path" && len(anchor.ToolSequence) > 0 {
			h.ProceduralSuccesses++
			if anchor.VerificationState == "passed" {
				h.VerifiedSuccesses++
			}
		}
		cohort, err := s.comparableWorkflowCohortFromSource(ctx, anchor, source, 5, 5)
		if err != nil {
			return h, err
		}
		if anchor.SkillKey == "" {
			h.MaxIndependentSuccesses = max(h.MaxIndependentSuccesses, independentWorkflowRuns(cohort.SuccessObservations))
		}
		h.Reasons[skillEvidenceReadinessReason(cohort, anchor)]++
	}
	// Version totals are attributed by frozen evidence, never by tenant alone:
	// another person's candidates must not appear as this person's learning.
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM skill_versions
		WHERE control_tenant_id=? AND json_valid(evidence_json)
		AND json_extract(evidence_json,'$.identity_tenant_id')=?
		AND json_extract(evidence_json,'$.person_id')=?
		AND COALESCE(json_extract(evidence_json,'$.workspace_id'),'')=? GROUP BY state`, tenantID, tenantID, personID, workspaceID)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return h, err
		}
		h.Versions[state] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return h, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM maintenance_jobs
		WHERE tenant_id=? AND analyzer_version=? AND json_valid(payload_json)
		AND json_extract(payload_json,'$.person_id')=?
		AND COALESCE(json_extract(payload_json,'$.workspace_id'),'')=? GROUP BY status`, tenantID, curatorVersion, personID, workspaceID)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return h, err
		}
		h.Jobs[state] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return h, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_skill_activations WHERE identity_tenant_id=? AND person_id=? AND workspace_id=?`, tenantID, personID, workspaceID).Scan(&h.Activations)
	return h, err
}
