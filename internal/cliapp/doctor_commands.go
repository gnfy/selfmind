package cliapp

// `selfmind doctor` — self-serve diagnostics. The default view reports only
// current problems and their next actions. `--verbose` and `--out` retain the
// redacted observability bundle used for deeper inspection and support. It is
// read-only, and it works whether or not the daemon is up: live gateway status
// when reachable, else durable state in control.db and the on-disk log.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
	uicommon "selfmind/internal/ui/common"
)

const (
	doctorRecentRuns   = 10
	doctorRecentErrors = 12
	doctorRecentEvents = 40
	doctorMaxPushes    = 10
	doctorLogLines     = 50
	// presenceRecentWindow labels an account as recently active in the presence
	// snapshot. Matches the daemon's presence TTL intent (durable last_seen_at is
	// the only cross-process signal a CLI-side doctor can read).
	presenceRecentWindow = 90 * time.Second
)

func (a *App) runDoctorCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "doctor" {
		return false, 0
	}
	return true, a.doctor(a.args[2:])
}

func (a *App) doctor(args []string) int {
	fs := flag.NewFlagSet("selfmind doctor", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	outPath := fs.String("out", "", "write the full diagnostic bundle to a file instead of stdout")
	verbose := fs.Bool("verbose", false, "print the full diagnostic bundle")
	probeModels := fs.Bool("probe-models", false, "send one bounded live request per unique configured role model")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dataDir := a.gatewayDataDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		fmt.Fprintf(a.stderr, "doctor: cannot open control store: %v\n", err)
		return 2
	}
	defer store.Close()

	ctx, cancel := contextWithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	// Diagnostics are read-only. Resolve the same platform identity used by
	// normal CLI requests, but never create a phantom person merely by running
	// doctor on a fresh or differently configured installation.
	identity, err := resolveDoctorIdentity(ctx, store, a.tenantID(), platformUserID())
	if err != nil {
		fmt.Fprintf(a.stderr, "doctor: cannot resolve identity: %v\n", err)
		return 2
	}

	configDiagnostics := a.collectConfigDiagnostics()
	promptSection := ""
	if cfg, loadErr := config.LoadConfig(config.Options{Path: a.configPath}); loadErr == nil {
		promptSection = buildPromptWorkspaceDoctorSection(store, dataDir, cfg)
	}
	fullReport := buildDoctorReport(ctx, store, identity, dataDir, a.gatewayStatusLine(), configDiagnostics.section(), promptSection, doctorLogLines)
	if cfg, loadErr := config.LoadConfig(config.Options{Path: a.configPath}); loadErr == nil {
		storage, storageErr := appcore.ResolveSkillStorage(cfg)
		if storageErr != nil {
			fullReport += "\n\n" + formatSkillPartitionDiagnostics(tools.SkillMigrationReport{}, storageErr, tools.PersonPartitionCleanupReport{}, nil)
		} else {
			skillRoot := storage.BaseDir()
			descriptionIssues, descriptionErr := tools.InspectSkillDescriptionsForTenant(a.tenantID(),
				tools.WithSkillStorage(map[string]interface{}{"_tenant_id": a.tenantID()}, storage))
			fullReport += "\n\n" + formatSkillDescriptionDiagnostics(descriptionIssues, descriptionErr)
			frontMatterIssues, frontMatterErr := tools.InspectSkillFrontMatterForTenant(a.tenantID(),
				tools.WithSkillStorage(map[string]interface{}{"_tenant_id": a.tenantID()}, storage))
			fullReport += "\n\n" + formatSkillFrontMatterDiagnostics(frontMatterIssues, frontMatterErr)
			migration, migrationErr := tools.MigratePersonSkillsToControl(skillRoot, a.tenantID(), false, tools.DefaultSkillMigrationGrace)
			knownPersons, knownErr := store.ListPersonIDs(ctx)
			cleanup := tools.PersonPartitionCleanupReport{Root: skillRoot}
			cleanupErr := knownErr
			if cleanupErr == nil {
				cleanup, cleanupErr = tools.CleanupOrphanPersonPartitions(skillRoot, knownPersons, false)
				cleanup.ControlDB = filepath.Join(dataDir, "control.db")
			}
			fullReport += "\n\n" + formatSkillPartitionDiagnostics(migration, migrationErr, cleanup, cleanupErr)
		}
	}
	doctorUnable := doctorReportHasReadFailure(fullReport)
	doctorFatal := doctorReportHasFatalIssue(fullReport)
	probeSection := ""
	probeFailed := false
	if *probeModels {
		gatewayProbeSection, gatewayProbeFailed := a.probeGatewayToolCatalog()
		cfg, loadErr := config.LoadConfig(config.Options{Path: a.configPath})
		if loadErr != nil {
			probeSection = strings.TrimSpace(gatewayProbeSection + "\n\n== Model role probes ==\n(error: " + oneLine(tools.RedactSensitive(loadErr.Error()), 180) + ")")
			probeFailed = true
		} else {
			probeCtx, probeCancel := context.WithTimeout(a.ctx, 90*time.Second)
			probes := appcore.ProbeConfiguredModelRoles(probeCtx, cfg)
			probeCancel()
			section, failed := formatModelRoleProbes(probes)
			probeSection = strings.TrimSpace(gatewayProbeSection + "\n\n" + section)
			probeFailed = gatewayProbeFailed || failed
		}
	}

	report := fullReport
	if !*verbose && strings.TrimSpace(*outPath) == "" {
		issues := collectDoctorIssues(fullReport, configDiagnostics)
		var informational []string
		if probeFailed {
			issues = append(issues, doctorIssue{
				Category: "MODEL",
				Title:    "Model role probes",
				Details:  doctorRenderedSectionBody(probeSection),
				Actions: []doctorAction{{
					Description: "Check the configured model routes and live provider contract.",
					Commands:    []string{"selfmind model"},
				}},
			})
		} else if probeSection != "" {
			informational = append(informational, probeSection)
		}
		report = formatDoctorSummary(issues, informational, a.doctorColorEnabled(), formatControlSchemaLine(store.SchemaStatus()))
	} else if probeSection != "" {
		report += "\n\n" + probeSection
	}

	if strings.TrimSpace(*outPath) != "" {
		if err := os.WriteFile(*outPath, []byte(report), 0600); err != nil {
			fmt.Fprintf(a.stderr, "doctor: cannot write %s: %v\n", *outPath, err)
			return 2
		}
		fmt.Fprintf(a.stdout, "Diagnostic bundle written to %s\n", *outPath)
		if doctorUnable {
			return 2
		}
		if probeFailed || doctorFatal {
			return 1
		}
		return 0
	}
	fmt.Fprintln(a.stdout, report)
	if doctorUnable {
		return 2
	}
	if probeFailed || doctorFatal {
		return 1
	}
	return 0
}

func (a *App) probeGatewayToolCatalog() (string, bool) {
	ctx, cancel := context.WithTimeout(a.ctx, 45*time.Second)
	defer cancel()
	data, status, err := gatewayrt.RequestToolCatalogProbe(ctx, a.gatewayURL(), a.gatewayDataDir())
	if err != nil {
		return "== Primary provider tool-catalog probe ==\nFAIL error=" + oneLine(tools.RedactSensitive(err.Error()), 180), true
	}
	if status >= http.StatusBadRequest {
		return fmt.Sprintf("== Primary provider tool-catalog probe ==\nFAIL status=%d error=%s", status, oneLine(tools.RedactSensitive(string(data)), 180)), true
	}
	var probe api.ProviderToolCatalogProbeResponse
	if err := json.Unmarshal(data, &probe); err != nil {
		return "== Primary provider tool-catalog probe ==\nFAIL error=invalid daemon response", true
	}
	state := "OK"
	if !probe.OK {
		state = "FAIL"
	}
	line := fmt.Sprintf("== Primary provider tool-catalog probe ==\n%s provider=%s model=%s protocol=%s tools=%d bytes=%d latency=%dms hash=%s",
		state, valueOrUnknown(probe.Provider), valueOrUnknown(probe.Model), valueOrUnknown(probe.Catalog.Protocol),
		probe.Catalog.Count, probe.Catalog.WireBytes, probe.LatencyMS, valueOrUnknown(probe.Catalog.Hash))
	if probe.Error != "" {
		line += " error=" + oneLine(tools.RedactSensitive(probe.Error), 180)
	}
	return line, !probe.OK
}

