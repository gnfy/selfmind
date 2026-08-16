package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// EvalResidueReport summarizes what CleanEvalResidue would delete (dry run) or
// actually deleted (apply). Counts are per table so a dry run can show the user
// exactly how much eval pollution accumulated in a real control.db.
type EvalResidueReport struct {
	Persons              int `json:"persons"`
	Accounts             int `json:"accounts"`
	Workspaces           int `json:"workspaces"`
	CurrentWorkspace     int `json:"current_workspace"`
	Tasks                int `json:"tasks"`
	CurrentTask          int `json:"current_task"`
	Runs                 int `json:"task_runs"`
	Events               int `json:"task_events"`
	Handoffs             int `json:"task_handoffs"`
	Artifacts            int `json:"task_artifacts"`
	ChannelMessages      int `json:"channel_messages"`
	Approvals            int `json:"approval_requests"`
	Notifications        int `json:"notifications"`
	Outbound             int `json:"outbound_messages"`
	WorkUnits            int `json:"run_work_units"`
	SkillActivations     int `json:"run_skill_activations"`
	SkillBindings        int `json:"task_skill_bindings"`
	WorkflowProfiles     int `json:"workflow_profiles"`
	EvolutionCandidates  int `json:"evolution_candidates"`
	WorkflowObservations int `json:"workflow_observations"`
	SkillVersions        int `json:"skill_versions"`
	SkillFailureGuards   int `json:"skill_failure_guards"`
	Tenants              int `json:"tenants"`

	// PersonIDs lists the selected eval-only persons so a dry run can be audited
	// before deleting anything.
	PersonIDs []string `json:"person_ids,omitempty"`
}

// Empty reports whether the selection found no eval residue at all.
func (r *EvalResidueReport) Empty() bool {
	if r == nil {
		return true
	}
	return r.Persons == 0 && r.Accounts == 0 && r.Tenants == 0
}

