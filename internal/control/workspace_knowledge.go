package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// WorkspaceKnowledgeSection is a bounded, deterministic projection of an
// authorized workspace convention file. It is procedural project knowledge,
// separate from person memory and task history.
type WorkspaceKnowledgeSection struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	PersonID    string    `json:"person_id"`
	WorkspaceID string    `json:"workspace_id"`
	FilePath    string    `json:"file_path"`
	FileName    string    `json:"file_name"`
	ContentHash string    `json:"content_hash"`
	FileMTime   int64     `json:"file_mtime"`
	Section     int       `json:"section_index"`
	Title       string    `json:"title"`
	Excerpt     string    `json:"excerpt"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type WorkspaceKnowledgeWrite struct {
	FilePath    string
	FileName    string
	ContentHash string
	FileMTime   int64
	Section     int
	Title       string
	Excerpt     string
}

// ReplaceWorkspaceKnowledge atomically replaces the deterministic projection
// for one workspace. Files removed from the scanner disappear from recall in
// the same transaction, so stale project rules cannot linger.
func (s *Store) ReplaceWorkspaceKnowledge(ctx context.Context, tenantID, personID, workspaceID string, sections []WorkspaceKnowledgeWrite) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	workspaceID = strings.TrimSpace(workspaceID)
	if personID == "" || workspaceID == "" {
		return fmt.Errorf("person and workspace are required")
	}
	var owned int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workspaces
		WHERE tenant_id = ? AND owner_person_id = ? AND id = ?`, tenantID, personID, workspaceID).Scan(&owned); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("workspace knowledge target is not owned by this person")
		}
		return err
	}
	sections = normalizeWorkspaceKnowledgeWrites(sections)
	current, err := s.ListWorkspaceKnowledge(ctx, tenantID, personID, workspaceID, 1000)
	if err != nil {
		return err
	}
	if sameWorkspaceKnowledge(current, sections) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_knowledge_sections
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ?`, tenantID, personID, workspaceID); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, item := range sections {
		id := workspaceKnowledgeID(tenantID, personID, workspaceID, item.FilePath, item.ContentHash, item.Section)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_knowledge_sections
			(id, tenant_id, person_id, workspace_id, file_path, file_name, content_hash, file_mtime,
			 section_index, title, excerpt, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, tenantID, personID, workspaceID, item.FilePath, item.FileName, item.ContentHash,
			item.FileMTime, item.Section, item.Title, item.Excerpt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeWorkspaceKnowledgeWrites(sections []WorkspaceKnowledgeWrite) []WorkspaceKnowledgeWrite {
	out := make([]WorkspaceKnowledgeWrite, 0, len(sections))
	seen := map[string]struct{}{}
	for _, item := range sections {
		item.FilePath = strings.TrimSpace(item.FilePath)
		item.FileName = strings.TrimSpace(item.FileName)
		item.ContentHash = strings.TrimSpace(item.ContentHash)
		item.Title = strings.TrimSpace(item.Title)
		item.Excerpt = strings.TrimSpace(item.Excerpt)
		if item.FilePath == "" || item.FileName == "" || item.ContentHash == "" || item.Excerpt == "" || item.Section < 0 {
			continue
		}
		if item.Title == "" {
			item.Title = item.FileName
		}
		key := item.FilePath + "\x00" + fmt.Sprint(item.Section)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func sameWorkspaceKnowledge(current []WorkspaceKnowledgeSection, next []WorkspaceKnowledgeWrite) bool {
	if len(current) != len(next) {
		return false
	}
	byKey := make(map[string]WorkspaceKnowledgeSection, len(current))
	for _, item := range current {
		byKey[item.FilePath+"\x00"+fmt.Sprint(item.Section)] = item
	}
	for _, item := range next {
		prior, ok := byKey[item.FilePath+"\x00"+fmt.Sprint(item.Section)]
		if !ok || prior.FileName != item.FileName || prior.ContentHash != item.ContentHash ||
			prior.Title != item.Title || prior.Excerpt != item.Excerpt {
			return false
		}
	}
	return true
}

func workspaceKnowledgeID(tenantID, personID, workspaceID, path, contentHash string, section int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{tenantID, personID, workspaceID, path, contentHash, fmt.Sprint(section)}, "\x00")))
	return "wknow_" + hex.EncodeToString(sum[:12])
}

func (s *Store) ListWorkspaceKnowledge(ctx context.Context, tenantID, personID, workspaceID string, limit int) ([]WorkspaceKnowledgeSection, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(workspaceID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 256
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, person_id, workspace_id, file_path, file_name,
		content_hash, file_mtime, section_index, title, excerpt, updated_at
		FROM workspace_knowledge_sections
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ?
		ORDER BY file_path ASC, section_index ASC LIMIT ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(workspaceID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspaceKnowledgeSection, 0)
	for rows.Next() {
		var item WorkspaceKnowledgeSection
		var updated int64
		if err := rows.Scan(&item.ID, &item.TenantID, &item.PersonID, &item.WorkspaceID,
			&item.FilePath, &item.FileName, &item.ContentHash, &item.FileMTime, &item.Section,
			&item.Title, &item.Excerpt, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt = time.Unix(updated, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}

// DeleteWorkspaceKnowledge is intentionally narrow and useful for tests and
// workspace-removal maintenance. Ordinary refreshes use replacement.
func (s *Store) DeleteWorkspaceKnowledge(ctx context.Context, tenantID, personID, workspaceID string) error {
	if s == nil || s.db == nil {
		return sql.ErrConnDone
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM workspace_knowledge_sections
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(workspaceID))
	return err
}
