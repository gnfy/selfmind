package cliapp

// memory_migrate_commands.go implements `selfmind maintenance migrate-memory`.
//
// Background maintenance used to write person memory into the legacy
// `default` partition (`<data-dir>/default/memory.db`) instead of the
// person partition (`<data-dir>/<person_id>/memory.db`) that the foreground
// agent actually reads. The write site is fixed; this command moves the
// stranded rows. Rows whose originating run can no longer be resolved to a
// person stay in `default` — that partition IS the legacy archive — and are
// only counted. Default is a dry-run report; `--apply` performs the move.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// legacyMemoryPartition is the partition pre-fix maintenance wrote into. It
// doubles as the archive for rows that cannot be attributed to a person, so
// the migration never deletes the partition itself.
const legacyMemoryPartition = "default"

// migratedMemoryTables are the person-memory tables the migration touches.
// Order is presentation-only; each person's move is a single transaction.
var migratedMemoryTables = []string{
	"facts",
	"canonical_memories",
	"memory_observations",
	"memory_evidence",
	"memory_events",
}

// memoryMigrationOptions carries the resolved CLI inputs so the core is
// directly testable without a flag.FlagSet.
type memoryMigrationOptions struct {
	dataDir string
	apply   bool
}

// personMigrationCounts reports how many rows moved (apply) or would move
// (dry-run) into one person partition.
type personMigrationCounts struct {
	Facts        int
	Canonicals   int
	Observations int
	Evidence     int
	Events       int
}

func (c personMigrationCounts) empty() bool {
	return c.Facts == 0 && c.Canonicals == 0 && c.Observations == 0 && c.Evidence == 0 && c.Events == 0
}

// memoryMigrationReport is the full outcome, shared by dry-run and apply so
// both print identical numbers.
type memoryMigrationReport struct {
	SourcePath string
	Applied    bool
	Persons    map[string]personMigrationCounts
	// Unresolved rows reference a run id that no longer resolves to exactly
	// one person; they stay in the legacy partition on purpose.
	UnresolvedFacts        int
	UnresolvedCanonicals   int
	UnresolvedObservations int
	// FactsWithoutRun are pre-attribution legacy facts with no run reference
	// at all; they also stay in the legacy partition.
	FactsWithoutRun int
}

// runMaintenanceMigrateMemory is the CLI entry for
// `selfmind maintenance migrate-memory [--apply] [--data-dir <dir>]`.
func (a *App) runMaintenanceMigrateMemory(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance migrate-memory", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "perform the migration (default is a dry-run report)")
	dataDir := fs.String("data-dir", "", "override the gateway data directory (default: resolved from config)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}

	fmt.Fprintf(a.stdout, "Legacy memory db: %s\n", filepath.Join(dir, legacyMemoryPartition, "memory.db"))
	// The daemon owns memory.db in normal operation; a concurrent writer
	// cannot corrupt the move (WAL + transactions) but can produce fresh
	// stranded rows mid-report, so advise stopping it first.
	fmt.Fprintln(a.stdout, "Warning: stop the daemon first (`selfmind gateway stop`) so no new rows are written during migration.")

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	report, err := runMemoryMigration(ctx, memoryMigrationOptions{dataDir: dir, apply: *apply})
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance migrate-memory: %v\n", err)
		return 1
	}
	printMemoryMigrationReport(a.stdout, report)
	return 0
}

func printMemoryMigrationReport(w io.Writer, report *memoryMigrationReport) {
	verb := "would move"
	if report.Applied {
		verb = "moved"
	}
	persons := make([]string, 0, len(report.Persons))
	for id := range report.Persons {
		persons = append(persons, id)
	}
	sort.Strings(persons)
	total := 0
	for _, id := range persons {
		c := report.Persons[id]
		if c.empty() {
			continue
		}
		total++
		fmt.Fprintf(w, "%s: %s facts=%d canonicals=%d observations=%d evidence=%d events=%d\n",
			id, verb, c.Facts, c.Canonicals, c.Observations, c.Evidence, c.Events)
	}
	if total == 0 {
		fmt.Fprintln(w, "No migratable rows found in the legacy partition.")
	}
	fmt.Fprintf(w, "Left in legacy partition (run unresolved): facts=%d canonicals=%d observations=%d\n",
		report.UnresolvedFacts, report.UnresolvedCanonicals, report.UnresolvedObservations)
	fmt.Fprintf(w, "Left in legacy partition (no run reference): facts=%d\n", report.FactsWithoutRun)
	if !report.Applied {
		fmt.Fprintln(w, "Dry-run only; re-run with --apply to migrate.")
	}
}

