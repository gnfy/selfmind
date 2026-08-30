package cliapp

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"

	_ "modernc.org/sqlite"
)

func TestFormatModelRoleProbesGroupsResultsAndReportsFailure(t *testing.T) {
	section, failed := formatModelRoleProbes([]appcore.ModelRoleProbe{
		{Roles: []string{"background_review", "memory_extract"}, Provider: "kimi-coding", Model: "kimi-for-coding", Latency: 1250 * time.Millisecond},
		{Roles: []string{"summarizer"}, Provider: "minimax", Model: "MiniMax-M3", Latency: time.Second, Err: errors.New("provider 403: secret-token")},
	})
	if !failed {
		t.Fatal("failed probe must make doctor fail")
	}
	for _, want := range []string{
		"OK roles=background_review,memory_extract provider=kimi-coding model=kimi-for-coding",
		"FAIL roles=summarizer provider=minimax model=MiniMax-M3",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("probe section missing %q:\n%s", want, section)
		}
	}
}

func TestFormatModelRoleProbesHandlesNoConfiguredRoles(t *testing.T) {
	section, failed := formatModelRoleProbes(nil)
	if failed || !strings.Contains(section, "no auxiliary model or explicit role overrides") {
		t.Fatalf("section=%q failed=%v", section, failed)
	}
}

func TestFormatSkillPartitionDiagnosticsIsActionable(t *testing.T) {
	healthy := formatSkillPartitionDiagnostics(tools.SkillMigrationReport{}, nil, tools.PersonPartitionCleanupReport{}, nil)
	if !strings.Contains(healthy, "healthy") {
		t.Fatalf("healthy section = %q", healthy)
	}
	pending := formatSkillPartitionDiagnostics(tools.SkillMigrationReport{
		Partitions: 2,
		Migrated:   3,
		Deduped:    1,
		Conflicts:  1,
	}, nil, tools.PersonPartitionCleanupReport{Candidates: 7, Protected: 1}, nil)
	for _, want := range []string{"partitions=2", "migrate-skills", "conflicts=1"} {
		if !strings.Contains(pending, want) {
			t.Fatalf("pending section missing %q: %s", want, pending)
		}
	}
	for _, want := range []string{"[MIGRATION]", "[CLEANUP]", "candidates=7", "cleanup-person-partitions"} {
		if !strings.Contains(pending, want) {
			t.Fatalf("pending section missing %q: %s", want, pending)
		}
	}
}

func TestSkillPresentationDiagnosticsAreFatalOnlyForBrokenReceipts(t *testing.T) {
	healthy := formatSkillPresentationDiagnostics(control.SkillPresentationDiagnostics{
		SchemaVersion: control.CurrentControlSchemaVersion, CurrentSchemaVersion: control.CurrentControlSchemaVersion,
	}, nil)
	if !strings.Contains(healthy, "status: healthy") || doctorReportHasFatalIssue(healthy) {
		t.Fatalf("healthy presentation diagnostics=%q", healthy)
	}
	leak := formatSkillPresentationDiagnostics(control.SkillPresentationDiagnostics{
		SchemaVersion: control.CurrentControlSchemaVersion, CurrentSchemaVersion: control.CurrentControlSchemaVersion,
		TerminalCandidateRefLeaks: 2,
	}, nil)
	if !strings.Contains(leak, "[WARNING]") || doctorReportHasFatalIssue(leak) {
		t.Fatalf("candidate-ref cleanup warning had wrong severity=%q", leak)
	}
	broken := formatSkillPresentationDiagnostics(control.SkillPresentationDiagnostics{
		SchemaVersion: control.CurrentControlSchemaVersion, CurrentSchemaVersion: control.CurrentControlSchemaVersion,
		InvalidDeliveryReceipts: 1,
		Issues: []control.SkillPresentationIssue{{
			ID: "skill_presentation.delivered_hash_mismatch", Code: "delivered_hash_mismatch", Severity: "fatal",
			Component: "delivery_receipt", Ref: "activation-1", Location: "run_skill_activations/activation-1/delivered_main_hash",
			Expected: "abc", Observed: "corrupt", Cause: "stored bytes do not match", Owner: "kernel skill delivery",
			Remediations: []control.SkillPresentationRemediation{{
				Description: "Restore the verified control database backup.",
				Commands:    []string{"selfmind maintenance restore-control --backup <path> --yes"},
			}},
			Verify: []string{"selfmind doctor --verbose"},
		}},
	}, nil)
	if !strings.Contains(broken, "[FATAL]") || !doctorReportHasFatalIssue(broken) {
		t.Fatalf("broken receipt was not fatal=%q", broken)
	}
	issues := collectDoctorIssues(broken, configDiagnostics{})
	if len(issues) != 1 || issues[0].Category != "SKILLS" {
		t.Fatalf("broken receipt issues=%+v", issues)
	}
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"delivered_hash_mismatch", "run_skill_activations/activation-1/delivered_main_hash", "expected: abc", "observed: corrupt", "restore-control --backup <path> --yes"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("typed Skill finding missing %q:\n%s", want, summary)
		}
	}
}