type doctorReportSection struct {
	Name string
	Body string
}

type doctorAction struct {
	Description string
	Commands    []string
}

type doctorIssue struct {
	Category string
	Title    string
	Details  string
	Actions  []doctorAction
}

// collectDoctorIssues turns the full diagnostic bundle into typed daily
// findings. Historical runs, errors, events, activity and logs remain useful
// evidence, but they are not current faults merely because they exist.
func collectDoctorIssues(report string, configDiagnostics configDiagnostics) []doctorIssue {
	var issues []doctorIssue
	for _, section := range parseDoctorReportSections(report) {
		body := strings.TrimSpace(section.Body)
		switch section.Name {
		case "Gateway":
			if !gatewayDoctorHealthy(body) {
				issues = append(issues, doctorIssue{
					Category: "GATEWAY",
					Title:    "Gateway runtime",
					Details:  body,
					Actions:  gatewayDoctorActions(body),
				})
			}
		case "Config":
			if issue, ok := configDoctorIssue(configDiagnostics); ok {
				issues = append(issues, issue)
			}
		case "Prompt workspace":
			if !strings.Contains(body, "status: healthy") {
				issues = append(issues, doctorIssue{
					Category: "PROMPT",
					Title:    "Prompt workspace",
					Details:  body,
					Actions: []doctorAction{
						{
							Description: "Validate the active prompt workspace and inspect the reported role file.",
							Commands:    []string{"selfmind prompt validate", "selfmind prompt show <role>"},
						},
						{
							Description: "After correcting the prompt file or its permissions, restart the gateway to activate it.",
							Commands:    []string{"selfmind gateway restart"},
						},
					},
				})
			}
		case "Workspace trust":
			if !strings.Contains(body, "no migrated workspaces require trust review") {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("TRUST", "Workspace trust", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "TRUST",
						Title:    "Workspace trust",
						Details:  doctorWithoutLines(body, "Review each path"),
						Actions: []doctorAction{{
							Description: "Review each path, then trust only the workspaces you approve.",
							Commands:    []string{"selfmind ws trust <workspace_id>"},
						}},
					})
				}
			}
		case "Background learning":
			if !strings.Contains(body, "status: healthy") {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("LEARNING", "Background learning", body))
				} else {
					var actions []doctorAction
					if !strings.Contains(body, "retrying: 0") || !strings.Contains(body, "provider-blocked: 0") {
						actions = append(actions, doctorAction{
							Description: "Requeue maintenance work that exhausted its retry limit.",
							Commands:    []string{"selfmind maintenance replay"},
						})
					}
					if strings.Contains(body, "network-blocked:") && !strings.Contains(body, "network-blocked: 0") {
						actions = append(actions, doctorAction{
							Description: "Restore the selected network route: on macOS start Clash or disable a stale System Proxy; on Linux restore the proxy/TUN or refresh the managed environment. SelfMind retries automatically after a detected route change.",
							Commands:    []string{"selfmind env refresh --restart", "selfmind doctor"},
						})
					}
					if strings.Contains(body, "prompt-revision-blocked:") && !strings.Contains(body, "prompt-revision-blocked: 0") {
						actions = append(actions, doctorAction{
							Description: "Restore the missing pinned prompt revision from backup, then explicitly replay the paused maintenance work; do not replace it with the current prompt.",
							Commands:    []string{"selfmind prompt list", "selfmind maintenance replay --limit 10", "selfmind doctor --verbose"},
						})
					}
					issues = append(issues, doctorIssue{
						Category: "LEARNING",
						Title:    "Background learning",
						Details:  body,
						Actions:  actions,
					})
				}
			}
		case "Skill presentation":
			if !strings.Contains(body, "status: healthy") {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("SKILLS", "Skill presentation contract", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "SKILLS",
						Title:    "Skill presentation contract",
						Details:  body,
						Actions:  skillPresentationDoctorActions(body),
					})
				}
			}
		case "Skill descriptions":
			if !strings.Contains(body, "status: healthy") {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("SKILLS", "Skill description metadata", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "SKILLS", Title: "Skill description metadata", Details: body,
						Actions: skillPresentationDoctorActions(body),
					})
				}
			}
		case "Pending approvals":
			if body != "(none)" {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("APPROVAL", "Pending approvals", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "APPROVAL",
						Title:    "Pending approvals",
						Details:  limitDoctorListItems(body, 5),
						Actions: []doctorAction{{
							Description: "Review every pending request, then approve or reject it explicitly.",
							Commands: []string{
								"selfmind approvals",
								"selfmind approve <token>",
								"selfmind reject <token>",
							},
						}},
					})
				}
			}
		case "Unconfirmed / failed pushes":
			if body != "(none)" {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("DELIVERY", "Message delivery", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "DELIVERY",
						Title:    "Message delivery",
						Details:  limitDoctorListItems(doctorListItems(body), 3),
						Actions: []doctorAction{{
							Description: "Send a fresh message in each affected IM chat, then open its recovery controls.",
							Commands:    []string{"/diag delivery"},
						}},
					})
				}
			}
		case "Cron governance":
			if strings.Contains(body, "warning:") || doctorSectionReadFailed(body) {
				if doctorSectionReadFailed(body) {
					issues = append(issues, doctorReadIssue("SCHEDULE", "Scheduler governance", body))
				} else {
					issues = append(issues, doctorIssue{
						Category: "SCHEDULE",
						Title:    "Scheduler governance",
						Details:  body,
						Actions: []doctorAction{
							{Description: "Restart the gateway to run scheduler governance repair.", Commands: []string{"selfmind gateway restart"}},
							{Description: "If the warning remains, inspect the full diagnostic bundle.", Commands: []string{"selfmind doctor --verbose"}},
						},
					})
				}
			}
		case "Person asset partitions":
			if !strings.Contains(body, "healthy:") {
				if strings.Contains(body, "inspect failed:") {
					issues = append(issues, doctorReadIssue("SKILLS", "Person asset partitions", body))
				} else {
					actions := []doctorAction{}
					if strings.Contains(body, "[MIGRATION]") {
						actions = append(actions,
							doctorAction{Description: "Preview the skill partition migration.", Commands: []string{"selfmind maintenance migrate-skills"}},
							doctorAction{Description: "Apply it after reviewing the preview.", Commands: []string{"selfmind maintenance migrate-skills --apply"}},
						)
					}
					if strings.Contains(body, "inconclusive") {
						actions = append(actions, doctorAction{
							Description: "Verify that the configured Skill root and control database belong to the same installation.",
							Commands:    []string{"selfmind config doctor", "selfmind doctor --verbose"},
						})
					} else if strings.Contains(body, "[CLEANUP]") {
						actions = append(actions,
							doctorAction{Description: "Preview orphan person asset partitions.", Commands: []string{"selfmind maintenance cleanup-person-partitions"}},
							doctorAction{Description: "Stop the gateway, then move reviewed orphans into recoverable quarantine.", Commands: []string{"selfmind gateway stop", "selfmind maintenance cleanup-person-partitions --apply", "selfmind gateway start"}},
						)
					}
					issues = append(issues, doctorIssue{
						Category: "SKILLS",
						Title:    "Person asset partitions",
						Details:  doctorWithoutLines(body, "- preview:", "- apply after review:", "- quarantine after review:"),
						Actions:  actions,
					})
				}
			}
		default:
			// A failed diagnostic query is itself actionable even when the
			// successfully-read contents of this section are verbose-only.
			if doctorSectionReadFailed(body) {
				issues = append(issues, doctorReadIssue("DATA", section.Name+" diagnostics", body))
			}
		}
	}
	return issues
}