// runMemoryMigration is the testable core: it plans (and with apply=true
// executes) the move of person-attributable rows from the legacy partition
// into per-person memory dbs.
func runMemoryMigration(ctx context.Context, opts memoryMigrationOptions) (*memoryMigrationReport, error) {
	dataDir := strings.TrimSpace(opts.dataDir)
	if dataDir == "" {
		return nil, fmt.Errorf("data dir is required")
	}
	sourcePath := filepath.Join(dataDir, legacyMemoryPartition, "memory.db")
	report := &memoryMigrationReport{
		SourcePath: sourcePath,
		Applied:    opts.apply,
		Persons:    map[string]personMigrationCounts{},
	}
	if _, err := os.Stat(sourcePath); err != nil {
		if os.IsNotExist(err) {
			return report, nil // nothing was ever written to the legacy partition
		}
		return nil, fmt.Errorf("stat legacy memory db: %w", err)
	}

	runPerson, err := loadRunPersonMap(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		return nil, err
	}

	src, err := openMigrationDB(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open legacy memory db: %w", err)
	}
	defer src.Close()

	plan, err := planMemoryMigration(ctx, src, runPerson)
	if err != nil {
		return nil, err
	}
	report.UnresolvedFacts = plan.unresolvedFacts
	report.UnresolvedCanonicals = plan.unresolvedCanonicals
	report.UnresolvedObservations = plan.unresolvedObservations
	report.FactsWithoutRun = plan.factsWithoutRun
	for personID, p := range plan.persons {
		report.Persons[personID] = personMigrationCounts{
			Facts:        len(p.factIDs),
			Canonicals:   len(p.canonicalIDs),
			Observations: len(p.observationIDs),
			Evidence:     p.evidenceCount,
			Events:       p.eventCount,
		}
	}
	if !opts.apply {
		return report, nil
	}

	personIDs := make([]string, 0, len(plan.persons))
	for id := range plan.persons {
		personIDs = append(personIDs, id)
	}
	sort.Strings(personIDs)
	for _, personID := range personIDs {
		// Safety: never write back into the legacy partition itself, and
		// never trust an empty person id from a corrupted control row.
		if personID == "" || personID == legacyMemoryPartition {
			continue
		}
		targetPath := filepath.Join(dataDir, personID, "memory.db")
		if filepath.Clean(targetPath) == filepath.Clean(sourcePath) {
			return nil, fmt.Errorf("refusing to migrate: target %s equals source", targetPath)
		}
		moved, err := migrateOnePerson(ctx, src, targetPath, plan.persons[personID])
		if err != nil {
			return nil, fmt.Errorf("migrate person %s: %w", personID, err)
		}
		report.Persons[personID] = moved
	}
	return report, nil
}

