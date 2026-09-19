package tools

import (
	"encoding/json"
	"strings"
	"sync"
)

// Guardian-style triage (batch C2).
//
// The one-word verdict answered "is this command safe?" and nothing else. Two
// things were missing, and both matter more than the word itself:
//
//   - WHY. A verdict with no rationale is unusable at the moment it matters: the
//     ask still reaches a human, who now has to redo the reasoning the judge
//     already did.
//   - WHO ASKED FOR IT. Risk and authorization are independent axes. `rm -rf
//     build` is low risk and, if the person just said "clean the build", fully
//     authorized; the same command with no such instruction is the model acting
//     on its own. Collapsing both into one word throws away the distinction that
//     actually predicts whether a person would say yes.
//
// The judge is therefore asked for strict JSON with four fields, and the parse
// still fails SAFE: malformed output, a missing outcome, or an unparseable risk
// level all degrade to ESCALATE (a human decides), never to APPROVE.

// TriageAssessment is the judge's structured answer. Empty fields mean the judge
// did not supply them (e.g. a legacy one-word reply), which is not an error.
type TriageAssessment struct {
	Response    ApprovalResponseMetadata `json:"-"`
	Restriction *RestrictionEvidence     `json:"restriction,omitempty"`
	// Risk is low|medium|high|critical.
	Risk string `json:"risk_level"`
	// Authorization is unknown|low|medium|high: how directly the person's own
	// words authorize this action.
	Authorization string `json:"user_authorization"`
	// Outcome is approve|deny|escalate.
	Outcome string `json:"outcome"`
	// Rationale is one short sentence, shown to the person at decision time.
	Rationale string `json:"rationale"`
}

// triageRationaleMaxRunes bounds the rationale that reaches a UI surface and a
// stored approval row. A judge that writes an essay must not be able to push the
// answer options off a panel.
const triageRationaleMaxRunes = 240

// guardianJudgePrompt is the JSON contract appended to the triage prompt. It is
// separate from buildTriagePrompt's injection defense so the two can be reasoned
// about independently.
const guardianJudgePrompt = `Return exactly one JSON object. All four fields, including outcome, are required:
{"outcome":"approve|deny|escalate","risk_level":"low|medium|high|critical","user_authorization":"unknown|low|medium|high","rationale":"one short sentence naming the actual target and applicable human evidence"}

You decide whether this specific operation needs additional human confirmation in smart mode. The flag explains why this review was invoked; it is not a verdict that a human must approve. Your approve verdict authorizes only this operation, subject to independently enforced safety and scope boundaries. Do not require a pre-existing approval grant as a condition for making this approval decision.

Evaluate the ACTUAL operation's targets and effects, not a similar action from the proposal. Read the person's request with the proposal they answered and later corrections. Resolve references, exceptions, timing, and conditions. A broad goal does not override an excluded item or a requirement to wait for confirmation. Creating the deliverable is execution, not just proposing it. System continuation preserves earlier human evidence but adds no authorization.

risk_level describes blast radius using the supplied filesystem, network and credential facts. Low risk or a temporary path is not permission.
user_authorization describes support for this exact effect in the accepted scope. Necessary routine inspection and verification may be implicit in the broader task; exact command wording is unnecessary.
outcome chooses by cause:
- deny: the actual operation conflicts with a clear applicable human restriction, or is intrinsically forbidden. Do not ask to override a known restriction. An excluded optional action is prohibited even if the main task is authorized.
- escalate: the evidence or authorization for the actual effects is missing or uncertain, or an explicit execution restriction requires a human decision. Unknown authority is not a denial. Arbitrary code with shared network or credentials, privileged execution, and uncontained unknown effects require human confirmation.
- approve: the effect is within accepted scope, respects all applicable constraints, and has bounded risk under the supplied execution facts. Routine local edits and observations may qualify. A host or shared-network label describes available capabilities, not proof that this operation uses network, credentials, or unbounded code. Judge its actual effects; do not invent a separate missing grant for an authorized, bounded local observation. Explicit execution restrictions still apply. Unrelated restrictions do not block accepted work.

When uncertain, escalate. Always include outcome and cite the applicable evidence in rationale.`

// A version-3 judgment is a structured decision, not a prose inference. Legacy
// snapshots retain bare-word compatibility in parseTriageAssessment.
func validateStructuredTriageReply(raw string) *ApprovalResponseError {
	payload, ok := extractJSONObject(raw)
	var assessment TriageAssessment
	if !ok || json.Unmarshal([]byte(payload), &assessment) != nil {
		return &ApprovalResponseError{Class: "invalid_json"}
	}
	outcome := strings.ToLower(strings.TrimSpace(assessment.Outcome))
	if normalizeGuardianField(assessment.Risk, guardianRiskLevels) == "" || normalizeGuardianField(assessment.Authorization, guardianAuthorizationLevels) == "" || strings.TrimSpace(assessment.Rationale) == "" || (outcome != "approve" && outcome != "deny" && outcome != "escalate") {
		return &ApprovalResponseError{Class: "incomplete_decision"}
	}
	return nil
}

