package control

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SkillAttribution is one observation that a work unit used a Skill's content
// without activating it. It is evidence of use, not of activation: it freezes no
// version, package hash, or resource manifest, and it is therefore never
// admissible for curator cohorts or repair thresholds.
type SkillAttribution struct {
	ControlTenantID string
	PersonID        string
	RunID           string
	WorkUnitID      string
	SkillKey        string
	SkillName       string
	PackagePath     string
	PackageName     string
	Scope           string
	Provenance      string
	ToolName        string
	ObservedAt      time.Time
}

// SkillAttributionSummary aggregates attribution for one logical Skill.
type SkillAttributionSummary struct {
	SkillKey       string
	SkillName      string
	Attributions   int
	LastObservedAt time.Time
}

// RecordSkillAttribution stores one observation and reports whether it was new.
// De-duplication is per work unit and keyed by package path, so a work unit that
// reads the same package repeatedly records one row.
func (s *Store) RecordSkillAttribution(ctx context.Context, record SkillAttribution) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("control store is required")
	}
	if strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.WorkUnitID) == "" {
		return false, fmt.Errorf("attribution requires run and work-unit identity")
	}
	if strings.TrimSpace(record.PackagePath) == "" {
		return false, fmt.Errorf("attribution requires a package path")
	}
	observed := record.ObservedAt
	if observed.IsZero() {
		observed = time.Now()
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_attributions(
		control_tenant_id, person_id, run_id, work_unit_id, skill_key, skill_name,
		package_path, package_name, scope, provenance, tool_name, observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		normalizeTenant(record.ControlTenantID), record.PersonID, record.RunID, record.WorkUnitID,
		record.SkillKey, record.SkillName, record.PackagePath, record.PackageName,
		record.Scope, record.Provenance, record.ToolName, observed.Unix())
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// HasSkillAttribution reports whether this work unit already recorded the
// package.
func (s *Store) HasSkillAttribution(ctx context.Context, controlTenantID, runID, workUnitID, packagePath string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("control store is required")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_attributions
		WHERE control_tenant_id=? AND run_id=? AND work_unit_id=? AND package_path=?`,
		normalizeTenant(controlTenantID), runID, workUnitID, packagePath).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// WorkUnitActivatedSkillKeys returns the Skills this work unit activated.
// Attribution for one of them is suppressed: reading an activated Skill's own
// resources is that activation's progressive disclosure, already recorded in its
// resource manifest, and counting it again would blur the one thing the implicit
// column means.
func (s *Store) WorkUnitActivatedSkillKeys(ctx context.Context, controlTenantID, runID, workUnitID string) (map[string]bool, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT skill_key FROM run_skill_activations
		WHERE control_tenant_id=? AND run_id=? AND work_unit_id=?`,
		normalizeTenant(controlTenantID), runID, workUnitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys[key] = true
	}
	return keys, rows.Err()
}

// SkillAttributionSummaries aggregates attribution per logical Skill for the
// stats projection and for usage recency of a Skill on a read-only root, where a
// sidecar usage file cannot be written.
func (s *Store) SkillAttributionSummaries(ctx context.Context, controlTenantID string) ([]SkillAttributionSummary, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT skill_key, skill_name, COUNT(*), MAX(observed_at)
		FROM skill_attributions WHERE control_tenant_id=? GROUP BY skill_key, skill_name`,
		normalizeTenant(controlTenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillAttributionSummary
	for rows.Next() {
		var item SkillAttributionSummary
		var observed int64
		if err := rows.Scan(&item.SkillKey, &item.SkillName, &item.Attributions, &observed); err != nil {
			return nil, err
		}
		item.LastObservedAt = time.Unix(observed, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}
