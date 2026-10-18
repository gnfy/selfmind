package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

type recordedEvidencePayload struct {
	Evidence kernel.RunEvidence `json:"evidence"`
}

// evidenceOutcome derives a verification verdict from durable runtime events.
// Model prose is intentionally not an input to the verdict.
func (c *RunCoordinator) evidenceOutcome(ctx context.Context, tenantID, runID string) (*api.VerificationOutcome, []string) {
	if c == nil || c.srv == nil || c.srv.Control == nil || runID == "" {
		return &api.VerificationOutcome{State: "blocked", Summary: "Verification evidence is unavailable; inspect the durable Run before completing."}, nil
	}
	run, err := c.srv.Control.GetRun(ctx, tenantID, runID)
	if err != nil || run == nil {
		return &api.VerificationOutcome{State: "blocked", Summary: "Verification evidence is unavailable; inspect the durable Run before completing."}, nil
	}
	var events []control.Event
	var before int64
	for {
		page, err := c.srv.Control.ListRunEvidenceEvents(ctx, run.TenantID, run.ID, before, 200)
		if err != nil {
			return &api.VerificationOutcome{State: "blocked", Summary: "Verification evidence is unavailable; inspect the durable Run before completing."}, nil
		}
		events = append(events, page...)
		if len(page) < 200 {
			break
		}
		before = page[len(page)-1].Cursor
	}

	var evidence []kernel.RunEvidence
	var changed []string
	// Whether this run made a file from nothing is decided by the EARLIEST
	// effect on that path: a later edit carries a real before-hash and would
	// otherwise look like a pre-existing file. Keyed on the effect's own
	// timestamp rather than iteration order, because ListTaskEvents returns
	// newest-first — reading "the first one seen" as "the first one that
	// happened" silently inverted this rule and the whole fix did nothing.
	type firstTouch struct {
		at      int64
		created bool
	}
	firstSeen := map[string]firstTouch{}
	for _, event := range events {
		if event.RunID != runID || event.Type != "evidence.recorded" {
			continue
		}
		var payload recordedEvidencePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Evidence.ToolName == "" {
			return &api.VerificationOutcome{State: "blocked", Summary: "Recorded verification evidence is malformed; inspect the durable Run before completing."}, changed
		}
		evidence = append(evidence, payload.Evidence)
		if payload.Evidence.Kind == "mutation" {
			for _, effect := range payload.Evidence.Files {
				if prev, known := firstSeen[effect.Path]; !known || payload.Evidence.StartedAt < prev.at {
					firstSeen[effect.Path] = firstTouch{at: payload.Evidence.StartedAt, created: effect.BeforeSHA256 == ""}
				}
				if effect.BeforeSHA256 != effect.AfterSHA256 {
					changed = appendUnique(changed, effect.Path, 32)
				}
			}
		}
	}
	// A file this run created and then removed left the workspace exactly as it
	// found it, so it is not a change anyone can verify — and demanding
	// verification for it strands an otherwise finished run in Attention
	// forever. Observed 2026-09-04: a turn whose real work (commit and push)
	// succeeded was held at verification_partial by a throwaway `.py` helper it
	// had already deleted, purely because `.py` is on the verifiable-suffix list
	// and `.md` — the files it actually edited — is not. A file the run DELETED
	// but did not create keeps its before-hash and still counts: removing real
	// source is a change worth verifying.
	transient := map[string]bool{}
	for path, touch := range firstSeen {
		if touch.created && !pathExists(path) {
			transient[path] = true
		}
	}
	files := make([]string, 0, len(changed))
	for _, path := range changed {
		if !transient[path] {
			files = append(files, path)
		}
	}
	adopted, adoptionErr := c.srv.Control.AdoptedRunVerificationChecks(ctx, tenantID, runID)
	current, currentErr := c.srv.Control.CurrentRunObservationChecks(ctx, tenantID, runID)
	adoptionErr = errors.Join(adoptionErr, currentErr)
	for _, check := range current {
		if check.Observation != nil && check.Observation.Kind == "command" {
			adopted = append(adopted, check)
		}
	}
	if len(evidence) == 0 && len(adopted) == 0 && adoptionErr == nil {
		condition, conditionErr := c.srv.Control.ResolveConditionAssessments(ctx, tenantID, runID, nil)
		if conditionErr == nil && len(condition.Conditions) == 0 {
			return nil, files
		}
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].StartedAt < evidence[j].StartedAt })

	result := &api.VerificationOutcome{Checks: adopted}
	for i := range result.Checks {
		result.Checks[i].NetworkMayFetch = tools.CommandMayFetchDependencies(result.Checks[i].Command)
	}
	var mutations []verification.Mutation
	for _, item := range evidence {
		if item.Kind == "mutation" && evidenceChangedFiles(item.Files, transient) {
			for _, effect := range item.Files {
				if !transient[effect.Path] && effect.BeforeSHA256 != effect.AfterSHA256 {
					mutations = append(mutations, verification.Mutation{Path: effect.Path, FinishedAt: item.FinishedAt})
					if effect.ResolvedPath != "" && effect.ResolvedPath != effect.Path {
						mutations = append(mutations, verification.Mutation{Path: effect.ResolvedPath, FinishedAt: item.FinishedAt})
					}
				}
			}
		}
		if item.Kind != "verification" || item.Command == nil {
			continue
		}
		check := api.VerificationCheck{
			ToolCallID: item.ToolCallID, Binding: item.Command.Binding,
			Kind:        item.Command.Kind,
			Command:     item.Command.Command,
			CWD:         item.Command.CWD,
			Status:      item.Status,
			ExitCode:    item.Command.ExitCode,
			StartedAt:   item.StartedAt,
			FinishedAt:  item.FinishedAt,
			EffectState: item.EffectState,
			Invoked:     item.Invoked,
		}
		// A refused preparation did not test the program. Preserve that fact
		// separately from the tool wrapper's failure and historical exit code.
		var processExit *int
		if item.Process != nil {
			started := item.Process.Started
			check.ProcessStarted = &started
			// What the runtime recorded about the network for this check's
			// process, so a pass cannot hide that it ran offline.
			check.NetworkDecision, check.NetworkPermission = item.Process.NetworkDecision, item.Process.NetworkPermission
			// The offline notice is for a command that can fetch dependencies;
			// derived from the recorded command, so earlier evidence reads the
			// same way.
			if executionenv.NetworkOfflineDespiteGrant(check.NetworkDecision, check.NetworkPermission) {
				check.NetworkMayFetch = tools.CommandMayFetchDependencies(check.Command)
			}
			processExit = item.Process.ExitCode
		}
		result.Checks = append(result.Checks, verification.WithDispatchFacts(check, processExit))
	}

	result.LatestMutationAt = verification.RelevantMutationAt(verification.Check{}, mutations)
	result.State, result.Summary = verification.StateWithMutations(mutations, result.Checks)
	if adoptionErr != nil {
		switch result.State {
		case "passed", "not_applicable", "not_run":
			result.State = "blocked"
		}
		result.Summary += " Adopted verification could not be revalidated: " + tools.RedactSensitive(adoptionErr.Error())
	} else if len(adopted) > 0 {
		result.Summary += fmt.Sprintf(" %d check(s) explicitly adopted from prior Runs; source identity and observation times are preserved.", len(adopted))
	}
	if err := c.projectCommandObservations(ctx, tenantID, runID, evidence, result); err != nil {
		result.State, result.Summary = "blocked", "Command dispatch evidence is unavailable; inspect the durable Run before completing."
		return result, files
	}
	if result.OrdinaryCommandAttempts > 0 && (result.State == "not_run" || result.State == "not_applicable") {
		result.Summary = fmt.Sprintf("%d ordinary command(s) were observed as started (%d failed, %d exit status unknown); %d attempt(s) were not dispatched and %d have unknown dispatch. These are execution observations, not a passing verification verdict.", result.OrdinaryCommands, result.OrdinaryCommandFailures, result.OrdinaryCommandExitUnknown, result.OrdinaryCommandNotDispatched, result.OrdinaryCommandDispatchUnknown)
		if result.State == "not_run" {
			result.Summary += " Use verify for the relevant check after the latest change."
		}
	}
	condition, conditionErr := c.srv.Control.ResolveConditionAssessments(ctx, tenantID, runID, nil)
	applyConditionProjection(result, condition, conditionErr)
	return result, files
}

