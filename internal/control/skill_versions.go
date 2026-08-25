package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type SkillVersion struct {
	ControlTenantID   string          `json:"control_tenant_id"`
	SkillKey          string          `json:"skill_key"`
	SkillName         string          `json:"skill_name"`
	VersionHash       string          `json:"version_hash"`
	ParentVersionHash string          `json:"parent_version_hash,omitempty"`
	State             string          `json:"state"`
	ContentRef        string          `json:"content_ref,omitempty"`
	ContentBody       string          `json:"content_body,omitempty"`
	PackageHash       string          `json:"package_hash,omitempty"`
	ResourceManifest  json.RawMessage `json:"resource_manifest,omitempty"`
	ObservationIDs    json.RawMessage `json:"source_observation_ids,omitempty"`
	EvidenceSetHash   string          `json:"evidence_set_hash,omitempty"`
	Evidence          json.RawMessage `json:"evidence,omitempty"`
	CreatedBy         string          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	PromotedAt        *time.Time      `json:"promoted_at,omitempty"`
}

func (s *Store) CreateSkillCandidateVersion(ctx context.Context, tenantID, skillKey, skillName, parentVersionHash, content, evidenceSetHash string, observationIDs []string, evidence interface{}) (string, error) {
	return s.CreateSkillPackageCandidateVersion(ctx, tenantID, skillKey, skillName, parentVersionHash,
		content, "", []byte(`[]`), evidenceSetHash, observationIDs, evidence)
}

func (s *Store) CreateSkillPackageCandidateVersion(ctx context.Context, tenantID, skillKey, skillName, parentVersionHash, content, packageHash string, resourceManifestJSON []byte, evidenceSetHash string, observationIDs []string, evidence interface{}) (string, error) {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(skillName) == "" {
		return "", fmt.Errorf("candidate name and content are required")
	}
	if len(resourceManifestJSON) == 0 {
		resourceManifestJSON = []byte(`[]`)
	}
	if !json.Valid(resourceManifestJSON) {
		return "", fmt.Errorf("candidate resource manifest must be valid JSON")
	}
	digest := sha256.Sum256([]byte(content))
	versionHash := fmt.Sprintf("%x", digest[:])
	idsJSON, _ := json.Marshal(observationIDs)
	evidenceJSON, _ := json.Marshal(evidence)
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_versions
		(control_tenant_id, skill_key, skill_name, version_hash, parent_version_hash, state,
		 content_body, package_hash, resource_manifest_json, source_observation_ids_json, evidence_set_hash, evidence_json,
		 created_by, created_at)
		VALUES(?,?,?,?,?,'candidate',?,?,?,?,?,?,'skill_curator',?)`, normalizeTenant(tenantID),
		skillKey, skillName, versionHash, parentVersionHash, content, strings.TrimSpace(packageHash), string(resourceManifestJSON), string(idsJSON),
		evidenceSetHash, string(evidenceJSON), time.Now().Unix())
	return versionHash, err
}

func (s *Store) SkillCandidateByEvidence(ctx context.Context, tenantID, evidenceSetHash string) (*SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, package_hash, resource_manifest_json, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=? AND evidence_set_hash=? ORDER BY created_at DESC LIMIT 1`,
		normalizeTenant(tenantID), strings.TrimSpace(evidenceSetHash))
	version, err := scanSkillVersion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return version, err
}

func (s *Store) GetSkillVersion(ctx context.Context, tenantID, skillKey, versionHash string) (*SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, package_hash, resource_manifest_json, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, versionHash)
	version, err := scanSkillVersion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return version, err
}

func (s *Store) ActiveSkillVersion(ctx context.Context, tenantID, skillKey string) (*SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, package_hash, resource_manifest_json, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
		normalizeTenant(tenantID), skillKey)
	version, err := scanSkillVersion(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return version, err
}

