package control

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"selfmind/internal/platform/log"
)

// SkillEvidenceDigest is the bounded deterministic input to the sole skill
// curator. Similarity may nominate observations, but only this comparable
// cohort gate can authorize a candidate proposal.
type SkillEvidenceDigest struct {
	EvidenceSetHash      string                `json:"evidence_set_hash"`
	WorkflowSignature    string                `json:"workflow_signature"`
	IdentityTenantID     string                `json:"identity_tenant_id"`
	ControlTenantID      string                `json:"control_tenant_id"`
	PersonID             string                `json:"person_id"`
	WorkspaceID          string                `json:"workspace_id,omitempty"`
	PublicationScope     string                `json:"publication_scope,omitempty"`
	TargetSkillKey       string                `json:"target_skill_key,omitempty"`
	TargetSkillName      string                `json:"target_skill_name,omitempty"`
	TargetActiveContent  string                `json:"target_active_content,omitempty"`
	ParentVersionHash    string                `json:"parent_version_hash,omitempty"`
	SuccessObservations  []WorkflowObservation `json:"success_observations"`
	NegativeObservations []WorkflowObservation `json:"negative_observations,omitempty"`
	ExpectedSavings      map[string]int64      `json:"expected_savings,omitempty"`
	PromptSnapshotHash   string                `json:"prompt_snapshot_hash,omitempty"`
}

func (s *Store) ReadySkillEvidenceDigestsForRun(ctx context.Context, tenantID, runID string) ([]SkillEvidenceDigest, error) {
	origins, err := s.workflowRunOrigins(ctx, []string{runID})
	if err != nil {
		return nil, err
	}
	if workflowOriginExcludedFromCuration(origins[runID]) {
		return nil, nil
	}
	anchors, err := s.workflowObservationsForRun(ctx, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, nil
	}
	source, err := s.loadWorkflowCohortSource(ctx, anchors[0])
	if err != nil {
		return nil, err
	}
	var out []SkillEvidenceDigest
	seenEvidence := map[string]bool{}
	for _, anchor := range anchors {
		cohort, err := s.comparableWorkflowCohortFromSource(ctx, anchor, source, 5, 5)
		if err != nil {
			return nil, err
		}
		createReady := cohort.TargetSkillKey == "" && independentWorkflowRuns(cohort.SuccessObservations) >= 3
		repairReady := cohort.TargetSkillKey != "" && cohort.TargetSkillName != "" && cohort.TargetActiveContent != "" &&
			cohort.ParentVersionHash == anchor.VersionHash && digestHasVerifiedRepairIncidentForObservation(cohort, anchor.ID) &&
			SkillRepairCandidateEvidenceReady(cohort)
		if !createReady && !repairReady {
			if reason := repairEvidenceSkipReason(cohort, anchor.ID); reason != "" {
				log.Info("skill repair evidence skipped", "run", anchor.RunID, "work_unit", anchor.WorkUnitID, "reason", reason)
			}
			continue
		}
		ids := make([]string, 0, len(cohort.SuccessObservations)+len(cohort.NegativeObservations))
		for _, observation := range cohort.SuccessObservations {
			ids = append(ids, observation.ID)
		}
		for _, observation := range cohort.NegativeObservations {
			ids = append(ids, observation.ID)
		}
		sort.Strings(ids)
		cohort.EvidenceSetHash = stableEvolutionHash(ids)
		if seenEvidence[cohort.EvidenceSetHash] {
			continue
		}
		seenEvidence[cohort.EvidenceSetHash] = true
		var existing int
		if err := s.db.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM skill_versions WHERE control_tenant_id=? AND evidence_set_hash=?) +
			(SELECT COUNT(*) FROM skill_candidate_evidence_snapshots WHERE control_tenant_id=? AND evidence_set_hash=?)`,
			cohort.ControlTenantID, cohort.EvidenceSetHash, cohort.ControlTenantID, cohort.EvidenceSetHash).Scan(&existing); err != nil {
			return nil, err
		}
		if existing > 0 {
			continue
		}
		cohort.ExpectedSavings = expectedWorkflowSavings(cohort.SuccessObservations)
		out = append(out, cohort)
	}
	return out, nil
}

func (s *Store) workflowObservationsForRun(ctx context.Context, tenantID, runID string) ([]WorkflowObservation, error) {
	rows, err := s.db.QueryContext(ctx, workflowObservationSelect+`
		FROM workflow_observations WHERE identity_tenant_id=? AND run_id=? ORDER BY created_at DESC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowObservation
	for rows.Next() {
		observation, err := scanWorkflowObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *observation)
	}
	return out, rows.Err()
}