// parseTriageAssessment reads the judge's reply. It accepts the JSON contract
// first, then falls back to the historical bare verdict word so an older or
// simpler judge keeps working. Anything else escalates.
func parseTriageAssessment(raw string) (TriageVerdict, TriageAssessment) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return TriageEscalate, TriageAssessment{}
	}
	if payload, ok := extractJSONObject(trimmed); ok {
		var assessment TriageAssessment
		if err := json.Unmarshal([]byte(payload), &assessment); err == nil {
			assessment.Risk = normalizeGuardianField(assessment.Risk, guardianRiskLevels)
			assessment.Authorization = normalizeGuardianField(assessment.Authorization, guardianAuthorizationLevels)
			assessment.Rationale = truncateRunes(toSingleLine(RedactSensitive(assessment.Rationale)), triageRationaleMaxRunes)
			switch strings.ToLower(strings.TrimSpace(assessment.Outcome)) {
			case "approve":
				assessment.Outcome = "approve"
				return TriageApprove, assessment
			case "deny":
				assessment.Outcome = "deny"
				return TriageDeny, assessment
			default:
				// Includes "escalate" and any unknown value: a judge that cannot
				// name its own outcome has not ruled.
				assessment.Outcome = "escalate"
				return TriageEscalate, assessment
			}
		}
	}
	// Legacy one-word contract.
	return parseTriageVerdict(trimmed), TriageAssessment{}
}

var guardianRiskLevels = map[string]struct{}{"low": {}, "medium": {}, "high": {}, "critical": {}}

var guardianAuthorizationLevels = map[string]struct{}{"unknown": {}, "low": {}, "medium": {}, "high": {}}

// normalizeGuardianField keeps only values the contract defines, so a stored or
// displayed level can never be free text from the model.
func normalizeGuardianField(value string, allowed map[string]struct{}) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if _, ok := allowed[normalized]; ok {
		return normalized
	}
	return ""
}

// extractJSONObject finds the outermost JSON object in a reply that may be
// wrapped in prose or a fenced code block. It scans braces rather than trusting
// the model to emit bare JSON.
func extractJSONObject(raw string) (string, bool) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return raw[start : end+1], true
}

// Guardian denial circuit breaker.
//
// A DENY is returned to the model as a user-style rejection, which is correct —
// but a model that keeps proposing variants of a denied action turns the funnel
// into a treadmill: the judge burns calls, the person sees nothing, and the run
// makes no progress. codex interrupts the turn after repeated guardian denials.
// SelfMind takes the conservative version of the same idea: after
// triageDenyBreakerLimit consecutive denials inside ONE run, triage stops ruling
// and the next dangerous op goes to the HUMAN. That moves the loop to the person
// who can end it, and never converts a denial into an approval.

// triageDenyBreakerLimit is how many consecutive judge denials in one run trip
// the breaker. Two is deliberate: one denial is normal, a second means the model
// is circling.
const triageDenyBreakerLimit = 2

var triageDenyBreaker = struct {
	mu   sync.Mutex
	runs map[string]int
}{runs: map[string]int{}}

// triageDenyBreakerTripped reports whether this run has already collected enough
// consecutive denials that triage should step aside.
func triageDenyBreakerTripped(runID string) bool {
	key := strings.TrimSpace(runID)
	if key == "" {
		return false
	}
	triageDenyBreaker.mu.Lock()
	defer triageDenyBreaker.mu.Unlock()
	return triageDenyBreaker.runs[key] >= triageDenyBreakerLimit
}

// recordTriageDenial counts one denial for a run and reports whether that tripped
// the breaker.
func recordTriageDenial(runID string) bool {
	key := strings.TrimSpace(runID)
	if key == "" {
		return false
	}
	triageDenyBreaker.mu.Lock()
	defer triageDenyBreaker.mu.Unlock()
	if len(triageDenyBreaker.runs) > triageBreakerMaxRuns {
		// Bounded: a long-lived daemon must not accumulate a counter per run
		// forever. Runs are terminal, so dropping the table is safe — the worst
		// case is one extra judge call for a run mid-loop.
		triageDenyBreaker.runs = map[string]int{}
	}
	triageDenyBreaker.runs[key]++
	return triageDenyBreaker.runs[key] >= triageDenyBreakerLimit
}

// clearTriageDenials resets a run's streak after any non-denial outcome: the
// breaker must measure CONSECUTIVE denials, not lifetime ones.
func clearTriageDenials(runID string) {
	key := strings.TrimSpace(runID)
	if key == "" {
		return
	}
	triageDenyBreaker.mu.Lock()
	defer triageDenyBreaker.mu.Unlock()
	delete(triageDenyBreaker.runs, key)
}

const triageBreakerMaxRuns = 256
