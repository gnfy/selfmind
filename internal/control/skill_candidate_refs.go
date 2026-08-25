package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const MaxSkillCandidateRefsPerWorkUnit = 256

type SkillCandidateRef struct {
	CandidateRef     string    `json:"candidate_ref"`
	IdentityTenantID string    `json:"identity_tenant_id"`
	ControlTenantID  string    `json:"control_tenant_id"`
	PersonID         string    `json:"person_id"`
	RunID            string    `json:"run_id"`
	WorkUnitID       string    `json:"work_unit_id"`
	SkillKey         string    `json:"skill_key"`
	SkillName        string    `json:"skill_name"`
	VersionHash      string    `json:"version_hash"`
	PackageHash      string    `json:"package_hash"`
	DescriptionHash  string    `json:"description_hash"`
	State            string    `json:"state"`
	DriftCount       int       `json:"drift_count"`
	IssuedAt         time.Time `json:"issued_at"`
	LastUsedAt       time.Time `json:"last_used_at,omitempty"`
}

type IssueSkillCandidateRefInput struct {
	IdentityTenantID string
	ControlTenantID  string
	PersonID         string
	RunID            string
	WorkUnitID       string
	SkillKey         string
	SkillName        string
	VersionHash      string
	PackageHash      string
	DescriptionHash  string
}

type SkillCandidateRefPruneReport struct {
	Terminal int      `json:"terminal"`
	Orphan   int      `json:"orphan"`
	Deleted  int      `json:"deleted"`
	Owners   []string `json:"owners,omitempty"`
}