func TestDoctorReportReadFailureUsesUnableSemantics(t *testing.T) {
	if !doctorReportHasReadFailure("== Skill descriptions ==\n(error: permission denied)") {
		t.Fatal("section read error was not classified as Doctor unable")
	}
	if doctorReportHasReadFailure("== Recent logs ==\n(log unavailable: rotated)") {
		t.Fatal("optional log unavailability must not make Doctor unable")
	}
	if doctorReportHasReadFailure("== Skill presentation ==\nstatus: healthy") {
		t.Fatal("healthy report was classified as Doctor unable")
	}
}

func TestGatewayToolSchemaDoctorStateUsesExplicitDenominators(t *testing.T) {
	state := gatewayToolSchemaDoctorState(api.GatewayStatusResponse{
		ToolSchemas: api.ToolSchemaHealth{
			Active: 12, RegisteredActive: 12, Hidden: 4, ProviderVisible: 7, Repaired: 1, Quarantined: 2,
		},
	})
	for _, want := range []string{"registered_active:12", "hidden:4", "provider_visible:7", "repaired:1", "quarantined:2"} {
		if !strings.Contains(state, want) {
			t.Fatalf("Doctor tool denominator missing %q: %s", want, state)
		}
	}
}

func TestSkillPartitionDiagnosticsBlocksCleanupWhenAuthorityIsEmpty(t *testing.T) {
	section := formatSkillPartitionDiagnostics(tools.SkillMigrationReport{}, nil, tools.PersonPartitionCleanupReport{
		Root: "/tmp/assets", ControlDB: "/tmp/data/control.db", Inconclusive: true, Candidates: 3,
	}, nil)
	issues := collectDoctorIssues(section, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"authority inconclusive", "selfmind config doctor", "selfmind doctor --verbose"} {
		if len(issues) != 1 || !strings.Contains(summary, want) {
			t.Fatalf("inconclusive cleanup missing %q: issues=%v\n%s", want, issues, summary)
		}
	}
	if strings.Contains(summary, "cleanup-person-partitions --apply") {
		t.Fatalf("inconclusive cleanup offered apply:\n%s", summary)
	}
}

func TestDoctorSummaryOmitsHealthyAndHistoricalSections(t *testing.T) {
	full := `SelfMind doctor — diagnostic bundle
generated: 2026-08-18T11:34:56Z

== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:0)

== Config ==
path: /tmp/config.yaml
status: ok
model: provider=test model=test
exec_sandbox: ready

== Workspace trust ==
no migrated workspaces require trust review

== Background learning ==
queued: 0  retrying: 0  running: 0  provider-blocked: 0
status: healthy

== Recent runs ==
- [done] an old successful run (1s)

== Recent errors ==
- 08-18 10:00 [tool:terminal] an old tool failure

== Pending approvals ==
(none)

== Queued tasks ==
1. ordinary queued work

== Recent events ==
08-18 10:00:00 run.finished done

== Unconfirmed / failed pushes ==
(none)

== Cron governance ==
jobs: 2 total, 0 system, retired skill-pruner rows: 0 keyed + 0 legacy, 0 duplicate system-key group(s)

== Presence (bound accounts) ==
- cli: last seen 1s ago (attached)

== Activity by channel ==
- cli: 100 messages

== Gateway log (last 50 lines) ==
an old warning

== Skill partitions ==
- healthy: no person-partitioned skill assets need migration`

	issues := collectDoctorIssues(full, configDiagnostics{Path: "/tmp/config.yaml"})
	if len(issues) != 0 {
		t.Fatalf("healthy and historical sections must not become issues: %+v", issues)
	}
	summary := formatDoctorSummary(issues, nil, false)
	if !strings.Contains(summary, "✓ No problems found.") || !strings.Contains(summary, "selfmind doctor --verbose") {
		t.Fatalf("unexpected healthy summary:\n%s", summary)
	}
	for _, unwanted := range []string{"Recent runs", "Recent errors", "Queued tasks", "Activity by channel", "Gateway log"} {
		if strings.Contains(summary, unwanted) {
			t.Fatalf("summary retained verbose-only section %q:\n%s", unwanted, summary)
		}
	}
}

