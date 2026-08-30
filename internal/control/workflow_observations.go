package control

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type WorkflowObservation struct {
	ID                     string                 `json:"id"`
	IdentityTenantID       string                 `json:"identity_tenant_id"`
	ControlTenantID        string                 `json:"control_tenant_id"`
	PersonID               string                 `json:"person_id"`
	WorkspaceID            string                 `json:"workspace_id,omitempty"`
	RunID                  string                 `json:"run_id"`
	WorkUnitID             string                 `json:"work_unit_id"`
	RelatedTaskID          string                 `json:"related_task_id,omitempty"`
	WorkflowSignature      string                 `json:"workflow_signature"`
	GoalDigest             string                 `json:"goal_digest,omitempty"`
	EnvironmentFingerprint string                 `json:"environment_fingerprint,omitempty"`
	SkillKey               string                 `json:"skill_key,omitempty"`
	VersionHash            string                 `json:"version_hash,omitempty"`
	ActivationState        string                 `json:"activation_state,omitempty"`
	OutcomeStatus          string                 `json:"outcome_status"`
	VerificationState      string                 `json:"verification_state,omitempty"`
	ToolSequence           []string               `json:"tool_sequence,omitempty"`
	ToolFailures           int                    `json:"tool_failures"`
	ProviderCalls          int                    `json:"provider_calls"`
	DurationMS             int64                  `json:"duration_ms"`
	InputTokens            int64                  `json:"input_tokens"`
	OutputTokens           int64                  `json:"output_tokens"`
	UserCorrected          bool                   `json:"user_corrected"`
	EvidenceRole           string                 `json:"evidence_role"`
	ToolEvidence           []WorkflowToolEvidence `json:"tool_evidence,omitempty"`
	Incident               *SkillIncidentEvidence `json:"incident,omitempty"`
	CreatedAt              time.Time              `json:"created_at"`
	Evidence               json.RawMessage        `json:"-"`
}

// WorkflowToolEvidence is the trusted, argument-free registry metadata captured
// for one tool step. Skill publication policy consumes this instead of inferring
// authority from a provider-visible tool name.
type WorkflowToolEvidence struct {
	ToolCallID       string   `json:"tool_call_id,omitempty"`
	Name             string   `json:"name"`
	Origin           string   `json:"origin,omitempty"`
	Category         string   `json:"category,omitempty"`
	RiskLevel        string   `json:"risk_level,omitempty"`
	ReadOnly         bool     `json:"read_only"`
	OperationClasses []string `json:"operation_classes,omitempty"`
}

// SkillIncidentEvidence joins an attributable Skill fallback to the ordinary
// planner's later recovery in the same work unit. It contains no raw tool output
// or arguments; the durable task events remain the source of truth.
type SkillIncidentEvidence struct {
	FailureSignature      string   `json:"failure_signature"`
	FailedStepID          string   `json:"failed_step_id"`
	ErrorCategory         string   `json:"error_category"`
	FailedToolCallID      string   `json:"failed_tool_call_id,omitempty"`
	ObservedErrorCategory string   `json:"observed_error_category,omitempty"`
	RepairClass           string   `json:"repair_class,omitempty"`
	FailureObserved       bool     `json:"failure_observed"`
	NormalizedInputShape  string   `json:"normalized_input_shape,omitempty"`
	Reason                string   `json:"reason"`
	RecoveryToolSequence  []string `json:"recovery_tool_sequence,omitempty"`
	RecoveryFailures      int      `json:"recovery_failures"`
	RecoveryVerified      bool     `json:"recovery_verified"`
}

type observationAccumulator struct {
	tools         []string
	toolEvidence  []WorkflowToolEvidence
	toolFailures  int
	providerCalls int
	inputTokens   int64
	outputTokens  int64
	corrected     bool
	incident      *SkillIncidentEvidence
	failures      []observedWorkflowToolFailure
	recovering    bool
}

