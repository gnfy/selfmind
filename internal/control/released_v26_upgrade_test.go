package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rowFixture is a control database written by an older binary's own store API
// with synthetic rows (testdata/README.md).
type rowFixture struct {
	file    string
	version int
	effects bool // the schema has external effect claims (v23+)
}

var releasedRowFixtures = []rowFixture{
	{file: "control-v17-beta.27-rows.sql", version: 17},
	{file: "control-v24-beta.28-rows.sql", version: 24, effects: true},
	{file: "control-v25-rows.sql", version: 25, effects: true},
}

func loadRowFixture(t *testing.T, dir string, fixture rowFixture) {
	t.Helper()
	dump, err := os.ReadFile(filepath.Join("testdata", fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(dump)); err != nil {
		t.Fatalf("load %s: %v", fixture.file, err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != fixture.version {
		t.Fatalf("%s is at schema %d err=%v, want %d", fixture.file, version, err, fixture.version)
	}
}

// fixtureProjection is every durable fact the upgrade must carry unchanged.
func fixtureProjection(t *testing.T, db *sql.DB, version int) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"run_plan_steps": `SELECT run_id, plan_version, step_id, sequence, step_text, status, success_criteria,
			source_step_id, source_plan_version FROM run_plan_steps ORDER BY run_id, plan_version, sequence`,
		"runs": `SELECT id, COALESCE(thread_id, ''), tenant_id, person_id, channel, status, COALESCE(input_summary, ''),
			COALESCE(resumes_run_id, ''), COALESCE(execution_roots_json, ''), COALESCE(recovery_contract_version, 0),
			started_at, COALESCE(finished_at, 0) FROM runs ORDER BY id`,
		"task_queue": `SELECT id, person_id, channel, platform, COALESCE(platform_user_id, ''), content, COALESCE(approval_mode, ''),
			COALESCE(execution_roots_json, ''), COALESCE(attachments_json, ''), COALESCE(thread_id, ''), COALESCE(run_id, ''),
			COALESCE(reply_to_run_id, ''), COALESCE(idempotency_key, ''), class, priority, not_before, status,
			claim_token, created_at FROM task_queue ORDER BY id`,
		"external_watches": `SELECT id, person_id, COALESCE(run_id, ''), status, finalized, notified, verdict_revision,
			COALESCE(preflight_receipt_json, ''), created_at, COALESCE(finished_at, 0) FROM external_watches ORDER BY id`,
		"approval_requests": `SELECT id, person_id, COALESCE(run_id, ''), status, COALESCE(waiter_state, ''), COALESCE(parked_at, 0),
			COALESCE(park_reason, ''), COALESCE(decision_recorded_at, 0), COALESCE(resume_queue_id, ''),
			COALESCE(authorization_state, '') FROM approval_requests ORDER BY id`,
		"clarify_requests": `SELECT id, person_id, COALESCE(run_id, ''), status, question, COALESCE(answer, '')
			FROM clarify_requests ORDER BY id`,
		"loop_checkpoints": `SELECT run_id, person_id, contract_version, iteration, outcome, detail, snapshot_json
			FROM loop_checkpoints ORDER BY run_id`,
		"task_events":   `SELECT id, cursor, COALESCE(run_id, ''), type FROM task_events ORDER BY cursor`,
		"counter":       `SELECT next_cursor FROM event_sequence`,
		"threads":       `SELECT id, person_id, kind, visibility, title FROM threads ORDER BY id`,
		"accounts":      `SELECT id, person_id, platform, platform_user_id, status FROM accounts ORDER BY id`,
		"ledger_names":  `SELECT version, name FROM schema_migrations ORDER BY version`,
		"inbound_dedup": `SELECT platform, message_id, created_at FROM inbound_dedup ORDER BY platform, message_id`,
	}
	if version >= 22 {
		queries["inbound_receipts"] = `SELECT platform, message_id, state, hex(payload), updated_at, last_error, tenant_id,
			person_id, preview FROM inbound_dedup ORDER BY platform, message_id`
	}
	if version >= 23 {
		queries["external_effect_claims"] = `SELECT id, person_id, run_id, effect_id, target_key, state, observation_ref,
			created_at FROM external_effect_claims ORDER BY id`
		queries["external_resource_waits"] = `SELECT run_id, effect_id, targets_json, status FROM external_resource_waits ORDER BY run_id`
	}
	if version >= 24 {
		queries["plan_cancellations"] = `SELECT run_id, plan_version, step_id, cancellation_disposition, cancellation_reason,
			user_takeover_quote FROM run_plan_steps ORDER BY run_id, plan_version, sequence`
	}
	if version >= 25 {
		queries["plan_scope_evidence"] = `SELECT run_id, plan_version, step_id, replacement_step_id, scope_change_quote
			FROM run_plan_steps ORDER BY run_id, plan_version, sequence`
	}
	out := map[string][]string{}
	for name, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("project %s: %v", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]sql.NullString, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(values))
			for i, value := range values {
				parts[i] = value.String
			}
			out[name] = append(out[name], strings.Join(parts, "\x1f"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return out
}

func sameProjection(t *testing.T, label string, want, got map[string][]string, upgradedLedger bool) {
	t.Helper()
	for name, rows := range want {
		gotRows := got[name]
		if name == "ledger_names" && upgradedLedger {
			// The upgrade appends its own steps; every earlier row is kept.
			if len(gotRows) < len(rows) {
				t.Fatalf("%s: ledger lost rows: %v -> %v", label, rows, gotRows)
			}
			gotRows = gotRows[:len(rows)]
		}
		if strings.Join(rows, "\n") != strings.Join(gotRows, "\n") {
			t.Fatalf("%s: %s changed across the upgrade:\nbefore %q\nafter  %q", label, name, rows, gotRows)
		}
	}
}

func TestReleasedRowFixturesUpgradeToV26WithoutNewAuthority(t *testing.T) {
	for _, fixture := range releasedRowFixtures {
		t.Run(fixture.file, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			loadRowFixture(t, dir, fixture)
			legacy, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			before := fixtureProjection(t, legacy, fixture.version)
			if len(before["runs"]) == 0 || len(before["task_queue"]) == 0 || len(before["external_watches"]) == 0 ||
				len(before["approval_requests"]) == 0 || len(before["clarify_requests"]) == 0 || len(before["loop_checkpoints"]) == 0 ||
				len(before["run_plan_steps"]) == 0 || len(before["inbound_dedup"]) == 0 {
				t.Fatalf("fixture is not representative: %v", before)
			}
			if fixture.version >= 22 && !strings.Contains(strings.Join(before["inbound_receipts"], "\n"), "\x1fdispatching\x1f") {
				t.Fatalf("receipt fixture has no uncertain receipt: %v", before["inbound_receipts"])
			}
			if fixture.version >= 24 && !strings.Contains(strings.Join(before["plan_cancellations"], "\n"), "user_takeover") {
				t.Fatalf("cancellation fixture has no assessed cancellation: %v", before["plan_cancellations"])
			}
			if fixture.effects && (len(before["external_effect_claims"]) == 0 || len(before["external_resource_waits"]) == 0) {
				t.Fatalf("effect fixture has no claims or waits: %v", before)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			store, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("upgrade %s: %v", fixture.file, err)
			}
			status := store.SchemaStatus()
			if status.Version != CurrentControlSchemaVersion || status.MigrationBackup == "" {
				t.Fatalf("schema status=%+v", status)
			}
			after := fixtureProjection(t, store.db, fixture.version)
			sameProjection(t, "upgrade", before, after, true)

			// The backup was taken before the first step: it still has the
			// released shape, and it is a sound database.
			backup, err := sql.Open("sqlite", status.MigrationBackup)
			if err != nil {
				t.Fatal(err)
			}
			var integrity string
			var backupVersion, admissionColumns int
			if err := backup.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("backup integrity=%q err=%v", integrity, err)
			}
			if err := backup.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&backupVersion); err != nil || backupVersion != fixture.version {
				t.Fatalf("backup version=%d err=%v, want %d", backupVersion, err, fixture.version)
			}
			if err := backup.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'admission_json'`).Scan(&admissionColumns); err != nil || admissionColumns != 0 {
				t.Fatalf("backup already has v26 columns=%d err=%v", admissionColumns, err)
			}
			sameProjection(t, "backup", before, fixtureProjection(t, backup, fixture.version), false)
			_ = backup.Close()

			// Historical rows gain no snapshot, order, settlement, or wait binding.
			for _, row := range before["runs"] {
				runID := strings.SplitN(row, "\x1f", 2)[0]
				admission, err := store.RunAdmissionSnapshot(ctx, DefaultTenantID, runID)
				if err != nil || !admission.IsZero() {
					t.Fatalf("historical run %s admission=%+v err=%v", runID, admission, err)
				}
			}
			for _, row := range before["task_queue"] {
				queued, err := store.GetQueued(ctx, DefaultTenantID, strings.SplitN(row, "\x1f", 2)[0])
				if err != nil || queued == nil || !queued.Admission.IsZero() {
					t.Fatalf("historical queue row admission=%+v err=%v", queued, err)
				}
			}
			for _, row := range before["external_watches"] {
				watch, err := store.GetExternalWatch(ctx, DefaultTenantID, strings.SplitN(row, "\x1f", 2)[0])
				if err != nil || watch == nil || watch.Sequence != 0 {
					t.Fatalf("historical watcher sequence=%+v err=%v", watch, err)
				}
			}
			var fabricated int
			if err := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM runs WHERE admission_json <> '')
				+ (SELECT COUNT(*) FROM task_queue WHERE admission_json <> '')
				+ (SELECT COUNT(*) FROM external_effect_claims WHERE sequence <> 0)
				+ (SELECT COUNT(*) FROM external_watches WHERE sequence <> 0)
				+ (SELECT COUNT(*) FROM external_effect_settlements)
				+ (SELECT COUNT(*) FROM loop_checkpoints WHERE wait_kind <> '' OR wait_ref <> '' OR wait_tool_call_id <> '')
				+ (SELECT COUNT(*) FROM clarify_requests WHERE parked_at <> 0)
				+ (SELECT COUNT(*) FROM run_plan_steps WHERE cancellation_quote_source <> '')
				+ (SELECT COUNT(*) FROM inbound_dedup WHERE claim_generation <> 0)
				+ (SELECT COUNT(*) FROM mcp_read_only_marks)`).Scan(&fabricated); err != nil || fabricated != 0 {
				t.Fatalf("upgrade fabricated %d snapshot/order/settlement/binding/source/generation/marking value(s) err=%v", fabricated, err)
			}
			var plannedRun string
			if err := store.db.QueryRow(`SELECT run_id FROM run_plan_steps LIMIT 1`).Scan(&plannedRun); err != nil {
				t.Fatal(err)
			}
			plan, err := store.LatestRunPlan(ctx, DefaultTenantID, plannedRun)
			if err != nil || plan == nil || len(plan.Steps) == 0 {
				t.Fatalf("historical plan = %+v err=%v", plan, err)
			}
			for _, step := range plan.Steps {
				if step.CancellationQuoteSource != "" {
					t.Fatalf("historical step %s gained a quote source %q", step.StepID, step.CancellationQuoteSource)
				}
			}
			units := make([]string, 0, len(plan.Steps))
			unitRows, err := store.db.Query(`SELECT work_unit_id FROM run_plan_steps WHERE run_id = ? AND plan_version = ? ORDER BY sequence`, plannedRun, plan.Version)
			if err != nil {
				t.Fatal(err)
			}
			for unitRows.Next() {
				var unit string
				if err := unitRows.Scan(&unit); err != nil {
					t.Fatal(err)
				}
				units = append(units, unit)
			}
			_ = unitRows.Close()
			// A step without a source hashes as it did under v25, so repeating
			// a historical snapshot is no change. Releases before v25 already
			// hashed without the v25 scope-evidence fields.
			if recomputed := hashRunPlanSteps(plan.Steps, units); fixture.version == 25 && recomputed != plan.ContentHash {
				t.Fatalf("historical plan hash changed: recomputed %s, stored %s", recomputed, plan.ContentHash)
			}

			// New work after the upgrade uses the same counter as history and
			// lands after every historical event.
			owner, err := store.ResolveAccount(ctx, DefaultTenantID, "cli", "fixture-owner")
			if err != nil || owner == nil {
				t.Fatalf("fixture owner: %+v %v", owner, err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "after upgrade", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRunWithOptions(ctx, task, "cli", "after upgrade", StartRunOptions{Admission: verifyOnlyCronAdmission()})
			if err != nil {
				t.Fatal(err)
			}
			if admission, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID); err != nil || admission != verifyOnlyCronAdmission() {
				t.Fatalf("new run admission=%+v err=%v", admission, err)
			}
			if fixture.version >= 22 {
				// A historical uncertain receipt stays listed for its owner, with
				// no claim generation recorded.
				uncertain, err := store.ListUncertainInboundForPerson(ctx, owner.TenantID, owner.PersonID, 20)
				if err != nil || len(uncertain) == 0 {
					t.Fatalf("historical uncertain receipts = %+v err=%v", uncertain, err)
				}
				for _, receipt := range uncertain {
					if receipt.ClaimGeneration != 0 {
						t.Fatalf("historical receipt %s/%s gained claim generation %d", receipt.Platform, receipt.MessageID, receipt.ClaimGeneration)
					}
				}
			}
			// No historical person marked an MCP tool read-only; the owner
			// can from now on, and it applies only to that server identity.
			if marked, err := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_fixture", "get_issue", time.Time{}); err != nil || marked {
				t.Fatalf("historical read-only marking = %t err=%v", marked, err)
			}
			if _, created, err := store.MarkMCPToolReadOnly(ctx, MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				ServerName: "github", ServerIdentity: "mcpsrv_fixture", ToolName: "get_issue",
				ActorAccountID: owner.AccountID, ActorChannel: "cli-after-upgrade"}); err != nil || !created {
				t.Fatalf("mark after upgrade: created=%t err=%v", created, err)
			}
			if marked, err := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_fixture", "get_issue", time.Time{}); err != nil || !marked {
				t.Fatalf("marking after upgrade = %t err=%v", marked, err)
			}
			historicalCursor := before["counter"][0]
			decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: run.ID, EffectID: "effect-after-upgrade", TargetKeys: []string{"registry:pypi"}})
			if err != nil || !decision.Granted || decision.Claims[0].Sequence <= 0 ||
				decision.Claims[0].Sequence <= mustAtoi(t, historicalCursor) {
				t.Fatalf("new claim after counter %s = %+v err=%v", historicalCursor, decision, err)
			}

			// A parked approval asked before v26 has no checkpoint binding and
			// stays answerable, and so does a pending clarification.
			var approvalID, approvalRun, approvalThread string
			if err := store.db.QueryRow(`SELECT id, run_id, thread_id FROM approval_requests
				WHERE status = 'pending' AND waiter_state = 'parked'`).Scan(&approvalID, &approvalRun, &approvalThread); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, DefaultTenantID, approvalRun)
			if err != nil || checkpoint == nil || checkpoint.WaitKind != "" || checkpoint.WaitRef != "" || checkpoint.WaitToolCallID != "" {
				t.Fatalf("historical checkpoint gained a wait binding: %+v err=%v", checkpoint, err)
			}
			parentAdmission, err := store.ExactParentAdmissionSnapshot(ctx, DefaultTenantID, owner.PersonID, approvalRun)
			if err != nil || !parentAdmission.IsZero() {
				t.Fatalf("historical parked run admission=%+v err=%v", parentAdmission, err)
			}
			answered, resumed, err := store.RespondParkedApprovalAndEnqueue(ctx, DefaultTenantID, owner.PersonID, approvalID, "approved", "cli",
				ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{TaskID: approvalThread, Content: "approved",
					IdempotencyKey: "approval-resume:" + approvalID, Admission: parentAdmission})
			if err != nil || answered == nil || answered.Status != "approved" || resumed == nil {
				t.Fatalf("historical parked approval is not answerable: %+v %+v err=%v", answered, resumed, err)
			}
			if row, err := store.GetQueued(ctx, DefaultTenantID, resumed.ID); err != nil || row == nil || !row.Admission.IsZero() || row.ApprovalID != approvalID {
				t.Fatalf("historical approval continuation = %+v err=%v", row, err)
			}
			var clarifyID string
			if err := store.db.QueryRow(`SELECT id FROM clarify_requests WHERE status = 'pending'`).Scan(&clarifyID); err != nil {
				t.Fatal(err)
			}
			if clarify, err := store.GetClarifyRequest(ctx, DefaultTenantID, clarifyID); err != nil || clarify == nil || clarify.ParkedAt != nil {
				t.Fatalf("historical clarification park=%+v err=%v", clarify, err)
			}
			if _, resumedClarify, err := store.AnswerClarifyRequestWithResume(ctx, DefaultTenantID, owner.PersonID, clarifyID,
				"release/1.2", "cli", QueuedTask{}); err != nil || resumedClarify == nil {
				t.Fatalf("historical clarification is not answerable: %+v err=%v", resumedClarify, err)
			}

			if fixture.effects {
				// A historical uncertain effect stays held until its owner settles it.
				var claimID, holderRun, waiterRun string
				if err := store.db.QueryRow(`SELECT id, run_id FROM external_effect_claims WHERE state = 'uncertain'`).Scan(&claimID, &holderRun); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow(`SELECT run_id FROM external_resource_waits WHERE status = 'pending'`).Scan(&waiterRun); err != nil {
					t.Fatal(err)
				}
				if ready, err := store.ListReadyExternalResourceWaits(ctx, 10); err != nil || len(ready) != 0 {
					t.Fatalf("upgrade released a held target: ready=%+v err=%v", ready, err)
				}
				settled, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
					ClaimID: claimID, ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "verified after upgrade"})
				if err != nil || settled.Replayed || settled.Settlement.RunID != holderRun {
					t.Fatalf("settle historical effect = %+v err=%v", settled, err)
				}
				ready, err := store.ListReadyExternalResourceWaits(ctx, 10)
				if err != nil || len(ready) != 1 || ready[0].RunID != waiterRun {
					t.Fatalf("settled target did not make its waiter ready: %+v err=%v", ready, err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			backups := countMigrationBackups(t, dir)
			reopened, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("reopen upgraded fixture: %v", err)
			}
			defer reopened.Close()
			if reopened.SchemaStatus().Version != CurrentControlSchemaVersion || reopened.SchemaStatus().MigrationBackup != "" ||
				countMigrationBackups(t, dir) != backups {
				t.Fatalf("reopen was not idempotent: %+v backups=%d", reopened.SchemaStatus(), countMigrationBackups(t, dir))
			}
		})
	}
}