func parseDoctorReportSections(report string) []doctorReportSection {
	var sections []doctorReportSection
	var current *doctorReportSection
	flush := func() {
		if current == nil {
			return
		}
		current.Body = strings.TrimSpace(current.Body)
		sections = append(sections, *current)
		current = nil
	}
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "== ") && strings.HasSuffix(line, " ==") {
			flush()
			current = &doctorReportSection{Name: strings.TrimSuffix(strings.TrimPrefix(line, "== "), " ==")}
			continue
		}
		if current == nil {
			continue
		}
		if current.Body != "" {
			current.Body += "\n"
		}
		current.Body += line
	}
	flush()
	return sections
}

func gatewayDoctorHealthy(body string) bool {
	if !strings.HasPrefix(strings.TrimSpace(body), "running (state=running ") {
		return false
	}
	if backgroundServiceDoctorUnhealthy(body) {
		return false
	}
	if strings.Contains(body, "build=mismatch:") {
		return false
	}
	if strings.Contains(body, "quarantined:") && !strings.Contains(body, "quarantined:0") {
		return false
	}
	if strings.Contains(body, "mcp=connected:") && !strings.Contains(body, ",failed:0") {
		return false
	}
	if strings.Contains(body, "catalog=invalid:") || strings.Contains(body, "schema_budget:over:") ||
		(strings.Contains(body, "dynamic_skills:") && !strings.Contains(body, "dynamic_skills:0")) {
		return false
	}
	return true
}

func gatewayDoctorActions(body string) []doctorAction {
	if strings.Contains(body, "dynamic_skills:") && !strings.Contains(body, "dynamic_skills:0") {
		return []doctorAction{
			{Description: "The reported first_source/provider-wire tool name identifies a per-Skill registry address. Keep that registration for compatibility but set exposure=hidden so it cannot enter provider tools.", Commands: []string{"selfmind doctor --verbose --out <path>"}},
			{Description: "Install the corrected binary, restart the daemon, then prove the real primary-provider catalogue contains dynamic_skills:0.", Commands: []string{"selfmind gateway restart", "selfmind doctor --probe-models --verbose"}},
		}
	}
	if strings.Contains(body, "catalog=invalid:") {
		return []doctorAction{
			{Description: "Use first_source to locate the registered owner and first_name to inspect the exact provider-wire tool name spelling; correct the owner or shared adapter normalization, not the Doctor output.", Commands: []string{"selfmind doctor --verbose --out <path>"}},
			{Description: "Restart on the corrected binary and run the live primary-provider catalogue probe.", Commands: []string{"selfmind gateway restart", "selfmind doctor --probe-models --verbose"}},
		}
	}
	if strings.Contains(body, "schema_budget:over:") {
		return []doctorAction{
			{Description: "Inspect the provider-wire schema budget and the largest remaining generic tools.", Commands: []string{"selfmind doctor --verbose"}},
			{Description: "Reduce or defer reviewed generic schemas without hiding required Skill discovery metadata.", Commands: []string{"selfmind doctor --probe-models --verbose"}},
		}
	}
	if strings.Contains(body, "mcp=connected:") && !strings.Contains(body, ",failed:0") {
		return []doctorAction{
			{Description: "Inspect MCP server names, transports, credentials, and tool-schema collisions.", Commands: []string{"selfmind config doctor", "selfmind doctor --verbose"}},
			{Description: "After correcting the MCP configuration or server, restart the gateway.", Commands: []string{"selfmind gateway restart"}},
		}
	}
	if strings.Contains(body, "quarantined:") && !strings.Contains(body, "quarantined:0") {
		return []doctorAction{
			{Description: "Inspect the quarantined external MCP tool schemas.", Commands: []string{"selfmind doctor --verbose"}},
			{Description: "After correcting or removing invalid schemas, restart the gateway.", Commands: []string{"selfmind gateway restart"}},
		}
	}
	if backgroundServiceDoctorUnhealthy(body) {
		return []doctorAction{
			{Description: "Install or refresh the operating-system background service.", Commands: []string{"selfmind gateway service install"}},
			{Description: "Restart the gateway through the service.", Commands: []string{"selfmind gateway restart"}},
		}
	}
	return []doctorAction{
		{Description: "Restart the gateway.", Commands: []string{"selfmind gateway restart"}},
		{Description: "If it still fails, run it in the foreground and inspect the error.", Commands: []string{"selfmind gateway run"}},
	}
}

func backgroundServiceDoctorUnhealthy(body string) bool {
	return strings.Contains(body, "launchd=error") ||
		strings.Contains(body, "launchd=installed-not-loaded") ||
		strings.Contains(body, "systemd=error") ||
		strings.Contains(body, "systemd=installed-not-running")
}

func skillPresentationDoctorActions(body string) []doctorAction {
	var actions []doctorAction
	var current *doctorAction
	flush := func() {
		if current == nil {
			return
		}
		actions = append(actions, *current)
		current = nil
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "repair: "):
			flush()
			current = &doctorAction{Description: strings.TrimSpace(strings.TrimPrefix(trimmed, "repair: "))}
		case strings.HasPrefix(trimmed, "command: ") && current != nil:
			current.Commands = append(current.Commands, strings.TrimSpace(strings.TrimPrefix(trimmed, "command: ")))
		}
	}
	flush()
	if len(actions) == 0 {
		return []doctorAction{{
			Description: "Preserve a full redacted finding bundle and inspect the exact location, expected value, and observed value before repair.",
			Commands:    []string{"selfmind doctor --verbose --out <path>"},
		}}
	}
	return actions
}

func doctorReportHasFatalIssue(report string) bool {
	for _, section := range parseDoctorReportSections(report) {
		body := strings.TrimSpace(section.Body)
		switch section.Name {
		case "Gateway":
			if strings.Contains(body, "catalog=invalid:") ||
				(strings.Contains(body, "dynamic_skills:") && !strings.Contains(body, "dynamic_skills:0")) {
				return true
			}
		case "Skill presentation":
			if strings.Contains(body, "[FATAL]") {
				return true
			}
		}
	}
	return false
}

// Exit code 2 means Doctor could not evaluate one or more diagnostic
// contracts. Historical log unavailability is intentionally excluded: logs
// are supporting evidence, while an explicit section read error makes a clean
// bill of health impossible.
func doctorReportHasReadFailure(report string) bool {
	for _, section := range parseDoctorReportSections(report) {
		if strings.HasPrefix(strings.TrimSpace(section.Body), "(error:") {
			return true
		}
	}
	return false
}

