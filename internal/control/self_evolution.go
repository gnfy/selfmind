package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const evolutionProfileVersion = 1

type EvolutionPolicy struct {
	Enabled                  bool
	Mode                     string
	ShadowAfterObservations  int
	PromoteAfterObservations int
	MinShadowRuns            int
	MaxShadowFailureRate     float64
}

func (p EvolutionPolicy) normalized() EvolutionPolicy {
	if strings.TrimSpace(p.Mode) == "" {
		p.Mode = "auto-readonly"
	}
	if strings.EqualFold(strings.TrimSpace(p.Mode), "auto") {
		p.Mode = "auto-readonly"
	}
	if p.ShadowAfterObservations <= 0 {
		p.ShadowAfterObservations = 3
	}
	if p.PromoteAfterObservations <= 0 {
		p.PromoteAfterObservations = 5
	}
	if p.MinShadowRuns <= 0 {
		p.MinShadowRuns = 3
	}
	if p.MaxShadowFailureRate <= 0 || p.MaxShadowFailureRate >= 1 {
		p.MaxShadowFailureRate = 0.05
	}
	return p
}

type WorkflowProfile struct {
	RunID              string            `json:"run_id"`
	TenantID           string            `json:"tenant_id"`
	PersonID           string            `json:"person_id"`
	TaskID             string            `json:"task_id"`
	WorkspaceID        string            `json:"workspace_id,omitempty"`
	WorkflowSignature  string            `json:"workflow_signature"`
	SkillVersions      map[string]string `json:"skill_versions,omitempty"`
	PlanHash           string            `json:"plan_hash,omitempty"`
	ToolSequence       []string          `json:"tool_sequence"`
	ToolCalls          int               `json:"tool_calls"`
	ToolFailures       int               `json:"tool_failures"`
	ProviderCalls      int               `json:"provider_calls"`
	DurationMS         int64             `json:"duration_ms"`
	InputTokens        int64             `json:"input_tokens"`
	OutputTokens       int64             `json:"output_tokens"`
	BilledInputTokens  int64             `json:"billed_input_tokens"`
	OutcomeStatus      string            `json:"outcome_status"`
	VerificationState  string            `json:"verification_state,omitempty"`
	ReadOnly           bool              `json:"read_only"`
	AppliedCandidateID string            `json:"applied_candidate_id,omitempty"`
	BatchItemFailures  int               `json:"batch_item_failures,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
}

type EvolutionCandidate struct {
	ID                  string          `json:"id"`
	TenantID            string          `json:"tenant_id"`
	PersonID            string          `json:"person_id"`
	WorkspaceID         string          `json:"workspace_id,omitempty"`
	LastTaskID          string          `json:"last_task_id,omitempty"`
	WorkflowSignature   string          `json:"workflow_signature"`
	Kind                string          `json:"kind"`
	Status              string          `json:"status"`
	Contract            json.RawMessage `json:"contract"`
	Repair              json.RawMessage `json:"repair,omitempty"`
	ObservationCount    int             `json:"observation_count"`
	ShadowRuns          int             `json:"shadow_runs"`
	ShadowMatches       int             `json:"shadow_matches"`
	FallbackCount       int             `json:"fallback_count"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	LastFailure         string          `json:"last_failure,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type EvolutionAdvice struct {
	CandidateID string          `json:"candidate_id"`
	Kind        string          `json:"kind"`
	Contract    json.RawMessage `json:"contract"`
}

type EvolutionHealth struct {
	Profiles24h int            `json:"profiles_24h"`
	Statuses    map[string]int `json:"statuses"`
	Fallbacks   int            `json:"fallbacks"`
}

func (s *Store) EvolutionHealthForPerson(ctx context.Context, tenantID, personID string) (EvolutionHealth, error) {
	health := EvolutionHealth{Statuses: map[string]int{}}
	if s == nil || s.db == nil {
		return health, nil
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_profiles
		WHERE tenant_id=? AND person_id=? AND created_at>=?`, tenantID, personID, since).Scan(&health.Profiles24h); err != nil {
		return health, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*), COALESCE(SUM(fallback_count),0)
		FROM evolution_candidates WHERE tenant_id=? AND person_id=? GROUP BY status`, tenantID, personID)
	if err != nil {
		return health, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count, fallbacks int
		if err := rows.Scan(&status, &count, &fallbacks); err != nil {
			return health, err
		}
		health.Statuses[status] = count
		health.Fallbacks += fallbacks
	}
	return health, rows.Err()
}

type evolutionEvent struct {
	typ       string
	payload   map[string]interface{}
	createdAt int64
}

func (s *Store) MaterializeWorkflowProfile(ctx context.Context, tenantID, runID string, policy EvolutionPolicy) (*WorkflowProfile, *EvolutionCandidate, error) {
	policy = policy.normalized()
	if s == nil || s.db == nil || !policy.Enabled {
		return nil, nil, nil
	}
	run, err := s.GetRun(ctx, tenantID, runID)
	if err != nil || run == nil {
		return nil, nil, err
	}
	task, err := s.GetTask(ctx, tenantID, run.TaskID)
	if err != nil || task == nil {
		return nil, nil, err
	}
	events, err := s.workflowEvents(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	profile := buildWorkflowProfile(run, task, events)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	skillsJSON, _ := json.Marshal(profile.SkillVersions)
	toolsJSON, _ := json.Marshal(profile.ToolSequence)
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_profiles(
		run_id, tenant_id, person_id, task_id, workspace_id, workflow_signature,
		skill_versions_json, plan_hash, tool_sequence_json, tool_calls, tool_failures,
		provider_calls, duration_ms, input_tokens, output_tokens, billed_input_tokens,
		outcome_status, verification_state, read_only, applied_candidate_id, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		profile.RunID, profile.TenantID, profile.PersonID, profile.TaskID, profile.WorkspaceID,
		profile.WorkflowSignature, string(skillsJSON), profile.PlanHash, string(toolsJSON),
		profile.ToolCalls, profile.ToolFailures, profile.ProviderCalls, profile.DurationMS,
		profile.InputTokens, profile.OutputTokens, profile.BilledInputTokens, profile.OutcomeStatus,
		profile.VerificationState, evolutionBoolInt(profile.ReadOnly), profile.AppliedCandidateID, profile.CreatedAt.Unix())
	if err != nil {
		return nil, nil, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		return profile, nil, nil
	}

	var candidate *EvolutionCandidate
	if profileBatchReadEligible(profile.ToolSequence) && countReadOperations(profile.ToolSequence) >= 3 {
		candidate, err = upsertEvolutionCandidate(ctx, tx, profile, policy)
		if err != nil {
			return nil, nil, err
		}
	}
	if profile.AppliedCandidateID != "" {
		if err := updateAppliedCandidate(ctx, tx, profile); err != nil {
			return nil, nil, err
		}
		candidate, err = getEvolutionCandidateByIDTx(ctx, tx, profile.TenantID, profile.PersonID, profile.WorkspaceID, profile.AppliedCandidateID)
		if err != nil && err != sql.ErrNoRows {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	profilePayload, _ := json.Marshal(profile)
	_, _ = s.AppendEvent(context.WithoutCancel(ctx), Event{
		TaskID: task.ID, RunID: run.ID, Type: "evolution.profiled", Visibility: "task",
		Payload: profilePayload, IdempotencyKey: "evolution-profile:" + run.ID,
	})
	if candidate != nil {
		candidatePayload, _ := json.Marshal(candidate)
		_, _ = s.AppendEvent(context.WithoutCancel(ctx), Event{
			TaskID: task.ID, RunID: run.ID, Type: "evolution.candidate.updated", Visibility: "task",
			Payload: candidatePayload, IdempotencyKey: fmt.Sprintf("evolution-candidate:%s:%s:%d", candidate.ID, run.ID, candidate.ObservationCount),
		})
	}
	return profile, candidate, nil
}

func (s *Store) workflowEvents(ctx context.Context, runID string) ([]evolutionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, COALESCE(payload_json,'{}'), created_at
		FROM task_events WHERE run_id = ? ORDER BY COALESCE(cursor, 0), created_at, rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []evolutionEvent
	for rows.Next() {
		var typ, raw string
		var created int64
		if err := rows.Scan(&typ, &raw, &created); err != nil {
			return nil, err
		}
		var payload map[string]interface{}
		_ = json.Unmarshal([]byte(raw), &payload)
		out = append(out, evolutionEvent{typ: typ, payload: payload, createdAt: created})
	}
	return out, rows.Err()
}

func buildWorkflowProfile(run *Run, task *Task, events []evolutionEvent) *WorkflowProfile {
	profile := &WorkflowProfile{
		RunID: run.ID, TenantID: run.TenantID, PersonID: run.PersonID, TaskID: run.TaskID,
		WorkspaceID: firstEvolutionValue(run.WorkspaceID, task.WorkspaceID), SkillVersions: map[string]string{},
		OutcomeStatus: run.Status, CreatedAt: run.StartedAt,
	}
	if run.FinishedAt != nil {
		profile.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	}
	var planSteps []string
	for _, event := range events {
		switch event.typ {
		case "skill.activated":
			name := stringValue(event.payload["name"])
			if name != "" {
				profile.SkillVersions[name] = stringValue(event.payload["version_hash"])
			}
		case "tool.started":
			tool := stringValue(event.payload["tool"])
			if tool != "" {
				profile.ToolCalls++
				profile.ToolSequence = append(profile.ToolSequence, tool)
			}
			if tool == "batch_read" {
				var args map[string]interface{}
				_ = json.Unmarshal([]byte(stringValue(event.payload["args"])), &args)
				profile.AppliedCandidateID = stringValue(args["candidate_id"])
			}
		case "tool.completed":
			if stringValue(event.payload["error"]) != "" || stringValue(event.payload["error_category"]) != "" {
				profile.ToolFailures++
			}
		case "evolution.batch_item":
			if profile.AppliedCandidateID == "" {
				profile.AppliedCandidateID = stringValue(event.payload["candidate_id"])
			}
			if ok, exists := event.payload["success"].(bool); exists && !ok {
				profile.BatchItemFailures++
			}
		case "provider.call.usage":
			profile.ProviderCalls++
			profile.InputTokens += int64Value(event.payload["input_tokens"])
			profile.OutputTokens += int64Value(event.payload["output_tokens"])
			profile.BilledInputTokens += int64Value(event.payload["billed_input_tokens"])
		case "plan.updated":
			planSteps = normalizedPlanSteps(event.payload["plan"])
		case "run.outcome", "run.finished":
			applyOutcomePayload(profile, event.payload)
		}
	}
	profile.ToolSequence = normalizedToolSequence(profile.ToolSequence)
	profile.ReadOnly = profileIsReadOnly(profile.ToolSequence)
	if len(planSteps) > 0 {
		profile.PlanHash = stableEvolutionHash(planSteps)
	}
	skills := make([]string, 0, len(profile.SkillVersions))
	for name, version := range profile.SkillVersions {
		skills = append(skills, name+"@"+version)
	}
	sort.Strings(skills)
	profile.WorkflowSignature = stableEvolutionHash(map[string]interface{}{
		"version": evolutionProfileVersion, "tools": collapsedToolSequence(profile.ToolSequence), "skills": skills,
	})
	return profile
}

func upsertEvolutionCandidate(ctx context.Context, tx *sql.Tx, profile *WorkflowProfile, policy EvolutionPolicy) (*EvolutionCandidate, error) {
	now := time.Now().Unix()
	contract, _ := json.Marshal(map[string]interface{}{
		"version": 1, "kind": "batch_read", "read_only": true, "max_operations": 8,
		"allowed_tools":     []string{"read_file", "search_files", "ls_r"},
		"evidence_required": true, "fallback": "ordinary_tools",
		"observed_sequence": profile.ToolSequence,
	})
	_, err := tx.ExecContext(ctx, `INSERT INTO evolution_candidates(
		id, tenant_id, person_id, workspace_id, last_task_id, workflow_signature, kind,
		status, contract_json, observation_count, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,'candidate',?,1,?,?)
		ON CONFLICT(tenant_id, person_id, workspace_id, workflow_signature, kind)
		DO UPDATE SET last_task_id=excluded.last_task_id,
		observation_count=evolution_candidates.observation_count+1, updated_at=excluded.updated_at`,
		"evo_"+uuid.NewString(), profile.TenantID, profile.PersonID, profile.WorkspaceID,
		profile.TaskID, profile.WorkflowSignature, "batch_read", string(contract), now, now)
	if err != nil {
		return nil, err
	}
	candidate, err := getEvolutionCandidateTx(ctx, tx, profile.TenantID, profile.PersonID, profile.WorkspaceID, profile.WorkflowSignature, "batch_read")
	if err != nil {
		return nil, err
	}
	success := profile.ToolFailures == 0 && isSuccessfulOutcome(profile.OutcomeStatus)
	status := candidate.Status
	recovered := false
	if status == "candidate" && candidate.ObservationCount >= policy.ShadowAfterObservations && policy.Mode != "observe" {
		status = "shadow"
	}
	if status == "degraded" {
		candidate.ShadowRuns++
		if success {
			candidate.ShadowMatches++
		}
		failureRate := 1.0
		if candidate.ShadowRuns > 0 {
			failureRate = 1 - float64(candidate.ShadowMatches)/float64(candidate.ShadowRuns)
		}
		if candidate.ShadowRuns >= policy.MinShadowRuns && failureRate <= policy.MaxShadowFailureRate {
			status = "shadow"
			recovered = true
			candidate.ShadowRuns = 0
			candidate.ShadowMatches = 0
			candidate.ConsecutiveFailures = 0
			candidate.LastFailure = ""
			candidate.Repair = json.RawMessage(`{}`)
		}
	}
	if status == "shadow" && !recovered {
		candidate.ShadowRuns++
		if success {
			candidate.ShadowMatches++
		}
		failureRate := 1.0
		if candidate.ShadowRuns > 0 {
			failureRate = 1 - float64(candidate.ShadowMatches)/float64(candidate.ShadowRuns)
		}
		if candidate.ObservationCount >= policy.PromoteAfterObservations && candidate.ShadowRuns >= policy.MinShadowRuns && failureRate <= policy.MaxShadowFailureRate {
			status = "eligible"
			if policy.Mode == "auto-readonly" {
				status = "enabled"
			}
		}
	}
	candidate.Status = status
	enabledAt := interface{}(nil)
	if status == "enabled" {
		enabledAt = now
	}
	repairJSON := string(candidate.Repair)
	if repairJSON == "" {
		repairJSON = "{}"
	}
	_, err = tx.ExecContext(ctx, `UPDATE evolution_candidates SET status=?, shadow_runs=?, shadow_matches=?,
		repair_json=?, consecutive_failures=?, last_failure=?, enabled_at=COALESCE(enabled_at, ?), updated_at=? WHERE id=?`,
		status, candidate.ShadowRuns, candidate.ShadowMatches, repairJSON,
		candidate.ConsecutiveFailures, candidate.LastFailure, enabledAt, now, candidate.ID)
	if err != nil {
		return nil, err
	}
	candidate.UpdatedAt = time.Unix(now, 0)
	return candidate, nil
}

func updateAppliedCandidate(ctx context.Context, tx *sql.Tx, profile *WorkflowProfile) error {
	now := time.Now().Unix()
	if profile.BatchItemFailures == 0 && profile.ToolFailures == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE evolution_candidates SET consecutive_failures=0,
			last_applied_at=?, updated_at=? WHERE tenant_id=? AND person_id=? AND workspace_id=? AND id=?`,
			now, now, profile.TenantID, profile.PersonID, profile.WorkspaceID, profile.AppliedCandidateID)
		return err
	}
	repair, _ := json.Marshal(map[string]interface{}{
		"kind": "contract_review", "reason": "batch_read item failed; ordinary tools remain the fallback",
		"run_id": profile.RunID, "failed_items": profile.BatchItemFailures,
	})
	_, err := tx.ExecContext(ctx, `UPDATE evolution_candidates SET status='degraded',
		repair_json=?, fallback_count=fallback_count+1, consecutive_failures=consecutive_failures+1,
		shadow_runs=0, shadow_matches=0, last_failure=?, last_applied_at=?, updated_at=?
		WHERE tenant_id=? AND person_id=? AND workspace_id=? AND id=?`, string(repair),
		fmt.Sprintf("run %s: %d batch item failures", profile.RunID, profile.BatchItemFailures),
		now, now, profile.TenantID, profile.PersonID, profile.WorkspaceID, profile.AppliedCandidateID)
	return err
}

