package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/verification"
)

// The journal stores derived source references, not adopted verification.
// They commit with Plan inheritance and carry invalidation through every wait.
// Historical Plans are not rewritten and a missing reference remains unknown.
func saveInheritedEvidenceReferencesTx(ctx context.Context, tx *sql.Tx, tenant, childRunID, parentRunID string) error {
	items, err := inheritedPlanEvidenceTx(ctx, tx, tenant, childRunID)
	if err != nil || len(items) == 0 {
		return err
	}
	for i := range items {
		if err := inheritedEvidenceChangesTx(ctx, tx, childRunID, &items[i]); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(inheritedEvidenceReference{Version: 1, ResumeSourceRunID: parentRunID, Items: items})
	if err != nil {
		return err
	}
	var threadID, channel string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id,channel FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).Scan(&threadID, &channel); err != nil {
		return err
	}
	var cursor int64
	if err := tx.QueryRowContext(ctx, `UPDATE event_sequence SET next_cursor=next_cursor+1 WHERE id=1 RETURNING next_cursor`).Scan(&cursor); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_events(id,cursor,thread_id,run_id,type,visibility,channel,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"event_"+uuid.NewString(), cursor, threadID, childRunID, "plan.evidence_inherited", "task", channel, string(raw), time.Now().Unix())
	return err
}

// A journal reference cannot enlarge a frozen evidence window, including
// references written by older binaries. Check the exact id against its current
// effective obligations before trusting either the reference or its freshness.
func inheritedSourceChecksTx(ctx context.Context, tx *sql.Tx, item InheritedPlanEvidence) ([]verification.Check, error) {
	var workUnitID string
	if err := tx.QueryRowContext(ctx, `SELECT work_unit_id FROM run_plan_steps
		WHERE run_id=? AND plan_version=? AND step_id=?`, item.SourceRunID, item.SourcePlanVersion, item.SourceStepID).Scan(&workUnitID); err != nil {
		return nil, err
	}
	_, _, checks, err := sourceStepVerificationTx(ctx, tx, item.SourceRunID, item.SourceStepID, workUnitID)
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		if check.ToolCallID == item.CheckID && check.Binding.Target == item.Target && check.Binding.Criterion == item.ObservedCriterion {
			return checks, nil
		}
	}
	return nil, sql.ErrNoRows
}

func inheritedEvidenceChangesTx(ctx context.Context, tx *sql.Tx, runID string, item *InheritedPlanEvidence) error {
	if item.CheckID == "" || item.ReuseBlockedReason != "" {
		return nil
	}
	var sameScope bool
	if err := tx.QueryRowContext(ctx, `SELECT a.tenant_id=b.tenant_id AND a.person_id=b.person_id
		AND COALESCE(a.workspace_id,'')=COALESCE(b.workspace_id,'')
		AND COALESCE(a.execution_roots_json,'[]')=COALESCE(b.execution_roots_json,'[]')
		FROM runs a JOIN runs b ON b.id=? WHERE a.id=?`, item.SourceRunID, runID).Scan(&sameScope); err != nil {
		return err
	}
	if !sameScope {
		item.ReuseBlockedReason = "execution scope changed along the continuation"
		return nil
	}
	checks, err := inheritedSourceChecksTx(ctx, tx, *item)
	if err == sql.ErrNoRows {
		item.ReuseBlockedReason = "source observation is unavailable"
		return nil
	}
	if err != nil {
		return err
	}
	for _, check := range checks {
		for _, id := range []string{item.SourceRunID, runID} {
			changed, err := verificationInputsChangedTx(ctx, tx, id, check)
			if err != nil {
				return err
			}
			if changed {
				item.ReuseBlockedReason = "recorded changes affect the check's declared inputs"
				return nil
			}
		}
	}
	return nil
}

// Reuse the within-Run dependency rule rather than invalidating a remote
// observation when an unrelated local report is written. Unrecorded paths and
// malformed mutation evidence stay conservative.
func verificationInputsChangedTx(ctx context.Context, tx *sql.Tx, runID string, check verification.Check) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND type='evidence.recorded'
		AND json_extract(payload_json,'$.evidence.kind')='mutation'`, runID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var mutations []verification.Mutation
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var p struct {
			Evidence workUnitEvidence `json:"evidence"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return false, err
		}
		m := p.Evidence
		if len(m.Files) == 0 {
			return true, nil
		}
		for _, file := range m.Files {
			if file.BeforeSHA256 == file.AfterSHA256 {
				continue
			}
			if file.Path == "" || m.FinishedAt <= 0 {
				return true, nil
			}
			mutations = append(mutations, verification.Mutation{Path: file.Path, FinishedAt: m.FinishedAt})
			if file.ResolvedPath != "" {
				mutations = append(mutations, verification.Mutation{Path: file.ResolvedPath, FinishedAt: m.FinishedAt})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return verification.RelevantMutationAt(check, mutations) > check.StartedAt, nil
}