func configDoctorIssue(d configDiagnostics) (doctorIssue, bool) {
	if !d.Missing && d.ReadError == "" && d.ParseError == "" && d.LoadError == "" &&
		!d.Upgrade.hasChanges() && d.ModelError == "" && d.SandboxWarning == "" {
		return doctorIssue{}, false
	}
	issue := doctorIssue{Category: "CONFIG", Title: "Configuration"}
	var body strings.Builder
	fmt.Fprintf(&body, "path: %s\n", d.Path)
	switch {
	case d.Missing:
		body.WriteString("status: missing")
		issue.Actions = append(issue.Actions, doctorAction{Description: "Create the configuration through guided setup.", Commands: []string{"selfmind setup"}})
	case d.ReadError != "":
		fmt.Fprintf(&body, "status: unreadable (%s)", oneLine(tools.RedactSensitive(d.ReadError), 160))
		issue.Actions = append(issue.Actions, doctorAction{Description: "Repair the config file permissions, then rerun diagnostics.", Commands: []string{"selfmind doctor"}})
	case d.ParseError != "":
		fmt.Fprintf(&body, "status: invalid YAML (%s)", oneLine(tools.RedactSensitive(d.ParseError), 160))
		issue.Actions = append(issue.Actions, doctorAction{Description: "Correct the YAML, then validate the configuration.", Commands: []string{"selfmind config doctor"}})
	case d.LoadError != "":
		fmt.Fprintf(&body, "status: load error (%s)", oneLine(tools.RedactSensitive(d.LoadError), 160))
		issue.Actions = append(issue.Actions, doctorAction{Description: "Correct the invalid setting, then validate the configuration.", Commands: []string{"selfmind config doctor"}})
	default:
		if d.Upgrade.hasChanges() {
			fmt.Fprintf(&body, "upgrade available: %d missing defaults, %d legacy, %d deprecated\n", len(d.Upgrade.Missing), len(d.Upgrade.Legacy), len(d.Upgrade.Deprecated))
			for _, item := range d.Upgrade.Legacy {
				fmt.Fprintf(&body, "  - %s\n", item)
			}
			for _, item := range d.Upgrade.Deprecated {
				fmt.Fprintf(&body, "  - %s\n", item)
			}
			issue.Actions = append(issue.Actions, doctorAction{Description: "Upgrade the configuration while preserving existing values.", Commands: []string{"selfmind config upgrade"}})
		}
		if d.ModelError != "" {
			fmt.Fprintf(&body, "model: not ready (%s)\n", oneLine(tools.RedactSensitive(d.ModelError), 160))
			issue.Actions = append(issue.Actions, doctorAction{Description: "Open Model Manager; each completed selection is checked automatically.", Commands: []string{"selfmind model", "selfmind setup"}})
		}
		if d.SandboxWarning != "" {
			fmt.Fprintf(&body, "exec_sandbox: %s\n", d.SandboxLine)
			issue.Actions = append(issue.Actions, doctorAction{Description: "Make bubblewrap and unprivileged user namespaces available, then restart SelfMind.", Commands: []string{"selfmind gateway restart"}})
		}
	}
	issue.Details = strings.TrimSpace(body.String())
	return issue, true
}

func doctorSectionReadFailed(body string) bool {
	body = strings.TrimSpace(body)
	return strings.HasPrefix(body, "(error:") || strings.HasPrefix(body, "(log unavailable:")
}

func doctorReadIssue(category, title, body string) doctorIssue {
	return doctorIssue{
		Category: category,
		Title:    title,
		Details:  body,
		Actions: []doctorAction{{
			Description: "Verify the SelfMind data files are readable, then rerun full diagnostics.",
			Commands:    []string{"selfmind doctor --verbose"},
		}},
	}
}

func doctorWithoutLines(body string, prefixes ...string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		drop := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func doctorListItems(body string) string {
	var items []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "- ") {
			items = append(items, line)
		}
	}
	return strings.Join(items, "\n")
}

func limitDoctorListItems(body string, maxItems int) string {
	if maxItems <= 0 {
		return body
	}
	var items, details []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "- ") {
			items = append(items, line)
			continue
		}
		details = append(details, line)
	}
	if len(items) <= maxItems {
		return body
	}
	lines := append([]string(nil), items[:maxItems]...)
	lines = append(lines, fmt.Sprintf("… %d more; run `selfmind doctor --verbose` to list them.", len(items)-maxItems))
	lines = append(lines, details...)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func doctorRenderedSectionBody(section string) string {
	parsed := parseDoctorReportSections(section)
	if len(parsed) == 0 {
		return strings.TrimSpace(section)
	}
	return strings.TrimSpace(parsed[0].Body)
}

func (a *App) doctorColorEnabled() bool {
	if !a.interactive {
		return false
	}
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") || strings.TrimSpace(os.Getenv("CLICOLOR")) == "0" {
		return false
	}
	return true
}

// formatDoctorSummary renders the default problem-only view. facts are short
// always-shown state lines (such as the control schema version) that are
// neither problems nor verbose-only detail.
func formatDoctorSummary(issues []doctorIssue, informational []string, colorEnabled bool, facts ...string) string {
	var sb strings.Builder
	sb.WriteString(doctorPaint(colorEnabled, "SelfMind doctor", uicommon.PaletteText, true))
	sb.WriteString("\n")
	if len(issues) == 0 {
		sb.WriteString(doctorPaint(colorEnabled, "✓ No problems found.", uicommon.PaletteGreen, true))
	} else if len(issues) == 1 {
		sb.WriteString(doctorPaint(colorEnabled, "⚠ 1 category needs attention.", uicommon.PaletteAmber, true))
	} else {
		sb.WriteString(doctorPaint(colorEnabled, fmt.Sprintf("⚠ %d categories need attention.", len(issues)), uicommon.PaletteAmber, true))
	}
	for _, fact := range facts {
		sb.WriteString("\n")
		sb.WriteString(doctorPaint(colorEnabled, fact, uicommon.PaletteSubtle, false))
	}
	for _, issue := range issues {
		sb.WriteString("\n\n")
		writeDoctorFinding(&sb, issue.Category, issue.Title, issue.Details, colorEnabled)
	}
	for _, section := range informational {
		parsed := parseDoctorReportSections(section)
		if len(parsed) == 0 {
			continue
		}
		sb.WriteString("\n\n")
		writeDoctorFinding(&sb, "MODEL", parsed[0].Name, parsed[0].Body, colorEnabled)
	}
	if actionCount := doctorActionCount(issues); actionCount > 0 {
		sb.WriteString("\n\n")
		sb.WriteString(doctorPaint(colorEnabled, fmt.Sprintf("Next actions (%d)", actionCount), uicommon.PaletteText, true))
		index := 0
		for _, issue := range issues {
			for _, action := range issue.Actions {
				index++
				sb.WriteString("\n\n")
				sb.WriteString(doctorPaint(colorEnabled, fmt.Sprintf("%d.", index), uicommon.PaletteAmber, true))
				sb.WriteString(" ")
				sb.WriteString(doctorCategoryTag(issue.Category, colorEnabled))
				sb.WriteString(" ")
				sb.WriteString(doctorPaint(colorEnabled, action.Description, uicommon.PaletteText, false))
				for _, command := range action.Commands {
					sb.WriteString("\n   ")
					sb.WriteString(doctorPaint(colorEnabled, "→ "+command, uicommon.PaletteBlue, true))
				}
			}
		}
	}
	sb.WriteString("\n\n")
	sb.WriteString(doctorPaint(colorEnabled, "More detail: ", uicommon.PaletteSubtle, false))
	sb.WriteString(doctorPaint(colorEnabled, "selfmind doctor --verbose", uicommon.PaletteBlue, true))
	return sb.String()
}

func writeDoctorFinding(sb *strings.Builder, category, title, details string, colorEnabled bool) {
	sb.WriteString(doctorCategoryTag(category, colorEnabled))
	sb.WriteString(" ")
	sb.WriteString(doctorPaint(colorEnabled, title, uicommon.PaletteText, true))
	for _, line := range strings.Split(strings.TrimSpace(details), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		sb.WriteString("\n  ")
		sb.WriteString(doctorPaint(colorEnabled, line, uicommon.PaletteMuted, false))
	}
}

func doctorCategoryTag(category string, colorEnabled bool) string {
	return doctorPaint(colorEnabled, "["+category+"]", doctorCategoryColor(category), true)
}

func doctorCategoryColor(category string) string {
	switch category {
	case "GATEWAY", "DELIVERY", "DATA":
		return uicommon.PaletteRed
	case "CONFIG", "LEARNING", "APPROVAL", "SCHEDULE":
		return uicommon.PaletteAmber
	default:
		return uicommon.PaletteBlue
	}
}

