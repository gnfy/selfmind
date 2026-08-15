package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LegacyTaskReferenceMigrationFinding describes one historical task_runs.work_key
// row considered by the explicit migration command. Legacy work keys are never
// promoted from inferred labels or summaries: the surface form must occur in
// the original user input stored on the run.
type LegacyTaskReferenceMigrationFinding struct {
	PersonID    string
	TaskID      string
	RunID       string
	WorkspaceID string
	Value       string
	Input       string
	Eligible    bool
	Imported    bool
	Reason      string
}

type LegacyTaskReferenceMigration struct {
	Scanned         int
	Eligible        int
	SkippedInferred int
	AlreadyImported int
	Applied         int
	Findings        []LegacyTaskReferenceMigrationFinding
}

// MigrateLegacyTaskReferences audits historical task_runs.work_key values and,
// when apply is true, imports only exact user-text evidence into governed task
// references. It is deliberately never called at daemon startup.
func (s *Store) MigrateLegacyTaskReferences(ctx context.Context, tenantID string, limit int, apply bool) (LegacyTaskReferenceMigration, error) {
	var result LegacyTaskReferenceMigration
	if s == nil || s.db == nil {
		return result, fmt.Errorf("control store is unavailable")
	}
	if limit <= 0 || limit > 100000 {
		return result, fmt.Errorf("limit must be between 1 and 100000")
	}
	tenantID = normalizeTenant(tenantID)
	rows, err := s.db.QueryContext(ctx, `SELECT r.person_id, r.task_id, r.id,
		COALESCE(r.workspace_id, ''), TRIM(COALESCE(r.work_key, '')), COALESCE(r.input_summary, '')
		FROM task_runs r
		JOIN tasks t ON t.tenant_id = r.tenant_id AND t.person_id = r.person_id AND t.id = r.task_id
		WHERE r.tenant_id = ? AND TRIM(COALESCE(r.work_key, '')) != ''
		ORDER BY r.started_at ASC, r.id ASC LIMIT ?`, tenantID, limit)
	if err != nil {
		return result, err
	}
	var candidates []LegacyTaskReferenceMigrationFinding
	for rows.Next() {
		var finding LegacyTaskReferenceMigrationFinding
		if err := rows.Scan(&finding.PersonID, &finding.TaskID, &finding.RunID,
			&finding.WorkspaceID, &finding.Value, &finding.Input); err != nil {
			_ = rows.Close()
			return result, err
		}
		candidates = append(candidates, finding)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	// The control store intentionally uses one SQLite connection. Finish the
	// read snapshot before checking or inserting evidence so --apply cannot
	// wait on its own open Rows cursor.
	for _, finding := range candidates {
		result.Scanned++
		if !TaskReferenceAppearsInText(finding.Input, finding.Value) {
			finding.Reason = "work key is absent from the original user input"
			result.SkippedInferred++
			result.Findings = append(result.Findings, finding)
			continue
		}
		finding.Eligible = true
		finding.Reason = "exact surface form in original user input"
		result.Eligible++
		provenance := "legacy_user_text"
		sourceRef := "task_runs.work_key:" + finding.RunID
		already, err := s.hasTaskReferenceEvidence(ctx, tenantID, finding.PersonID, finding.TaskID,
			finding.Value, provenance, sourceRef)
		if err != nil {
			return result, err
		}
		if already {
			finding.Imported = true
			result.AlreadyImported++
			result.Findings = append(result.Findings, finding)
			continue
		}
		if apply {
			if _, err := s.UpsertTaskReference(ctx, TaskReferenceWrite{
				TenantID: tenantID, PersonID: finding.PersonID, TaskID: finding.TaskID,
				WorkspaceID: finding.WorkspaceID, Class: TaskReferenceLiteral, Value: finding.Value,
				Status: TaskReferenceCandidate, RunID: finding.RunID,
				Provenance: provenance, SourceRef: sourceRef,
			}); err != nil {
				return result, fmt.Errorf("import run %s: %w", finding.RunID, err)
			}
			finding.Imported = true
			result.Applied++
		}
		result.Findings = append(result.Findings, finding)
	}
	return result, nil
}

func (s *Store) hasTaskReferenceEvidence(ctx context.Context, tenantID, personID, taskID, value, provenance, sourceRef string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1
		FROM task_reference_evidence e
		JOIN task_references r ON r.id = e.reference_id
		WHERE r.tenant_id = ? AND r.person_id = ? AND r.task_id = ?
		  AND r.normalized_value = ? AND e.evidence_hash = ? LIMIT 1`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(taskID),
		NormalizeTaskReference(value), referenceEvidenceHash(value, provenance, sourceRef)).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return found == 1, err
}