// PruneSkillCandidateRefs previews or deletes only refs whose work unit is
// terminal or missing. Live refs are outside the target query and cannot be
// removed by this maintenance path.
func (s *Store) PruneSkillCandidateRefs(ctx context.Context, tenantID string, apply bool) (SkillCandidateRefPruneReport, error) {
	var report SkillCandidateRefPruneReport
	if s == nil || s.db == nil {
		return report, fmt.Errorf("control store is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	tenantID = normalizeTenant(tenantID)
	rows, err := tx.QueryContext(ctx, `SELECT r.candidate_ref, r.person_id, r.run_id, r.work_unit_id,
		CASE WHEN w.id IS NULL THEN 'orphan' ELSE 'terminal' END
		FROM skill_candidate_refs r
		LEFT JOIN run_work_units w ON w.identity_tenant_id=r.identity_tenant_id
			AND w.run_id=r.run_id AND w.id=r.work_unit_id
		WHERE r.identity_tenant_id=? AND
			(w.id IS NULL OR w.status IN ('completed','parked','fallback','failed','cancelled'))
		ORDER BY r.issued_at, r.candidate_ref`, tenantID)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var ref, personID, runID, workUnitID, class string
		if err := rows.Scan(&ref, &personID, &runID, &workUnitID, &class); err != nil {
			rows.Close()
			return report, err
		}
		if class == "orphan" {
			report.Orphan++
		} else {
			report.Terminal++
		}
		if len(report.Owners) < 10 {
			report.Owners = append(report.Owners, fmt.Sprintf("%s person=%s run=%s work_unit=%s ref=%s", class, personID, runID, workUnitID, ref))
		}
	}
	if err := rows.Close(); err != nil {
		return report, err
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	if apply && report.Terminal+report.Orphan > 0 {
		result, err := tx.ExecContext(ctx, `DELETE FROM skill_candidate_refs WHERE candidate_ref IN (
			SELECT r.candidate_ref FROM skill_candidate_refs r
			LEFT JOIN run_work_units w ON w.identity_tenant_id=r.identity_tenant_id
				AND w.run_id=r.run_id AND w.id=r.work_unit_id
			WHERE r.identity_tenant_id=? AND
				(w.id IS NULL OR w.status IN ('completed','parked','fallback','failed','cancelled'))
		)`, tenantID)
		if err != nil {
			return report, err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return report, err
		}
		report.Deleted = int(deleted)
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

// IssueSkillCandidateRef writes only the durable identity mapping. Display
// descriptions and ranking remain the bounded volatile prompt slice, so refs
// survive resume/restart without growing a duplicate catalogue in control.db.
func (s *Store) IssueSkillCandidateRef(ctx context.Context, input IssueSkillCandidateRefInput) (*SkillCandidateRef, error) {
	input.IdentityTenantID = normalizeTenant(input.IdentityTenantID)
	input.ControlTenantID = normalizeTenant(input.ControlTenantID)
	if input.PersonID == "" || input.RunID == "" || input.WorkUnitID == "" || input.SkillKey == "" || input.SkillName == "" || input.VersionHash == "" || input.PackageHash == "" || input.DescriptionHash == "" {
		return nil, fmt.Errorf("candidate ref requires person, run, work unit, Skill identity, package, and description hashes")
	}
	ref, err := SkillCandidateRefForInput(input)
	if err != nil {
		return nil, err
	}
	if existing, resolveErr := s.ResolveSkillCandidateRef(ctx, input.IdentityTenantID, input.PersonID, input.RunID, input.WorkUnitID, ref); resolveErr != nil {
		return nil, resolveErr
	} else if existing != nil {
		return existing, nil
	}
	var issued int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_candidate_refs
		WHERE identity_tenant_id=? AND person_id=? AND run_id=? AND work_unit_id=?`,
		input.IdentityTenantID, input.PersonID, input.RunID, input.WorkUnitID).Scan(&issued); err != nil {
		return nil, err
	}
	if issued >= MaxSkillCandidateRefsPerWorkUnit {
		return nil, fmt.Errorf("candidate ref limit reached for work unit: %d", MaxSkillCandidateRefsPerWorkUnit)
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO skill_candidate_refs
		(candidate_ref, identity_tenant_id, control_tenant_id, person_id, run_id, work_unit_id,
		 skill_key, skill_name, version_hash, package_hash, description_hash, state, issued_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'issued',?)
		ON CONFLICT(candidate_ref) DO NOTHING`, ref, input.IdentityTenantID, input.ControlTenantID,
		input.PersonID, input.RunID, input.WorkUnitID, input.SkillKey, input.SkillName,
		input.VersionHash, input.PackageHash, input.DescriptionHash, now)
	if err != nil {
		return nil, err
	}
	return s.ResolveSkillCandidateRef(ctx, input.IdentityTenantID, input.PersonID, input.RunID, input.WorkUnitID, ref)
}

// SkillCandidateRefForInput derives the opaque identity without issuing it.
// Gateway catalog allocation uses this to budget the exact visible line before
// it persists only the included subset.
func SkillCandidateRefForInput(input IssueSkillCandidateRefInput) (string, error) {
	input.IdentityTenantID = normalizeTenant(input.IdentityTenantID)
	input.ControlTenantID = normalizeTenant(input.ControlTenantID)
	if input.PersonID == "" || input.RunID == "" || input.WorkUnitID == "" || input.SkillKey == "" || input.SkillName == "" || input.VersionHash == "" || input.PackageHash == "" || input.DescriptionHash == "" {
		return "", fmt.Errorf("candidate ref requires person, run, work unit, Skill identity, package, and description hashes")
	}
	identity := strings.Join([]string{input.IdentityTenantID, input.PersonID, input.RunID, input.WorkUnitID, input.SkillKey, input.PackageHash, input.DescriptionHash}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("skref_%x", digest[:8]), nil
}

func (s *Store) ResolveSkillCandidateRef(ctx context.Context, tenantID, personID, runID, workUnitID, candidateRef string) (*SkillCandidateRef, error) {
	row := s.db.QueryRowContext(ctx, `SELECT candidate_ref, identity_tenant_id, control_tenant_id,
		person_id, run_id, work_unit_id, skill_key, skill_name, version_hash, package_hash,
		description_hash, state, drift_count, issued_at, last_used_at
		FROM skill_candidate_refs WHERE candidate_ref=? AND identity_tenant_id=? AND person_id=?
		AND run_id=? AND work_unit_id=?`, strings.TrimSpace(candidateRef), normalizeTenant(tenantID), personID, runID, workUnitID)
	ref, err := scanSkillCandidateRef(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ref, err
}

func (s *Store) RecordSkillCandidateRefUse(ctx context.Context, candidateRef, state string, incrementDrift bool) (*SkillCandidateRef, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		state = "selected"
	}
	driftIncrement := 0
	if incrementDrift {
		driftIncrement = 1
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE skill_candidate_refs
		SET state=?, drift_count=drift_count+?, last_used_at=? WHERE candidate_ref=?`, state, driftIncrement, now, strings.TrimSpace(candidateRef)); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT candidate_ref, identity_tenant_id, control_tenant_id,
		person_id, run_id, work_unit_id, skill_key, skill_name, version_hash, package_hash,
		description_hash, state, drift_count, issued_at, last_used_at
		FROM skill_candidate_refs WHERE candidate_ref=?`, strings.TrimSpace(candidateRef))
	return scanSkillCandidateRef(row)
}

func scanSkillCandidateRef(row skillLifecycleScanner) (*SkillCandidateRef, error) {
	var ref SkillCandidateRef
	var issuedAt, lastUsedAt int64
	if err := row.Scan(&ref.CandidateRef, &ref.IdentityTenantID, &ref.ControlTenantID, &ref.PersonID,
		&ref.RunID, &ref.WorkUnitID, &ref.SkillKey, &ref.SkillName, &ref.VersionHash,
		&ref.PackageHash, &ref.DescriptionHash, &ref.State, &ref.DriftCount, &issuedAt, &lastUsedAt); err != nil {
		return nil, err
	}
	ref.IssuedAt = time.Unix(issuedAt, 0)
	if lastUsedAt > 0 {
		ref.LastUsedAt = time.Unix(lastUsedAt, 0)
	}
	return &ref, nil
}
