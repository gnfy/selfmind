package cliapp

// memory_dedup_commands.go implements `selfmind maintenance memory-dedup`.
//
// Legacy-import duplicate-evidence defect: before importLegacyFacts gained its
// run/scope/hash guard, a run-attributed legacy fact was re-materialized as a
// memory_observation under the fact's own uuid even when the live intake path
// had already recorded the SAME evidence for the SAME run under a
// deterministic `obs_<hash>` id. The id-keyed INSERT OR IGNORE bounds each
// fact to one duplicate, but every duplicated fact inflates
// evidence_count/occurrences and therefore REINFORCE math. This one-time
// cleanup finds those duplicate-evidence groups and, with --apply, removes the
// redundant import rows and corrects the counters. Default is a dry-run
// report; every applied correction leaves an audit event.

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"selfmind/internal/kernel/memory"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// dedupObservationRow is one observation participating in a duplicate group.
type dedupObservationRow struct {
	id              string
	runID           string
	analyzerVersion int
	workspaceID     string
	target          string
	scope           string
	source          string
	content         string
	hash            string
	confidencePrior float64
	status          string
	createdAt       int64
	legacyTwin      bool
}

// dedupEvidenceGroup is one canonical whose evidence set contains more than
// one observation for the same (run_id, scope, normalized_hash) — i.e. the
// same piece of evidence counted more than once.
type dedupEvidenceGroup struct {
	memoryID  string
	runID     string
	scope     string
	hash      string
	keeper    dedupObservationRow
	redundant []dedupObservationRow
}

// memoryDedupReport aggregates one partition's outcome; dry-run and apply
// share it so both print identical numbers.
type memoryDedupReport struct {
	Groups              int
	RemovedEdges        int // evidence edges removed (or that would be)
	RemovedObservations int // import observations deleted (or that would be)
	AffectedCanonicals  int
}