func TestDoctorSummaryKeepsOnlyActionableSectionsAndRemedies(t *testing.T) {
	full := `SelfMind doctor — diagnostic bundle

== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:0)

== Config ==
path: /tmp/config.yaml
status: upgrade available

== Workspace trust ==
- review required: repo (ws_123)
    /tmp/repo
Review each path, then run ` + "`selfmind ws trust <workspace_id>`" + ` or leave it untrusted.

== Background learning ==
queued: 0  retrying: 2  running: 0  provider-blocked: 0
status: foreground work can continue; background learning is degraded

== Recent errors ==
- 08-18 10:00 [tool:terminal] historical failure

== Pending approvals ==
- tool_call [terminal] (approval_123)

== Unconfirmed / failed pushes ==
- [weixin/sent_unconfirmed] result
Recovery: send ANY message from that chat.

== Cron governance ==
jobs: 60 total
warning: unusually many cron jobs — check for runaway system registration.

== Skill partitions ==
[MIGRATION] person-owned skill assets: partitions=1 migrate=1 dedupe=0 conflicts=0
- preview: selfmind maintenance migrate-skills
- apply after review: selfmind maintenance migrate-skills --apply`
	diag := configDiagnostics{
		Path: "/tmp/config.yaml",
		Upgrade: configUpgradeReport{
			Deprecated: []string{"tasks.maintenance_fallback_roles is deprecated"},
		},
	}

	issues := collectDoctorIssues(full, diag)
	if len(issues) != 7 {
		t.Fatalf("got %d actionable sections, want 7: %+v", len(issues), issues)
	}
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{
		"⚠ 7 categories need attention.",
		"[CONFIG] Configuration",
		"[TRUST] Workspace trust",
		"[LEARNING] Background learning",
		"[APPROVAL] Pending approvals",
		"[DELIVERY] Message delivery",
		"[SCHEDULE] Scheduler governance",
		"[SKILLS] Skill partitions",
		"Next actions (9)",
		"selfmind config upgrade",
		"selfmind ws trust <workspace_id>",
		"selfmind maintenance replay",
		"selfmind approve <token>",
		"/diag delivery",
		"warning: unusually many cron jobs",
		"selfmind maintenance migrate-skills --apply",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "historical failure") || strings.Contains(summary, "Recent errors") {
		t.Fatalf("historical errors must remain verbose-only:\n%s", summary)
	}
	actionsAt := strings.Index(summary, "Next actions")
	for _, command := range []string{"selfmind config upgrade", "selfmind ws trust <workspace_id>", "/diag delivery"} {
		if commandAt := strings.Index(summary, command); commandAt < actionsAt {
			t.Fatalf("command %q must be grouped under Next actions:\n%s", command, summary)
		}
	}
	if strings.Contains(summary, "\x1b[") {
		t.Fatalf("plain summary must not contain ANSI escapes: %q", summary)
	}
}

func TestDoctorNetworkBlockedLearningNamesRouteRecoveryWithoutReplay(t *testing.T) {
	full := `SelfMind doctor — diagnostic bundle

== Background learning ==
queued: 0  retrying: 0  running: 0  provider-blocked: 0  network-blocked: 2  prompt-revision-blocked: 0
last error: proxyconnect tcp: connection refused
status: foreground work can continue; background learning is degraded`

	issues := collectDoctorIssues(full, configDiagnostics{})
	if len(issues) != 1 {
		t.Fatalf("issues=%+v", issues)
	}
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"start Clash", "disable a stale System Proxy", "selfmind env refresh --restart", "retries automatically"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "selfmind maintenance replay") {
		t.Fatalf("network recovery must not be reduced to manual replay:\n%s", summary)
	}
}