func doctorPaint(enabled bool, text, color string, bold bool) string {
	if !enabled {
		return text
	}
	color = strings.TrimPrefix(strings.TrimSpace(color), "#")
	if len(color) != 6 {
		return text
	}
	red, redErr := strconv.ParseUint(color[0:2], 16, 8)
	green, greenErr := strconv.ParseUint(color[2:4], 16, 8)
	blue, blueErr := strconv.ParseUint(color[4:6], 16, 8)
	if redErr != nil || greenErr != nil || blueErr != nil {
		return text
	}
	weight := ""
	if bold {
		weight = "1;"
	}
	return fmt.Sprintf("\x1b[%s38;2;%d;%d;%dm%s\x1b[0m", weight, red, green, blue, text)
}

func doctorActionCount(issues []doctorIssue) int {
	count := 0
	for _, issue := range issues {
		count += len(issue.Actions)
	}
	return count
}

func formatSkillPartitionDiagnostics(report tools.SkillMigrationReport, err error, cleanup tools.PersonPartitionCleanupReport, cleanupErr error) string {
	var sb strings.Builder
	sb.WriteString("== Person asset partitions ==\n")
	if err != nil {
		fmt.Fprintf(&sb, "- migration inspect failed: %s", oneLine(tools.RedactSensitive(err.Error()), 180))
		return sb.String()
	}
	if cleanupErr != nil {
		fmt.Fprintf(&sb, "- cleanup inspect failed: %s", oneLine(tools.RedactSensitive(cleanupErr.Error()), 180))
		return sb.String()
	}
	if report.Partitions == 0 && cleanup.Candidates == 0 {
		sb.WriteString("- healthy: skill assets use the control partition and no orphan person asset partitions were found")
		return sb.String()
	}
	if report.Partitions > 0 {
		fmt.Fprintf(&sb, "[MIGRATION] person-owned skill assets: partitions=%d migrate=%d dedupe=%d conflicts=%d\n", report.Partitions, report.Migrated, report.Deduped, report.Conflicts)
		sb.WriteString("- preview: selfmind maintenance migrate-skills\n")
		sb.WriteString("- apply after review: selfmind maintenance migrate-skills --apply\n")
	}
	if cleanup.Inconclusive {
		fmt.Fprintf(&sb, "[CLEANUP] authority inconclusive: known_persons=%d candidates=%d root=%s control_db=%s\n",
			cleanup.KnownPersons, cleanup.Candidates, cleanup.Root, cleanup.ControlDB)
		sb.WriteString("- verify: selfmind config doctor && selfmind doctor --verbose")
		return strings.TrimSpace(sb.String())
	}
	if cleanup.Candidates > 0 {
		fmt.Fprintf(&sb, "[CLEANUP] orphan person asset partitions: candidates=%d protected=%d skipped=%d\n", cleanup.Candidates, cleanup.Protected, cleanup.Skipped)
		sb.WriteString("- preview: selfmind maintenance cleanup-person-partitions\n")
		sb.WriteString("- quarantine after review: selfmind gateway stop && selfmind maintenance cleanup-person-partitions --apply && selfmind gateway start")
	}
	return strings.TrimSpace(sb.String())
}

func resolveDoctorIdentity(ctx context.Context, store *control.Store, tenantID, userID string) (*control.IdentityContext, error) {
	identity, err := store.ResolveAccount(ctx, tenantID, "cli", userID)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		return identity, nil
	}
	return &control.IdentityContext{
		TenantID:       tenantID,
		Platform:       "cli",
		PlatformUserID: userID,
	}, nil
}

func formatModelRoleProbes(probes []appcore.ModelRoleProbe) (string, bool) {
	var sb strings.Builder
	sb.WriteString("== Model role probes ==\n")
	if len(probes) == 0 {
		sb.WriteString("(no auxiliary model or explicit role overrides)")
		return sb.String(), false
	}
	failed := false
	for _, probe := range probes {
		roles := strings.Join(probe.Roles, ",")
		if probe.Err != nil {
			failed = true
			fmt.Fprintf(&sb, "- FAIL roles=%s provider=%s model=%s latency=%s error=%s\n",
				roles, valueOrUnknown(probe.Provider), valueOrUnknown(probe.Model), probe.Latency.Round(time.Millisecond),
				oneLine(tools.RedactSensitive(probe.Err.Error()), 180))
			continue
		}
		toolsStatus := "skipped"
		if probe.NativeToolsTested {
			toolsStatus = "passed"
		}
		thinkingStatus := ""
		if probe.ThinkingToolLoopTested {
			thinkingStatus = " thinking=failed"
			if probe.ThinkingToolLoopPassed {
				thinkingStatus = " thinking=passed"
			}
		}
		contractStatus := "n/a"
		if probe.MaintenanceContractTested {
			contractStatus = "failed"
			if probe.MaintenanceContractPassed {
				contractStatus = "passed"
			}
		}
		fmt.Fprintf(&sb, "- OK roles=%s provider=%s model=%s latency=%s native_tools=%s maintenance_contract=%s%s\n",
			roles, valueOrUnknown(probe.Provider), valueOrUnknown(probe.Model), probe.Latency.Round(time.Millisecond), toolsStatus, contractStatus, thinkingStatus)
	}
	return strings.TrimSpace(sb.String()), failed
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return strings.TrimSpace(value)
}

// tenantID resolves the CLI's default tenant, mirroring the gateway runner.
func (a *App) tenantID() string {
	if t := strings.TrimSpace(os.Getenv("SELF_TENANT_ID")); t != "" {
		return t
	}
	return control.DefaultTenantID
}

// gatewayStatusLine returns a one-line gateway status, preferring the live HTTP
// status endpoint and falling back to the on-disk PID record so doctor still
// reports something useful when the daemon is unreachable or down.
func (a *App) gatewayStatusLine() string {
	serviceState := strings.TrimSpace(gatewayServiceDoctorLine())
	withService := func(status string) string {
		if serviceState == "" {
			return status
		}
		return status + " " + serviceState
	}
	ctx, cancel := contextWithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	if data, code, err := gatewayrt.RequestStatus(ctx, a.gatewayURL()); err == nil && code < 400 {
		var status api.GatewayStatusResponse
		if json.Unmarshal(data, &status) == nil {
			rt := status.Runtime
			buildState := gatewayBuildState(rt.BuildFingerprint)
			schemaState := gatewayToolSchemaDoctorState(status)
			mcpState := ""
			if status.MCP.Configured > 0 {
				mcpState = fmt.Sprintf(" mcp=connected:%d,failed:%d", status.MCP.Connected, status.MCP.Failed)
				if len(status.MCP.Failures) > 0 {
					failure := status.MCP.Failures[0]
					mcpState += fmt.Sprintf(",first:%s:%s", oneLine(failure.Name, 40), oneLine(failure.Error, 120))
				}
			}
			catalogState := gatewayToolCatalogDoctorState(status.ToolCatalog)
			return withService(fmt.Sprintf("running (state=%s pid=%d addr=%s active_runs=%d build=%s%s%s%s)", status.State, rt.PID, rt.Addr, status.ActiveRunCount, buildState, schemaState, mcpState, catalogState))
		}
	}
	manager := gatewayrt.NewManager(a.gatewayDataDir(), "")
	if rec, ok := manager.RunningRecord(); ok {
		if rec.HeartbeatStale(time.Now()) {
			return withService(fmt.Sprintf("unreachable (pid=%d heartbeat stale since=%s); inspect daemon logs", rec.PID, rec.HeartbeatAt))
		}
		return withService(fmt.Sprintf("running (pid=%d addr=%s), HTTP status unavailable", rec.PID, rec.Addr))
	}
	if rec, err := gatewayrt.ReadStatusRecord(manager.Paths.StatePath); err == nil && rec.State == "crashed" {
		if strings.Contains(strings.ToLower(rec.ExitReason), "host or wsl session restarted") {
			return withService(fmt.Sprintf("not running (previous host/session ended; instance=%s last_heartbeat=%s)", rec.InstanceID, rec.HeartbeatAt))
		}
		return withService(fmt.Sprintf("crashed (instance=%s reason=%s last_heartbeat=%s)", rec.InstanceID, rec.ExitReason, rec.HeartbeatAt))
	}
	return withService("not running")
}