func evidenceChangedFiles(files []kernel.FileEffect, transient map[string]bool) bool {
	for _, effect := range files {
		if !transient[effect.Path] && effect.BeforeSHA256 != effect.AfterSHA256 {
			return true
		}
	}
	return false
}

// pathExists answers only the question asked: is the file there now. An
// unreadable path counts as present, because "cannot tell" must not be read as
// "the run cleaned up after itself".
func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return true
}

func verificationState(latestMutation int64, checks []api.VerificationCheck) (string, string) {
	return verification.State(latestMutation, checks)
}

func latestVerificationAttempts(checks []api.VerificationCheck) []api.VerificationCheck {
	return verification.Latest(checks)
}

func verificationClaimMismatches(outcome api.RunOutcome) []string {
	if !hasPositiveVerificationClaim(outcome.Tests) {
		return nil
	}
	state := "not_run"
	if outcome.Verification != nil && outcome.Verification.State != "" {
		state = outcome.Verification.State
	}
	if state == "passed" {
		return nil
	}
	if outcome.Verification != nil && outcome.Verification.OrdinaryCommands > 0 {
		return []string{fmt.Sprintf("%d ordinary command(s) were executed, but the claimed verification has no passing structured evidence (state: %s). Bind the relevant check with verify before claiming it passed.", outcome.Verification.OrdinaryCommands, state)}
	}
	return []string{fmt.Sprintf("The response claims successful verification, but runtime evidence is %s.", state)}
}