func TestDoctorSummaryReportsGatewayAndDiagnosticReadFailures(t *testing.T) {
	full := `SelfMind doctor — diagnostic bundle

== Gateway ==
not running

== Recent events ==
(error: database is locked)`
	issues := collectDoctorIssues(full, configDiagnostics{})
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2: %v", len(issues), issues)
	}
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"selfmind gateway restart", "selfmind gateway run", "data files are readable", "doctor --verbose"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestDoctorSummarySurfacesQuarantinedExternalTools(t *testing.T) {
	full := `== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:2)`
	issues := collectDoctorIssues(full, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	if len(issues) != 1 || !strings.Contains(summary, "external MCP tool schemas") {
		t.Fatalf("quarantined tools must be actionable: %v", issues)
	}
}

func TestDoctorSummarySurfacesMCPConnectionFailures(t *testing.T) {
	full := `== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:0 mcp=connected:1,failed:1,first:github:connection refused)`
	issues := collectDoctorIssues(full, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"[GATEWAY]", "MCP server names", "selfmind config doctor", "selfmind gateway restart"} {
		if len(issues) != 1 || !strings.Contains(summary, want) {
			t.Fatalf("MCP failure missing %q: issues=%v\n%s", want, issues, summary)
		}
	}
	if gatewayDoctorHealthy("running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 mcp=connected:2,failed:0)") != true {
		t.Fatal("healthy MCP status was treated as a gateway issue")
	}
}

func TestDoctorSummarySurfacesProviderWireCatalogFailureAsFatal(t *testing.T) {
	full := `== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:37,hidden:0,provider_visible:37,repaired:0,quarantined:0 catalog=invalid:1,protocol:openai_chat,count:37,dynamic_skills:1,first_index:15,first_name:skill:proto-contract,first_code:invalid_name)`
	issues := collectDoctorIssues(full, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	if len(issues) != 1 || !doctorReportHasFatalIssue(full) {
		t.Fatalf("provider catalogue failure was not fatal: issues=%v\n%s", issues, summary)
	}
	for _, want := range []string{"skill:proto-contract", "provider-wire tool name", "doctor --probe-models --verbose"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("provider catalogue diagnosis missing %q:\n%s", want, summary)
		}
	}
	if doctorReportHasFatalIssue("== Gateway ==\nrunning (state=running pid=1 addr=127.0.0.1:8765 active_runs=0 catalog=valid:36,protocol:openai_chat,bytes:100,dynamic_skills:0,hash=ok)") {
		t.Fatal("healthy provider catalogue was treated as fatal")
	}
}

func TestDoctorCountsDynamicSkillsFromSourceIdentityAfterWireAliasing(t *testing.T) {
	line := gatewayToolCatalogDoctorState(llm.ToolCatalogPreview{
		Protocol: "openai_responses", Count: 1, WireBytes: 120, BudgetBytes: 1024,
		Names: []string{"skill_proto_contract_a1b2"},
		Entries: []llm.ToolCatalogEntry{{
			Index: 0, SourceName: "skill:proto-contract", WireName: "skill_proto_contract_a1b2",
		}},
	})
	if !strings.Contains(line, "dynamic_skills:1") {
		t.Fatalf("Doctor lost source identity after provider aliasing: %s", line)
	}
	full := "== Gateway ==\nrunning (state=running pid=1 addr=127.0.0.1:8765 active_runs=0" + line + ")"
	issues := collectDoctorIssues(full, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	for _, want := range []string{"exposure=hidden", "dynamic_skills:0", "doctor --probe-models --verbose"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("dynamic Skill repair is missing %q:\n%s", want, summary)
		}
	}
}

func TestDoctorReportsProviderSchemaBudgetWithoutMakingItContractFatal(t *testing.T) {
	full := `== Gateway ==
running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 catalog=valid:36,protocol:openai_chat,bytes:60000,schema_budget:over:49152,dynamic_skills:0,hash=ok)`
	issues := collectDoctorIssues(full, configDiagnostics{})
	summary := formatDoctorSummary(issues, nil, false)
	if len(issues) != 1 || !strings.Contains(summary, "provider-wire schema budget") {
		t.Fatalf("schema budget issue=%+v\n%s", issues, summary)
	}
	if doctorReportHasFatalIssue(full) {
		t.Fatal("cost budget warning was treated as a broken wire contract")
	}
}

func TestDoctorSummaryReportsGatewayBuildAndServiceMismatchWithCorrectRemedy(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		unwanted string
	}{
		{
			name: "client daemon build mismatch",
			body: "running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 build=mismatch:v0.1.0-beta.17 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:0)",
			want: "selfmind gateway restart",
		},
		{
			name:     "launchd installed but unloaded",
			body:     "running (state=running pid=123 addr=127.0.0.1:8765 active_runs=0 tools=registered_active:10,hidden:0,provider_visible:10,repaired:0,quarantined:0) launchd=installed-not-loaded",
			want:     "selfmind gateway service install",
			unwanted: "external MCP tool schemas",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full := "== Gateway ==\n" + tt.body
			issues := collectDoctorIssues(full, configDiagnostics{})
			summary := formatDoctorSummary(issues, nil, false)
			if len(issues) != 1 || !strings.Contains(summary, tt.want) {
				t.Fatalf("gateway issue missing remedy %q: %v", tt.want, issues)
			}
			if tt.unwanted != "" && strings.Contains(summary, tt.unwanted) {
				t.Fatalf("gateway issue used wrong remedy %q: %s", tt.unwanted, summary)
			}
		})
	}
}