func getEvolutionCandidateByIDTx(ctx context.Context, tx *sql.Tx, tenantID, personID, workspaceID, candidateID string) (*EvolutionCandidate, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, workspace_id, last_task_id,
		workflow_signature, kind, status, contract_json, repair_json, observation_count,
		shadow_runs, shadow_matches, fallback_count, consecutive_failures, last_failure,
		created_at, updated_at FROM evolution_candidates
		WHERE tenant_id=? AND person_id=? AND workspace_id=? AND id=?`,
		tenantID, personID, workspaceID, candidateID)
	return scanEvolutionCandidate(row)
}

func getEvolutionCandidateTx(ctx context.Context, tx *sql.Tx, tenantID, personID, workspaceID, signature, kind string) (*EvolutionCandidate, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, workspace_id, last_task_id,
		workflow_signature, kind, status, contract_json, repair_json, observation_count,
		shadow_runs, shadow_matches, fallback_count, consecutive_failures, last_failure,
		created_at, updated_at FROM evolution_candidates
		WHERE tenant_id=? AND person_id=? AND workspace_id=? AND workflow_signature=? AND kind=?`,
		tenantID, personID, workspaceID, signature, kind)
	return scanEvolutionCandidate(row)
}

func (s *Store) EnabledEvolutionAdvice(ctx context.Context, tenantID, personID, taskID string) (*EvolutionAdvice, error) {
	if s == nil || s.db == nil || strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, contract_json FROM evolution_candidates
		WHERE tenant_id=? AND person_id=? AND last_task_id=? AND status='enabled'
		ORDER BY updated_at DESC LIMIT 1`, tenantID, personID, taskID)
	var advice EvolutionAdvice
	var contract string
	if err := row.Scan(&advice.CandidateID, &advice.Kind, &contract); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	advice.Contract = json.RawMessage(contract)
	return &advice, nil
}

type evolutionRowScanner interface{ Scan(...interface{}) error }

func scanEvolutionCandidate(row evolutionRowScanner) (*EvolutionCandidate, error) {
	var candidate EvolutionCandidate
	var contract, repair string
	var created, updated int64
	if err := row.Scan(&candidate.ID, &candidate.TenantID, &candidate.PersonID, &candidate.WorkspaceID,
		&candidate.LastTaskID, &candidate.WorkflowSignature, &candidate.Kind, &candidate.Status,
		&contract, &repair, &candidate.ObservationCount, &candidate.ShadowRuns, &candidate.ShadowMatches,
		&candidate.FallbackCount, &candidate.ConsecutiveFailures, &candidate.LastFailure, &created, &updated); err != nil {
		return nil, err
	}
	candidate.Contract = json.RawMessage(contract)
	candidate.Repair = json.RawMessage(repair)
	candidate.CreatedAt = time.Unix(created, 0)
	candidate.UpdatedAt = time.Unix(updated, 0)
	return &candidate, nil
}

func normalizedToolSequence(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		family := evolutionToolFamily(tool)
		if family == "" {
			continue
		}
		out = append(out, family)
	}
	return out
}

func collapsedToolSequence(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		if len(out) == 0 || out[len(out)-1] != tool {
			out = append(out, tool)
		}
	}
	return out
}

func evolutionToolFamily(tool string) string {
	switch strings.TrimSpace(tool) {
	case "read_file", "cat":
		return "file.read"
	case "search_files", "grep":
		return "file.search"
	case "ls_r", "list_files":
		return "file.list"
	case "web_search":
		return "web.search"
	case "web_extract":
		return "web.extract"
	case "session_search":
		return "session.search"
	case "skills_list", "skill_view":
		return "skill.read"
	case "batch_read":
		return "batch.read"
	case "update_plan", "finish_run", "clarify", "get_current_time", "tool_search":
		return ""
	default:
		return strings.TrimSpace(tool)
	}
}

func profileIsReadOnly(sequence []string) bool {
	if len(sequence) == 0 {
		return false
	}
	for _, tool := range sequence {
		switch tool {
		case "file.read", "file.search", "file.list", "web.search", "web.extract", "session.search", "skill.read", "batch.read":
		default:
			return false
		}
	}
	return true
}

func countReadOperations(sequence []string) int {
	count := 0
	for _, tool := range sequence {
		switch tool {
		case "file.read", "file.search", "file.list":
			count++
		}
	}
	return count
}

func profileBatchReadEligible(sequence []string) bool {
	if len(sequence) == 0 {
		return false
	}
	for _, tool := range sequence {
		switch tool {
		case "file.read", "file.search", "file.list", "skill.read":
		default:
			return false
		}
	}
	return true
}

func normalizedPlanSteps(raw interface{}) []string {
	items, _ := raw.([]interface{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]interface{}); ok {
			if step := strings.TrimSpace(stringValue(row["step"])); step != "" {
				out = append(out, step)
			}
		}
	}
	return out
}

func applyOutcomePayload(profile *WorkflowProfile, payload map[string]interface{}) {
	if nested, ok := payload["outcome"].(map[string]interface{}); ok {
		payload = nested
	}
	if status := stringValue(payload["status"]); status != "" {
		profile.OutcomeStatus = status
	}
	verification := ""
	if nested, ok := payload["verification"].(map[string]interface{}); ok {
		verification = stringValue(nested["state"])
	}
	profile.VerificationState = firstEvolutionValue(
		stringValue(payload["verification_state"]),
		verification,
	)
}

func stableEvolutionHash(value interface{}) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest[:])
}

func isSuccessfulOutcome(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "succeeded", "success":
		return true
	default:
		return false
	}
}

func stringValue(value interface{}) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func int64Value(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func evolutionBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstEvolutionValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