func hasPositiveVerificationClaim(claims []string) bool {
	for _, claim := range claims {
		lower := strings.ToLower(claim)
		if verificationClaimContainsAny(lower, []string{
			"failed", "failure", "not pass", "did not pass", "not run", "was not run",
			"失败", "未通过", "未运行", "没有运行",
		}) {
			continue
		}
		verificationCue := verificationClaimContainsAny(lower, []string{
			"test", "pytest", "go test", "cargo test", "go vet", "lint", "typecheck", "type-check",
			"go build", "cargo build", "npm run build", "pnpm build", "yarn build", "mvn package", "gradle build",
			"syntax check", "smoke check", "verification", "verify command",
			"测试", "构建检查", "构建验证", "语法检查", "冒烟检查", "校验",
		})
		positiveCue := verificationClaimContainsAny(" "+lower, []string{
			" passed", " pass", " success", " succeeded", " ok", " green",
			"通过", "成功", "无错误",
		})
		if verificationCue && positiveCue {
			return true
		}
	}
	return false
}

func verificationClaimContainsAny(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func verificationRequiresResume(verification *api.VerificationOutcome, changedFiles []string) bool {
	if verification == nil {
		return false
	}
	if len(verification.ConditionBlockers) > 0 {
		return true
	}
	switch verification.State {
	case "failed":
		return !verification.ConditionReviewComplete
	case "stale", "blocked", "partial":
		return true
	case "not_run":
		return hasVerifiableFile(changedFiles)
	default:
		return false
	}
}

func applyVerificationOutcome(outcome api.RunOutcome) api.RunOutcome {
	if !verificationRequiresResume(outcome.Verification, outcome.Files) {
		return outcome
	}
	if verificationPreservesParkedStatus(outcome.Status) {
		// A registered external wait, an explicit user gate, or another durable
		// parked state is the primary reason this turn stopped. Verification is
		// still useful evidence, but must not erase the wake-up semantics by
		// converting the task to verification_partial.
		outcome = reconcileStructuredOutcome(outcome)
		outcome.NextSteps = appendUnique(outcome.NextSteps, verificationNextStep(outcome.Verification), 8)
		return outcome
	}
	outcome.Status = api.RunStatusVerificationPartial
	outcome.Resumable = true
	// A bare "failed" is replaced too. Once the status is forced to
	// verification_partial the reason has to describe the VERIFICATION, because
	// that is what the status now asserts and what consumers switch on
	// (task_view.go groups verification_incomplete/verification_failed). The
	// model's own account of the failure survives in Summary and Risks, so
	// nothing is lost by preferring the specific reason over the generic one.
	if outcome.CompletionReason == "" || outcome.CompletionReason == "completed" || outcome.CompletionReason == "failed" {
		switch outcome.Verification.State {
		case "failed":
			outcome.CompletionReason = "verification_failed"
		case "blocked":
			outcome.CompletionReason = "verification_blocked"
		default:
			outcome.CompletionReason = "verification_incomplete"
		}
	}
	outcome.NextSteps = appendUnique(outcome.NextSteps, verificationNextStep(outcome.Verification), 8)
	return outcome
}

func verificationPreservesParkedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "waiting_external", "waiting_user", "waiting_finalization", "blocked":
		return true
	default:
		return false
	}
}

func hasVerifiableFile(files []string) bool {
	for _, path := range files {
		lower := strings.ToLower(path)
		for _, suffix := range []string{
			".go", ".rs", ".py", ".js", ".jsx", ".ts", ".tsx", ".java", ".kt", ".kts",
			".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".php", ".rb", ".swift", ".scala",
			".sh", ".bash", ".zsh", ".ps1", ".sql", ".html", ".css", ".vue", ".svelte",
		} {
			if strings.HasSuffix(lower, suffix) {
				return true
			}
		}
	}
	return false
}

