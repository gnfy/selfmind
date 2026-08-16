package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type WorkflowObservation struct {
	ID                     string          `json:"id"`
	IdentityTenantID       string          `json:"identity_tenant_id"`
	ControlTenantID        string          `json:"control_tenant_id"`
	PersonID               string          `json:"person_id"`
	WorkspaceID            string          `json:"workspace_id,omitempty"`
	RunID                  string          `json:"run_id"`
	WorkUnitID             string          `json:"work_unit_id"`
	RelatedTaskID          string          `json:"related_task_id,omitempty"`
	WorkflowSignature      string          `json:"workflow_signature"`
	GoalDigest             string          `json:"goal_digest,omitempty"`
	EnvironmentFingerprint string          `json:"environment_fingerprint,omitempty"`
	SkillKey               string          `json:"skill_key,omitempty"`
	VersionHash            string          `json:"version_hash,omitempty"`
	ActivationState        string          `json:"activation_state,omitempty"`
	OutcomeStatus          string          `json:"outcome_status"`
	VerificationState      string          `json:"verification_state,omitempty"`
	ToolSequence           []string        `json:"tool_sequence,omitempty"`
	ToolFailures           int             `json:"tool_failures"`
	ProviderCalls          int             `json:"provider_calls"`
	DurationMS             int64           `json:"duration_ms"`
	InputTokens            int64           `json:"input_tokens"`
	OutputTokens           int64           `json:"output_tokens"`
	UserCorrected          bool            `json:"user_corrected"`
	EvidenceRole           string          `json:"evidence_role"`
	CreatedAt              time.Time       `json:"created_at"`
	Evidence               json.RawMessage `json:"-"`
}

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
	TargetSkillKey       string                `json:"target_skill_key,omitempty"`
	TargetSkillName      string                `json:"target_skill_name,omitempty"`
	TargetActiveContent  string                `json:"target_active_content,omitempty"`
	ParentVersionHash    string                `json:"parent_version_hash,omitempty"`
	SuccessObservations  []WorkflowObservation `json:"success_observations"`
	NegativeObservations []WorkflowObservation `json:"negative_observations,omitempty"`
	ExpectedSavings      map[string]int64      `json:"expected_savings,omitempty"`
}

type observationAccumulator struct {
	tools         []string
	toolFailures  int
	providerCalls int
	inputTokens   int64
	outputTokens  int64
	corrected     bool
}

