package control

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SkillUsageStat is the canonical user-visible Skill usage projection. It is
// derived only from durable activations and their terminal work-unit outcomes;
// the legacy memory.db skill_metrics table is intentionally excluded.
type SkillUsageStat struct {
	SkillKey   string    `json:"skill_key"`
	SkillName  string    `json:"skill_name"`
	Calls      int       `json:"calls"`
	Completed  int       `json:"completed"`
	Fallbacks  int       `json:"fallbacks"`
	Failures   int       `json:"failures"`
	Parked     int       `json:"parked"`
	Cancelled  int       `json:"cancelled"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// SkillUsageStats returns one control-tenant-scoped aggregate per logical
// Skill. Activation state attributes the Skill result; the joined work-unit
// state separates an explicit fallback followed by ordinary-planner recovery
// from a work unit that ultimately failed.
func (s *Store) SkillUsageStats(ctx context.Context, controlTenantID string) ([]SkillUsageStat, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.skill_key, a.skill_name,
		COUNT(*),
		SUM(CASE WHEN a.state='completed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN a.state='fallback' AND COALESCE(w.status, '')<>'failed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN COALESCE(w.status, '')='failed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN a.state='parked' OR COALESCE(w.status, '')='parked' THEN 1 ELSE 0 END),
		SUM(CASE WHEN a.state='cancelled' OR COALESCE(w.status, '')='cancelled' THEN 1 ELSE 0 END),
		MAX(a.selected_at)
		FROM run_skill_activations a
		LEFT JOIN run_work_units w ON w.identity_tenant_id=a.identity_tenant_id
			AND w.run_id=a.run_id AND w.id=a.work_unit_id
		WHERE a.control_tenant_id=?
		GROUP BY a.skill_key, a.skill_name`, normalizeTenant(controlTenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []SkillUsageStat
	for rows.Next() {
		var item SkillUsageStat
		var selectedAt int64
		if err := rows.Scan(&item.SkillKey, &item.SkillName, &item.Calls, &item.Completed,
			&item.Fallbacks, &item.Failures, &item.Parked, &item.Cancelled, &selectedAt); err != nil {
			return nil, err
		}
		item.LastUsedAt = time.Unix(selectedAt, 0)
		stats = append(stats, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].LastUsedAt.Equal(stats[j].LastUsedAt) {
			return strings.ToLower(stats[i].SkillName) < strings.ToLower(stats[j].SkillName)
		}
		return stats[i].LastUsedAt.After(stats[j].LastUsedAt)
	})
	return stats, nil
}