// runMaintenanceMemoryDedup is the CLI entry for
// `selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]`.
func (a *App) runMaintenanceMemoryDedup(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance memory-dedup", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "remove redundant import evidence and correct counters (default is a dry-run report)")
	partition := fs.String("partition", "", "single partition to dedup (e.g. person_<id> or default); empty scans default + person_* partitions")
	dataDir := fs.String("data-dir", "", "memory data directory (default: the configured storage data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}

	// The daemon owns memory.db in normal operation; WAL keeps the apply
	// transaction safe, but a live intake writer can create fresh groups
	// mid-report, so advise stopping it first.
	fmt.Fprintln(a.stdout, "Warning: stop the daemon first (`selfmind gateway stop`) so no new evidence is written during dedup.")

	explicit := strings.TrimSpace(*partition)
	partitions := []string{explicit}
	if explicit == "" {
		partitions = listMemoryAuditPartitions(dir)
	}
	total := memoryDedupReport{}
	exit := 0
	for _, part := range partitions {
		dbPath := filepath.Join(dir, part, "memory.db")
		if _, err := os.Stat(dbPath); err != nil {
			if explicit != "" {
				fmt.Fprintf(a.stderr, "memory-dedup: partition %s has no memory.db (%v)\n", part, err)
				return 1
			}
			continue // enumerated partition without a layered store yet
		}
		report, err := dedupMemoryPartition(a.ctx, a.stdout, part, dbPath, *apply)
		if err != nil {
			fmt.Fprintf(a.stderr, "memory-dedup: %s: %v\n", part, err)
			exit = 1
			continue
		}
		if report.Groups > 0 {
			verb := "would remove"
			if *apply {
				verb = "removed"
			}
			fmt.Fprintf(a.stdout, "[%s] %d duplicate groups: %s %d evidence edges, %d legacy observations, %d canonicals corrected\n",
				part, report.Groups, verb, report.RemovedEdges, report.RemovedObservations, report.AffectedCanonicals)
		}
		total.Groups += report.Groups
		total.RemovedEdges += report.RemovedEdges
		total.RemovedObservations += report.RemovedObservations
		total.AffectedCanonicals += report.AffectedCanonicals
	}
	if total.Groups == 0 {
		fmt.Fprintln(a.stdout, "No duplicate evidence groups found.")
		return exit
	}
	if *apply {
		fmt.Fprintf(a.stdout, "Removed %d evidence edges and %d import observations; corrected %d canonicals (audit events appended).\n",
			total.RemovedEdges, total.RemovedObservations, total.AffectedCanonicals)
	} else {
		fmt.Fprintf(a.stdout, "Dry-run only: %d duplicate groups found; re-run with --apply to remove them.\n", total.Groups)
	}
	return exit
}

// dedupMemoryPartition detects (and with apply=true removes) duplicate
// evidence inside one partition db. All writes happen in one transaction so a
// failure leaves the partition untouched.
func dedupMemoryPartition(ctx context.Context, w io.Writer, partition, dbPath string, apply bool) (memoryDedupReport, error) {
	var report memoryDedupReport
	db, err := openMigrationDB(dbPath)
	if err != nil {
		return report, fmt.Errorf("open memory db: %w", err)
	}
	defer db.Close()
	// A partition that predates the layered model has nothing to dedup.
	for _, table := range []string{"facts", "memory_observations", "memory_evidence", "canonical_memories", "memory_events"} {
		ok, err := tableExists(ctx, db, "main", table)
		if err != nil {
			return report, err
		}
		if !ok {
			return report, nil
		}
	}

	groups, err := findDuplicateEvidenceGroups(ctx, db)
	if err != nil {
		return report, err
	}
	report.Groups = len(groups)
	if len(groups) == 0 {
		return report, nil
	}
	for _, g := range groups {
		ids := make([]string, 0, len(g.redundant))
		for _, r := range g.redundant {
			ids = append(ids, r.id)
		}
		fmt.Fprintf(w, "[%s] canonical %s run %s: keep %s, redundant %s\n",
			partition, shortDedupID(g.memoryID), g.runID, g.keeper.id, strings.Join(ids, ", "))
	}
	if !apply {
		// Dry run reports the same numbers apply would produce, without writes.
		countDedupPlan(ctx, db, groups, &report)
		return report, nil
	}
	return applyDedupGroups(ctx, db, groups, report)
}

// findDuplicateEvidenceGroups returns every (canonical, run, scope, hash)
// whose evidence set holds more than one observation. Group keys are read
// fully before the per-group member queries: the db is opened with a single
// connection, so a nested query while a result set is open would deadlock.
func findDuplicateEvidenceGroups(ctx context.Context, db *sql.DB) ([]dedupEvidenceGroup, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.memory_id, o.run_id, o.scope, o.normalized_hash
		FROM memory_evidence e
		JOIN memory_observations o ON o.id = e.observation_id
		WHERE TRIM(COALESCE(o.run_id, '')) != '' AND e.relation = 'supports'
		GROUP BY e.memory_id, o.run_id, o.scope, o.normalized_hash
		HAVING COUNT(DISTINCT o.id) > 1
		ORDER BY e.memory_id, o.run_id`)
	if err != nil {
		return nil, err
	}
	var groups []dedupEvidenceGroup
	for rows.Next() {
		var g dedupEvidenceGroup
		if err := rows.Scan(&g.memoryID, &g.runID, &g.scope, &g.hash); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range groups {
		if err := loadDedupGroupMembers(ctx, db, &groups[i]); err != nil {
			return nil, err
		}
	}
	// Drop groups that resolved to fewer than two members (defensive; the
	// HAVING clause should prevent this).
	out := groups[:0]
	for _, g := range groups {
		if len(g.redundant) > 0 {
			out = append(out, g)
		}
	}
	return out, nil
}

// loadDedupGroupMembers only returns rows that can be proven to have come from
// the old legacy importer. A same-run/hash UUID observation is not enough:
// another extractor may have produced independent evidence. The redundant
// row must still exist in facts with the same id/run/target/content, and the
// group must contain a deterministic obs_ intake row to keep.
func loadDedupGroupMembers(ctx context.Context, db *sql.DB, g *dedupEvidenceGroup) error {
	rows, err := db.QueryContext(ctx, `SELECT o.id, COALESCE(o.run_id, ''),
		COALESCE(o.analyzer_version, 0), COALESCE(o.workspace_id, ''), o.target,
		COALESCE(o.scope, ''), COALESCE(o.source, ''), o.content, o.normalized_hash,
		COALESCE(o.confidence_prior, 0), COALESCE(o.status, ''), COALESCE(o.created_at, 0),
		EXISTS(SELECT 1 FROM facts f WHERE f.id = o.id
			AND TRIM(COALESCE(f.created_from_run, '')) = o.run_id
			AND COALESCE(f.target, '') = o.target
			AND COALESCE(f.content, '') = o.content)
		FROM memory_evidence e
		JOIN memory_observations o ON o.id = e.observation_id
		WHERE e.memory_id = ? AND o.run_id = ? AND o.scope = ? AND o.normalized_hash = ?
			AND e.relation = 'supports'
		GROUP BY o.id
		ORDER BY o.created_at ASC, o.id ASC`, g.memoryID, g.runID, g.scope, g.hash)
	if err != nil {
		return err
	}
	defer rows.Close()
	var members []dedupObservationRow
	for rows.Next() {
		var m dedupObservationRow
		var legacyTwin int
		if err := rows.Scan(&m.id, &m.runID, &m.analyzerVersion, &m.workspaceID,
			&m.target, &m.scope, &m.source, &m.content, &m.hash,
			&m.confidencePrior, &m.status, &m.createdAt, &legacyTwin); err != nil {
			return err
		}
		m.legacyTwin = legacyTwin != 0
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(members) < 2 {
		return nil
	}
	keeperIdx := -1
	for i, m := range members {
		if strings.HasPrefix(m.id, "obs_") {
			keeperIdx = i // earliest obs_ row wins (rows are created_at ordered)
			break
		}
	}
	if keeperIdx < 0 {
		return nil
	}
	g.keeper = members[keeperIdx]
	for i, m := range members {
		if i != keeperIdx && m.legacyTwin {
			g.redundant = append(g.redundant, m)
		}
	}
	return nil
}

// countDedupPlan computes what apply would remove, without writing. Edge and
// canonical counts are exact; the observation count mirrors apply's rule
// (only proven legacy import rows with no other citing canonical are deleted).
func countDedupPlan(ctx context.Context, db *sql.DB, groups []dedupEvidenceGroup, report *memoryDedupReport) {
	affected := map[string]bool{}
	for _, g := range groups {
		affected[g.memoryID] = true
		for _, r := range g.redundant {
			var edges int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence
				WHERE memory_id = ? AND observation_id = ? AND relation = 'supports'`, g.memoryID, r.id).Scan(&edges); err == nil {
				report.RemovedEdges += edges
			}
			var others int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence
				WHERE observation_id = ? AND memory_id != ?`, r.id, g.memoryID).Scan(&others); err == nil && others == 0 {
				report.RemovedObservations++
			}
		}
	}
	report.AffectedCanonicals = len(affected)
}

// applyDedupGroups removes the redundant evidence in one transaction:
// redundant edges go, redundant IMPORT observations go (a deterministic
// `obs_` row is never deleted — it is live-intake evidence, only its
// duplicate edge was wrong), affected canonicals get their counters recomputed
// from the remaining supports edges, and each affected canonical gains one
// audit event so the operation is reviewable and attributable afterwards.
func applyDedupGroups(ctx context.Context, db *sql.DB, groups []dedupEvidenceGroup, report memoryDedupReport) (memoryDedupReport, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck // rollback error is secondary
		}
	}()

	now := time.Now().UTC().Unix()
	removedByMemory := map[string][]string{}
	snapshots := map[string]*memory.DedupUndoSnapshot{}
	for _, g := range groups {
		snapshot := snapshots[g.memoryID]
		if snapshot == nil {
			snapshot = &memory.DedupUndoSnapshot{}
			if err := tx.QueryRowContext(ctx, `SELECT id, confidence, evidence_count, occurrences, COALESCE(updated_at, 0)
				FROM canonical_memories WHERE id = ?`, g.memoryID).Scan(
				&snapshot.Canonical.ID, &snapshot.Canonical.Confidence,
				&snapshot.Canonical.EvidenceCount, &snapshot.Canonical.Occurrences,
				&snapshot.Canonical.UpdatedAt); err != nil {
				return report, fmt.Errorf("snapshot canonical %s: %w", g.memoryID, err)
			}
			snapshots[g.memoryID] = snapshot
		}
		for _, r := range g.redundant {
			var relation string
			var edgeCreatedAt int64
			if err := tx.QueryRowContext(ctx, `SELECT relation, COALESCE(created_at, 0)
				FROM memory_evidence WHERE memory_id = ? AND observation_id = ? AND relation = 'supports'`,
				g.memoryID, r.id).Scan(&relation, &edgeCreatedAt); err != nil {
				return report, fmt.Errorf("snapshot evidence edge %s -> %s: %w", g.memoryID, r.id, err)
			}
			snapshot.Observations = append(snapshot.Observations, memory.DedupObservationSnapshot{
				ID: r.id, RunID: r.runID, AnalyzerVersion: r.analyzerVersion,
				WorkspaceID: r.workspaceID, Target: r.target, Scope: r.scope,
				Source: r.source, Content: r.content, NormalizedHash: r.hash,
				ConfidencePrior: r.confidencePrior, Status: r.status, CreatedAt: r.createdAt,
			})
			snapshot.Evidence = append(snapshot.Evidence, memory.DedupEvidenceSnapshot{
				MemoryID: g.memoryID, ObservationID: r.id, Relation: relation, CreatedAt: edgeCreatedAt,
			})
			res, err := tx.ExecContext(ctx, `DELETE FROM memory_evidence
				WHERE memory_id = ? AND observation_id = ? AND relation = 'supports'`, g.memoryID, r.id)
			if err != nil {
				return report, fmt.Errorf("delete evidence edge %s -> %s: %w", g.memoryID, r.id, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				report.RemovedEdges += int(n)
			}
			removedByMemory[g.memoryID] = append(removedByMemory[g.memoryID], r.id)
			// A shared observation cited by another canonical must survive so
			// that relation never dangles.
			var one int
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM memory_evidence WHERE observation_id = ? LIMIT 1`, r.id).Scan(&one)
			switch {
			case err == sql.ErrNoRows:
				if _, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE id = ?`, r.id); err != nil {
					return report, fmt.Errorf("delete import observation %s: %w", r.id, err)
				}
				report.RemovedObservations++
			case err != nil:
				return report, err
			}
		}
	}

	memIDs := make([]string, 0, len(removedByMemory))
	for id := range removedByMemory {
		memIDs = append(memIDs, id)
	}
	sort.Strings(memIDs)
	for _, memID := range memIDs {
		var supports int
		var baseConfidence float64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
			COALESCE(MAX(CASE WHEN o.confidence_prior > 0 THEN o.confidence_prior ELSE 0.5 END), 0.5)
			FROM memory_evidence e
			JOIN memory_observations o ON o.id = e.observation_id
			WHERE e.memory_id = ? AND e.relation = ?`, memID, memory.RelationSupports).
			Scan(&supports, &baseConfidence); err != nil {
			return report, err
		}
		confidence := memory.RepetitionBoost(baseConfidence, supports)
		// Counter correction is allowed on every canonical, including pinned
		// and user-confirmed ones — content and status stay untouched. One
		// immutable supports observation represents one occurrence, so all three
		// derived fields are recomputed from the surviving evidence.
		if _, err := tx.ExecContext(ctx, `UPDATE canonical_memories SET
			confidence = ?,
			evidence_count = ?,
			occurrences = ?,
			updated_at = ?
			WHERE id = ?`, confidence, supports, supports, now, memID); err != nil {
			return report, fmt.Errorf("recount canonical %s: %w", memID, err)
		}
		detail, _ := json.Marshal(map[string]interface{}{
			"removed_observations": removedByMemory[memID],
			"evidence_count":       supports,
			"occurrences":          supports,
			"confidence":           confidence,
		})
		snapshot, err := json.Marshal(snapshots[memID])
		if err != nil {
			return report, fmt.Errorf("marshal dedup snapshot for %s: %w", memID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_events
			(id, actor, action, memory_id, snapshot, detail, created_at)
			VALUES (?, 'memory-dedup', 'dedup', ?, ?, ?, ?)`,
			uuid.New().String(), memID, string(snapshot), string(detail), now); err != nil {
			return report, fmt.Errorf("audit event for %s: %w", memID, err)
		}
		report.AffectedCanonicals++
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	committed = true
	return report, nil
}

func shortDedupID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