func (s *Store) MaterializeWorkflowObservations(ctx context.Context, tenantID, runID string) ([]WorkflowObservation, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	tenant := normalizeTenant(tenantID)
	run, err := s.GetRun(ctx, tenant, runID)
	if err != nil || run == nil || run.FinishedAt == nil {
		return nil, err
	}
	units, err := s.ListRunWorkUnits(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		return nil, nil
	}
	activations, err := s.runSkillActivations(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	activationByUnit := map[string]SkillActivation{}
	for _, activation := range activations {
		activationByUnit[activation.WorkUnitID] = activation
	}
	events, err := s.workflowEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	metrics := accumulateWorkUnitEvents(units, events)
	environmentFingerprint := ""
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(environment_fingerprint,'') FROM execution_leases WHERE tenant_id=? AND run_id=?`, tenant, runID).Scan(&environmentFingerprint)
	now := time.Now()
	inserted := make([]WorkflowObservation, 0, len(units))
	for _, unit := range units {
		if unit.Status != WorkUnitCompleted && unit.Status != WorkUnitFallback && unit.Status != WorkUnitFailed && unit.Status != WorkUnitCancelled {
			continue
		}
		activation := activationByUnit[unit.ID]
		metric := metrics[unit.ID]
		role := workflowEvidenceRole(unit, activation, metric)
		goalDigest := sanitizeWorkflowGoal(unit.GoalDigest)
		signature := comparableWorkflowSignature(goalDigest, activation.SkillKey, activation.VersionHash, metric.tools)
		durationMS := int64(0)
		if unit.StartedAt != nil && unit.FinishedAt != nil && !unit.FinishedAt.Before(*unit.StartedAt) {
			durationMS = unit.FinishedAt.Sub(*unit.StartedAt).Milliseconds()
		}
		observation := WorkflowObservation{
			ID: "observation_" + uuid.NewString(), IdentityTenantID: tenant,
			ControlTenantID: firstEvolutionValue(activation.ControlTenantID, tenant),
			PersonID:        run.PersonID, WorkspaceID: run.WorkspaceID, RunID: run.ID,
			WorkUnitID: unit.ID, RelatedTaskID: unit.RelatedTaskID,
			WorkflowSignature: signature, GoalDigest: goalDigest,
			EnvironmentFingerprint: environmentFingerprint, SkillKey: activation.SkillKey,
			VersionHash: activation.VersionHash, ActivationState: activation.State,
			OutcomeStatus: unit.Status, VerificationState: unit.VerificationState,
			ToolSequence: normalizedToolSequence(metric.tools), ToolFailures: metric.toolFailures,
			ProviderCalls: metric.providerCalls, DurationMS: durationMS,
			InputTokens: metric.inputTokens, OutputTokens: metric.outputTokens,
			UserCorrected: metric.corrected, EvidenceRole: role, CreatedAt: now,
		}
		toolsJSON, _ := json.Marshal(observation.ToolSequence)
		result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_observations
			(id, identity_tenant_id, control_tenant_id, person_id, workspace_id, run_id,
			 work_unit_id, related_task_id, workflow_signature, goal_digest, environment_fingerprint,
			 skill_key, version_hash, activation_state, outcome_status, verification_state,
			 tool_sequence_json, tool_failures, provider_calls, duration_ms, input_tokens,
			 output_tokens, user_corrected, evidence_role, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.ID,
			observation.IdentityTenantID, observation.ControlTenantID, observation.PersonID,
			observation.WorkspaceID, observation.RunID, observation.WorkUnitID,
			observation.RelatedTaskID, observation.WorkflowSignature, observation.GoalDigest,
			observation.EnvironmentFingerprint, observation.SkillKey, observation.VersionHash,
			observation.ActivationState, observation.OutcomeStatus, observation.VerificationState,
			string(toolsJSON), observation.ToolFailures, observation.ProviderCalls,
			observation.DurationMS, observation.InputTokens, observation.OutputTokens,
			evolutionBoolInt(observation.UserCorrected), observation.EvidenceRole, now.Unix())
		if err != nil {
			return nil, err
		}
		if n, _ := result.RowsAffected(); n == 1 {
			inserted = append(inserted, observation)
		}
	}
	return inserted, nil
}

func (s *Store) runSkillActivations(ctx context.Context, tenantID, runID string) ([]SkillActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, identity_tenant_id, control_tenant_id, person_id,
		workspace_id, run_id, sequence, work_unit_id, execution_lane, primary_task_id,
		related_task_id, skill_key, skill_name, version_hash, activation_source, attachment_mode,
		state, fallback_reason, selected_at, finished_at FROM run_skill_activations
		WHERE identity_tenant_id=? AND run_id=? ORDER BY sequence`, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillActivation
	for rows.Next() {
		activation, err := scanSkillActivation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *activation)
	}
	return out, rows.Err()
}

func accumulateWorkUnitEvents(units []RunWorkUnit, events []evolutionEvent) map[string]*observationAccumulator {
	out := make(map[string]*observationAccumulator, len(units))
	for i := range units {
		out[units[i].ID] = &observationAccumulator{}
	}
	current := units[0].ID
	for _, event := range events {
		if event.typ == "plan.updated" {
			if id := activeWorkUnitIDFromEvent(event.payload); id != "" {
				if _, ok := out[id]; ok {
					current = id
				}
			}
		}
		if id := stringValue(event.payload["work_unit_id"]); id != "" {
			if _, ok := out[id]; ok {
				current = id
			}
		}
		acc := out[current]
		if acc == nil {
			continue
		}
		switch event.typ {
		case "tool.started":
			if tool := stringValue(event.payload["tool"]); tool != "" {
				acc.tools = append(acc.tools, tool)
			}
		case "tool.completed":
			if stringValue(event.payload["error"]) != "" || stringValue(event.payload["error_category"]) != "" {
				acc.toolFailures++
			}
		case "provider.call.usage":
			acc.providerCalls++
			acc.inputTokens += int64Value(event.payload["input_tokens"])
			acc.outputTokens += int64Value(event.payload["output_tokens"])
		case "run.steering_consumed", "skill.fallback":
			acc.corrected = true
		}
	}
	return out
}