func gatewayToolSchemaDoctorState(status api.GatewayStatusResponse) string {
	registeredActive := status.ToolSchemas.RegisteredActive
	if registeredActive == 0 {
		registeredActive = status.ToolSchemas.Active
	}
	providerVisible := status.ToolSchemas.ProviderVisible
	if providerVisible == 0 {
		providerVisible = status.ToolCatalog.Count
	}
	if registeredActive+status.ToolSchemas.Quarantined == 0 {
		return ""
	}
	return fmt.Sprintf(" tools=registered_active:%d,hidden:%d,provider_visible:%d,repaired:%d,quarantined:%d",
		registeredActive, status.ToolSchemas.Hidden, providerVisible, status.ToolSchemas.Repaired, status.ToolSchemas.Quarantined)
}

func gatewayToolCatalogDoctorState(preview llm.ToolCatalogPreview) string {
	if strings.TrimSpace(preview.Protocol) == "" && preview.Count == 0 && preview.Hash == "" && len(preview.Issues) == 0 {
		return ""
	}
	dynamicSkills := 0
	for _, entry := range preview.Entries {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.SourceName)), "skill:") {
			dynamicSkills++
		}
	}
	// Backward-compatible decoding of a status response from an older daemon.
	if len(preview.Entries) == 0 {
		for _, name := range preview.Names {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "skill:") {
				dynamicSkills++
			}
		}
	}
	if len(preview.Issues) == 0 {
		budgetState := "within"
		if preview.OverBudget {
			budgetState = "over"
		}
		return fmt.Sprintf(" catalog=valid:%d,protocol:%s,bytes:%d,schema_budget:%s:%d,dynamic_skills:%d,hash:%s",
			preview.Count, oneLine(preview.Protocol, 40), preview.WireBytes, budgetState,
			preview.BudgetBytes, dynamicSkills, oneLine(preview.Hash, 24))
	}
	first := preview.Issues[0]
	return fmt.Sprintf(" catalog=invalid:%d,protocol:%s,count:%d,dynamic_skills:%d,first_index:%d,first_source:%s,first_name:%s,first_code:%s",
		len(preview.Issues), oneLine(preview.Protocol, 40), preview.Count, dynamicSkills, first.Index,
		oneLine(first.SourceName, 80), oneLine(first.Name, 80), oneLine(first.Code, 40))
}

// buildDoctorReport assembles the redacted diagnostic bundle from durable
// control-plane state and the on-disk log. It is separated from CLI plumbing so
// it can be unit-tested against a seeded temp store.
func buildDoctorReport(ctx context.Context, store *control.Store, identity *control.IdentityContext, dataDir, gatewayStatus, configSection, promptSection string, logLines int) string {
	var sb strings.Builder
	sb.WriteString("SelfMind doctor — diagnostic bundle\n")
	fmt.Fprintf(&sb, "generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "person: %s  tenant: %s\n", identity.PersonID, identity.TenantID)
	fmt.Fprintf(&sb, "data dir: %s\n", dataDir)
	fmt.Fprintf(&sb, "%s\n\n", formatControlSchemaLine(store.SchemaStatus()))

	fmt.Fprintf(&sb, "== Gateway ==\n%s\n\n", gatewayStatus)
	if strings.TrimSpace(configSection) != "" {
		sb.WriteString(configSection)
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(promptSection) != "" {
		sb.WriteString(promptSection)
		sb.WriteString("\n\n")
	}

	sb.WriteString(formatSkillPresentationDiagnostics(store.InspectSkillPresentation(ctx, identity.TenantID, identity.PersonID)))
	sb.WriteString("\n\n")

	sb.WriteString("== Workspace trust ==\n")
	if workspaces, err := store.ListWorkspaces(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else {
		pending := 0
		for _, workspace := range workspaces {
			if workspace.TrustSource != "migration_review_required" {
				continue
			}
			pending++
			fmt.Fprintf(
				&sb,
				"- review required: %s (%s)\n    %s\n",
				oneLine(workspace.Name, 60),
				workspace.ID,
				oneLine(workspace.LocalPath, 160),
			)
		}
		if pending == 0 {
			sb.WriteString("no migrated workspaces require trust review\n")
		} else {
			sb.WriteString("Review each path, then run `selfmind ws trust <workspace_id>` or leave it untrusted.\n")
		}
	}
	sb.WriteString("\n")

	sb.WriteString("== Background learning ==\n")
	if health, err := store.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else {
		fmt.Fprintf(&sb, "queued: %d  retrying: %d  running: %d  provider-blocked: %d  network-blocked: %d  prompt-revision-blocked: %d\n",
			health.Pending, health.Failed, health.Running,
			health.Blocked-health.BlockedPrompt-health.NetworkBlocked, health.NetworkBlocked, health.BlockedPrompt)
		if !health.OldestPendingAt.IsZero() {
			fmt.Fprintf(&sb, "oldest unfinished: %s\n", time.Since(health.OldestPendingAt).Round(time.Second))
		}
		if !health.LastSuccessAt.IsZero() {
			fmt.Fprintf(&sb, "last success: %s\n", health.LastSuccessAt.UTC().Format(time.RFC3339))
		}
		if reason := strings.TrimSpace(health.LastError); reason != "" {
			fmt.Fprintf(&sb, "last error: %s\n", oneLine(tools.RedactSensitive(reason), 200))
		}
		for i, route := range health.BlockedRoutes {
			if i >= 3 {
				break
			}
			nextProbe := "probe pending"
			if !route.NextProbeAt.IsZero() {
				nextProbe = route.NextProbeAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&sb, "blocked route: %s/%s  next probe: %s\n", route.Provider, route.Model, nextProbe)
		}
		if health.Failed > 0 || health.Blocked > 0 {
			sb.WriteString("status: foreground work can continue; background learning is degraded\n")
		} else {
			sb.WriteString("status: healthy\n")
		}
	}
	sb.WriteString("\n")

	// Recent runs.
	sb.WriteString("== Recent runs ==\n")
	if runs, err := store.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, doctorRecentRuns); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(runs) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, r := range runs {
			title := strings.TrimSpace(r.TaskTitle)
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&sb, "- [%s] %s (%s)\n", r.Status, oneLine(title, 60), r.Elapsed().Round(time.Second))
			if strings.TrimSpace(r.LastError) != "" {
				fmt.Fprintf(&sb, "    error: %s\n", oneLine(tools.RedactSensitive(r.LastError), 160))
			}
		}
	}
	sb.WriteString("\n")

	// Recent errors: run-level failures + tool failures, aggregated newest
	// first, so "what has been going wrong lately" is one glance instead of
	// reading each run's events by hand.
	sb.WriteString("== Recent errors ==\n")
	if errs, err := store.ListRecentErrors(ctx, identity.TenantID, identity.PersonID, doctorRecentErrors); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(errs) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range errs {
			fmt.Fprintf(&sb, "- %s [%s:%s] %s\n",
				e.When.Format("01-02 15:04"), e.Kind, oneLine(e.Source, 24),
				oneLine(tools.RedactSensitive(e.Message), 160))
		}
	}
	sb.WriteString("\n")

	// Pending approvals.
	sb.WriteString("== Pending approvals ==\n")
	if approvals, err := store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(approvals) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, ap := range approvals {
			fmt.Fprintf(&sb, "- %s [%s] (%s)\n", ap.ActionType, oneLine(tools.RedactSensitive(string(ap.Payload)), 120), ap.ID)
		}
	}
	sb.WriteString("\n")

	// Queued tasks.
	sb.WriteString("== Queued tasks ==\n")
	if queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(queued) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for i, q := range queued {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, oneLine(tools.RedactSensitive(q.Content), 80))
		}
	}
	sb.WriteString("\n")

	// Recent event timeline — the real per-turn/tool/approval/error detail
	// (task_events), which the sparse gateway.log does not carry.
	sb.WriteString("== Recent events ==\n")
	if events, err := store.ListRecentEventsForPerson(ctx, identity.TenantID, identity.PersonID, doctorRecentEvents); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(events) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range events {
			ts := e.CreatedAt.Format("01-02 15:04:05")
			line := fmt.Sprintf("%s  %-18s %s", ts, e.Type, oneLine(tools.RedactSensitive(e.Preview), 100))
			sb.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	sb.WriteString("\n")

	// Outbound pushes that never confirmed.
	sb.WriteString("== Unconfirmed / failed pushes ==\n")
	if pushes, err := store.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, time.Unix(0, 0), doctorMaxPushes); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(pushes) == 0 {
		sb.WriteString("(none)\n")
	} else {
		unconfirmed := false
		for _, p := range pushes {
			if p.Status == "sent_unconfirmed" {
				unconfirmed = true
			}
			fmt.Fprintf(&sb, "- [%s/%s] %s\n", p.Platform, p.Status, oneLine(tools.RedactSensitive(p.Content), 80))
		}
		if unconfirmed {
			sb.WriteString("note: sent_unconfirmed = the platform accepted the push but delivery is doubtful.\n")
			sb.WriteString("  On WeChat/iLink this happens when the proactive-push session token (context_token)\n")
			sb.WriteString("  went stale — the API returns success yet the message never reaches the phone.\n")
			sb.WriteString("  Recovery: send ANY message from that chat; the fresh inbound renews the token and\n")
			sb.WriteString("  arms a one-shot catch-up re-push of recent unconfirmed notices (bounded, no duplicates).\n")
		}
	}
	sb.WriteString("\n")

	// Cron governance: system jobs must stay singular. The 2026-07 incident
	// registered one skill-pruner per data-directory entry (~2.5k rows) whose
	// daily fire kept touching stale eval/person partitions; this section
	// makes that class of runaway visible before it burns a night of I/O.
	sb.WriteString("== Cron governance ==\n")
	sb.WriteString(cronGovernanceSection(ctx, dataDir))
	sb.WriteString("\n")

	// Presence snapshot (durable last_seen recency per bound account).
	sb.WriteString("== Presence (bound accounts) ==\n")
	if accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(accounts) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, acct := range accounts {
			seen := "never seen"
			attached := ""
			if acct.LastSeenAt > 0 {
				age := time.Since(time.Unix(acct.LastSeenAt, 0))
				seen = age.Round(time.Second).String() + " ago"
				if age <= presenceRecentWindow {
					attached = " (attached)"
				}
			}
			fmt.Fprintf(&sb, "- %s: last seen %s%s\n", acct.Platform, seen, attached)
		}
	}
	sb.WriteString("\n")

	// Activity by channel (durable per-person trajectory).
	sb.WriteString("== Activity by channel ==\n")
	if counts, err := store.CountChannelMessagesByChannel(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(counts) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, c := range counts {
			fmt.Fprintf(&sb, "- %s: %d messages\n", c.Channel, c.Count)
		}
	}
	sb.WriteString("\n")

	// Gateway log tail.
	fmt.Fprintf(&sb, "== Gateway log (last %d lines) ==\n", logLines)
	logPath := gatewayrt.ResolvePaths(dataDir).LogPath
	if tail, err := tailLines(logPath, logLines); err != nil {
		fmt.Fprintf(&sb, "(log unavailable: %v)\n", err)
	} else if len(tail) == 0 {
		sb.WriteString("(empty)\n")
	} else {
		for _, line := range tail {
			sb.WriteString(tools.RedactSensitive(line) + "\n")
		}
	}

	return strings.TrimSpace(sb.String())
}