type observedWorkflowToolFailure struct {
	ToolCallID    string
	ErrorCategory string
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
		if unit.Status != WorkUnitCompleted && unit.Status != WorkUnitParked && unit.Status != WorkUnitFallback && unit.Status != WorkUnitFailed && unit.Status != WorkUnitCancelled {
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
			ToolEvidence:  append([]WorkflowToolEvidence(nil), metric.toolEvidence...),
			ProviderCalls: metric.providerCalls, DurationMS: durationMS,
			InputTokens: metric.inputTokens, OutputTokens: metric.outputTokens,
			UserCorrected: metric.corrected, EvidenceRole: role, CreatedAt: now,
		}
		attachObservationIncident(&observation, metric)
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
			if err := s.recordSkillVersionObservationHealth(ctx, observation); err != nil {
				return nil, err
			}
			inserted = append(inserted, observation)
		}
	}
	return inserted, nil
}

func (s *Store) recordSkillVersionObservationHealth(ctx context.Context, observation WorkflowObservation) error {
	if strings.TrimSpace(observation.SkillKey) == "" || strings.TrimSpace(observation.VersionHash) == "" {
		return nil
	}
	if observation.OutcomeStatus == WorkUnitCompleted && observation.VerificationState == "passed" && observation.EvidenceRole == "success_path" {
		dependencyFingerprint, environmentFingerprint, verifiedAt := skillEvidenceHealth(
			mustJSONBytes(SkillEvidenceDigest{SuccessObservations: []WorkflowObservation{observation}}), observation.CreatedAt)
		_, err := s.db.ExecContext(ctx, `UPDATE skill_versions SET dependency_fingerprint=?,
			verification_environment_fingerprint=?, last_verified_at=?
			WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='active'`,
			dependencyFingerprint, environmentFingerprint, verifiedAt, observation.ControlTenantID,
			observation.SkillKey, observation.VersionHash)
		return err
	}
	incident := observation.Incident
	if incident == nil || !incident.FailureObserved || strings.TrimSpace(incident.FailedToolCallID) == "" {
		return nil
	}
	switch ClassifySkillRepairIncident(incident) {
	case SkillRepairClassDeterministicInterface, SkillRepairClassStablePrecondition, SkillRepairClassSemantic:
		_, err := s.db.ExecContext(ctx, `UPDATE skill_versions SET state='quarantined'
			WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='active'
			AND parent_version_hash<>'' AND created_by='skill_curator'`, observation.ControlTenantID,
			observation.SkillKey, observation.VersionHash)
		return err
	default:
		return nil
	}
}

func mustJSONBytes(value interface{}) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (s *Store) runSkillActivations(ctx context.Context, tenantID, runID string) ([]SkillActivation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, identity_tenant_id, control_tenant_id, person_id,
		workspace_id, run_id, sequence, work_unit_id, execution_lane, primary_task_id,
		related_task_id, skill_key, skill_name, version_hash, activation_source, attachment_mode,
		state, fallback_reason, selected_at, finished_at, package_hash, delivery_contract_version,
		delivery_mode, delivered_main, delivered_main_hash, delivered_main_bytes, resource_manifest_json FROM run_skill_activations
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
		case "skill.activated":
			// Failures before activation cannot authorize a rewrite of the newly
			// activated Skill.
			acc.failures = nil
		case "tool.started":
			if tool := stringValue(event.payload["tool"]); tool != "" {
				acc.tools = append(acc.tools, tool)
				acc.toolEvidence = append(acc.toolEvidence, workflowToolEvidenceFromPayload(tool, event.payload))
				if acc.recovering && acc.incident != nil {
					acc.incident.RecoveryToolSequence = append(acc.incident.RecoveryToolSequence, tool)
				}
			}
		case "tool.completed":
			if stringValue(event.payload["tool_origin"]) != "" {
				mergeWorkflowToolEvidence(acc, workflowToolEvidenceFromPayload(stringValue(event.payload["tool"]), event.payload))
			}
			failed := stringValue(event.payload["error"]) != "" || stringValue(event.payload["error_category"]) != ""
			if failed {
				acc.toolFailures++
				if acc.recovering && acc.incident != nil {
					acc.incident.RecoveryFailures++
				} else {
					acc.failures = append(acc.failures, observedWorkflowToolFailure{
						ToolCallID: stringValue(event.payload["tool_call_id"]), ErrorCategory: stringValue(event.payload["error_category"]),
					})
					if len(acc.failures) > 16 {
						acc.failures = append([]observedWorkflowToolFailure(nil), acc.failures[len(acc.failures)-16:]...)
					}
				}
			}
		case "provider.call.usage":
			acc.providerCalls++
			acc.inputTokens += int64Value(event.payload["input_tokens"])
			acc.outputTokens += int64Value(event.payload["output_tokens"])
		case "run.steering_consumed":
			acc.corrected = true
		case "skill.fallback":
			acc.corrected = true
			acc.recovering = true
			acc.incident = &SkillIncidentEvidence{
				FailureSignature:     stringValue(event.payload["failure_signature"]),
				FailedStepID:         stringValue(event.payload["failed_step_id"]),
				ErrorCategory:        stringValue(event.payload["error_category"]),
				NormalizedInputShape: stringValue(event.payload["normalized_input_shape"]),
				Reason:               stringValue(event.payload["reason"]),
			}
			failedCallID := stringValue(event.payload["failed_tool_call_id"])
			if failure := matchingObservedWorkflowFailure(acc.failures, failedCallID); failure != nil {
				acc.incident.FailedToolCallID = failure.ToolCallID
				acc.incident.ObservedErrorCategory = failure.ErrorCategory
				acc.incident.FailureObserved = SkillRepairObservedFailureEligible(acc.incident.ErrorCategory, failure.ErrorCategory)
				acc.incident.RepairClass = ClassifySkillRepairIncident(acc.incident)
			}
			acc.failures = nil
		}
	}
	return out
}