func activeWorkUnitIDFromEvent(payload map[string]interface{}) string {
	raw, _ := payload["work_units"].([]interface{})
	for _, item := range raw {
		row, _ := item.(map[string]interface{})
		if stringValue(row["plan_status"]) == "in_progress" {
			return stringValue(row["id"])
		}
	}
	return ""
}

func workflowEvidenceRole(unit RunWorkUnit, activation SkillActivation, metric *observationAccumulator) string {
	if metric != nil && (metric.corrected || metric.toolFailures > 0) {
		return "failure_guard"
	}
	if activation.State == SkillActivationFallback || unit.Status == WorkUnitFallback || unit.Status == WorkUnitFailed {
		return "failure_guard"
	}
	if unit.Status == WorkUnitCompleted {
		switch unit.VerificationState {
		case "passed", "not_applicable":
			return "success_path"
		default:
			return "audit"
		}
	}
	return "audit"
}

var workflowVariableToken = regexp.MustCompile(`(?i)(?:[a-z]:)?[/\\][^\s]+|\b[0-9a-f]{7,}\b|\b\d+\b`)
var workflowSecretAssignment = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|credential)\b\s*[:=]\s*[^\s,;]+`)
var workflowCredentialLiteral = regexp.MustCompile(`(?i)\b(?:ghp_[a-z0-9]{20,}|github_pat_[a-z0-9_]{40,}|sk-(?:ant-)?[a-z0-9_-]{20,}|AKIA[0-9A-Z]{16})\b`)

func sanitizeWorkflowGoal(goal string) string {
	goal = workflowSecretAssignment.ReplaceAllString(goal, "$1=[redacted]")
	goal = workflowCredentialLiteral.ReplaceAllString(goal, "[redacted-credential]")
	goal = strings.TrimSpace(goal)
	if len(goal) > 512 {
		goal = goal[:512]
	}
	return goal
}

// NormalizeSkillInputShape produces a non-reversible, deterministic shape key
// for exact failure-guard matching without storing the user's raw request.
func NormalizeSkillInputShape(input string) string {
	normalized := strings.ToLower(strings.TrimSpace(workflowVariableToken.ReplaceAllString(input, "<var>")))
	if normalized == "" {
		return ""
	}
	return stableEvolutionHash(strings.Join(strings.Fields(normalized), " "))
}

func comparableWorkflowSignature(goal, skillKey, versionHash string, tools []string) string {
	features := workflowGoalFeatures(goal)
	if len(features) > 8 {
		features = features[:8]
	}
	families := uniqueSortedStrings(collapsedToolSequence(normalizedToolSequence(tools)))
	return stableEvolutionHash(map[string]interface{}{
		"version": 3, "goal_features": features, "tool_families": families,
		"skill_key": strings.TrimSpace(skillKey), "skill_version": strings.TrimSpace(versionHash),
	})
}

func (s *Store) ReadySkillEvidenceDigestsForRun(ctx context.Context, tenantID, runID string) ([]SkillEvidenceDigest, error) {
	anchors, err := s.workflowObservationsForRun(ctx, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	var out []SkillEvidenceDigest
	seenEvidence := map[string]bool{}
	for _, anchor := range anchors {
		cohort, err := s.comparableWorkflowCohort(ctx, anchor, 5, 2)
		if err != nil || independentWorkflowRuns(cohort.SuccessObservations) < 3 {
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
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_versions WHERE control_tenant_id=? AND evidence_set_hash=?`,
			cohort.ControlTenantID, cohort.EvidenceSetHash).Scan(&existing); err != nil || existing > 0 {
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

func (s *Store) comparableWorkflowCohort(ctx context.Context, anchor WorkflowObservation, successLimit, negativeLimit int) (SkillEvidenceDigest, error) {
	rows, err := s.db.QueryContext(ctx, workflowObservationSelect+`
		FROM workflow_observations WHERE identity_tenant_id=? AND person_id=? AND workspace_id=?
		ORDER BY created_at DESC LIMIT 48`, anchor.IdentityTenantID, anchor.PersonID, anchor.WorkspaceID)
	if err != nil {
		return SkillEvidenceDigest{}, err
	}
	defer rows.Close()
	digest := SkillEvidenceDigest{
		WorkflowSignature: anchor.WorkflowSignature, IdentityTenantID: anchor.IdentityTenantID,
		ControlTenantID: anchor.ControlTenantID, PersonID: anchor.PersonID, WorkspaceID: anchor.WorkspaceID,
		TargetSkillKey: anchor.SkillKey, ParentVersionHash: anchor.VersionHash,
	}
	for rows.Next() {
		observation, err := scanWorkflowObservation(rows)
		if err != nil {
			return SkillEvidenceDigest{}, err
		}
		if !workflowObservationsComparable(anchor, *observation) {
			continue
		}
		if observation.EvidenceRole == "success_path" && len(digest.SuccessObservations) < successLimit {
			digest.SuccessObservations = append(digest.SuccessObservations, *observation)
		} else if observation.EvidenceRole == "failure_guard" && len(digest.NegativeObservations) < negativeLimit {
			digest.NegativeObservations = append(digest.NegativeObservations, *observation)
		}
	}
	if digest.TargetSkillKey != "" {
		_ = s.db.QueryRowContext(ctx, `SELECT skill_name, content_body FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
			digest.ControlTenantID, digest.TargetSkillKey).Scan(&digest.TargetSkillName, &digest.TargetActiveContent)
	}
	return digest, rows.Err()
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

func workflowGoalFeatures(goal string) []string {
	goal = strings.ToLower(strings.TrimSpace(workflowVariableToken.ReplaceAllString(goal, "<var>")))
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	var word, cjk []rune
	flush := func() {
		if len(word) >= 2 {
			add(string(word))
		}
		word = word[:0]
		if len(cjk) > 0 {
			if len(cjk) <= 4 {
				add(string(cjk))
			}
			for i := 0; i+2 <= len(cjk); i++ {
				add(string(cjk[i : i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range goal {
		switch {
		case unicode.Is(unicode.Han, r):
			if len(word) > 0 {
				flush()
			}
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
			if len(cjk) > 0 {
				flush()
			}
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func stringSetJaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	left := map[string]bool{}
	for _, value := range a {
		left[value] = true
	}
	intersection := 0
	union := len(left)
	for _, value := range b {
		if left[value] {
			intersection++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
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

func (s *Store) CreateSkillCandidateVersion(ctx context.Context, tenantID, skillKey, skillName, parentVersionHash, content, evidenceSetHash string, observationIDs []string, evidence interface{}) (string, error) {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(skillName) == "" {
		return "", fmt.Errorf("candidate name and content are required")
	}
	digest := sha256.Sum256([]byte(content))
	versionHash := fmt.Sprintf("%x", digest[:])
	idsJSON, _ := json.Marshal(observationIDs)
	evidenceJSON, _ := json.Marshal(evidence)
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_versions
		(control_tenant_id, skill_key, skill_name, version_hash, parent_version_hash, state,
		 content_body, source_observation_ids_json, evidence_set_hash, evidence_json,
		 created_by, created_at)
		VALUES(?,?,?,?,?,'candidate',?,?,?,?, 'skill_curator',?)`, normalizeTenant(tenantID),
		skillKey, skillName, versionHash, parentVersionHash, content, string(idsJSON),
		evidenceSetHash, string(evidenceJSON), time.Now().Unix())
	return versionHash, err
}

type SkillVersion struct {
	ControlTenantID   string          `json:"control_tenant_id"`
	SkillKey          string          `json:"skill_key"`
	SkillName         string          `json:"skill_name"`
	VersionHash       string          `json:"version_hash"`
	ParentVersionHash string          `json:"parent_version_hash,omitempty"`
	State             string          `json:"state"`
	ContentRef        string          `json:"content_ref,omitempty"`
	ContentBody       string          `json:"content_body,omitempty"`
	ObservationIDs    json.RawMessage `json:"source_observation_ids,omitempty"`
	EvidenceSetHash   string          `json:"evidence_set_hash,omitempty"`
	Evidence          json.RawMessage `json:"evidence,omitempty"`
	CreatedBy         string          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	PromotedAt        *time.Time      `json:"promoted_at,omitempty"`
}

func (s *Store) SkillCandidateByEvidence(ctx context.Context, tenantID, evidenceSetHash string) (*SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=? AND evidence_set_hash=? ORDER BY created_at DESC LIMIT 1`,
		normalizeTenant(tenantID), strings.TrimSpace(evidenceSetHash))
	version, err := scanSkillVersion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return version, err
}

func (s *Store) GetSkillVersion(ctx context.Context, tenantID, skillKey, versionHash string) (*SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, versionHash)
	version, err := scanSkillVersion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return version, err
}

func scanSkillVersion(row skillLifecycleScanner) (*SkillVersion, error) {
	var version SkillVersion
	var observations, evidence string
	var created int64
	var promoted sql.NullInt64
	if err := row.Scan(&version.ControlTenantID, &version.SkillKey, &version.SkillName,
		&version.VersionHash, &version.ParentVersionHash, &version.State, &version.ContentRef,
		&version.ContentBody, &observations, &version.EvidenceSetHash, &evidence,
		&version.CreatedBy, &created, &promoted); err != nil {
		return nil, err
	}
	version.ObservationIDs = json.RawMessage(observations)
	version.Evidence = json.RawMessage(evidence)
	version.CreatedAt = time.Unix(created, 0)
	if promoted.Valid {
		at := time.Unix(promoted.Int64, 0)
		version.PromotedAt = &at
	}
	return &version, nil
}

func (s *Store) PromoteSkillCandidate(ctx context.Context, tenantID, skillKey, versionHash, contentRef string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, versionHash).Scan(&state); err != nil {
		return err
	}
	if state != "candidate" {
		return fmt.Errorf("skill version is %s, not candidate", state)
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE skill_failure_guards SET state='resolved'
		WHERE control_tenant_id=? AND skill_key=? AND version_hash IN
		(SELECT version_hash FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND state='active')`,
		normalizeTenant(tenantID), skillKey, normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='previous' WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
		normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='active', content_ref=?, promoted_at=?
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='candidate'`,
		strings.TrimSpace(contentRef), now, normalizeTenant(tenantID), skillKey, versionHash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RejectSkillCandidate(ctx context.Context, tenantID, skillKey, versionHash string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE skill_versions SET state='rejected'
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='candidate'`,
		normalizeTenant(tenantID), skillKey, versionHash)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("skill candidate not found")
	}
	return nil
}

func (s *Store) ListSkillVersions(ctx context.Context, tenantID, skillKey, state string, limit int) ([]SkillVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=?`
	args := []interface{}{normalizeTenant(tenantID)}
	if strings.TrimSpace(skillKey) != "" {
		query += ` AND skill_key=?`
		args = append(args, strings.TrimSpace(skillKey))
	}
	if strings.TrimSpace(state) != "" {
		query += ` AND state=?`
		args = append(args, strings.TrimSpace(state))
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillVersion
	for rows.Next() {
		version, err := scanSkillVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *version)
	}
	return out, rows.Err()
}

func (s *Store) ActivatePreviousSkillVersion(ctx context.Context, tenantID, skillKey, targetVersionHash, contentRef string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, targetVersionHash).Scan(&state); err != nil {
		return err
	}
	if state != "previous" {
		return fmt.Errorf("rollback target is %s, not previous", state)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='previous' WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
		normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='active', content_ref=?, promoted_at=?
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='previous'`,
		strings.TrimSpace(contentRef), time.Now().Unix(), normalizeTenant(tenantID), skillKey, targetVersionHash); err != nil {
		return err
	}
	return tx.Commit()
}