func TestDoctorSummaryBoundsPendingSessionPushesAndAddsRecovery(t *testing.T) {
	full := `== Unconfirmed / failed pushes ==
- [weixin/pending_session] one
- [weixin/pending_session] two
- [weixin/pending_session] three
- [weixin/pending_session] four
- [weixin/pending_session] five`
	issues := collectDoctorIssues(full, configDiagnostics{})
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1: %v", len(issues), issues)
	}
	for _, want := range []string{"… 2 more", "Send a fresh message", "/diag delivery"} {
		summary := formatDoctorSummary(issues, nil, false)
		if !strings.Contains(summary, want) {
			t.Fatalf("delivery issue missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(issues[0].Details, "four") || strings.Contains(issues[0].Details, "five") {
		t.Fatalf("compact delivery section exceeded its item bound:\n%s", issues[0].Details)
	}
}

func TestDoctorSummaryColorPolicyUsesSemanticCategoriesAndHonorsNoColor(t *testing.T) {
	issue := doctorIssue{
		Category: "CONFIG",
		Title:    "Configuration",
		Details:  "upgrade available",
		Actions: []doctorAction{{
			Description: "Upgrade the configuration.",
			Commands:    []string{"selfmind config upgrade"},
		}},
	}
	for category, want := range map[string]string{
		"CONFIG":   "#b9824f",
		"DELIVERY": "#ff6b6b",
		"TRUST":    "#2f9de8",
	} {
		if got := doctorCategoryColor(category); got != want {
			t.Fatalf("category %s color = %q, want TUI palette color %q", category, got, want)
		}
	}
	colored := formatDoctorSummary([]doctorIssue{issue}, nil, true)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("color-enabled summary must contain ANSI styling: %q", colored)
	}
	plain := formatDoctorSummary([]doctorIssue{issue}, nil, false)
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("plain summary must not contain ANSI styling: %q", plain)
	}

	t.Setenv("NO_COLOR", "1")
	app := &App{interactive: true}
	if app.doctorColorEnabled() {
		t.Fatal("NO_COLOR must disable doctor styling")
	}
}

func TestCronGovernanceIgnoresHistoricalUserReservedPrefix(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT, system_key TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, system_key) VALUES
		('skill-pruner-user-report', '15 9 * * 1', 'send report', 'default', 'weixin', '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	section := cronGovernanceSection(context.Background(), dir)
	if !strings.Contains(section, "retired skill-pruner rows: 0 keyed + 0 legacy") {
		t.Fatalf("unexpected cron governance section:\n%s", section)
	}
	if strings.Contains(section, "obsolete Skill metric-pruner") {
		t.Fatalf("historical user job must not trigger a system warning:\n%s", section)
	}
}

func TestCronGovernanceReportsRetiredKeyedSkillPruner(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT, system_key TEXT NOT NULL DEFAULT '');
		INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, system_key)
		VALUES ('skill-pruner-default', '0 3 * * *', 'skill_prune:default', 'default', 'cli', 'skill-pruner:default')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	section := cronGovernanceSection(context.Background(), dir)
	if !strings.Contains(section, "retired skill-pruner rows: 1 keyed + 0 legacy") ||
		!strings.Contains(section, "obsolete Skill metric-pruner") {
		t.Fatalf("retired keyed row was not reported:\n%s", section)
	}
}

func TestCronGovernanceReportsLegacySystemShape(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT, system_key TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, system_key)
		VALUES ('skill-pruner-person_abc', '0 3 * * *', 'skill_prune:person_abc', 'person_abc', 'cli', '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	section := cronGovernanceSection(context.Background(), dir)
	if !strings.Contains(section, "retired skill-pruner rows: 0 keyed + 1 legacy") ||
		!strings.Contains(section, "obsolete Skill metric-pruner") {
		t.Fatalf("legacy system-shaped row was not reported:\n%s", section)
	}
}