func mustAtoi(t *testing.T, value string) int64 {
	t.Helper()
	out, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("not a counter value: %q", value)
	}
	return out
}

func version26Migration(t *testing.T) schemaMigration {
	t.Helper()
	for _, migration := range orderedMigrations {
		if migration.Version == 26 {
			return migration
		}
	}
	t.Fatal("v26 migration is missing")
	return schemaMigration{}
}

func TestVersion26StepIsIdempotentAndCapabilityInert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loadRowFixture(t, dir, releasedRowFixtures[2])
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	step := version26Migration(t)
	for attempt := 0; attempt < 2; attempt++ {
		if err := step.Apply(ctx, db); err != nil {
			t.Fatalf("apply v26 attempt %d: %v", attempt+1, err)
		}
		if err := verifyMigrationInvariants(ctx, db, before); err != nil {
			t.Fatalf("v26 attempt %d broke an invariant: %v", attempt+1, err)
		}
	}
	for _, column := range []struct{ table, name string }{
		{"runs", "admission_json"}, {"task_queue", "admission_json"},
		{"external_effect_claims", "sequence"}, {"external_watches", "sequence"},
		{"loop_checkpoints", "wait_kind"}, {"loop_checkpoints", "wait_ref"}, {"loop_checkpoints", "wait_tool_call_id"},
		{"clarify_requests", "parked_at"}, {"run_plan_steps", "cancellation_quote_source"},
		{"inbound_dedup", "claim_generation"},
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, column.table, column.name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s.%s columns=%d err=%v", column.table, column.name, count, err)
		}
	}
	var settlements, marks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM external_effect_settlements`).Scan(&settlements); err != nil || settlements != 0 {
		t.Fatalf("settlements=%d err=%v", settlements, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_read_only_marks`).Scan(&marks); err != nil || marks != 0 {
		t.Fatalf("read-only markings=%d err=%v", marks, err)
	}
}