// CleanEvalResidue removes rows the eval harness historically leaked into a
// shared control.db: persons whose ONLY accounts have platform `eval`, plus
// everything keyed to those persons (accounts, workspaces, current_task /
// current_workspace pointers, tasks, task_runs, task_events, task_handoffs,
// task_artifacts, channel messages, approvals, notifications, outbound
// messages, and person-scoped Skill/evolution projections) and any non-default
// tenants left empty by the deletion.
//
// The selection is deliberately conservative: a person with even one non-eval
// account (e.g. a real cli/local binding) is never touched. When apply is
// false, nothing is deleted and the report describes what WOULD be removed.
func (s *Store) CleanEvalResidue(ctx context.Context, apply bool) (*EvalResidueReport, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is not open")
	}
	report := &EvalResidueReport{}

	// Select eval-only persons: at least one eval account, no non-eval account.
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.tenant_id FROM persons p
		WHERE EXISTS (SELECT 1 FROM accounts a WHERE a.person_id = p.id AND a.platform = 'eval')
		  AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.person_id = p.id AND a.platform != 'eval')`)
	if err != nil {
		return nil, err
	}
	var personIDs []string
	tenantIDs := map[string]bool{}
	for rows.Next() {
		var personID, tenantID string
		if err := rows.Scan(&personID, &tenantID); err != nil {
			rows.Close()
			return nil, err
		}
		personIDs = append(personIDs, personID)
		tenantIDs[tenantID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	report.PersonIDs = personIDs
	report.Persons = len(personIDs)
	if len(personIDs) == 0 {
		return report, nil
	}

	taskIDs, err := s.collectColumn(ctx, `tasks`, `person_id`, `id`, personIDs)
	if err != nil {
		return nil, err
	}
	report.Tasks = len(taskIDs)

	// Count everything first so dry run and apply report the same numbers.
	counts := []struct {
		dst   *int
		table string
		key   string
		ids   []string
	}{
		{&report.Accounts, "accounts", "person_id", personIDs},
		{&report.Workspaces, "workspaces", "owner_person_id", personIDs},
		{&report.CurrentWorkspace, "current_workspace", "person_id", personIDs},
		{&report.CurrentTask, "current_task", "person_id", personIDs},
		{&report.Runs, "task_runs", "person_id", personIDs},
		{&report.Events, "task_events", "task_id", taskIDs},
		{&report.Handoffs, "task_handoffs", "task_id", taskIDs},
		{&report.Artifacts, "task_artifacts", "task_id", taskIDs},
		{&report.ChannelMessages, "channel_messages", "person_id", personIDs},
		{&report.Approvals, "approval_requests", "person_id", personIDs},
		{&report.Notifications, "notifications", "person_id", personIDs},
		{&report.Outbound, "outbound_messages", "person_id", personIDs},
		{&report.WorkUnits, "run_work_units", "person_id", personIDs},
		{&report.SkillActivations, "run_skill_activations", "person_id", personIDs},
		{&report.SkillBindings, "task_skill_bindings", "person_id", personIDs},
		{&report.WorkflowProfiles, "workflow_profiles", "person_id", personIDs},
		{&report.EvolutionCandidates, "evolution_candidates", "person_id", personIDs},
		{&report.WorkflowObservations, "workflow_observations", "person_id", personIDs},
	}
	for _, c := range counts {
		n, err := s.countByColumn(ctx, c.table, c.key, c.ids)
		if err != nil {
			return nil, err
		}
		*c.dst = n
	}

	// Tenants that would be left with no persons after the deletion (eval runs
	// historically created one throwaway `eval-...` tenant per harness). The
	// default tenant is never removed.
	emptyTenants, err := s.tenantsEmptiedBy(ctx, tenantIDs, personIDs)
	if err != nil {
		return nil, err
	}
	report.Tenants = len(emptyTenants)
	if report.SkillVersions, err = s.countByColumn(ctx, "skill_versions", "control_tenant_id", emptyTenants); err != nil {
		return nil, err
	}
	if report.SkillFailureGuards, err = s.countByColumn(ctx, "skill_failure_guards", "control_tenant_id", emptyTenants); err != nil {
		return nil, err
	}

	if !apply {
		return report, nil
	}

	// Apply all deletions in one transaction so a failure cannot leave e.g.
	// orphaned tasks whose person row is already gone.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	deletes := []struct {
		table string
		key   string
		ids   []string
	}{
		{"workflow_observations", "person_id", personIDs},
		{"run_skill_activations", "person_id", personIDs},
		{"run_work_units", "person_id", personIDs},
		{"workflow_profiles", "person_id", personIDs},
		{"evolution_candidates", "person_id", personIDs},
		{"task_skill_bindings", "person_id", personIDs},
		{"task_events", "task_id", taskIDs},
		{"task_handoffs", "task_id", taskIDs},
		{"task_artifacts", "task_id", taskIDs},
		{"task_runs", "person_id", personIDs},
		{"current_task", "person_id", personIDs},
		{"tasks", "person_id", personIDs},
		{"channel_messages", "person_id", personIDs},
		{"approval_requests", "person_id", personIDs},
		{"notifications", "person_id", personIDs},
		{"outbound_messages", "person_id", personIDs},
		{"current_workspace", "person_id", personIDs},
		{"workspaces", "owner_person_id", personIDs},
		{"accounts", "person_id", personIDs},
		{"persons", "id", personIDs},
	}
	for _, d := range deletes {
		if err := execDeleteChunked(ctx, tx, d.table, d.key, d.ids); err != nil {
			return nil, err
		}
	}
	for _, tenantID := range emptyTenants {
		if _, err := tx.ExecContext(ctx, `DELETE FROM skill_failure_guards WHERE control_tenant_id = ?`, tenantID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM skill_versions WHERE control_tenant_id = ?`, tenantID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tenants WHERE id = ?`, tenantID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return report, nil
}

// tenantsEmptiedBy returns the candidate tenants whose entire person set is
// contained in the to-be-deleted person list (excluding the default tenant).
func (s *Store) tenantsEmptiedBy(ctx context.Context, tenantIDs map[string]bool, personIDs []string) ([]string, error) {
	doomed := make(map[string]bool, len(personIDs))
	for _, id := range personIDs {
		doomed[id] = true
	}
	var out []string
	for tenantID := range tenantIDs {
		if tenantID == DefaultTenantID {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM persons WHERE tenant_id = ?`, tenantID)
		if err != nil {
			return nil, err
		}
		empties := true
		for rows.Next() {
			var personID string
			if err := rows.Scan(&personID); err != nil {
				rows.Close()
				return nil, err
			}
			if !doomed[personID] {
				empties = false
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if empties {
			out = append(out, tenantID)
		}
	}
	return out, nil
}

// collectColumn returns SELECT <sel> FROM <table> WHERE <key> IN (ids), chunked
// to stay under SQLite bind-variable limits.
func (s *Store) collectColumn(ctx context.Context, table, key, sel string, ids []string) ([]string, error) {
	var out []string
	for _, chunk := range chunkStrings(ids, 400) {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s IN (%s)`, sel, table, key, placeholders(len(chunk)))
		rows, err := s.db.QueryContext(ctx, query, toAnySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) countByColumn(ctx context.Context, table, key string, ids []string) (int, error) {
	total := 0
	for _, chunk := range chunkStrings(ids, 400) {
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IN (%s)`, table, key, placeholders(len(chunk)))
		var n int
		if err := s.db.QueryRowContext(ctx, query, toAnySlice(chunk)...).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func execDeleteChunked(ctx context.Context, tx *sql.Tx, table, key string, ids []string) error {
	for _, chunk := range chunkStrings(ids, 400) {
		query := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, table, key, placeholders(len(chunk)))
		if _, err := tx.ExecContext(ctx, query, toAnySlice(chunk)...); err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func chunkStrings(ids []string, size int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	var chunks [][]string
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

func toAnySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
