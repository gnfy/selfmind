package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"selfmind/internal/executionenv"
)

// RunRootReservation is execution occupancy, independent of whether the
// process registry still has a worker or cancel handle. A durably parked human
// wait retains its scope and checkpoint but releases its physical directory.
type RunRootReservation struct {
	RunID          string
	ExecutionRoots []executionenv.RootBinding
}

func (s *Store) ListRunRootReservations(ctx context.Context, tenantID, personID string) ([]RunRootReservation, error) {
	return s.listRunRoots(ctx, `SELECT id, COALESCE(execution_roots_json, '[]') FROM runs
		WHERE tenant_id=? AND person_id=? AND execution_class='work' AND status='running'`,
		normalizeTenant(tenantID), personID)
}

// ListRunRootActivitySince supplies a conservative revalidation projection.
// A Run allowed to write overlapping roots may have changed facts while a human
// wait was parked. No old permission, effect claim or checkpoint is rewritten.
func (s *Store) ListRunRootActivitySince(ctx context.Context, tenantID, personID, parentID, currentID string, since time.Time) ([]RunRootReservation, error) {
	tenantID = normalizeTenant(tenantID)
	lineage, err := runLineage(ctx, s.db, tenantID, personID, parentID)
	if err != nil {
		return nil, err
	}
	activity, err := s.listRunRoots(ctx, `SELECT id, COALESCE(execution_roots_json, '[]') FROM runs
		WHERE tenant_id=? AND person_id=? AND execution_class='work' AND id NOT IN (?, ?)
		AND (status='running' OR finished_at>=?) LIMIT 501`,
		tenantID, personID, parentID, currentID, since.Unix())
	if err != nil {
		return nil, err
	}
	// Earlier exact ancestors are already represented in the saved ledger.
	// Second-resolution timestamps can otherwise count a recent ancestor as
	// independent work during successive human waits.
	out := activity[:0]
	for _, run := range activity {
		if !lineage[run.RunID] {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s *Store) listRunRoots(ctx context.Context, query string, args ...any) ([]RunRootReservation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRootReservation
	for rows.Next() {
		var reservation RunRootReservation
		var roots string
		if err := rows.Scan(&reservation.RunID, &roots); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(roots), &reservation.ExecutionRoots); err != nil {
			return nil, fmt.Errorf("decode Run execution roots: %w", err)
		}
		out = append(out, reservation)
		if len(out) > 500 {
			return nil, fmt.Errorf("Run root activity exceeds the bounded revalidation window")
		}
	}
	return out, rows.Err()
}