// Every v26 value is inert for history, so the framework refuses any step that
// manufactures one, and any step that changes what a claim or watcher records.
func TestMigrationInvariantsRejectFabricatedAuthorityAndOrder(t *testing.T) {
	for name, mutation := range map[string]string{
		"run admission snapshot":   `UPDATE runs SET admission_json = '{"version":1}' WHERE rowid = (SELECT MIN(rowid) FROM runs)`,
		"queue admission snapshot": `UPDATE task_queue SET admission_json = '{"version":1}' WHERE rowid = (SELECT MIN(rowid) FROM task_queue)`,
		"person settlement": `INSERT INTO external_effect_settlements (id, tenant_id, person_id, claim_id, run_id, effect_id, created_at)
			SELECT 'effectsettle_forged', tenant_id, person_id, id, run_id, effect_id, 1 FROM external_effect_claims WHERE state = 'uncertain'`,
		"claim order":         `UPDATE external_effect_claims SET sequence = 7`,
		"watcher order":       `UPDATE external_watches SET sequence = 7`,
		"claim observation":   `UPDATE external_effect_claims SET state = 'observed', observation_ref = 'migration' WHERE state = 'uncertain'`,
		"dropped watcher":     `DELETE FROM external_watches WHERE status = 'pending'`,
		"forged claim":        `INSERT INTO external_effect_claims (id, tenant_id, person_id, run_id, effect_id, target_key, state, created_at, updated_at) SELECT 'effectclaim_forged', tenant_id, person_id, id, 'forged', '*', 'uncertain', 1, 1 FROM runs LIMIT 1`,
		"resource wait queue": `UPDATE external_resource_waits SET status = 'queued'`,
		"checkpoint wait binding": `UPDATE loop_checkpoints SET wait_kind = 'approval', wait_tool_call_id = 'call_forged',
			wait_ref = (SELECT id FROM approval_requests LIMIT 1)`,
		"clarification park":        `UPDATE clarify_requests SET parked_at = 1`,
		"cancellation quote source": `UPDATE run_plan_steps SET cancellation_quote_source = 'steering:forged' WHERE status = 'cancelled'`,
		"inbound claim generation":  `UPDATE inbound_dedup SET claim_generation = 1 WHERE state = 'dispatching'`,
		"person read-only marking": `INSERT INTO mcp_read_only_marks (id, tenant_id, person_id, server_name, server_identity, tool_name,
			marked_at, marked_by_account_id, marked_channel)
			SELECT 'mcpro_forged', tenant_id, person_id, 'github', 'mcpsrv_forged', 'delete_repo', 1, id, 'cli' FROM accounts LIMIT 1`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			loadRowFixture(t, dir, releasedRowFixtures[2])
			db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			before, err := captureMigrationInvariants(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if err := version26Migration(t).Apply(ctx, db); err != nil {
				t.Fatal(err)
			}
			if err := verifyMigrationInvariants(ctx, db, before); err != nil {
				t.Fatalf("inert v26 step failed verification: %v", err)
			}
			result, err := db.ExecContext(ctx, mutation)
			if err != nil {
				t.Fatal(err)
			}
			if changed, _ := result.RowsAffected(); changed == 0 {
				t.Fatal("mutation changed nothing; the fixture does not cover it")
			}
			if err := verifyMigrationInvariants(ctx, db, before); err == nil {
				t.Fatal("a step that fabricated or rewrote durable effect state passed verification")
			}
		})
	}
}

