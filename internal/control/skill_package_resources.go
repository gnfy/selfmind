package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type SkillPackageResource struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	ContentBody string `json:"-"`
	Bytes       int    `json:"bytes"`
}

func (s *Store) RecordSkillPackageResources(ctx context.Context, tenantID, skillKey, packageHash string, resources []SkillPackageResource) error {
	if strings.TrimSpace(skillKey) == "" || strings.TrimSpace(packageHash) == "" {
		return fmt.Errorf("Skill package identity is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, resource := range resources {
		digest := sha256.Sum256([]byte(resource.ContentBody))
		actualHash := fmt.Sprintf("%x", digest[:])
		if strings.TrimSpace(resource.Path) == "" || resource.ContentHash != actualHash || resource.Bytes != len(resource.ContentBody) {
			return fmt.Errorf("Skill package resource receipt is invalid for %q", resource.Path)
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO skill_package_resources
			(control_tenant_id, skill_key, package_hash, resource_path, content_hash, content_body, content_bytes, created_at)
			VALUES(?,?,?,?,?,?,?,?)`, normalizeTenant(tenantID), skillKey, packageHash, resource.Path,
			resource.ContentHash, resource.ContentBody, resource.Bytes, time.Now().Unix())
		if err != nil {
			return err
		}
		if inserted, _ := result.RowsAffected(); inserted == 0 {
			var hash, body string
			var size int
			if err := tx.QueryRowContext(ctx, `SELECT content_hash, content_body, content_bytes
				FROM skill_package_resources WHERE control_tenant_id=? AND skill_key=? AND package_hash=? AND resource_path=?`,
				normalizeTenant(tenantID), skillKey, packageHash, resource.Path).Scan(&hash, &body, &size); err != nil {
				return err
			}
			if hash != resource.ContentHash || body != resource.ContentBody || size != resource.Bytes {
				return fmt.Errorf("immutable Skill package resource %q conflicts with stored bytes", resource.Path)
			}
		}
	}
	return tx.Commit()
}

func (s *Store) SkillPackageResource(ctx context.Context, tenantID, skillKey, packageHash, resourcePath string) (*SkillPackageResource, error) {
	var resource SkillPackageResource
	err := s.db.QueryRowContext(ctx, `SELECT resource_path, content_hash, content_body, content_bytes
		FROM skill_package_resources WHERE control_tenant_id=? AND skill_key=? AND package_hash=? AND resource_path=?`,
		normalizeTenant(tenantID), skillKey, packageHash, strings.TrimSpace(resourcePath)).Scan(
		&resource.Path, &resource.ContentHash, &resource.ContentBody, &resource.Bytes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &resource, err
}

func (s *Store) ListSkillPackageResources(ctx context.Context, tenantID, skillKey, packageHash string) ([]SkillPackageResource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_path, content_hash, content_body, content_bytes
		FROM skill_package_resources WHERE control_tenant_id=? AND skill_key=? AND package_hash=? ORDER BY resource_path`,
		normalizeTenant(tenantID), skillKey, packageHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var resources []SkillPackageResource
	for rows.Next() {
		var resource SkillPackageResource
		if err := rows.Scan(&resource.Path, &resource.ContentHash, &resource.ContentBody, &resource.Bytes); err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}
