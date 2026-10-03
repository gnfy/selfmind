package control

import (
	"context"
	"fmt"
	"time"
)

type WorkChainProjection struct {
	Runs           int
	Chains         int
	ResumeEdges    int
	LatestStatuses map[string]int
	ResumeOrigins  map[string]int
	Truncated      bool
}

// WorkChainsSince groups observed Runs by their committed parent edges, not
// by the presence of a particular event. Ancestors outside the report window
// establish identity but do not inflate its Run or edge counts.
func (s *Store) WorkChainsSince(ctx context.Context, tenantID, personID string, since time.Time, limit int) (WorkChainProjection, error) {
	result := WorkChainProjection{LatestStatuses: map[string]int{}, ResumeOrigins: map[string]int{}}
	if personID == "" {
		return result, fmt.Errorf("work chain owner is required")
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,COALESCE((SELECT json_extract(e.payload_json,'$.origin')
		FROM task_events e WHERE e.run_id=r.id AND e.type='run.started' ORDER BY e.cursor DESC LIMIT 1),'manual')
		FROM runs r WHERE r.tenant_id=? AND r.person_id=? AND (r.started_at>=? OR r.finished_at>=?)
		ORDER BY r.started_at DESC,r.id DESC LIMIT ?`, normalizeTenant(tenantID), personID, since.Unix(), since.Unix(), limit+1)
	if err != nil {
		return result, err
	}
	type entry struct{ id, origin string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.origin); err != nil {
			_ = rows.Close()
			return result, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if len(entries) > limit {
		result.Truncated = true
		entries = entries[:limit]
	}
	result.Runs = len(entries)
	cache := map[string]*Run{}
	load := func(id string) (*Run, error) {
		if r, ok := cache[id]; ok {
			return r, nil
		}
		if len(cache) >= 20000 {
			return nil, fmt.Errorf("work chain ancestor safety cap reached")
		}
		r, err := s.GetRun(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if r == nil || r.PersonID != personID {
			return nil, fmt.Errorf("work chain parent is unavailable for this owner")
		}
		cache[id] = r
		return r, nil
	}
	type latest struct {
		depth  int
		status string
	}
	chains := map[string]latest{}
	for _, entry := range entries {
		run, err := load(entry.id)
		if err != nil {
			return result, err
		}
		if run.ResumesRunID != "" {
			result.ResumeEdges++
			origin := entry.origin
			if origin == "" {
				origin = "manual"
			}
			result.ResumeOrigins[origin]++
		}
		root := run
		seen := map[string]bool{run.ID: true}
		for root.ResumesRunID != "" {
			if seen[root.ResumesRunID] || len(seen) >= 128 {
				return result, fmt.Errorf("work chain contains a cycle or exceeds the ancestor bound")
			}
			root, err = load(root.ResumesRunID)
			if err != nil {
				return result, err
			}
			seen[root.ID] = true
		}
		if prev, ok := chains[root.ID]; !ok || len(seen) > prev.depth {
			chains[root.ID] = latest{len(seen), run.Status}
		}
	}
	result.Chains = len(chains)
	for _, chain := range chains {
		result.LatestStatuses[chain.status]++
	}
	return result, nil
}
