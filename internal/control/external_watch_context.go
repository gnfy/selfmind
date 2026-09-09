package control

import "context"

// ListRunExternalWatches selects bounded evidence by exact execution ownership.
// It does not expose another Run's watches even if they share a Thread.
func (s *Store) ListRunExternalWatches(ctx context.Context, tenantID, personID, runID string) ([]ExternalWatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalWatchColumns+` FROM external_watches
 WHERE tenant_id=? AND person_id=? AND run_id=? ORDER BY created_at, id LIMIT 16`, normalizeTenant(tenantID), personID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var watches []ExternalWatch
	for rows.Next() {
		watch, err := scanExternalWatch(rows)
		if err != nil {
			return nil, err
		}
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}