func matchingObservedWorkflowFailure(failures []observedWorkflowToolFailure, requestedCallID string) *observedWorkflowToolFailure {
	requestedCallID = strings.TrimSpace(requestedCallID)
	for i := len(failures) - 1; i >= 0; i-- {
		if requestedCallID != "" && failures[i].ToolCallID != requestedCallID {
			continue
		}
		failure := failures[i]
		return &failure
	}
	return nil
}

func workflowToolEvidenceFromPayload(tool string, payload map[string]interface{}) WorkflowToolEvidence {
	evidence := WorkflowToolEvidence{
		ToolCallID: stringValue(payload["tool_call_id"]), Name: tool, Origin: stringValue(payload["tool_origin"]),
		Category: stringValue(payload["tool_category"]), RiskLevel: stringValue(payload["tool_risk_level"]),
	}
	if readOnly, ok := payload["tool_read_only"].(bool); ok {
		evidence.ReadOnly = readOnly
	}
	switch raw := payload["operation_classes"].(type) {
	case []string:
		evidence.OperationClasses = append(evidence.OperationClasses, raw...)
	case []interface{}:
		for _, value := range raw {
			if class := stringValue(value); class != "" {
				evidence.OperationClasses = append(evidence.OperationClasses, class)
			}
		}
	}
	return evidence
}

func mergeWorkflowToolEvidence(acc *observationAccumulator, completed WorkflowToolEvidence) {
	if acc == nil {
		return
	}
	for i := len(acc.toolEvidence) - 1; i >= 0; i-- {
		if completed.ToolCallID != "" && acc.toolEvidence[i].ToolCallID == completed.ToolCallID {
			acc.toolEvidence[i] = completed
			return
		}
	}
	acc.toolEvidence = append(acc.toolEvidence, completed)
}

func attachObservationIncident(observation *WorkflowObservation, metric *observationAccumulator) {
	if observation == nil || metric == nil || metric.incident == nil {
		return
	}
	incident := *metric.incident
	incident.RepairClass = ClassifySkillRepairIncident(&incident)
	incident.RecoveryToolSequence = normalizedToolSequence(incident.RecoveryToolSequence)
	incident.RecoveryVerified = observation.OutcomeStatus == WorkUnitCompleted &&
		observation.VerificationState == "passed" && len(incident.RecoveryToolSequence) > 0 && incident.RecoveryFailures == 0
	observation.Incident = &incident
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