// formatControlSchemaLine reports the control.db schema this process accepted
// against the version the binary requires, plus the pre-migration backup made
// by this open when it crossed a migration boundary.
func formatControlSchemaLine(status control.StoreSchemaStatus) string {
	backup := strings.TrimSpace(status.MigrationBackup)
	if backup == "" {
		backup = "none"
	}
	return fmt.Sprintf("control schema: %d/%d (backup: %s)", status.Version, status.CurrentVersion, backup)
}

func formatSkillPresentationDiagnostics(report control.SkillPresentationDiagnostics, err error) string {
	var sb strings.Builder
	sb.WriteString("== Skill presentation ==\n")
	if err != nil {
		fmt.Fprintf(&sb, "(error: %s)", oneLine(tools.RedactSensitive(err.Error()), 180))
		return sb.String()
	}
	fmt.Fprintf(&sb, "schema: %d/%d  activations: %d (legacy=%d full=%d paged=%d)\n",
		report.SchemaVersion, report.CurrentSchemaVersion, report.Activations,
		report.LegacyActivations, report.FullActivations, report.PagedActivations)
	fmt.Fprintf(&sb, "delivery receipts: invalid=%d  package resources: %d invalid=%d\n",
		report.InvalidDeliveryReceipts, report.PackageResources, report.InvalidResourceReceipts)
	fmt.Fprintf(&sb, "candidate refs: live=%d terminal_leaks=%d drift_limit_exceeded=%d\n",
		report.CandidateRefs, report.TerminalCandidateRefLeaks, report.CandidateRefsOverDriftLimit)
	for _, issue := range report.Issues {
		fmt.Fprintf(&sb, "- [%s] id=%s code=%s component=%s ref=%s\n",
			strings.ToUpper(issue.Severity), oneLine(issue.ID, 100), oneLine(issue.Code, 80),
			oneLine(issue.Component, 80), oneLine(issue.Ref, 120))
		fmt.Fprintf(&sb, "  location: %s\n", oneLine(issue.Location, 200))
		fmt.Fprintf(&sb, "  expected: %s\n", oneLine(issue.Expected, 200))
		fmt.Fprintf(&sb, "  observed: %s\n", oneLine(issue.Observed, 200))
		fmt.Fprintf(&sb, "  cause: %s\n", oneLine(issue.Cause, 240))
		fmt.Fprintf(&sb, "  owner: %s\n", oneLine(issue.Owner, 160))
		for _, remediation := range issue.Remediations {
			fmt.Fprintf(&sb, "  repair: %s\n", oneLine(remediation.Description, 240))
			for _, command := range remediation.Commands {
				fmt.Fprintf(&sb, "    command: %s\n", oneLine(command, 200))
			}
		}
		for _, command := range issue.Verify {
			fmt.Fprintf(&sb, "  verify: %s\n", oneLine(command, 200))
		}
	}
	switch {
	case report.Fatal():
		sb.WriteString("[FATAL] status: stored Skill presentation invariants are broken")
	case !report.Healthy():
		sb.WriteString("[WARNING] status: stale terminal candidate refs require lifecycle cleanup")
	default:
		sb.WriteString("status: healthy")
	}
	return strings.TrimSpace(sb.String())
}