func verificationNextStep(verification *api.VerificationOutcome) string {
	if verification == nil {
		return "Run the appropriate verification and continue."
	}
	if len(verification.ConditionBlockers) > 0 {
		return strings.Join(verification.ConditionBlockers, " ")
	}
	switch verification.State {
	case "failed":
		return "Assess the recorded result against the user's goal: report a conclusive finding, or correct an inconclusive observation method."
	case "stale":
		return "Run verification again after the latest file change."
	case "blocked", "partial":
		return "Resolve the blocked verification and continue."
	default:
		return "Use verify to run the relevant test, build, lint, syntax, smoke, or custom check and continue."
	}
}

// withCompletionNotice states, in the turn's own reply, that the run did not
// finish and how to pick it up. The RUN outcome is the only place this is
// known: the turn's streamed self-report can say "completed" while the run
// resolves to verification_partial / missing_final_response, and that outcome is
// written after the stream closes — which is why this belongs beside the
// verification notice rather than in the router's stream summary.
//
// Observed 2026-09-04: a run whose model stopped without a final answer told the
// person a tool had errored (two earlier failures had in fact been recovered
// from) and never mentioned that replying "continue" would resume it, even
// though the outcome had already computed exactly that.
func withCompletionNotice(content string, outcome api.RunOutcome) string {
	content = strings.TrimSpace(content)
	switch strings.TrimSpace(outcome.Status) {
	case "", "done", "completed":
		return content
	}
	reason := strings.ReplaceAll(strings.TrimSpace(outcome.CompletionReason), "_", " ")
	notice := "SelfMind stopped before full completion"
	if reason != "" && reason != "completed" {
		notice += " (" + reason + ")"
	}
	if outcome.Resumable {
		notice += `; reply "continue" to resume.`
	} else {
		notice += "."
	}
	// The router's stream summary appends the same sentence when the TURN itself
	// reported incomplete. Saying it twice is worse than saying it once.
	if strings.Contains(content, "SelfMind stopped before full completion") {
		return content
	}
	if content == "" {
		return notice
	}
	return content + "\n\n" + notice
}

func withVerificationNotice(content string, verification *api.VerificationOutcome, mismatches []string) string {
	if verification == nil || verification.State == "not_applicable" && len(verification.ConditionBlockers) == 0 && (verification.ConditionState == "" || verification.ConditionState == "unassessed") {
		return strings.TrimSpace(content)
	}
	line := conciseVerificationNotice(verification)
	if line == "" && len(mismatches) == 0 {
		return strings.TrimSpace(content)
	}
	if len(mismatches) > 0 {
		if line != "" {
			line += " "
		}
		line += "The assistant's verification claim was not accepted."
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return line
	}
	return content + "\n\n" + line
}

// offlineVerificationNotice states what a pass did not cover. A check that ran
// without network although the workspace's network grant permits it is still a
// pass; the person is told, because "passed" would otherwise read as coverage
// of anything that needs a remote service. A bare pass adds nothing.
func offlineVerificationNotice(outcome *api.VerificationOutcome) string {
	offline := verification.OfflineDespiteGrant(outcome.Checks)
	if offline == 0 {
		return ""
	}
	return fmt.Sprintf("Verification passed without network: %d check(s) ran offline although this workspace's network grant permits it, so coverage that needs remote data was not exercised. Ask for a re-run with network access (network: true) if it matters.", offline)
}

func conciseVerificationNotice(verification *api.VerificationOutcome) string {
	if verification == nil {
		return ""
	}
	if len(verification.ConditionBlockers) > 0 {
		return "Condition review incomplete: " + truncateRunes(toOneLine(strings.Join(verification.ConditionBlockers, " ")), 400)
	}
	switch verification.ConditionState {
	case "not_met":
		if !verification.ConditionReviewComplete {
			return "Observed condition: not met. Other recorded checks remain unresolved."
		}
		return "Observed condition: not met."
	case "met":
		if !verification.ConditionReviewComplete {
			return "Observed condition: met. Other recorded checks remain unresolved."
		}
		return strings.TrimSpace("Observed condition: met. " + offlineVerificationNotice(verification))
	case "unknown":
		return "Observed condition: undetermined."
	}
	switch verification.State {
	case "passed":
		return offlineVerificationNotice(verification)
	case "not_run":
		return "Verification incomplete: no structured check evidence after file changes; use verify for the relevant check."
	case "stale":
		return "Verification incomplete: checks ran before the latest file change."
	case "failed":
		return "Verification failed."
	case "blocked":
		return "Verification blocked."
	case "partial":
		return "Verification incomplete: some checks were blocked."
	default:
		if summary := strings.TrimSpace(verification.Summary); summary != "" {
			return "Verification: " + summary
		}
		return ""
	}
}