func scanSkillVersion(row skillLifecycleScanner) (*SkillVersion, error) {
	var version SkillVersion
	var manifest, observations, evidence string
	var created int64
	var promoted sql.NullInt64
	if err := row.Scan(&version.ControlTenantID, &version.SkillKey, &version.SkillName,
		&version.VersionHash, &version.ParentVersionHash, &version.State, &version.ContentRef,
		&version.ContentBody, &version.PackageHash, &manifest, &observations, &version.EvidenceSetHash, &evidence,
		&version.CreatedBy, &created, &promoted); err != nil {
		return nil, err
	}
	version.ObservationIDs = json.RawMessage(observations)
	version.ResourceManifest = json.RawMessage(manifest)
	version.Evidence = json.RawMessage(evidence)
	version.CreatedAt = time.Unix(created, 0)
	if promoted.Valid {
		at := time.Unix(promoted.Int64, 0)
		version.PromotedAt = &at
	}
	return &version, nil
}

func (s *Store) PromoteSkillCandidate(ctx context.Context, tenantID, skillKey, versionHash, contentRef string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, parentVersionHash string
	if err := tx.QueryRowContext(ctx, `SELECT state, parent_version_hash FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, versionHash).Scan(&state, &parentVersionHash); err != nil {
		return err
	}
	if state != "candidate" {
		return fmt.Errorf("skill version is %s, not candidate", state)
	}
	var activeVersionHash string
	activeErr := tx.QueryRowContext(ctx, `SELECT version_hash FROM skill_versions
		WHERE control_tenant_id=? AND skill_key=? AND state='active'`, normalizeTenant(tenantID), skillKey).Scan(&activeVersionHash)
	if activeErr != nil && activeErr != sql.ErrNoRows {
		return activeErr
	}
	if parentVersionHash == "" {
		if activeErr == nil {
			return fmt.Errorf("parentless Skill candidate cannot replace active version %s", activeVersionHash)
		}
	} else if activeErr == sql.ErrNoRows || activeVersionHash != parentVersionHash {
		return fmt.Errorf("skill candidate parent %s is not the current active version", parentVersionHash)
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE skill_failure_guards SET state='resolved'
		WHERE control_tenant_id=? AND skill_key=? AND version_hash IN
		(SELECT version_hash FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND state='active')`,
		normalizeTenant(tenantID), skillKey, normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='previous' WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
		normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='active', content_ref=?, promoted_at=?
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='candidate'`,
		strings.TrimSpace(contentRef), now, normalizeTenant(tenantID), skillKey, versionHash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RejectSkillCandidate(ctx context.Context, tenantID, skillKey, versionHash string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE skill_versions SET state='rejected'
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='candidate'`,
		normalizeTenant(tenantID), skillKey, versionHash)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("skill candidate not found")
	}
	return nil
}

func (s *Store) ListSkillVersions(ctx context.Context, tenantID, skillKey, state string, limit int) ([]SkillVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT control_tenant_id, skill_key, skill_name, version_hash,
		parent_version_hash, state, content_ref, content_body, package_hash, resource_manifest_json, source_observation_ids_json,
		evidence_set_hash, evidence_json, created_by, created_at, promoted_at
		FROM skill_versions WHERE control_tenant_id=?`
	args := []interface{}{normalizeTenant(tenantID)}
	if strings.TrimSpace(skillKey) != "" {
		query += ` AND skill_key=?`
		args = append(args, strings.TrimSpace(skillKey))
	}
	if strings.TrimSpace(state) != "" {
		query += ` AND state=?`
		args = append(args, strings.TrimSpace(state))
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillVersion
	for rows.Next() {
		version, err := scanSkillVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *version)
	}
	return out, rows.Err()
}

func (s *Store) ActivatePreviousSkillVersion(ctx context.Context, tenantID, skillKey, targetVersionHash, contentRef string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM skill_versions WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`,
		normalizeTenant(tenantID), skillKey, targetVersionHash).Scan(&state); err != nil {
		return err
	}
	if state != "previous" {
		return fmt.Errorf("rollback target is %s, not previous", state)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='previous' WHERE control_tenant_id=? AND skill_key=? AND state='active'`,
		normalizeTenant(tenantID), skillKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='active', content_ref=?, promoted_at=?
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=? AND state='previous'`,
		strings.TrimSpace(contentRef), time.Now().Unix(), normalizeTenant(tenantID), skillKey, targetVersionHash); err != nil {
		return err
	}
	return tx.Commit()
}
