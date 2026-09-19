package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RunAttentionOutcomes reads only the exact owned Runs displayed in a page.
// A later outcome in the same historical grouping cannot relabel another Run.
func (s *Store) RunAttentionOutcomes(ctx context.Context, tenant, person string, ids []string) (map[string]LatestRunOutcome, error) {
	out := map[string]LatestRunOutcome{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 100 {
		return nil, fmt.Errorf("attention outcome page exceeds 100 Runs")
	}
	args := []interface{}{normalizeTenant(tenant), person}
	for _, id := range ids {
		args = append(args, id)
	}
	query := `SELECT e.run_id,e.payload_json FROM task_events e JOIN runs r ON r.id=e.run_id
	 WHERE r.tenant_id=? AND r.person_id=? AND r.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)
	 AND e.type IN ('run.finished','run.interrupted')
	 AND e.rowid=(SELECT e2.rowid FROM task_events e2 WHERE e2.run_id=e.run_id
	 AND e2.type IN ('run.finished','run.interrupted') ORDER BY e2.created_at DESC,e2.rowid DESC LIMIT 1)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var payload struct {
			Outcome struct {
				CompletionReason string   `json:"completion_reason"`
				Resumable        bool     `json:"resumable"`
				Summary          string   `json:"summary"`
				NextSteps        []string `json:"next_steps"`
				Verification     struct {
					Summary string `json:"summary"`
				} `json:"verification"`
			} `json:"outcome"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			p := payload.Outcome
			out[id] = LatestRunOutcome{CompletionReason: p.CompletionReason, Resumable: p.Resumable, Summary: p.Summary, NextSteps: p.NextSteps, VerificationSummary: p.Verification.Summary}
		}
	}
	return out, rows.Err()
}
