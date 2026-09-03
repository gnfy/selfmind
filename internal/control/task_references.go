package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	TaskReferenceLiteral     = "literal"
	TaskReferenceEntity      = "entity"
	TaskReferenceDescriptive = "descriptive"

	TaskReferenceShadow     = "shadow"
	TaskReferenceCandidate  = "candidate"
	TaskReferenceActive     = "active"
	TaskReferenceConflicted = "conflicted"
	TaskReferenceSuperseded = "superseded"
)

type TaskReference struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	PersonID        string    `json:"person_id"`
	TaskID          string    `json:"task_id"`
	WorkspaceID     string    `json:"workspace_id,omitempty"`
	Class           string    `json:"class"`
	RawValue        string    `json:"raw_value"`
	NormalizedValue string    `json:"normalized_value"`
	Status          string    `json:"status"`
	UserConfirmed   bool      `json:"user_confirmed"`
	SupportCount    int       `json:"support_count,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type TaskReferenceWrite struct {
	TenantID      string
	PersonID      string
	TaskID        string
	WorkspaceID   string
	Class         string
	Value         string
	Status        string
	UserConfirmed bool
	RunID         string
	Provenance    string
	SourceRef     string
}

type TaskReferenceMatch struct {
	Reference TaskReference
	Task      Task
}

type TaskReferenceCard struct {
	Reference TaskReference
	Card      TaskCard
}

type TaskResolutionRecord struct {
	TenantID              string
	PersonID              string
	RunID                 string
	InputHash             string
	MatchedSurfaceForms   []string
	UnmatchedSalientTerms []string
	CandidateTaskIDs      []string
	SelectedTaskID        string
	FinalTaskID           string
	Reason                string
	Outcome               string
	AttachPolicy          interface{}
	AnalyzerEvaluated     bool
}

type TaskReferenceStats struct {
	Shadow               int
	Candidate            int
	Active               int
	Conflicted           int
	Superseded           int
	ResolutionPending    int
	ResolutionCorrected  int
	ResolutionUnverified int
	KnowledgeFiles       int
	KnowledgeSections    int
}

func (s *Store) ReadTaskReferenceStats(ctx context.Context, tenantID, personID string) (TaskReferenceStats, error) {
	var stats TaskReferenceStats
	if s == nil || s.db == nil {
		return stats, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM task_references
		WHERE tenant_id = ? AND person_id = ? GROUP BY status`, tenantID, personID)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return stats, err
		}
		switch status {
		case TaskReferenceShadow:
			stats.Shadow = count
		case TaskReferenceCandidate:
			stats.Candidate = count
		case TaskReferenceActive:
			stats.Active = count
		case TaskReferenceConflicted:
			stats.Conflicted = count
		case TaskReferenceSuperseded:
			stats.Superseded = count
		}
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	resolutionRows, err := s.db.QueryContext(ctx, `SELECT outcome, COUNT(*) FROM task_resolution_events
		WHERE tenant_id = ? AND person_id = ? GROUP BY outcome`, tenantID, personID)
	if err != nil {
		return stats, err
	}
	for resolutionRows.Next() {
		var outcome string
		var count int
		if err := resolutionRows.Scan(&outcome, &count); err != nil {
			resolutionRows.Close()
			return stats, err
		}
		switch outcome {
		case "pending":
			stats.ResolutionPending = count
		case "corrected":
			stats.ResolutionCorrected = count
		case "unverified", "accepted_unverified":
			stats.ResolutionUnverified += count
		}
	}
	if err := resolutionRows.Close(); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT workspace_id || char(0) || file_path)
		FROM workspace_knowledge_sections WHERE tenant_id = ? AND person_id = ?`, tenantID, personID).
		Scan(&stats.KnowledgeSections, &stats.KnowledgeFiles); err != nil {
		return stats, err
	}
	return stats, nil
}

// NormalizeTaskReference is deliberately language-agnostic. It preserves
// Unicode letters and numbers while folding case, punctuation, and whitespace
// so Chinese aliases, URLs, build ids, and ordinary names use one exact-match
// path without a vendor/ticket regex.
func NormalizeTaskReference(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	space := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case r == '/' || r == ':' || r == '#' || r == '_' || r == '-' || r == '.':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// TaskReferenceAppearsInText performs a normalized exact surface-form match.
// CJK references may be embedded in a sentence without whitespace. Latin
// references require letter/number boundaries so a short confirmed alias does
// not match inside an unrelated longer word.
func TaskReferenceAppearsInText(text, reference string) bool {
	haystack := []rune(NormalizeTaskReference(text))
	needle := []rune(NormalizeTaskReference(reference))
	if len(haystack) == 0 || len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	hasCJK := false
	for _, r := range needle {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			hasCJK = true
			break
		}
	}
	for start := 0; start+len(needle) <= len(haystack); start++ {
		matched := true
		for i := range needle {
			if haystack[start+i] != needle[i] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if hasCJK {
			return true
		}
		leftOK := start == 0 || !(unicode.IsLetter(haystack[start-1]) || unicode.IsNumber(haystack[start-1]))
		end := start + len(needle)
		rightOK := end == len(haystack) || !(unicode.IsLetter(haystack[end]) || unicode.IsNumber(haystack[end]))
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

func validTaskReferenceClass(class string) bool {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case TaskReferenceLiteral, TaskReferenceEntity, TaskReferenceDescriptive:
		return true
	default:
		return false
	}
}

func validTaskReferenceValue(value string, userConfirmed bool) bool {
	normalized := NormalizeTaskReference(value)
	runes := utf8.RuneCountInString(normalized)
	if runes < 3 || runes > 96 {
		return false
	}
	if userConfirmed {
		return true
	}
	// Single generic words are poor automatic addresses. Identifiers, URLs,
	// multi-word phrases, and CJK names remain eligible.
	hasSpecificShape := strings.ContainsAny(normalized, "0123456789/#:_-.") || strings.Contains(normalized, " ")
	hasCJK := false
	for _, r := range normalized {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			hasCJK = true
			break
		}
	}
	return hasSpecificShape || hasCJK || runes >= 8
}

func ValidTaskReference(value string, userConfirmed bool) bool {
	return validTaskReferenceValue(value, userConfirmed)
}

func referenceEvidenceHash(value, provenance, sourceRef string) string {
	sum := sha256.Sum256([]byte(NormalizeTaskReference(value) + "\x00" + strings.TrimSpace(provenance) + "\x00" + strings.TrimSpace(sourceRef)))
	return hex.EncodeToString(sum[:])
}

func (s *Store) UpsertTaskReference(ctx context.Context, input TaskReferenceWrite) (*TaskReference, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	input.TenantID = normalizeTenant(input.TenantID)
	input.PersonID = strings.TrimSpace(input.PersonID)
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.Class = strings.ToLower(strings.TrimSpace(input.Class))
	input.Value = strings.TrimSpace(input.Value)
	input.Status = strings.ToLower(strings.TrimSpace(input.Status))
	input.Provenance = strings.ToLower(strings.TrimSpace(input.Provenance))
	if input.PersonID == "" || input.TaskID == "" || !validTaskReferenceClass(input.Class) || !validTaskReferenceValue(input.Value, input.UserConfirmed) {
		return nil, fmt.Errorf("invalid task reference")
	}
	if input.Status == "" {
		input.Status = TaskReferenceShadow
	}
	switch input.Status {
	case TaskReferenceShadow, TaskReferenceCandidate, TaskReferenceActive:
	default:
		return nil, fmt.Errorf("invalid task reference status %q", input.Status)
	}
	if input.Provenance == "" {
		input.Provenance = "analyzer"
	}
	var taskWorkspace string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(workspace_id, '') FROM threads
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		input.TenantID, input.PersonID, input.TaskID).Scan(&taskWorkspace); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("task reference target is not owned by this person")
		}
		return nil, err
	}
	if strings.TrimSpace(input.WorkspaceID) == "" {
		input.WorkspaceID = taskWorkspace
	}
	normalized := NormalizeTaskReference(input.Value)
	now := time.Now().Unix()
	id := "tref_" + uuid.NewString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_references
		(id, tenant_id, person_id, thread_id, workspace_id, class, raw_value, normalized_value, status, user_confirmed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, person_id, normalized_value, thread_id) DO UPDATE SET
			workspace_id = CASE WHEN excluded.workspace_id != '' THEN excluded.workspace_id ELSE task_references.workspace_id END,
			raw_value = CASE WHEN length(excluded.raw_value) < length(task_references.raw_value) THEN excluded.raw_value ELSE task_references.raw_value END,
			user_confirmed = MAX(task_references.user_confirmed, excluded.user_confirmed),
			status = CASE WHEN excluded.user_confirmed = 1 THEN 'active'
			              WHEN task_references.status = 'shadow' AND excluded.status = 'candidate' THEN 'candidate'
			              ELSE task_references.status END,
			updated_at = excluded.updated_at`,
		id, input.TenantID, input.PersonID, input.TaskID, strings.TrimSpace(input.WorkspaceID), input.Class,
		input.Value, normalized, input.Status, boolInt(input.UserConfirmed), now, now)
	if err != nil {
		return nil, err
	}
	ref, err := s.getTaskReferenceByBinding(ctx, input.TenantID, input.PersonID, normalized, input.TaskID)
	if err != nil || ref == nil {
		return ref, err
	}
	evidenceID := "trefev_" + uuid.NewString()
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO task_reference_evidence
		(id, reference_id, run_id, provenance, source_ref, evidence_hash, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, evidenceID, ref.ID, strings.TrimSpace(input.RunID), input.Provenance,
		strings.TrimSpace(input.SourceRef), referenceEvidenceHash(input.Value, input.Provenance, input.SourceRef), now)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileTaskReferenceValue(ctx, input.TenantID, input.PersonID, normalized); err != nil {
		return nil, err
	}
	return s.getTaskReferenceByBinding(ctx, input.TenantID, input.PersonID, normalized, input.TaskID)
}