// loadRunPersonMap builds run_id -> person_id from control.db with a
// read-only raw query. Opening via control.OpenStore would create the db and
// run schema migrations as a side effect, which a reporting command must not
// do, so a missing control.db simply resolves nothing.
func loadRunPersonMap(ctx context.Context, controlPath string) (map[string]string, error) {
	if _, err := os.Stat(controlPath); err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("stat control.db: %w", err)
	}
	db, err := openMigrationDB(controlPath)
	if err != nil {
		return nil, fmt.Errorf("open control.db: %w", err)
	}
	defer db.Close()
	ok, err := tableExists(ctx, db, "main", "runs")
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]string{}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(person_id, '') FROM runs`)
	if err != nil {
		return nil, fmt.Errorf("read task_runs: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var runID, personID string
		if err := rows.Scan(&runID, &personID); err != nil {
			return nil, err
		}
		if strings.TrimSpace(personID) != "" {
			out[runID] = strings.TrimSpace(personID)
		}
	}
	return out, rows.Err()
}

// openMigrationDB opens a sqlite db with a single connection so ATTACH and
// transactions always share one underlying connection (database/sql would
// otherwise route statements across pooled connections that do not see the
// attached target).
func openMigrationDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// personMigrationPlan is the per-person id set to move. Evidence and event
// rows are keyed by canonical id, so only their counts are planned.
type personMigrationPlan struct {
	factIDs        []string
	canonicalIDs   []string
	observationIDs []string
	evidenceCount  int
	eventCount     int
}

type memoryMigrationPlan struct {
	persons                map[string]*personMigrationPlan
	unresolvedFacts        int
	unresolvedCanonicals   int
	unresolvedObservations int
	factsWithoutRun        int
}

func (p *memoryMigrationPlan) person(id string) *personMigrationPlan {
	if p.persons[id] == nil {
		p.persons[id] = &personMigrationPlan{}
	}
	return p.persons[id]
}

// planMemoryMigration attributes each legacy row to a person:
//   - facts: via facts.created_from_run (standalone rows, no relations)
//   - orphan observations (no evidence link): via memory_observations.run_id
//   - canonical memories: per connected component over evidence links. A
//     canonical, its memory_evidence rows, its memory_events rows, and ALL
//     its evidence-linked observations move together, and only when every
//     linked observation's run resolves to the same person. Otherwise the
//     entire component stays in the legacy partition (counted as
//     unresolved), so a relation never dangles across two databases.
func planMemoryMigration(ctx context.Context, src *sql.DB, runPerson map[string]string) (*memoryMigrationPlan, error) {
	plan := &memoryMigrationPlan{persons: map[string]*personMigrationPlan{}}

	// facts --------------------------------------------------------------
	if ok, err := tableExists(ctx, src, "main", "facts"); err != nil {
		return nil, err
	} else if ok {
		cols, err := tableColumns(ctx, src, "main", "facts")
		if err != nil {
			return nil, err
		}
		if !cols.has("created_from_run") {
			// Pre-attribution schema: every fact predates run attribution
			// and stays in the legacy partition.
			if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM facts`).Scan(&plan.factsWithoutRun); err != nil {
				return nil, err
			}
		} else {
			rows, err := src.QueryContext(ctx, `SELECT id, TRIM(COALESCE(created_from_run, '')) FROM facts`)
			if err != nil {
				return nil, err
			}
			if err := scanRunAttributed(rows, runPerson, func(id, person string) {
				plan.person(person).factIDs = append(plan.person(person).factIDs, id)
			}, &plan.factsWithoutRun, &plan.unresolvedFacts); err != nil {
				return nil, err
			}
		}
	}

	// observations ---------------------------------------------------------
	obsRun := map[string]string{} // observation id -> trimmed run id ("" when none)
	if ok, err := tableExists(ctx, src, "main", "memory_observations"); err != nil {
		return nil, err
	} else if ok {
		rows, err := src.QueryContext(ctx, `SELECT id, TRIM(COALESCE(run_id, '')) FROM memory_observations`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, runID string
			if err := rows.Scan(&id, &runID); err != nil {
				return nil, err
			}
			obsRun[id] = runID
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// evidence links -------------------------------------------------------
	// Evidence rows are both the join table to move and the edges of the
	// component graph below.
	evidenceByMemory := map[string][]string{}
	linkedObs := map[string]bool{} // observations referenced by any evidence row
	links := newComponentLinker()
	if ok, err := tableExists(ctx, src, "main", "memory_evidence"); err != nil {
		return nil, err
	} else if ok {
		rows, err := src.QueryContext(ctx, `SELECT memory_id, observation_id FROM memory_evidence`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var memID, obsID string
			if err := rows.Scan(&memID, &obsID); err != nil {
				return nil, err
			}
			evidenceByMemory[memID] = append(evidenceByMemory[memID], obsID)
			linkedObs[obsID] = true
			links.union("m:"+memID, "o:"+obsID)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// canonical memories ----------------------------------------------------
	canonicalSet := map[string]bool{}
	if ok, err := tableExists(ctx, src, "main", "canonical_memories"); err != nil {
		return nil, err
	} else if ok {
		rows, err := src.QueryContext(ctx, `SELECT id FROM canonical_memories`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			canonicalSet[id] = true
			links.touch("m:" + id) // a canonical with no evidence is its own component
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// connected components ---------------------------------------------------
	// Each component moves in full or not at all: an observation shared by
	// two canonicals, or a canonical whose evidence spans two persons, must
	// never leave one half of a relation behind in the legacy db.
	canonicalPerson := map[string]string{}
	for _, comp := range links.components() {
		var canonicals, observations []string
		dangling := false
		for _, node := range comp {
			switch {
			case strings.HasPrefix(node, "m:"):
				id := strings.TrimPrefix(node, "m:")
				if !canonicalSet[id] {
					dangling = true // evidence references a missing canonical
					continue
				}
				canonicals = append(canonicals, id)
			case strings.HasPrefix(node, "o:"):
				id := strings.TrimPrefix(node, "o:")
				if _, exists := obsRun[id]; !exists {
					dangling = true // evidence references a missing observation
					continue
				}
				observations = append(observations, id)
			}
		}
		// The component resolves only when it is structurally complete and
		// every linked observation's run maps to one single person.
		person := ""
		resolvable := !dangling && len(canonicals) > 0 && len(observations) > 0
		for _, obsID := range observations {
			p := ""
			if runID := obsRun[obsID]; runID != "" {
				p = runPerson[runID]
			}
			if p == "" || (person != "" && p != person) {
				resolvable = false
				break
			}
			person = p
		}
		if resolvable {
			for _, id := range canonicals {
				canonicalPerson[id] = person
				plan.person(person).canonicalIDs = append(plan.person(person).canonicalIDs, id)
				plan.person(person).evidenceCount += len(evidenceByMemory[id])
			}
			plan.person(person).observationIDs = append(plan.person(person).observationIDs, observations...)
			continue
		}
		plan.unresolvedCanonicals += len(canonicals)
		for _, obsID := range observations {
			// Only observations that reference a run were ever candidates to
			// move; run-less legacy imports stay silently as before.
			if obsRun[obsID] != "" {
				plan.unresolvedObservations++
			}
		}
	}

	// orphan observations (no evidence link) --------------------------------
	for obsID, runID := range obsRun {
		if linkedObs[obsID] || runID == "" {
			continue // component-handled above, or run-less legacy import
		}
		person := runPerson[runID]
		if person == "" {
			plan.unresolvedObservations++
			continue
		}
		plan.person(person).observationIDs = append(plan.person(person).observationIDs, obsID)
	}
	if ok, err := tableExists(ctx, src, "main", "memory_events"); err != nil {
		return nil, err
	} else if ok {
		rows, err := src.QueryContext(ctx, `SELECT COALESCE(memory_id, '') FROM memory_events`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var memID string
			if err := rows.Scan(&memID); err != nil {
				return nil, err
			}
			if person := canonicalPerson[memID]; person != "" {
				plan.person(person).eventCount++
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// scanRunAttributed consumes (id, run_id) rows and routes each id to its
// person, counting no-run and unresolved rows. It closes rows.
func scanRunAttributed(rows *sql.Rows, runPerson map[string]string, emit func(id, person string), noRun, unresolved *int) error {
	defer rows.Close()
	for rows.Next() {
		var id, runID string
		if err := rows.Scan(&id, &runID); err != nil {
			return err
		}
		if runID == "" {
			*noRun++
			continue
		}
		person := runPerson[runID]
		if person == "" {
			*unresolved++
			continue
		}
		emit(id, person)
	}
	return rows.Err()
}

// componentLinker is a small union-find over string nodes, used to group
// canonical memories and their evidence-linked observations into connected
// components that migrate atomically.
type componentLinker struct {
	parent map[string]string
}

func newComponentLinker() *componentLinker {
	return &componentLinker{parent: map[string]string{}}
}

// touch registers a node without linking it, so an isolated canonical still
// forms its own (unresolvable) component.
func (l *componentLinker) touch(node string) {
	l.find(node)
}

func (l *componentLinker) find(node string) string {
	root, ok := l.parent[node]
	if !ok {
		l.parent[node] = node
		return node
	}
	if root == node {
		return node
	}
	top := l.find(root)
	l.parent[node] = top // path compression
	return top
}

func (l *componentLinker) union(a, b string) {
	ra, rb := l.find(a), l.find(b)
	if ra != rb {
		l.parent[ra] = rb
	}
}

// components returns the node groups. Order is unspecified.
func (l *componentLinker) components() map[string][]string {
	out := map[string][]string{}
	for node := range l.parent {
		root := l.find(node)
		out[root] = append(out[root], node)
	}
	return out
}

// migrateOnePerson moves one person's rows in a single transaction on the
// source db with the target attached: INSERT OR IGNORE into the target, then
// DELETE from the source. INSERT OR IGNORE on the primary keys makes a
// re-run after a partial failure idempotent; the DELETE count is reported as
// "moved" because it is the number of rows that actually left the legacy
// partition.
func migrateOnePerson(ctx context.Context, src *sql.DB, targetPath string, plan *personMigrationPlan) (personMigrationCounts, error) {
	var counts personMigrationCounts
	if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
		return counts, fmt.Errorf("create target dir: %w", err)
	}
	if err := ensureTargetSchema(ctx, src, targetPath); err != nil {
		return counts, err
	}
	if _, err := src.ExecContext(ctx, `ATTACH DATABASE ? AS dst`, targetPath); err != nil {
		return counts, fmt.Errorf("attach target db: %w", err)
	}
	defer src.ExecContext(context.Background(), `DETACH DATABASE dst`) //nolint:errcheck // best-effort cleanup

	// A pre-existing person db may predate columns the source has gained
	// (e.g. facts.created_from_run); align before column-listed inserts.
	// Column lists are resolved here as well: the db has one pooled
	// connection, so a PRAGMA on src while the transaction below holds that
	// connection would deadlock.
	columnsByTable := map[string]columnSet{}
	for _, table := range migratedMemoryTables {
		if err := alignTargetColumns(ctx, src, table); err != nil {
			return counts, err
		}
		cols, err := tableColumns(ctx, src, "main", table)
		if err != nil {
			return counts, err
		}
		columnsByTable[table] = cols
	}

	tx, err := src.BeginTx(ctx, nil)
	if err != nil {
		return counts, err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck // rollback error is secondary
		}
	}()

	if counts.Facts, err = moveRowsByKey(ctx, tx, columnsByTable["facts"], "facts", "id", plan.factIDs); err != nil {
		return counts, err
	}
	if counts.Canonicals, err = moveRowsByKey(ctx, tx, columnsByTable["canonical_memories"], "canonical_memories", "id", plan.canonicalIDs); err != nil {
		return counts, err
	}
	if counts.Observations, err = moveRowsByKey(ctx, tx, columnsByTable["memory_observations"], "memory_observations", "id", plan.observationIDs); err != nil {
		return counts, err
	}
	// Evidence and events are keyed by their canonical memory id.
	if counts.Evidence, err = moveRowsByKey(ctx, tx, columnsByTable["memory_evidence"], "memory_evidence", "memory_id", plan.canonicalIDs); err != nil {
		return counts, err
	}
	if counts.Events, err = moveRowsByKey(ctx, tx, columnsByTable["memory_events"], "memory_events", "memory_id", plan.canonicalIDs); err != nil {
		return counts, err
	}
	if err := tx.Commit(); err != nil {
		return counts, err
	}
	committed = true
	return counts, nil
}

// ensureTargetSchema copies missing table and index DDL from the source db's
// sqlite_master to the target, so the migration never invents its own schema
// and stays correct if the provider schema evolves.
func ensureTargetSchema(ctx context.Context, src *sql.DB, targetPath string) error {
	type ddl struct{ name, sql string }
	var ddls []ddl
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(migratedMemoryTables)), ",")
	args := make([]interface{}, len(migratedMemoryTables))
	for i, t := range migratedMemoryTables {
		args[i] = t
	}
	rows, err := src.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master
		 WHERE type IN ('table', 'index') AND sql IS NOT NULL AND tbl_name IN (`+placeholders+`)
		 ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END`, args...)
	if err != nil {
		return fmt.Errorf("read source schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d ddl
		if err := rows.Scan(&d.name, &d.sql); err != nil {
			return err
		}
		ddls = append(ddls, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	target, err := openMigrationDB(targetPath)
	if err != nil {
		return fmt.Errorf("open target db: %w", err)
	}
	defer target.Close()
	for _, d := range ddls {
		var one int
		err := target.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE name = ?`, d.name).Scan(&one)
		if err == nil {
			continue // object already exists on the target
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := target.ExecContext(ctx, d.sql); err != nil {
			return fmt.Errorf("create %s on target: %w", d.name, err)
		}
	}
	return nil
}

// alignTargetColumns adds source columns missing on the attached target so a
// column-listed INSERT ... SELECT cannot fail against an older target schema.
// NOT NULL is intentionally dropped: ALTER TABLE cannot add a NOT NULL column
// without a default, and relaxing it never loses data.
func alignTargetColumns(ctx context.Context, src *sql.DB, table string) error {
	srcCols, err := tableInfo(ctx, src, "main", table)
	if err != nil {
		return err
	}
	dstCols, err := tableInfo(ctx, src, "dst", table)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range dstCols {
		have[c.name] = true
	}
	for _, c := range srcCols {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE dst.%s ADD COLUMN %s %s`, table, c.name, c.colType)
		if c.dflt.Valid {
			stmt += " DEFAULT " + c.dflt.String
		}
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("add column %s.%s on target: %w", table, c.name, err)
		}
	}
	return nil
}

// moveRowsByKey copies matching rows into the attached target and deletes
// them from the source, in ID chunks to stay under sqlite's bind limits. The
// column list is precomputed by the caller (a PRAGMA here would need a second
// pooled connection while the transaction holds the only one). The
// deleted-row count is returned so an idempotent re-run reports zero.
func moveRowsByKey(ctx context.Context, tx *sql.Tx, cols columnSet, table, keyColumn string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	colList := strings.Join(cols, ", ")
	moved := 0
	const chunkSize = 200
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		insert := fmt.Sprintf(`INSERT OR IGNORE INTO dst.%s (%s) SELECT %s FROM main.%s WHERE %s IN (%s)`,
			table, colList, colList, table, keyColumn, placeholders)
		if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
			return moved, fmt.Errorf("copy %s: %w", table, err)
		}
		del := fmt.Sprintf(`DELETE FROM main.%s WHERE %s IN (%s)`, table, keyColumn, placeholders)
		res, err := tx.ExecContext(ctx, del, args...)
		if err != nil {
			return moved, fmt.Errorf("delete source %s: %w", table, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			moved += int(n)
		}
	}
	return moved, nil
}

// columnInfo is the subset of PRAGMA table_info the migration needs.
type columnInfo struct {
	name    string
	colType string
	dflt    sql.NullString
}

type columnSet []string

func (s columnSet) has(name string) bool {
	for _, c := range s {
		if c == name {
			return true
		}
	}
	return false
}

func tableColumns(ctx context.Context, db *sql.DB, schema, table string) (columnSet, error) {
	info, err := tableInfo(ctx, db, schema, table)
	if err != nil {
		return nil, err
	}
	out := make(columnSet, 0, len(info))
	for _, c := range info {
		out = append(out, c.name)
	}
	return out, nil
}

func tableInfo(ctx context.Context, db *sql.DB, schema, table string) ([]columnInfo, error) {
	// schema and table are internal constants, never user input.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA %s.table_info(%s)`, schema, table))
	if err != nil {
		return nil, fmt.Errorf("table_info %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var (
			cid, notnull, pk int
			c                columnInfo
		)
		if err := rows.Scan(&cid, &c.name, &c.colType, &notnull, &c.dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func tableExists(ctx context.Context, db *sql.DB, schema, table string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT 1 FROM %s.sqlite_master WHERE type = 'table' AND name = ?`, schema), table).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
