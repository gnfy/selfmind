package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/platform/log"

	"github.com/google/uuid"
)

// SQLite implementation of the layered memory model. All functions here run
// inside the provider's single worker goroutine on an already-open tenant DB
// (see SQLiteProvider.worker), so they need no additional locking.

// importLegacyFacts incrementally imports rows from the legacy `facts` table
// into the layered model. Idempotency: each legacy fact becomes AT MOST one
// observation, keyed by the fact's own id (INSERT OR IGNORE); a rerun after a
// partial import resumes where it stopped. A run-attributed fact whose
// evidence the live intake path already recorded (same run + scope +
// normalized statement) is skipped entirely — that evidence exists, only under
// a deterministic `obs_` id. Legacy facts with the same
// normalized content in the same target+scope fold into ONE canonical memory
// whose evidence/occurrence counters carry the repetition signal. The `facts`
// table itself is never modified.
func importLegacyFacts(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, target, content, created_at,
		COALESCE(source,''), COALESCE(scope,''), COALESCE(confidence,0),
		COALESCE(created_from_run,''), last_verified_at
		FROM facts WHERE target != 'profile'`)
	if err != nil {
		return err
	}
	var facts []Fact
	for rows.Next() {
		var f Fact
		var createdAt string
		var lastVerified sql.NullInt64
		if err := rows.Scan(&f.ID, &f.Target, &f.Content, &createdAt,
			&f.Source, &f.Scope, &f.Confidence, &f.CreatedFromRun, &lastVerified); err != nil {
			continue
		}
		f.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		if lastVerified.Valid && lastVerified.Int64 > 0 {
			f.LastVerifiedAt = time.Unix(lastVerified.Int64, 0).UTC()
		}
		facts = append(facts, f)
	}
	rows.Close()
	if len(facts) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	imported := 0
	for _, f := range facts {
		if strings.TrimSpace(f.Content) == "" || strings.TrimSpace(f.ID) == "" {
			continue
		}
		created := f.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		source := f.Source
		if source == "" {
			source = "legacy"
		}
		scope := normalizedConsolidationScope(f)
		hash := NormalizedContentHash(f.Content)

		// Duplicate-evidence guard: a run-attributed legacy fact usually has a
		// live-intake twin already recorded as a deterministic `obs_<hash>`
		// observation for the same (run, target, scope, statement). Re-materializing it
		// under the legacy fact's own id would double-count the SAME evidence
		// (the id-keyed INSERT OR IGNORE below cannot see the twin because the
		// two paths use different ids), inflating evidence_count/occurrences and
		// REINFORCE math. One run may contribute one observation per statement;
		// runless legacy facts have no intake twin and keep current behavior.
		if strings.TrimSpace(f.CreatedFromRun) != "" {
			var one int
			err := tx.QueryRow(`SELECT 1 FROM memory_observations
				WHERE run_id = ? AND target = ? AND scope = ? AND normalized_hash = ?
				  AND id LIKE 'obs_%' LIMIT 1`,
				f.CreatedFromRun, f.Target, scope, hash).Scan(&one)
			if err == nil {
				continue // live intake already recorded this evidence
			}
			if err != sql.ErrNoRows {
				return err
			}
		}

		result, err := tx.Exec(`INSERT OR IGNORE INTO memory_observations
			(id, run_id, workspace_id, target, scope, source, content, normalized_hash, confidence_prior, status, created_at)
			VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.CreatedFromRun, f.Target, scope, source, f.Content, hash,
			f.Confidence, ObservationAccepted, created.UTC().Unix())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			continue // already imported on a previous open
		}
		imported++

		verified := f.LastVerifiedAt
		if verified.IsZero() {
			verified = created
		}
		confidence := f.Confidence
		if confidence <= 0 {
			confidence = 0.5 // legacy neutral, mirrors EffectiveConfidence
		}
		pinned := strings.EqualFold(f.Target, "pinned")
		userConfirmed := pinned || strings.EqualFold(f.Source, SourceUser)

		var canonicalID string
		var existingConfidence float64
		err = tx.QueryRow(`SELECT id, confidence FROM canonical_memories
			WHERE target = ? AND scope = ? AND normalized_hash = ?`,
			f.Target, scope, hash).Scan(&canonicalID, &existingConfidence)
		switch {
		case err == sql.ErrNoRows:
			canonicalID = uuid.New().String()
			if _, err := tx.Exec(`INSERT INTO canonical_memories
				(id, target, scope, content, normalized_hash, status, pinned, user_confirmed,
				 confidence, evidence_count, occurrences, last_verified_at, valid_from, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?, ?, ?, ?)`,
				canonicalID, f.Target, scope, f.Content, hash, CanonicalActive,
				boolToInt(pinned), boolToInt(userConfirmed), confidence,
				verified.UTC().Unix(), created.UTC().Unix(), created.UTC().Unix(), time.Now().UTC().Unix()); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// Same statement observed again: corroboration, not a new belief.
			// created_at keeps the EARLIEST sighting, last_verified_at the latest.
			if _, err := tx.Exec(`UPDATE canonical_memories SET
				evidence_count = evidence_count + 1,
				occurrences = occurrences + 1,
				confidence = ?,
				pinned = MAX(pinned, ?),
				user_confirmed = MAX(user_confirmed, ?),
				last_verified_at = MAX(COALESCE(last_verified_at,0), ?),
				created_at = MIN(COALESCE(created_at,?), ?),
				updated_at = ?
				WHERE id = ?`,
				RepetitionBoost(existingConfidence, 2), boolToInt(pinned), boolToInt(userConfirmed),
				verified.UTC().Unix(), created.UTC().Unix(), created.UTC().Unix(),
				time.Now().UTC().Unix(), canonicalID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_evidence (memory_id, observation_id, relation, created_at)
			VALUES (?, ?, ?, ?)`, canonicalID, f.ID, RelationSupports, time.Now().UTC().Unix()); err != nil {
			return err
		}
	}

	if imported > 0 {
		detail, _ := json.Marshal(map[string]int{"imported_observations": imported, "scanned_facts": len(facts)})
		if _, err := tx.Exec(`INSERT INTO memory_events (id, actor, action, detail, created_at)
			VALUES (?, 'import', 'import', ?, ?)`,
			uuid.New().String(), string(detail), time.Now().UTC().Unix()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if imported > 0 {
		log.Info("memory: imported legacy facts into layered model", "imported", imported, "scanned", len(facts))
	}
	return nil
}

func listCanonicalMemories(db *sql.DB, filter CanonicalFilter) ([]CanonicalMemory, error) {
	statuses := filter.Statuses
	if len(statuses) == 0 {
		statuses = []string{CanonicalActive}
	}
	query := `SELECT id, target, scope, category, content, normalized_hash, status,
		pinned, user_confirmed, confidence, evidence_count, occurrences,
		COALESCE(last_verified_at,0), COALESCE(last_accessed_at,0),
		COALESCE(valid_from,0), COALESCE(valid_until,0), superseded_by, revision,
		COALESCE(created_at,0), COALESCE(updated_at,0)
		FROM canonical_memories WHERE status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
	args := make([]interface{}, 0, len(statuses)+2)
	for _, s := range statuses {
		args = append(args, s)
	}
	if filter.Target != "" {
		query += " AND target = ?"
		args = append(args, filter.Target)
	}
	query += " ORDER BY confidence DESC, created_at ASC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CanonicalMemory
	for rows.Next() {
		var c CanonicalMemory
		var pinned, confirmed int
		var lastVerified, lastAccessed, validFrom, validUntil, createdAt, updatedAt int64
		if err := rows.Scan(&c.ID, &c.Target, &c.Scope, &c.Category, &c.Content, &c.NormalizedHash, &c.Status,
			&pinned, &confirmed, &c.Confidence, &c.EvidenceCount, &c.Occurrences,
			&lastVerified, &lastAccessed, &validFrom, &validUntil, &c.SupersededBy, &c.Revision,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.Pinned, c.UserConfirmed = pinned != 0, confirmed != 0
		c.LastVerifiedAt = epochTime(lastVerified)
		c.LastAccessedAt = epochTime(lastAccessed)
		c.ValidFrom = epochTime(validFrom)
		c.ValidUntil = epochTime(validUntil)
		c.CreatedAt = epochTime(createdAt)
		c.UpdatedAt = epochTime(updatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

func observationsForMemory(db *sql.DB, memoryID string) ([]Observation, error) {
	rows, err := db.Query(`SELECT o.id, o.run_id, o.analyzer_version, o.workspace_id, o.target, o.scope,
		o.source, o.content, o.normalized_hash, o.confidence_prior, o.status, COALESCE(o.created_at,0)
		FROM memory_observations o
		JOIN memory_evidence e ON e.observation_id = o.id
		WHERE e.memory_id = ?
		ORDER BY o.created_at ASC`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var createdAt int64
		if err := rows.Scan(&o.ID, &o.RunID, &o.AnalyzerVersion, &o.WorkspaceID, &o.Target, &o.Scope,
			&o.Source, &o.Content, &o.NormalizedHash, &o.ConfidencePrior, &o.Status, &createdAt); err != nil {
			return nil, err
		}
		o.CreatedAt = epochTime(createdAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

func listMemoryEvents(db *sql.DB, limit int) ([]MemoryEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(`SELECT id, actor, action, memory_id, observation_id, confidence, snapshot, detail, COALESCE(created_at,0)
		FROM memory_events ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryEvent
	for rows.Next() {
		var e MemoryEvent
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.MemoryID, &e.ObservationID, &e.Confidence, &e.Snapshot, &e.Detail, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = epochTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// applyIntakeWrite lands one intake ruling transactionally: observation row,
// canonical effect, evidence edge, audit event. Ref lookups go by statement
// identity (target + normalized hash) over ACTIVE rows, so the legacy-fact
// world and the canonical world stay aligned without a mapping table.
func applyIntakeWrite(db *sql.DB, w IntakeWrite) error {
	content := strings.TrimSpace(w.Content)
	if content == "" {
		content = strings.TrimSpace(w.RefContent)
	}
	if content == "" {
		return nil
	}
	now := time.Now().UTC().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	obsID := uuid.New().String()
	if strings.TrimSpace(w.RunID) != "" {
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", w.RunID, w.AnalyzerVersion,
			strings.ToUpper(strings.TrimSpace(w.Decision)), w.DecisionKey, w.Target, w.Scope, NormalizedContentHash(content))
		digest := sha256.Sum256([]byte(key))
		obsID = fmt.Sprintf("obs_%x", digest[:16])
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO memory_observations
		(id, run_id, analyzer_version, workspace_id, target, scope, source, content, normalized_hash, confidence_prior, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obsID, w.RunID, w.AnalyzerVersion, w.WorkspaceID, w.Target, w.Scope, w.Source, content,
		NormalizedContentHash(content), BaseConfidence(w.Source), ObservationAccepted, now)
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 0 {
		return tx.Commit() // exact replay of an already-applied proposal item
	}

	findActive := func(statement string) (id string, confidence float64, ok bool) {
		err := tx.QueryRow(`SELECT id, confidence FROM canonical_memories
				WHERE target = ? AND scope = ? AND normalized_hash = ? AND status IN (?, ?) LIMIT 1`,
			w.Target, w.Scope, NormalizedContentHash(statement), CanonicalActive, CanonicalConflicted).Scan(&id, &confidence)
		return id, confidence, err == nil
	}
	reinforce := func(id string, confidence float64) error {
		_, err := tx.Exec(`UPDATE canonical_memories SET
			evidence_count = evidence_count + 1, occurrences = occurrences + 1,
			confidence = ?, last_verified_at = ?, updated_at = ? WHERE id = ?`,
			RepetitionBoost(confidence, 2), now, now, id)
		return err
	}
	insertCanonical := func(status string) (string, error) {
		id := uuid.New().String()
		pinned := strings.EqualFold(w.Target, "pinned")
		var validUntil interface{}
		if !w.ValidUntil.IsZero() {
			validUntil = w.ValidUntil.Unix()
		}
		_, err := tx.Exec(`INSERT INTO canonical_memories
			(id, target, scope, category, content, normalized_hash, status, pinned, user_confirmed,
			 confidence, evidence_count, occurrences, last_verified_at, valid_from, valid_until, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?, ?, ?, ?, ?)`,
			id, w.Target, w.Scope, strings.TrimSpace(w.Category), content, NormalizedContentHash(content), status,
			boolToInt(pinned), boolToInt(pinned || strings.EqualFold(w.Source, SourceUser)),
			BaseConfidence(w.Source), now, now, validUntil, now, now)
		return id, err
	}
	edge := func(memoryID, relation string) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO memory_evidence (memory_id, observation_id, relation, created_at)
			VALUES (?, ?, ?, ?)`, memoryID, obsID, relation, now)
		return err
	}
	event := func(action, memoryID, snapshot string) error {
		_, err := tx.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, observation_id, confidence, snapshot, created_at)
			VALUES (?, 'intake', ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), action, memoryID, obsID, w.Confidence, snapshot, now)
		return err
	}

	decision := strings.ToUpper(strings.TrimSpace(w.Decision))
	if decision == "REINFORCE" || decision == "SUPERSEDE" || decision == "CONFLICT" {
		if _, _, ok := findActive(w.RefContent); !ok {
			// The proposal referred to a belief that vanished before this
			// transaction. Do not reinterpret that stale reference as ADD.
			_, _ = tx.Exec(`DELETE FROM memory_observations WHERE id = ?`, obsID)
			return tx.Commit()
		}
	}
	switch decision {
	case "REINFORCE":
		if id, conf, ok := findActive(w.RefContent); ok {
			if err := reinforce(id, conf); err != nil {
				return err
			}
			if err := edge(id, RelationSupports); err != nil {
				return err
			}
			if err := event("reinforce", id, ""); err != nil {
				return err
			}
			return tx.Commit()
		}
		return tx.Commit()

	case "ADD", "":
		if id, conf, ok := findActive(content); ok {
			// Identical statement already believed: corroboration, not a copy.
			if err := reinforce(id, conf); err != nil {
				return err
			}
			if err := edge(id, RelationSupports); err != nil {
				return err
			}
			if err := event("reinforce", id, ""); err != nil {
				return err
			}
			return tx.Commit()
		}
		id, err := insertCanonical(CanonicalActive)
		if err != nil {
			return err
		}
		if err := edge(id, RelationSupports); err != nil {
			return err
		}
		if err := event("create", id, ""); err != nil {
			return err
		}
		return tx.Commit()

	case "SUPERSEDE":
		newID, err := insertCanonical(CanonicalActive)
		if err != nil {
			return err
		}
		if oldID, _, ok := findActive(w.RefContent); ok {
			var snapshot string
			_ = tx.QueryRow(`SELECT content FROM canonical_memories WHERE id = ?`, oldID).Scan(&snapshot)
			if _, err := tx.Exec(`UPDATE canonical_memories SET status = ?, valid_until = ?, superseded_by = ?, updated_at = ? WHERE id = ?`,
				CanonicalSuperseded, now, newID, now, oldID); err != nil {
				return err
			}
			if err := edge(oldID, RelationSupersedes); err != nil {
				return err
			}
			if err := event("supersede", oldID, snapshot); err != nil {
				return err
			}
		}
		if err := edge(newID, RelationSupports); err != nil {
			return err
		}
		if err := event("create", newID, ""); err != nil {
			return err
		}
		return tx.Commit()

	case "CONFLICT":
		newID, err := insertCanonical(CanonicalConflicted)
		if err != nil {
			return err
		}
		if err := edge(newID, RelationSupports); err != nil {
			return err
		}
		if oldID, _, ok := findActive(w.RefContent); ok {
			if err := edge(oldID, RelationContradicts); err != nil {
				return err
			}
			if err := event("conflict", oldID, ""); err != nil {
				return err
			}
		}
		if err := event("create", newID, ""); err != nil {
			return err
		}
		return tx.Commit()
	}
	return tx.Commit()
}

// applyMerge folds judged cluster members into one new canonical. Members
// become archived with their evidence re-pointed; the full member snapshot in
// the event makes undo a restore, not a reconstruction.
type mergeUndoSnapshot struct {
	Members  []CanonicalMemory   `json:"members"`
	Evidence map[string][]string `json:"evidence"`
}

func applyMerge(db *sql.DB, w MergeWrite) error {
	if len(w.MemberIDs) < 2 || strings.TrimSpace(w.Canonical) == "" {
		return nil
	}
	now := time.Now().UTC().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var members []CanonicalMemory
	evidenceByMember := make(map[string][]string)
	evidence, occurrences := 0, 0
	confidence := 0.0
	earliest, latestVerified := int64(0), int64(0)
	for _, id := range w.MemberIDs {
		var c CanonicalMemory
		var pinned, confirmed int
		var created, verified sql.NullInt64
		err := tx.QueryRow(`SELECT id, target, scope, content, status, pinned, user_confirmed, confidence,
			evidence_count, occurrences, created_at, last_verified_at
			FROM canonical_memories WHERE id = ?`, id).
			Scan(&c.ID, &c.Target, &c.Scope, &c.Content, &c.Status, &pinned, &confirmed, &c.Confidence,
				&c.EvidenceCount, &c.Occurrences, &created, &verified)
		if err != nil {
			return fmt.Errorf("merge member %s: %w", id, err)
		}
		if pinned != 0 || confirmed != 0 {
			return fmt.Errorf("merge member %s is protected", id)
		}
		members = append(members, c)
		rows, err := tx.Query(`SELECT observation_id FROM memory_evidence WHERE memory_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var observationID string
			if rows.Scan(&observationID) == nil {
				evidenceByMember[id] = append(evidenceByMember[id], observationID)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		evidence += c.EvidenceCount
		occurrences += c.Occurrences
		if c.Confidence > confidence {
			confidence = c.Confidence
		}
		if created.Valid && (earliest == 0 || created.Int64 < earliest) {
			earliest = created.Int64
		}
		if verified.Valid && verified.Int64 > latestVerified {
			latestVerified = verified.Int64
		}
	}
	if earliest == 0 {
		earliest = now
	}

	newID := uuid.New().String()
	if _, err := tx.Exec(`INSERT INTO canonical_memories
		(id, target, scope, content, normalized_hash, status, pinned, user_confirmed,
		 confidence, evidence_count, occurrences, last_verified_at, valid_from, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?)`,
		newID, w.Target, w.Scope, strings.TrimSpace(w.Canonical), NormalizedContentHash(w.Canonical),
		CanonicalActive, RepetitionBoost(confidence, len(members)),
		evidence, occurrences, latestVerified, earliest, earliest, now); err != nil {
		return err
	}
	for _, m := range members {
		if _, err := tx.Exec(`UPDATE canonical_memories SET status = ?, superseded_by = ?, updated_at = ? WHERE id = ?`,
			CanonicalArchived, newID, now, m.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE memory_evidence SET memory_id = ? WHERE memory_id = ?`, newID, m.ID); err != nil {
			return err
		}
	}
	snapshot, _ := json.Marshal(mergeUndoSnapshot{Members: members, Evidence: evidenceByMember})
	detail, _ := json.Marshal(map[string]interface{}{"cluster_id": w.ClusterID, "member_ids": w.MemberIDs})
	if _, err := tx.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, confidence, snapshot, detail, created_at)
		VALUES (?, ?, 'merge', ?, ?, ?, ?, ?)`,
		uuid.New().String(), w.Actor, newID, w.Confidence, string(snapshot), string(detail), now); err != nil {
		return err
	}
	return tx.Commit()
}

func undoMemoryEvent(db *sql.DB, eventID, actor string) error {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return fmt.Errorf("memory event id is required")
	}
	var action, memoryID, snapshot string
	if err := db.QueryRow(`SELECT action, COALESCE(memory_id,''), COALESCE(snapshot,'')
		FROM memory_events WHERE id = ?`, eventID).Scan(&action, &memoryID, &snapshot); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("memory event not found: %s", eventID)
		}
		return err
	}
	now := time.Now().UTC().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	switch action {
	case "archive":
		if memoryID == "" {
			return fmt.Errorf("archive event %s has no memory id", eventID)
		}
		if _, err := tx.Exec(`UPDATE canonical_memories SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
			CanonicalActive, now, memoryID, CanonicalArchived); err != nil {
			return err
		}
	case "merge":
		var snap mergeUndoSnapshot
		if json.Unmarshal([]byte(snapshot), &snap) != nil || len(snap.Members) < 2 || len(snap.Evidence) == 0 {
			return fmt.Errorf("merge event %s predates reversible evidence snapshots", eventID)
		}
		for _, member := range snap.Members {
			if _, err := tx.Exec(`UPDATE canonical_memories SET status = ?, superseded_by = ?, updated_at = ? WHERE id = ?`,
				member.Status, member.SupersededBy, now, member.ID); err != nil {
				return err
			}
			for _, observationID := range snap.Evidence[member.ID] {
				if _, err := tx.Exec(`UPDATE memory_evidence SET memory_id = ? WHERE memory_id = ? AND observation_id = ?`,
					member.ID, memoryID, observationID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE canonical_memories SET status = ?, updated_at = ? WHERE id = ?`,
			CanonicalForgotten, now, memoryID); err != nil {
			return err
		}
	case "pin", "unpin":
		var snap canonicalProtectionSnapshot
		if memoryID == "" || json.Unmarshal([]byte(snapshot), &snap) != nil {
			return fmt.Errorf("%s event %s has no protection snapshot", action, eventID)
		}
		if _, err := tx.Exec(`UPDATE canonical_memories SET pinned = ?, user_confirmed = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`,
			sqliteBool(snap.Pinned), sqliteBool(snap.UserConfirmed), now, memoryID); err != nil {
			return err
		}
	case "dedup":
		var snap DedupUndoSnapshot
		if memoryID == "" || json.Unmarshal([]byte(snapshot), &snap) != nil ||
			snap.Canonical.ID != memoryID || len(snap.Observations) == 0 || len(snap.Evidence) == 0 {
			return fmt.Errorf("dedup event %s has no reversible evidence snapshot", eventID)
		}
		for _, observation := range snap.Observations {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_observations
				(id, run_id, analyzer_version, workspace_id, target, scope, source, content,
				 normalized_hash, confidence_prior, status, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				observation.ID, observation.RunID, observation.AnalyzerVersion,
				observation.WorkspaceID, observation.Target, observation.Scope,
				observation.Source, observation.Content, observation.NormalizedHash,
				observation.ConfidencePrior, observation.Status, observation.CreatedAt); err != nil {
				return err
			}
		}
		for _, evidence := range snap.Evidence {
			if evidence.MemoryID != memoryID {
				return fmt.Errorf("dedup event %s contains evidence for another memory", eventID)
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_evidence
				(memory_id, observation_id, relation, created_at) VALUES (?, ?, ?, ?)`,
				evidence.MemoryID, evidence.ObservationID, evidence.Relation, evidence.CreatedAt); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE canonical_memories SET confidence = ?, evidence_count = ?,
			occurrences = ?, updated_at = ? WHERE id = ?`, snap.Canonical.Confidence,
			snap.Canonical.EvidenceCount, snap.Canonical.Occurrences,
			snap.Canonical.UpdatedAt, memoryID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("memory event action %q cannot be undone", action)
	}
	detail, _ := json.Marshal(map[string]string{"event_id": eventID, "original_action": action})
	if _, err := tx.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, detail, created_at)
		VALUES (?, ?, 'undo', ?, ?, ?)`, uuid.New().String(), actor, memoryID, string(detail), now); err != nil {
		return err
	}
	return tx.Commit()
}

func setCanonicalStatusByHash(db *sql.DB, target, scope, content, status, actor string) error {
	res, err := db.Exec(`UPDATE canonical_memories SET status = ?,
		valid_until = CASE WHEN ? = ? THEN NULL ELSE valid_until END,
		superseded_by = CASE WHEN ? = ? THEN '' ELSE superseded_by END,
		updated_at = ?
		WHERE target = ? AND scope = ? AND normalized_hash = ? AND status != ?`,
		status, status, CanonicalActive, status, CanonicalActive,
		time.Now().UTC().Unix(), target, scope, NormalizedContentHash(content), status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = db.Exec(`INSERT INTO memory_events (id, actor, action, detail, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			uuid.New().String(), actor, status, content, time.Now().UTC().Unix())
	}
	return err
}

func setCanonicalStatusByID(db *sql.DB, id, status, actor string) error {
	res, err := db.Exec(`UPDATE canonical_memories SET status = ?,
		valid_until = CASE WHEN ? = ? THEN NULL ELSE valid_until END,
		superseded_by = CASE WHEN ? = ? THEN '' ELSE superseded_by END,
		updated_at = ? WHERE id = ? AND status != ?`,
		status, status, CanonicalActive, status, CanonicalActive,
		time.Now().UTC().Unix(), id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = db.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			uuid.New().String(), actor, status, id, time.Now().UTC().Unix())
	}
	return err
}

type canonicalProtectionSnapshot struct {
	Pinned        bool `json:"pinned"`
	UserConfirmed bool `json:"user_confirmed"`
}

func setCanonicalPinned(db *sql.DB, id string, pinned bool, actor string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldPinned, oldConfirmed int
	if err := tx.QueryRow(`SELECT pinned, user_confirmed FROM canonical_memories WHERE id = ?`, id).Scan(&oldPinned, &oldConfirmed); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("canonical memory not found: %s", id)
		}
		return err
	}
	newPinned := sqliteBool(pinned)
	newConfirmed := oldConfirmed
	if pinned {
		newConfirmed = 1
	}
	if oldPinned == newPinned && oldConfirmed == newConfirmed {
		return tx.Commit()
	}
	now := time.Now().UTC().Unix()
	if _, err := tx.Exec(`UPDATE canonical_memories SET pinned = ?, user_confirmed = ?,
		last_verified_at = CASE WHEN ? = 1 THEN ? ELSE last_verified_at END,
		updated_at = ?, revision = revision + 1 WHERE id = ?`,
		newPinned, newConfirmed, newPinned, now, now, id); err != nil {
		return err
	}
	snapshot, _ := json.Marshal(canonicalProtectionSnapshot{Pinned: oldPinned != 0, UserConfirmed: oldConfirmed != 0})
	action := "unpin"
	if pinned {
		action = "pin"
	}
	if _, err := tx.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, snapshot, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, uuid.New().String(), actor, action, id, string(snapshot), now); err != nil {
		return err
	}
	return tx.Commit()
}

func sqliteBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func touchCanonicalAccess(db *sql.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC().Unix()
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE canonical_memories SET last_accessed_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
	}
	return nil
}

func archiveCanonicals(db *sql.DB, ids []string, actor, reason string) error {
	now := time.Now().UTC().Unix()
	for _, id := range ids {
		res, err := db.Exec(`UPDATE canonical_memories SET status = ?, updated_at = ?
			WHERE id = ? AND status = ? AND pinned = 0 AND user_confirmed = 0`,
			CanonicalArchived, now, id, CanonicalActive)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if _, err := db.Exec(`INSERT INTO memory_events (id, actor, action, memory_id, detail, created_at)
				VALUES (?, ?, 'archive', ?, ?, ?)`,
				uuid.New().String(), actor, id, reason, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordConsolidationJudgement(db *sql.DB, clusterID, action string, confidence float64, detail string) error {
	payload, _ := json.Marshal(map[string]interface{}{"cluster_id": clusterID, "action": action, "detail": detail})
	_, err := db.Exec(`INSERT INTO memory_events (id, actor, action, confidence, detail, created_at)
		VALUES (?, 'consolidator', 'consolidation.judged', ?, ?, ?)`,
		uuid.New().String(), confidence, string(payload), time.Now().UTC().Unix())
	return err
}

func listJudgedClusterIDs(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT detail FROM memory_events WHERE action IN ('consolidation.judged','merge') AND detail != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			continue
		}
		var payload struct {
			ClusterID string `json:"cluster_id"`
		}
		if json.Unmarshal([]byte(detail), &payload) == nil && payload.ClusterID != "" {
			out[payload.ClusterID] = true
		}
	}
	return out, rows.Err()
}

func epochTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Exported CanonicalStore methods routed through the single-worker actor.

func (p *SQLiteProvider) ListCanonicalMemories(ctx context.Context, tenantID string, filter CanonicalFilter) ([]CanonicalMemory, error) {
	val, err := p.call("ListCanonicalMemories", tenantID, filter)
	if err != nil || val == nil {
		return nil, err
	}
	return val.([]CanonicalMemory), nil
}

func (p *SQLiteProvider) ObservationsForMemory(ctx context.Context, tenantID, memoryID string) ([]Observation, error) {
	val, err := p.call("ObservationsForMemory", tenantID, memoryID)
	if err != nil || val == nil {
		return nil, err
	}
	return val.([]Observation), nil
}

func (p *SQLiteProvider) ListMemoryEvents(ctx context.Context, tenantID string, limit int) ([]MemoryEvent, error) {
	val, err := p.call("ListMemoryEvents", tenantID, limit)
	if err != nil || val == nil {
		return nil, err
	}
	return val.([]MemoryEvent), nil
}

func (p *SQLiteProvider) ApplyIntakeWrite(ctx context.Context, tenantID string, w IntakeWrite) error {
	_, err := p.call("ApplyIntakeWrite", tenantID, w)
	return err
}

func (p *SQLiteProvider) ApplyMerge(ctx context.Context, tenantID string, w MergeWrite) error {
	_, err := p.call("ApplyMerge", tenantID, w)
	return err
}

func (p *SQLiteProvider) SetCanonicalStatusByHash(ctx context.Context, tenantID, target, scope, content, status, actor string) error {
	_, err := p.call("SetCanonicalStatusByHash", tenantID, target, scope, content, status, actor)
	return err
}

func (p *SQLiteProvider) SetCanonicalStatus(ctx context.Context, tenantID, id, status, actor string) error {
	_, err := p.call("SetCanonicalStatus", tenantID, id, status, actor)
	return err
}

func (p *SQLiteProvider) SetCanonicalPinned(ctx context.Context, tenantID, id string, pinned bool, actor string) error {
	_, err := p.call("SetCanonicalPinned", tenantID, id, pinned, actor)
	return err
}

func (p *SQLiteProvider) TouchCanonicalAccess(ctx context.Context, tenantID string, ids []string) error {
	_, err := p.call("TouchCanonicalAccess", tenantID, ids)
	return err
}

func (p *SQLiteProvider) ArchiveCanonicals(ctx context.Context, tenantID string, ids []string, actor, reason string) error {
	_, err := p.call("ArchiveCanonicals", tenantID, ids, actor, reason)
	return err
}

func (p *SQLiteProvider) ListJudgedClusterIDs(ctx context.Context, tenantID string) (map[string]bool, error) {
	val, err := p.call("ListJudgedClusterIDs", tenantID)
	if err != nil || val == nil {
		return nil, err
	}
	return val.(map[string]bool), nil
}

// RecordConsolidationJudgement stores one shadow-mode judge decision as an
// audit event; the cluster id in detail doubles as the re-run checkpoint.
func (p *SQLiteProvider) RecordConsolidationJudgement(ctx context.Context, tenantID, clusterID, action string, confidence float64, detail string) error {
	_, err := p.call("RecordConsolidationJudgement", tenantID, clusterID, action, confidence, detail)
	return err
}

func (p *SQLiteProvider) UndoMemoryEvent(ctx context.Context, tenantID, eventID, actor string) error {
	_, err := p.call("UndoMemoryEvent", tenantID, eventID, actor)
	return err
}