func (s *Store) getTaskReferenceByBinding(ctx context.Context, tenantID, personID, normalized, taskID string) (*TaskReference, error) {
	row := s.db.QueryRowContext(ctx, `SELECT r.id, r.tenant_id, r.person_id, r.thread_id, r.workspace_id,
		r.class, r.raw_value, r.normalized_value, r.status, r.user_confirmed, r.created_at, r.updated_at,
		(SELECT MAX(0,
			COUNT(DISTINCT CASE WHEN e.provenance IN ('user_text', 'legacy_user_text') THEN NULLIF(e.run_id, '') END) -
			COUNT(DISTINCT CASE WHEN e.provenance = 'corrected' THEN NULLIF(e.run_id, '') END))
		 FROM task_reference_evidence e WHERE e.reference_id = r.id)
		FROM task_references r WHERE r.tenant_id = ? AND r.person_id = ? AND r.normalized_value = ? AND r.thread_id = ?`,
		normalizeTenant(tenantID), personID, normalized, taskID)
	return scanTaskReference(row)
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

type taskReferenceQueryExecer interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func scanTaskReference(row rowScanner) (*TaskReference, error) {
	var ref TaskReference
	var confirmed int
	var created, updated int64
	if err := row.Scan(&ref.ID, &ref.TenantID, &ref.PersonID, &ref.TaskID, &ref.WorkspaceID,
		&ref.Class, &ref.RawValue, &ref.NormalizedValue, &ref.Status, &confirmed, &created, &updated, &ref.SupportCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	ref.UserConfirmed = confirmed != 0
	ref.CreatedAt = time.Unix(created, 0)
	ref.UpdatedAt = time.Unix(updated, 0)
	return &ref, nil
}

func (s *Store) reconcileTaskReferenceValue(ctx context.Context, tenantID, personID, normalized string) error {
	return reconcileTaskReferenceValueWith(ctx, s.db, tenantID, personID, normalized)
}

func reconcileTaskReferenceValueWith(ctx context.Context, db taskReferenceQueryExecer, tenantID, personID, normalized string) error {
	rows, err := db.QueryContext(ctx, `SELECT r.id, r.thread_id, r.status, r.user_confirmed,
		(SELECT MAX(0,
			COUNT(DISTINCT CASE WHEN e.provenance IN ('user_text', 'legacy_user_text') THEN NULLIF(e.run_id, '') END) -
			COUNT(DISTINCT CASE WHEN e.provenance = 'corrected' THEN NULLIF(e.run_id, '') END))
		 FROM task_reference_evidence e WHERE e.reference_id = r.id)
		FROM task_references r WHERE r.tenant_id = ? AND r.person_id = ? AND r.normalized_value = ? AND r.status != 'superseded'`,
		normalizeTenant(tenantID), personID, normalized)
	if err != nil {
		return err
	}
	defer rows.Close()
	type binding struct {
		id, task, current  string
		confirmed, support int
	}
	var bindings []binding
	qualifiedTasks := map[string]struct{}{}
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.id, &b.task, &b.current, &b.confirmed, &b.support); err != nil {
			return err
		}
		bindings = append(bindings, b)
		if b.confirmed != 0 {
			qualifiedTasks[b.task] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range bindings {
		// Automatic promotion is frozen (simplification P2): references are
		// aliases and search hints, so only an explicit user confirmation may
		// activate one. Run-count support keeps a reference at candidate — a
		// useful recall signal — and its old negative-evidence loop died with
		// the post-run MOVE routing, so self-promotion would have had no
		// correction path left.
		status := TaskReferenceCandidate
		if b.current == TaskReferenceShadow && b.support == 0 && b.confirmed == 0 {
			status = TaskReferenceShadow
		}
		if b.confirmed != 0 {
			status = TaskReferenceActive
		}
		if len(qualifiedTasks) > 1 {
			status = TaskReferenceConflicted
		}
		if _, err := db.ExecContext(ctx, `UPDATE task_references SET status = ?, updated_at = ? WHERE id = ?`, status, time.Now().Unix(), b.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) FindTaskReferenceMatches(ctx context.Context, tenantID, personID, input string, limit int) ([]TaskReferenceMatch, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	normalizedInput := NormalizeTaskReference(input)
	if normalizedInput == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.tenant_id, r.person_id, r.thread_id, r.workspace_id,
		r.class, r.raw_value, r.normalized_value, r.status, r.user_confirmed, r.created_at, r.updated_at,
		(SELECT MAX(0,
			COUNT(DISTINCT CASE WHEN e.provenance IN ('user_text', 'legacy_user_text') THEN NULLIF(e.run_id, '') END) -
			COUNT(DISTINCT CASE WHEN e.provenance = 'corrected' THEN NULLIF(e.run_id, '') END))
		 FROM task_reference_evidence e WHERE e.reference_id = r.id),
		t.workspace_id, t.title,
		`+threadDerivedStatusSQL(ThreadActivitySettled)+`,
		COALESCE(t.kind, 'work'), COALESCE(t.visibility, 'listed'),
		COALESCE(t.pinned, 0), COALESCE(t.summary, ''),
		COALESCE((SELECT h.next_steps_json FROM task_handoffs h WHERE h.thread_id=t.id ORDER BY h.created_at DESC, h.id DESC LIMIT 1), '[]'),
		'', COALESCE((SELECT rr.id FROM runs rr WHERE rr.tenant_id=t.tenant_id AND rr.thread_id=t.id AND rr.status='running' ORDER BY rr.started_at DESC, rr.id DESC LIMIT 1), ''),
		COALESCE((SELECT rr.channel FROM runs rr WHERE rr.tenant_id=t.tenant_id AND rr.thread_id=t.id ORDER BY rr.started_at DESC, rr.id DESC LIMIT 1), ''),
		CASE WHEN t.visibility='archived' THEN t.updated_at ELSE NULL END,
		t.last_activity_at, t.created_at, t.updated_at
		FROM task_references r JOIN threads t ON t.tenant_id = r.tenant_id AND t.id = r.thread_id
		WHERE r.tenant_id = ? AND r.person_id = ? AND r.status = 'active'
		ORDER BY r.user_confirmed DESC, r.updated_at DESC LIMIT 500`, normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskReferenceMatch
	for rows.Next() {
		var match TaskReferenceMatch
		var confirmed, pinned int
		var refCreated, refUpdated, taskCreated, taskUpdated, lastActivity int64
		var archived sql.NullInt64
		var nextSteps string
		if err := rows.Scan(&match.Reference.ID, &match.Reference.TenantID, &match.Reference.PersonID,
			&match.Reference.TaskID, &match.Reference.WorkspaceID, &match.Reference.Class, &match.Reference.RawValue,
			&match.Reference.NormalizedValue, &match.Reference.Status, &confirmed, &refCreated, &refUpdated,
			&match.Reference.SupportCount, &match.Task.WorkspaceID, &match.Task.Title, &match.Task.Status,
			&match.Task.Kind, &match.Task.Visibility, &pinned, &match.Task.CurrentSummary, &nextSteps,
			&match.Task.BlockedReason, &match.Task.ActiveRunID, &match.Task.LastChannel, &archived,
			&lastActivity, &taskCreated, &taskUpdated); err != nil {
			return nil, err
		}
		if !TaskReferenceAppearsInText(normalizedInput, match.Reference.NormalizedValue) {
			continue
		}
		match.Reference.UserConfirmed = confirmed != 0
		match.Reference.CreatedAt = time.Unix(refCreated, 0)
		match.Reference.UpdatedAt = time.Unix(refUpdated, 0)
		match.Task.ID = match.Reference.TaskID
		match.Task.TenantID = match.Reference.TenantID
		match.Task.PersonID = match.Reference.PersonID
		match.Task.Pinned = pinned != 0
		_ = json.Unmarshal([]byte(nextSteps), &match.Task.NextSteps)
		if archived.Valid {
			value := time.Unix(archived.Int64, 0)
			match.Task.ArchivedAt = &value
		}
		match.Task.LastActivityAt = time.Unix(lastActivity, 0)
		match.Task.CreatedAt = time.Unix(taskCreated, 0)
		match.Task.UpdatedAt = time.Unix(taskUpdated, 0)
		out = append(out, match)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// ListTaskReferenceCards returns queryable reference bindings with the same
// compact task-card projection used by the ordinary recall source. Hidden,
// archived, and cancelled work is excluded from automatic recall.
func (s *Store) ListTaskReferenceCards(ctx context.Context, tenantID, personID string, statuses []string, limit int) ([]TaskReferenceCard, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	args := []interface{}{normalizeTenant(tenantID), personID}
	query := `SELECT r.id, r.tenant_id, r.person_id, r.thread_id, r.workspace_id,
		r.class, r.raw_value, r.normalized_value, r.status, r.user_confirmed, r.created_at, r.updated_at,
		(SELECT MAX(0,
			COUNT(DISTINCT CASE WHEN e.provenance IN ('user_text', 'legacy_user_text') THEN NULLIF(e.run_id, '') END) -
			COUNT(DISTINCT CASE WHEN e.provenance = 'corrected' THEN NULLIF(e.run_id, '') END))
		 FROM task_reference_evidence e WHERE e.reference_id = r.id),
		t.id, COALESCE(t.workspace_id, ''), t.title,
		` + threadDerivedStatusSQL(ThreadActivitySettled) + `,
		COALESCE(t.summary, ''), t.updated_at,
		COALESCE(h.summary, ''), COALESCE(h.changed_files_json, '[]')
		FROM task_references r
		JOIN threads t ON t.tenant_id = r.tenant_id AND t.id = r.thread_id
		LEFT JOIN task_handoffs h ON h.id = (
			SELECT id FROM task_handoffs WHERE thread_id = t.id ORDER BY created_at DESC, rowid DESC LIMIT 1
		)
		WHERE r.tenant_id = ? AND r.person_id = ?
		  AND COALESCE(t.visibility, 'listed') != 'archived'`
	if len(statuses) > 0 {
		query += " AND r.status IN (" + strings.TrimRight(strings.Repeat("?,", len(statuses)), ",") + ")"
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += " ORDER BY r.user_confirmed DESC, r.updated_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskReferenceCard
	for rows.Next() {
		var item TaskReferenceCard
		var confirmed int
		var refCreated, refUpdated, cardUpdated int64
		var filesJSON string
		if err := rows.Scan(&item.Reference.ID, &item.Reference.TenantID, &item.Reference.PersonID,
			&item.Reference.TaskID, &item.Reference.WorkspaceID, &item.Reference.Class, &item.Reference.RawValue,
			&item.Reference.NormalizedValue, &item.Reference.Status, &confirmed, &refCreated, &refUpdated,
			&item.Reference.SupportCount, &item.Card.TaskID, &item.Card.WorkspaceID, &item.Card.Title,
			&item.Card.Status, &item.Card.Summary, &cardUpdated, &item.Card.HandoffSummary, &filesJSON); err != nil {
			return nil, err
		}
		item.Reference.UserConfirmed = confirmed != 0
		item.Reference.CreatedAt = time.Unix(refCreated, 0)
		item.Reference.UpdatedAt = time.Unix(refUpdated, 0)
		item.Card.UpdatedAt = time.Unix(cardUpdated, 0)
		_ = json.Unmarshal([]byte(filesJSON), &item.Card.ChangedFiles)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListTaskReferences(ctx context.Context, tenantID, personID string, statuses []string, limit int) ([]TaskReference, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args := []interface{}{normalizeTenant(tenantID), personID}
	query := `SELECT r.id, r.tenant_id, r.person_id, r.thread_id, r.workspace_id,
		r.class, r.raw_value, r.normalized_value, r.status, r.user_confirmed, r.created_at, r.updated_at,
		(SELECT MAX(0,
			COUNT(DISTINCT CASE WHEN e.provenance IN ('user_text', 'legacy_user_text') THEN NULLIF(e.run_id, '') END) -
			COUNT(DISTINCT CASE WHEN e.provenance = 'corrected' THEN NULLIF(e.run_id, '') END))
		 FROM task_reference_evidence e WHERE e.reference_id = r.id)
		FROM task_references r WHERE r.tenant_id = ? AND r.person_id = ?`
	if len(statuses) > 0 {
		query += " AND r.status IN (" + strings.TrimRight(strings.Repeat("?,", len(statuses)), ",") + ")"
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += " ORDER BY r.user_confirmed DESC, r.updated_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskReference
	for rows.Next() {
		ref, err := scanTaskReference(rows)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			out = append(out, *ref)
		}
	}
	return out, rows.Err()
}

func (s *Store) ListTaskReferencesForTask(ctx context.Context, tenantID, personID, taskID string, limit int) ([]TaskReference, error) {
	refs, err := s.ListTaskReferences(ctx, tenantID, personID, nil, 1000)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out := make([]TaskReference, 0, limit)
	for _, ref := range refs {
		if ref.TaskID != taskID || ref.Status == TaskReferenceSuperseded {
			continue
		}
		out = append(out, ref)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) SupersedeTaskReference(ctx context.Context, tenantID, personID, taskID, value string) (bool, error) {
	normalized := NormalizeTaskReference(value)
	if normalized == "" {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE task_references SET status = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND thread_id = ? AND normalized_value = ? AND status != ?`,
		TaskReferenceSuperseded, time.Now().Unix(), normalizeTenant(tenantID), personID, taskID, normalized, TaskReferenceSuperseded)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		if err := s.reconcileTaskReferenceValue(ctx, tenantID, personID, normalized); err != nil {
			return false, err
		}
	}
	return affected > 0, nil
}

func (s *Store) RecordTaskResolution(ctx context.Context, record TaskResolutionRecord) error {
	matched, _ := json.Marshal(boundedStrings(record.MatchedSurfaceForms, 8))
	unmatched, _ := json.Marshal(boundedStrings(record.UnmatchedSalientTerms, 8))
	candidates, _ := json.Marshal(boundedStrings(record.CandidateTaskIDs, 12))
	policy, _ := json.Marshal(record.AttachPolicy)
	outcome := strings.TrimSpace(record.Outcome)
	if outcome == "" {
		outcome = "unverified"
	}
	if strings.TrimSpace(record.RunID) != "" {
		result, err := s.db.ExecContext(ctx, `UPDATE task_resolution_events SET
			matched_surface_forms_json = ?, unmatched_salient_tokens_json = ?, candidates_json = ?,
			selected_task_id = ?, final_task_id = ?, reason = ?, outcome = ?, attach_policy_json = ?,
			analyzer_evaluated = ?, created_at = ?
			WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, string(matched), string(unmatched), string(candidates),
			record.SelectedTaskID, record.FinalTaskID, record.Reason, outcome, string(policy),
			boolInt(record.AnalyzerEvaluated), time.Now().Unix(), normalizeTenant(record.TenantID), record.PersonID, record.RunID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			return s.applyTaskReferenceCorrection(ctx, record)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_resolution_events
		(id, tenant_id, person_id, run_id, input_hash, matched_surface_forms_json, unmatched_salient_tokens_json,
		 candidates_json, selected_task_id, final_task_id, reason, outcome, attach_policy_json, analyzer_evaluated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"tres_"+uuid.NewString(), normalizeTenant(record.TenantID), record.PersonID, record.RunID,
		record.InputHash, string(matched), string(unmatched), string(candidates), record.SelectedTaskID,
		record.FinalTaskID, record.Reason, outcome, string(policy), boolInt(record.AnalyzerEvaluated), time.Now().Unix())
	if err != nil {
		return err
	}
	return s.applyTaskReferenceCorrection(ctx, record)
}

// applyTaskReferenceCorrection turns a post-run MOVE/NEW correction into
// negative evidence for the exact automatic bindings used at ingress. It is
// idempotent per run and never downgrades a user-confirmed binding.
func (s *Store) applyTaskReferenceCorrection(ctx context.Context, record TaskResolutionRecord) error {
	if strings.TrimSpace(record.Outcome) != "corrected" ||
		strings.TrimSpace(record.SelectedTaskID) == "" || record.SelectedTaskID == record.FinalTaskID ||
		strings.TrimSpace(record.RunID) == "" {
		return nil
	}
	for _, surface := range boundedStrings(record.MatchedSurfaceForms, 8) {
		normalized := NormalizeTaskReference(surface)
		if normalized == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM task_references
			WHERE tenant_id = ? AND person_id = ? AND thread_id = ? AND normalized_value = ?
			  AND user_confirmed = 0 AND status != ?`, normalizeTenant(record.TenantID),
			record.PersonID, record.SelectedTaskID, normalized, TaskReferenceSuperseded)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO task_reference_evidence
				(id, reference_id, run_id, provenance, source_ref, evidence_hash, observed_at)
				VALUES (?, ?, ?, 'corrected', ?, ?, ?)`, "trefev_"+uuid.NewString(), id, record.RunID,
				"resolution:"+record.RunID, referenceEvidenceHash(surface, "corrected", record.RunID), time.Now().Unix())
			if err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			if err := s.reconcileTaskReferenceValue(ctx, record.TenantID, record.PersonID, normalized); err != nil {
				return err
			}
		}
	}
	return nil
}

func boundedStrings(values []string, limit int) []string {
	out := make([]string, 0, limit)
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if utf8.RuneCountInString(value) > 96 {
			value = string([]rune(value)[:96])
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}