func independentWorkflowRuns(observations []WorkflowObservation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		if observation.RunID != "" {
			seen[observation.RunID] = true
		}
	}
	return len(seen)
}

const workflowObservationSelect = `SELECT id, identity_tenant_id, control_tenant_id, person_id,
	workspace_id, run_id, work_unit_id, related_task_id, workflow_signature, goal_digest,
	environment_fingerprint, skill_key, version_hash, activation_state, outcome_status,
	verification_state, tool_sequence_json, tool_failures, provider_calls, duration_ms,
	input_tokens, output_tokens, user_corrected, evidence_role, created_at`

type workflowCohortSource struct {
	candidates []WorkflowObservation
	origins    map[string]string
	evidence   map[string]map[string]*observationAccumulator
}

func (s *Store) loadWorkflowCohortSource(ctx context.Context, anchor WorkflowObservation) (workflowCohortSource, error) {
	rows, err := s.db.QueryContext(ctx, workflowObservationSelect+`
		FROM workflow_observations WHERE identity_tenant_id=? AND person_id=? AND workspace_id=?
		ORDER BY created_at DESC LIMIT 48`, anchor.IdentityTenantID, anchor.PersonID, anchor.WorkspaceID)
	if err != nil {
		return workflowCohortSource{}, err
	}
	var candidates []WorkflowObservation
	for rows.Next() {
		observation, scanErr := scanWorkflowObservation(rows)
		if scanErr != nil {
			rows.Close()
			return workflowCohortSource{}, scanErr
		}
		candidates = append(candidates, *observation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return workflowCohortSource{}, err
	}
	rows.Close()
	runIDs := make([]string, 0, len(candidates))
	for _, observation := range candidates {
		runIDs = append(runIDs, observation.RunID)
	}
	origins, err := s.workflowRunOrigins(ctx, runIDs)
	if err != nil {
		return workflowCohortSource{}, err
	}
	evidence, err := s.workflowEvidenceForRuns(ctx, anchor.IdentityTenantID, runIDs)
	if err != nil {
		return workflowCohortSource{}, err
	}
	return workflowCohortSource{candidates: candidates, origins: origins, evidence: evidence}, nil
}

func (s *Store) comparableWorkflowCohort(ctx context.Context, anchor WorkflowObservation, successLimit, negativeLimit int) (SkillEvidenceDigest, error) {
	source, err := s.loadWorkflowCohortSource(ctx, anchor)
	if err != nil {
		return SkillEvidenceDigest{}, err
	}
	return s.comparableWorkflowCohortFromSource(ctx, anchor, source, successLimit, negativeLimit)
}

func (s *Store) comparableWorkflowCohortFromSource(ctx context.Context, anchor WorkflowObservation, source workflowCohortSource, successLimit, negativeLimit int) (SkillEvidenceDigest, error) {
	digest := SkillEvidenceDigest{
		WorkflowSignature: anchor.WorkflowSignature, IdentityTenantID: anchor.IdentityTenantID,
		ControlTenantID: anchor.ControlTenantID, PersonID: anchor.PersonID, WorkspaceID: anchor.WorkspaceID,
		TargetSkillKey: anchor.SkillKey, ParentVersionHash: anchor.VersionHash,
	}
	if strings.TrimSpace(anchor.SkillKey) == "" && strings.TrimSpace(anchor.WorkspaceID) != "" {
		digest.PublicationScope = "workspace"
	} else {
		digest.PublicationScope = "user"
	}
	for _, observation := range source.candidates {
		if workflowOriginExcludedFromCuration(source.origins[observation.RunID]) || !workflowObservationsComparable(anchor, observation) {
			continue
		}
		metrics := source.evidence[observation.RunID]
		if metric := metrics[observation.WorkUnitID]; metric != nil {
			observation.ToolEvidence = append([]WorkflowToolEvidence(nil), metric.toolEvidence...)
			attachObservationIncident(&observation, metric)
		}
		if observation.EvidenceRole == "success_path" && len(observation.ToolSequence) > 0 && len(digest.SuccessObservations) < successLimit {
			digest.SuccessObservations = append(digest.SuccessObservations, observation)
		} else if observation.EvidenceRole == "failure_guard" && len(digest.NegativeObservations) < negativeLimit {
			digest.NegativeObservations = append(digest.NegativeObservations, observation)
		}
	}
	if digest.TargetSkillKey != "" {
		active, activeErr := s.ActiveSkillVersion(ctx, digest.ControlTenantID, digest.TargetSkillKey)
		if activeErr != nil {
			return SkillEvidenceDigest{}, activeErr
		}
		if active != nil && active.VersionHash == anchor.VersionHash && active.CreatedBy == "skill_curator" && skillRepairContentTopologyEligible(active.ContentBody) {
			digest.TargetSkillName = active.SkillName
			digest.TargetActiveContent = active.ContentBody
			digest.ParentVersionHash = active.VersionHash
			var parentEvidence SkillEvidenceDigest
			if json.Unmarshal(active.Evidence, &parentEvidence) == nil &&
				(parentEvidence.PublicationScope == "workspace" || parentEvidence.PublicationScope == "user") {
				digest.PublicationScope = parentEvidence.PublicationScope
			}
		} else {
			digest.ParentVersionHash = ""
		}
	}
	return digest, nil
}

func skillRepairContentTopologyEligible(content string) bool {
	want := CanonicalSkillSectionOrder()
	var got []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			got = append(got, strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))))
		}
	}
	return len(got) == len(want) && strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func (s *Store) workflowEvidenceForRuns(ctx context.Context, tenantID string, runIDs []string) (map[string]map[string]*observationAccumulator, error) {
	uniqueRunIDs := uniqueSortedStrings(runIDs)
	unitsByRun := make(map[string][]RunWorkUnit, len(uniqueRunIDs))
	eventsByRun := make(map[string][]evolutionEvent, len(uniqueRunIDs))
	for _, chunk := range chunkStrings(uniqueRunIDs, 300) {
		unitRows, err := s.db.QueryContext(ctx, `SELECT id, identity_tenant_id, person_id, workspace_id,
			run_id, sequence, primary_task_id, related_task_id, goal_digest, plan_status, status,
			outcome_summary, verification_state, verification_refs_json, started_at, created_at, finished_at,
			started_cursor, finished_cursor
			FROM run_work_units WHERE identity_tenant_id=? AND run_id IN (`+placeholders(len(chunk))+`)
			ORDER BY run_id, sequence`, append([]interface{}{normalizeTenant(tenantID)}, toAnySlice(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for unitRows.Next() {
			unit, scanErr := scanRunWorkUnit(unitRows)
			if scanErr != nil {
				unitRows.Close()
				return nil, scanErr
			}
			unitsByRun[unit.RunID] = append(unitsByRun[unit.RunID], *unit)
		}
		if err := unitRows.Err(); err != nil {
			unitRows.Close()
			return nil, err
		}
		unitRows.Close()

		eventRows, err := s.db.QueryContext(ctx, `SELECT run_id, type, COALESCE(payload_json,'{}'), created_at
			FROM task_events WHERE run_id IN (`+placeholders(len(chunk))+`) AND type IN (
			'run.started', 'skill.activated', 'tool.started', 'tool.completed',
			'evolution.batch_item', 'provider.call.usage', 'plan.updated',
			'run.outcome', 'run.finished', 'run.steering_consumed', 'skill.fallback'
			) ORDER BY run_id, COALESCE(cursor, 0), created_at, rowid`, toAnySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for eventRows.Next() {
			var eventRunID, typ, raw string
			var created int64
			if err := eventRows.Scan(&eventRunID, &typ, &raw, &created); err != nil {
				eventRows.Close()
				return nil, err
			}
			var payload map[string]interface{}
			_ = json.Unmarshal([]byte(raw), &payload)
			eventsByRun[eventRunID] = append(eventsByRun[eventRunID], evolutionEvent{typ: typ, payload: payload, createdAt: created})
		}
		if err := eventRows.Err(); err != nil {
			eventRows.Close()
			return nil, err
		}
		eventRows.Close()
	}
	evidence := make(map[string]map[string]*observationAccumulator, len(uniqueRunIDs))
	for _, evidenceRunID := range uniqueRunIDs {
		units := unitsByRun[evidenceRunID]
		if len(units) == 0 {
			evidence[evidenceRunID] = map[string]*observationAccumulator{}
			continue
		}
		evidence[evidenceRunID] = accumulateWorkUnitEvents(units, eventsByRun[evidenceRunID])
	}
	return evidence, nil
}

func digestHasVerifiedRepairIncidentForObservation(digest SkillEvidenceDigest, observationID string) bool {
	for _, observation := range digest.NegativeObservations {
		if observation.ID != observationID || !VerifiedSkillRepairIncident(observation) {
			continue
		}
		return true
	}
	return false
}

// VerifiedSkillRepairIncident reports whether one negative observation carries a
// directly attributable Skill defect plus a verified same-work-unit recovery. It
// is the single definition of that gate: readiness selection here and curator
// publication in the app layer must not drift apart.
func VerifiedSkillRepairIncident(observation WorkflowObservation) bool {
	incident := observation.Incident
	_, categoryEligible := NormalizeSkillRepairErrorCategory(incidentCategory(incident))
	return incident != nil && incident.RecoveryVerified && incident.FailureObserved &&
		SkillRepairObservedFailureEligible(incident.ErrorCategory, incident.ObservedErrorCategory) &&
		strings.TrimSpace(incident.FailureSignature) != "" &&
		strings.TrimSpace(incident.FailedStepID) != "" && categoryEligible
}

const (
	SkillRepairClassDeterministicInterface = "deterministic_interface"
	SkillRepairClassStablePrecondition     = "stable_precondition"
	SkillRepairClassSemantic               = "semantic"
	SkillRepairClassNotApplicable          = "not_applicable"
	SkillRepairClassTransientEnvironment   = "transient_environment"
)

// ClassifySkillRepairIncident combines the model's narrow repair hypothesis
// with daemon-observed tool failure evidence. The model cannot select the
// automatic threshold by itself.
func ClassifySkillRepairIncident(incident *SkillIncidentEvidence) string {
	if incident == nil || !SkillRepairObservedFailureEligible(incident.ErrorCategory, incident.ObservedErrorCategory) {
		return SkillRepairClassTransientEnvironment
	}
	repairCategory, _ := NormalizeSkillRepairErrorCategory(incident.ErrorCategory)
	observedCategory := strings.ToLower(strings.TrimSpace(incident.ObservedErrorCategory))
	switch repairCategory {
	case "schema_changed":
		return SkillRepairClassDeterministicInterface
	case "invalid_procedure":
		switch observedCategory {
		case "tool_schema", "interface_drift", "syntax":
			return SkillRepairClassDeterministicInterface
		default:
			return SkillRepairClassSemantic
		}
	case "stale_precondition":
		if observedCategory == "interface_drift" {
			return SkillRepairClassDeterministicInterface
		}
		if observedCategory == "not_found" {
			return SkillRepairClassStablePrecondition
		}
		return SkillRepairClassSemantic
	case "verification_mismatch":
		return SkillRepairClassSemantic
	case "missing_failure_guard":
		return SkillRepairClassNotApplicable
	default:
		return SkillRepairClassTransientEnvironment
	}
}

// SkillRepairCandidateEvidenceReady decides whether an attributable incident
// may materialize an immutable proposal. Semantic and not-applicable incidents
// can be reviewed as candidates before they satisfy automatic publication.
func SkillRepairCandidateEvidenceReady(digest SkillEvidenceDigest) bool {
	for _, observation := range digest.NegativeObservations {
		if !VerifiedSkillRepairIncident(observation) {
			continue
		}
		switch ClassifySkillRepairIncident(observation.Incident) {
		case SkillRepairClassDeterministicInterface, SkillRepairClassStablePrecondition,
			SkillRepairClassSemantic, SkillRepairClassNotApplicable:
			return true
		}
	}
	return false
}

// SkillRepairAutomaticPromotionReady applies class-specific publication
// thresholds to a frozen comparable cohort.
func SkillRepairAutomaticPromotionReady(digest SkillEvidenceDigest) bool {
	semanticRunsBySignature := map[string]map[string]bool{}
	for _, observation := range digest.NegativeObservations {
		if !VerifiedSkillRepairIncident(observation) {
			continue
		}
		incident := observation.Incident
		switch ClassifySkillRepairIncident(incident) {
		case SkillRepairClassDeterministicInterface:
			return true
		case SkillRepairClassStablePrecondition:
			if digest.PublicationScope == "workspace" && strings.TrimSpace(digest.WorkspaceID) != "" {
				return true
			}
		case SkillRepairClassSemantic:
			signature := strings.TrimSpace(incident.FailureSignature)
			runID := strings.TrimSpace(observation.RunID)
			if signature == "" || runID == "" {
				continue
			}
			if semanticRunsBySignature[signature] == nil {
				semanticRunsBySignature[signature] = map[string]bool{}
			}
			semanticRunsBySignature[signature][runID] = true
			if len(semanticRunsBySignature[signature]) >= 3 {
				return true
			}
		}
	}
	return false
}

func repairEvidenceSkipReason(digest SkillEvidenceDigest, observationID string) string {
	for _, observation := range digest.NegativeObservations {
		if observation.ID != observationID || observation.Incident == nil {
			continue
		}
		incident := observation.Incident
		switch {
		case strings.TrimSpace(digest.TargetActiveContent) == "" || strings.TrimSpace(digest.ParentVersionHash) == "":
			return "target_not_curator_repairable"
		case !incident.RecoveryVerified:
			return "recovery_not_verified"
		case strings.TrimSpace(incident.FailedToolCallID) == "":
			return "failed_tool_call_not_observed"
		case !SkillRepairObservedFailureEligible(incident.ErrorCategory, incident.ObservedErrorCategory):
			return "failure_category_mismatch"
		case strings.TrimSpace(incident.FailureSignature) == "":
			return "missing_failure_signature"
		case strings.TrimSpace(incident.FailedStepID) == "":
			return "missing_failed_step"
		case !SkillRepairCandidateEvidenceReady(digest):
			return "repair_class_threshold_not_met:" + ClassifySkillRepairIncident(incident)
		}
	}
	return ""
}

func incidentCategory(incident *SkillIncidentEvidence) string {
	if incident == nil {
		return ""
	}
	return incident.ErrorCategory
}

func (s *Store) workflowRunOrigins(ctx context.Context, runIDs []string) (map[string]string, error) {
	origins := map[string]string{}
	for _, chunk := range chunkStrings(uniqueSortedStrings(runIDs), 400) {
		rows, err := s.db.QueryContext(ctx, `SELECT run_id, COALESCE(payload_json, '{}')
			FROM task_events WHERE type='run.started' AND run_id IN (`+placeholders(len(chunk))+`)
			ORDER BY run_id, COALESCE(cursor, 0), created_at, rowid`, toAnySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var runID, raw string
			if err := rows.Scan(&runID, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			if _, exists := origins[runID]; exists {
				continue
			}
			var payload struct {
				Origin string `json:"origin"`
			}
			_ = json.Unmarshal([]byte(raw), &payload)
			origins[runID] = strings.TrimSpace(payload.Origin)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return origins, nil
}

func workflowOriginExcludedFromCuration(origin string) bool {
	switch strings.ToLower(strings.TrimSpace(origin)) {
	case "watch", "external-watch-finalization":
		return true
	default:
		return false
	}
}

func workflowObservationsComparable(anchor, candidate WorkflowObservation) bool {
	if candidate.EnvironmentFingerprint != anchor.EnvironmentFingerprint || candidate.SkillKey != anchor.SkillKey {
		return false
	}
	if anchor.SkillKey != "" && candidate.VersionHash != anchor.VersionHash {
		return false
	}
	goalSimilarity := stringSetJaccard(workflowGoalFeatures(anchor.GoalDigest), workflowGoalFeatures(candidate.GoalDigest))
	toolSimilarity := stringSetJaccard(uniqueSortedStrings(anchor.ToolSequence), uniqueSortedStrings(candidate.ToolSequence))
	if anchor.SkillKey != "" {
		return goalSimilarity >= 0.10 && (toolSimilarity >= 0.34 || len(anchor.ToolSequence) == 0 || len(candidate.ToolSequence) == 0)
	}
	return goalSimilarity >= 0.18 && toolSimilarity >= 0.50
}

func expectedWorkflowSavings(observations []WorkflowObservation) map[string]int64 {
	if len(observations) == 0 {
		return nil
	}
	durations := make([]int64, 0, len(observations))
	tools := make([]int64, 0, len(observations))
	for _, observation := range observations {
		durations = append(durations, observation.DurationMS)
		tools = append(tools, int64(len(observation.ToolSequence)))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(tools, func(i, j int) bool { return tools[i] < tools[j] })
	return map[string]int64{"baseline_median_duration_ms": durations[len(durations)/2], "baseline_median_tool_rounds": tools[len(tools)/2]}
}

func scanWorkflowObservation(row skillLifecycleScanner) (*WorkflowObservation, error) {
	var observation WorkflowObservation
	var toolsJSON string
	var corrected int
	var created int64
	if err := row.Scan(&observation.ID, &observation.IdentityTenantID, &observation.ControlTenantID,
		&observation.PersonID, &observation.WorkspaceID, &observation.RunID, &observation.WorkUnitID,
		&observation.RelatedTaskID, &observation.WorkflowSignature, &observation.GoalDigest,
		&observation.EnvironmentFingerprint, &observation.SkillKey, &observation.VersionHash,
		&observation.ActivationState, &observation.OutcomeStatus, &observation.VerificationState,
		&toolsJSON, &observation.ToolFailures, &observation.ProviderCalls, &observation.DurationMS,
		&observation.InputTokens, &observation.OutputTokens, &corrected, &observation.EvidenceRole,
		&created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(toolsJSON), &observation.ToolSequence)
	observation.UserCorrected = corrected != 0
	observation.CreatedAt = time.Unix(created, 0)
	return &observation, nil
}