func TestV26UpgradeChecksIntegrityBeforeTheFirstStep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loadRowFixture(t, dir, releasedRowFixtures[2])
	dbPath := filepath.Join(dir, "control.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db, dataDir: dir, events: newEventAppendBus()}
	err = store.prepareAndMigrateSchema(ctx, dir, dbPath, true, func(context.Context, *sql.DB) error {
		return errors.New("simulated corruption")
	}, orderedMigrations)
	if err == nil || !strings.Contains(err.Error(), "pre-migration integrity check") {
		t.Fatalf("corrupt v25 store error=%v", err)
	}
	var version, columns int
	if err := db.QueryRow(`SELECT MAX(version), (SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'admission_json') FROM schema_migrations`).Scan(&version, &columns); err != nil || version != 25 || columns != 0 {
		t.Fatalf("refused upgrade wrote: version=%d admission columns=%d err=%v", version, columns, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("refused upgrade left a backup directory: %v", err)
	}
}

func TestNewerSchemaIsRefusedWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'future', 1)`, CurrentControlSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "control.db")
	digest := func() [32]byte {
		t.Helper()
		data, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	before := digest()
	if _, err := OpenStore(dir); err == nil || !strings.Contains(err.Error(), "newer than this SelfMind binary") {
		t.Fatalf("future schema error=%v", err)
	}
	if digest() != before {
		t.Fatal("refusing a newer schema still modified control.db")
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("refusing a newer schema created a backup: %v", err)
	}
}