func formatSkillDescriptionDiagnostics(issues []tools.SkillDescriptionDiagnostic, err error) string {
	var sb strings.Builder
	sb.WriteString("== Skill descriptions ==\n")
	if err != nil {
		fmt.Fprintf(&sb, "(error: %s)", oneLine(tools.RedactSensitive(err.Error()), 180))
		return sb.String()
	}
	if len(issues) == 0 {
		sb.WriteString("status: healthy")
		return sb.String()
	}
	fmt.Fprintf(&sb, "[WARNING] over_limit=%d; assets remain loadable but descriptions are capped on the model presentation surface\n", len(issues))
	for _, issue := range issues {
		fmt.Fprintf(&sb, "- [WARNING] id=skill_presentation.description_over_limit code=description_over_limit component=catalog_metadata ref=%s\n", oneLine(issue.Name, 100))
		fmt.Fprintf(&sb, "  location: %s\n", oneLine(issue.Path, 220))
		fmt.Fprintf(&sb, "  expected: description <= %d Unicode characters and <= %d UTF-8 bytes\n", tools.SkillDescriptionMaxChars, tools.SkillDescriptionMaxBytes)
		fmt.Fprintf(&sb, "  observed: chars=%d bytes=%d scope=%s source=%s writable=%t\n", issue.Chars, issue.Bytes, issue.Scope, issue.Source, issue.Writable)
		sb.WriteString("  cause: existing Skill metadata predates or bypassed the managed authoring ceiling\n")
		sb.WriteString("  owner: the Skill file shown in location\n")
		sb.WriteString("  repair: Shorten only the front-matter description in the owning Skill to the stated limits; keep the instruction body intact.\n")
		sb.WriteString("    command: selfmind gateway restart\n")
		sb.WriteString("  verify: selfmind doctor --verbose\n")
	}
	return strings.TrimSpace(sb.String())
}

func formatSkillFrontMatterDiagnostics(issues []tools.SkillFrontMatterDiagnostic, err error) string {
	var sb strings.Builder
	sb.WriteString("== Skill front matter ==\n")
	if err != nil {
		fmt.Fprintf(&sb, "(error: %s)", oneLine(tools.RedactSensitive(err.Error()), 180))
		return sb.String()
	}
	if len(issues) == 0 {
		sb.WriteString("status: healthy")
		return sb.String()
	}
	fmt.Fprintf(&sb, "[WARNING] unmodelled_keys=%d; the assets load and run, but these declarations have no effect here\n", len(issues))
	for _, issue := range issues {
		fmt.Fprintf(&sb, "- [WARNING] id=skill_presentation.unknown_front_matter_key code=unknown_front_matter_key component=catalog_metadata ref=%s\n", oneLine(issue.Name, 100))
		fmt.Fprintf(&sb, "  location: %s\n", oneLine(issue.Path, 220))
		sb.WriteString("  expected: every front-matter key is one SelfMind models\n")
		fmt.Fprintf(&sb, "  observed: keys=%s scope=%s source=%s provenance=%s writable=%t\n",
			oneLine(strings.Join(issue.Keys, ","), 200), issue.Scope, issue.Source,
			emptyDoctorValue(issue.Provenance), issue.Writable)
		sb.WriteString("  cause: the Skill was authored for another agent whose front-matter vocabulary is wider\n")
		sb.WriteString("  owner: the Skill file shown in location\n")
		sb.WriteString("  repair: Confirm the declaration is not load-bearing here. Remove it from a writable Skill, or accept that a read-only external asset carries a key this runtime ignores.\n")
		sb.WriteString("  verify: selfmind doctor --verbose\n")
	}
	return strings.TrimSpace(sb.String())
}

func emptyDoctorValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func buildPromptWorkspaceDoctorSection(store *control.Store, dataDir string, cfg *config.Config) string {
	snapshot, status := appcore.InspectRuntimePromptSnapshot(cfg, dataDir)
	degraded := status.Degraded()
	var sb strings.Builder
	sb.WriteString("== Prompt workspace ==\n")
	fmt.Fprintf(&sb, "root: %s\n", status.ActiveRoot)
	if status.ActiveError == "" {
		sb.WriteString("active: valid\n")
	} else {
		fmt.Fprintf(&sb, "active: invalid (%s)\n", status.ActiveErrorKind)
		fmt.Fprintf(&sb, "error: %s\n", oneLine(tools.RedactSensitive(status.ActiveError), 240))
	}
	fmt.Fprintf(&sb, "startup selection: %s (%s)\n", status.Source, shortPromptHash(snapshot.Hash()))
	if status.FallbackError != "" && status.Source == appcore.PromptSourceBuiltIn {
		fmt.Fprintf(&sb, "last-known-good unavailable: %s\n", oneLine(tools.RedactSensitive(status.FallbackError), 200))
	}

	if store != nil {
		manager := gatewayrt.NewManager(dataDir, "")
		if record, ok := manager.RunningRecord(); ok && strings.TrimSpace(record.InstanceID) != "" {
			if event, err := store.GatewayRuntimeEventForInstance(context.Background(), record.InstanceID, "prompt.snapshot.loaded"); err == nil {
				var runtimeStatus struct {
					SnapshotHash    string `json:"snapshot_hash"`
					Source          string `json:"source"`
					Degraded        bool   `json:"degraded"`
					ActivationError string `json:"activation_error"`
				}
				if json.Unmarshal(event.Payload, &runtimeStatus) == nil && strings.TrimSpace(runtimeStatus.Source) != "" {
					fmt.Fprintf(&sb, "running daemon: %s (%s)\n", runtimeStatus.Source, shortPromptHash(runtimeStatus.SnapshotHash))
					if runtimeStatus.ActivationError != "" {
						fmt.Fprintf(&sb, "activation warning: %s\n", oneLine(tools.RedactSensitive(runtimeStatus.ActivationError), 200))
					}
					degraded = degraded || runtimeStatus.Degraded
				}
			}
		}
	}
	if degraded {
		sb.WriteString("status: degraded; foreground endpoints remain available on the selected safe snapshot")
	} else {
		sb.WriteString("status: healthy")
	}
	return sb.String()
}

// oneLine collapses whitespace and bounds a string for a compact bundle line.
func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

// tailLines returns the last n lines of a file, or an empty slice for a missing
// file (doctor must not fail because a log was never created).
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// cronGovernanceSection summarizes cron_jobs health from the cron database:
// totals, system-job count, obsolete Skill metric-pruner rows, and duplicate
// system keys. Read-only; a missing db reports cleanly.
func cronGovernanceSection(ctx context.Context, dataDir string) string {
	db, err := sql.Open("sqlite", "file:"+dataDir+"/cron.db?mode=ro")
	if err != nil {
		return fmt.Sprintf("(error: %v)\n", err)
	}
	defer db.Close()
	var total, system, retiredPruner, legacyPruner, dupGroups int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs").Scan(&total); err != nil {
		return fmt.Sprintf("(error: %v)\n", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs WHERE COALESCE(system_key,'') != ''").Scan(&system)
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs WHERE system_key = 'skill-pruner:default'").Scan(&retiredPruner)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs
		WHERE COALESCE(system_key, '') = '' AND name LIKE 'skill-pruner-%'
		  AND cron_expr = '0 3 * * *' AND channel = 'cli'
		  AND prompt LIKE 'skill_prune:%'`).Scan(&legacyPruner)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT 1 FROM cron_jobs WHERE COALESCE(system_key, '') != ''
		GROUP BY system_key HAVING COUNT(*) > 1)`).Scan(&dupGroups)
	out := fmt.Sprintf("jobs: %d total, %d system, retired skill-pruner rows: %d keyed + %d legacy, %d duplicate system-key group(s)\n",
		total, system, retiredPruner, legacyPruner, dupGroups)
	if retiredPruner+legacyPruner > 0 {
		out += "warning: obsolete Skill metric-pruner jobs remain; restart the daemon to run the retirement migration.\n"
	}
	if dupGroups > 0 {
		out += "warning: duplicate cron rows detected — restart the daemon (EnsureJob collapses them) or inspect cron.db.\n"
	}
	if total > 50 {
		out += "warning: unusually many cron jobs — check for runaway system registration.\n"
	}
	return out
}
