package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

type recordedEvidencePayload struct {
	Evidence kernel.RunEvidence `json:"evidence"`
}

// evidenceOutcome derives a verification verdict from durable runtime events.
// Model prose is intentionally not an input to the verdict.
func (c *RunCoordinator) evidenceOutcome(ctx context.Context, taskID, runID string) (*api.VerificationOutcome, []string) {
	if c == nil || c.srv == nil || c.srv.Control == nil || taskID == "" || runID == "" {
		return nil, nil
	}
	events, err := c.srv.Control.ListTaskEvents(ctx, taskID, 200)
	if err != nil {
		return nil, nil
	}

	var evidence []kernel.RunEvidence
	var files []string
	for _, event := range events {
		if event.RunID != runID || event.Type != "evidence.recorded" {
			continue
		}
		var payload recordedEvidencePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Evidence.ToolName == "" {
			continue
		}
		evidence = append(evidence, payload.Evidence)
		if payload.Evidence.Kind == "mutation" && payload.Evidence.Status == "succeeded" {
			for _, effect := range payload.Evidence.Files {
				if effect.BeforeSHA256 != effect.AfterSHA256 {
					files = appendUnique(files, effect.Path, 32)
				}
			}
		}
	}
	if len(evidence) == 0 {
		return nil, files
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].StartedAt < evidence[j].StartedAt })

	result := &api.VerificationOutcome{}
	for _, item := range evidence {
		if item.Kind == "mutation" && item.Status == "succeeded" && evidenceChangedFiles(item.Files) && item.FinishedAt > result.LatestMutationAt {
			result.LatestMutationAt = item.FinishedAt
		}
		if item.Kind != "verification" || item.Command == nil {
			continue
		}
		result.Checks = append(result.Checks, api.VerificationCheck{
			Kind:       item.Command.Kind,
			Command:    item.Command.Command,
			CWD:        item.Command.CWD,
			Status:     item.Status,
			ExitCode:   item.Command.ExitCode,
			StartedAt:  item.StartedAt,
			FinishedAt: item.FinishedAt,
		})
	}

	result.State, result.Summary = verificationState(result.LatestMutationAt, result.Checks)
	return result, files
}

func evidenceChangedFiles(files []kernel.FileEffect) bool {
	for _, effect := range files {
		if effect.BeforeSHA256 != effect.AfterSHA256 {
			return true
		}
	}
	return false
}

func verificationState(latestMutation int64, checks []api.VerificationCheck) (string, string) {
	if len(checks) == 0 {
		if latestMutation == 0 {
			return "not_applicable", "No code mutation or verification was recorded."
		}
		return "not_run", "Files changed, but no verification command was recorded after the change."
	}

	current := checks
	if latestMutation > 0 {
		current = nil
		for _, check := range checks {
			if check.StartedAt >= latestMutation {
				current = append(current, check)
			}
		}
		if len(current) == 0 {
			return "stale", "Verification exists, but it ran before the latest file change."
		}
	}

	current = latestVerificationAttempts(current)
	passed, failed, blocked := 0, 0, 0
	for _, check := range current {
		switch check.Status {
		case "succeeded":
			passed++
		case "blocked":
			blocked++
		default:
			failed++
		}
	}
	switch {
	case failed > 0:
		return "failed", fmt.Sprintf("%d current verification check(s) failed.", failed)
	case passed > 0 && blocked == 0:
		return "passed", fmt.Sprintf("%d current verification check(s) passed.", passed)
	case passed == 0 && blocked > 0:
		return "blocked", fmt.Sprintf("%d verification check(s) were blocked.", blocked)
	default:
		return "partial", fmt.Sprintf("%d verification check(s) passed and %d were blocked.", passed, blocked)
	}
}

// latestVerificationAttempts lets a corrected retry of the same check replace
// its earlier result. Distinct commands remain independent so a lightweight
// fallback cannot hide a failed build or test suite.
func latestVerificationAttempts(checks []api.VerificationCheck) []api.VerificationCheck {
	latest := make(map[string]api.VerificationCheck, len(checks))
	order := make([]string, 0, len(checks))
	for _, check := range checks {
		key := strings.TrimSpace(check.Kind) + "\x00" + strings.TrimSpace(check.Command) + "\x00" + strings.TrimSpace(check.CWD)
		previous, exists := latest[key]
		if !exists {
			order = append(order, key)
		}
		if !exists || check.FinishedAt >= previous.FinishedAt {
			latest[key] = check
		}
	}
	result := make([]api.VerificationCheck, 0, len(latest))
	for _, key := range order {
		result = append(result, latest[key])
	}
	return result
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

func mergeEvidenceFiles(verification *api.VerificationOutcome, evidenceFiles, claimedFiles []string) (*api.VerificationOutcome, []string) {
	files := append([]string(nil), claimedFiles...)
	for _, path := range evidenceFiles {
		files = appendUnique(files, path, 32)
	}
	return verification, files
}

func verificationRequiresResume(verification *api.VerificationOutcome, changedFiles []string) bool {
	if verification == nil {
		return false
	}
	switch verification.State {
	case "failed", "stale", "blocked", "partial":
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
	switch verification.State {
	case "failed":
		return "Fix the failed verification, then run it again."
	case "stale":
		return "Run verification again after the latest file change."
	case "blocked", "partial":
		return "Resolve the blocked verification and continue."
	default:
		return "Run an appropriate test, build, lint, syntax, or smoke check and continue."
	}
}

func withVerificationNotice(content string, verification *api.VerificationOutcome, mismatches []string) string {
	if verification == nil || verification.State == "not_applicable" {
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

func conciseVerificationNotice(verification *api.VerificationOutcome) string {
	if verification == nil {
		return ""
	}
	switch verification.State {
	case "passed":
		return ""
	case "not_run":
		return "Verification incomplete: no check ran after file changes."
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
