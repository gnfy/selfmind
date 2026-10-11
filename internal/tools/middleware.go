package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

type ApprovalResumeAuthorizationStore interface {
	ClaimApprovalResumeAuthorization(ctx context.Context, tenantID, personID, runID, fingerprint string) (approvalID, decisionID, grantKey string, claimed bool, err error)
}

func AuthMiddleware(mem interface {
	GetPermission(ctx context.Context, tenantID, toolName string) (bool, error)
}) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			tenantID, _ := args["_tenant_id"].(string)
			toolName, _ := args["_tool_name"].(string)
			if tenantID == "" {
				return "", fmt.Errorf("[Auth] tenantID not found in args")
			}
			if mem != nil {
				allowed, err := mem.GetPermission(context.Background(), tenantID, toolName)
				if err == nil && !allowed {
					return "", fmt.Errorf("[Auth] tenant %s is not allowed to use tool %s", tenantID, toolName)
				}
			}
			return next(args)
		}
	}
}

func ApprovalMiddleware(dryRun bool) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			if dryRun {
				log.Warn("approval middleware: dry_run=true, tool execution pending")
				return "", fmt.Errorf("[Approval] dry_run=true, tool execution pending approval")
			}
			log.Debug("approval middleware executing")
			return next(args)
		}
	}
}

type RateLimitMiddleware struct {
	maxCalls int

	mu    sync.Mutex
	count int
}

func RateLimit(maxCalls int) *RateLimitMiddleware {
	return &RateLimitMiddleware{maxCalls: maxCalls}
}

func (r *RateLimitMiddleware) Middleware(next ToolExecutor) ToolExecutor {
	return func(args map[string]interface{}) (string, error) {
		r.mu.Lock()
		if r.count >= r.maxCalls {
			r.mu.Unlock()
			return "", fmt.Errorf("rate limit exceeded: max %d calls", r.maxCalls)
		}
		r.count++
		r.mu.Unlock()
		return next(args)
	}
}

func LoggingMiddleware(next ToolExecutor) ToolExecutor {
	return func(args map[string]interface{}) (string, error) {
		log.Debug("tool executing", "args", RedactSensitive(MarshalArgs(args)))
		result, err := next(args)
		if err != nil {
			log.Debug("tool error", "error", RedactSensitive(err.Error()))
		} else {
			log.Debug("tool result", "result", func() string {
				// Mask before cutting: a cut secret is no longer recognizable.
				masked := RedactSensitive(result)
				if len(masked) > 200 {
					return masked[:200] + "..."
				}
				return masked
			}())
		}
		return result, err
	}
}

func TenantIsolationMiddleware(tenantID string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			args["_tenant_id"] = tenantID
			return next(args)
		}
	}
}

func EnvVarMiddleware(requiredVars ...string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			for _, v := range requiredVars {
				if os.Getenv(v) == "" {
					return "", fmt.Errorf("missing required environment variable: %s", v)
				}
			}
			return next(args)
		}
	}
}

// ApprovalMode is the codex-style per-turn approval policy. It controls WHICH
// tool calls need human approval, layered on top of the dangerous-op heuristic.
type ApprovalMode string

const (
	// ApprovalOnRequest asks only when an op trips the dangerous-op
	// heuristic (destructive command, restricted/out-of-workspace path).
	ApprovalOnRequest ApprovalMode = "on-request"
	// ApprovalReadOnly asks before ANY file write/edit or command execution.
	ApprovalReadOnly ApprovalMode = "read-only"
	// ApprovalAutoEdit auto-approves in-workspace file edits but asks before
	// running commands (and before edits the heuristic flags as dangerous).
	ApprovalAutoEdit ApprovalMode = "auto-edit"
	// ApprovalFullAuto auto-approves everything (workspace scope still applies,
	// and the hard-floor deny set in hardlineToolCall still fires).
	ApprovalFullAuto ApprovalMode = "full-auto"
	// ApprovalSmart is the layered funnel mode. It gates on a dangerous op like
	// on-request, but (H2) inserts an LLM triage step between the
	// session/persistent allowlist and the human ask: a cheap judge auto-approves
	// clearly-safe ops (recording a task-scope class grant so it is asked at most
	// once per class per task), blocks clearly-damaging ones, and escalates
	// everything uncertain to the human ask. With no judge installed it degrades
	// to on-request (human ask) — it never auto-approves without a judge.
	ApprovalSmart ApprovalMode = "smart"

	// DefaultApprovalMode is used only when a person has not selected a mode and
	// the request does not carry one. Smart preserves the hard safety floor and
	// degrades to a human ask when no triage judge is available.
	DefaultApprovalMode ApprovalMode = ApprovalSmart
)

// NormalizeApprovalMode maps free-form input to a known mode, defaulting to
// on-request.
func NormalizeApprovalMode(s string) ApprovalMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read-only", "readonly", "read":
		return ApprovalReadOnly
	case "auto-edit", "autoedit", "auto", "edit":
		return ApprovalAutoEdit
	case "full-auto", "fullauto", "full", "yolo":
		return ApprovalFullAuto
	case "smart":
		return ApprovalSmart
	default:
		return ApprovalOnRequest
	}
}

// EffectiveApprovalMode resolves an optional persisted/request value. Empty
// selects the product default; invalid non-empty input remains fail-safe and
// normalizes to on-request.
func EffectiveApprovalMode(s string) ApprovalMode {
	if strings.TrimSpace(s) == "" {
		return DefaultApprovalMode
	}
	return NormalizeApprovalMode(s)
}

// ApprovalModeAutonomy ranks a mode by how much it lets a Run do before a
// person is asked: read-only 0, on-request and auto-edit 1 (every command
// asks, edits flow), smart 2 (a command the sandbox contains does not ask, and
// a judge may answer for the person), full-auto 3. approvalNeeded is the
// contract the order follows: a lower rank asks at least wherever a higher one
// does (TestApprovalModeAutonomyFollowsWhatEachModeAsks).
func ApprovalModeAutonomy(mode ApprovalMode) int {
	switch mode {
	case ApprovalReadOnly:
		return 0
	case ApprovalFullAuto:
		return 3
	case ApprovalSmart:
		return 2
	}
	return 1
}

// NarrowerApprovalMode is the one of two modes that lets a Run do less before
// asking. On a tie it keeps a.
func NarrowerApprovalMode(a, b ApprovalMode) ApprovalMode {
	if ApprovalModeAutonomy(b) < ApprovalModeAutonomy(a) {
		return b
	}
	return a
}

// IsKnownApprovalModeWord reports whether s is one of the accepted mode words
// (including the aliases NormalizeApprovalMode understands). It is the single
// authoritative word list shared by every /mode entry point, so a typo is
// rejected instead of silently defaulting to on-request — NormalizeApprovalMode
// alone cannot tell "on-request" from garbage since both map to ApprovalOnRequest.
func IsKnownApprovalModeWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on-request", "onrequest", "request",
		"read-only", "readonly", "read",
		"auto-edit", "autoedit", "auto", "edit",
		"full-auto", "fullauto", "full", "yolo",
		"smart":
		return true
	default:
		return false
	}
}

// CanonicalApprovalModes returns the canonical mode strings only (no aliases),
// the single source for the CLI --mode flag, which is intentionally stricter
// than the /mode command and rejects aliases like "yolo".
func CanonicalApprovalModes() []string {
	return []string{
		string(ApprovalOnRequest),
		string(ApprovalReadOnly),
		string(ApprovalAutoEdit),
		string(ApprovalFullAuto),
		string(ApprovalSmart),
	}
}

var writeTools = map[string]struct{}{
	"write_file": {}, "patch": {}, "edit": {}, "apply_patch": {}, "edit_file": {},
}
var execTools = map[string]struct{}{
	"terminal": {}, "verify": {}, "execute_command": {}, "shell": {}, "execute_code": {}, "watch_external": {},
}

const unclassifiedExternalApprovalReason = "unclassified external tool requires one-time human approval"

func isWriteTool(name string) bool { _, ok := writeTools[name]; return ok }
func isExecTool(name string) bool  { _, ok := execTools[name]; return ok }

// delegatedExecTools hand a command to the DAEMON to run later, unattended,
// instead of running it inside this turn. The distinction matters to a
// prohibition: "do not execute the polling command directly" forbids the agent
// running it now, and asks for exactly this delegation instead.
var delegatedExecTools = map[string]struct{}{
	"watch_external": {},
}

var deleteExecBinaries = map[string]struct{}{
	"rm": {}, "rmdir": {}, "unlink": {}, "shred": {}, "wipefs": {},
}

// operationClassesFor names what this call can do, in the same vocabulary a
// prohibition is expressed in. `dangerous` only contributes a fallback class:
// the heuristic says an op looked risky, which is never something the person
// said, so it must not make an unrelated deny apply.
func operationClassesFor(toolName string, args map[string]interface{}, dangerous bool) []OperationClass {
	var classes []OperationClass
	if policy, ok := args[toolExecutionPolicyArg].(toolExecutionPolicy); ok {
		classes = append(classes, policy.OperationClasses...)
	}
	if unclassifiedExternalToolCall(args) {
		// An external server can perform any of these effects and its description
		// is untrusted. Keep explicit user prohibitions effective until an
		// operator-owned per-tool policy can narrow the capability.
		classes = append(classes, OpClassWrite, OpClassDelete, OpClassExecInTurn, OpClassNetwork)
	}
	if isWriteTool(toolName) {
		classes = append(classes, OpClassWrite)
	}
	if isExecTool(toolName) {
		if observationOnlyExec(toolName, args) {
			classes = append(classes, OpClassObserve)
		} else if _, delegated := delegatedExecTools[toolName]; delegated {
			classes = append(classes, OpClassExecDelegated)
		} else {
			classes = append(classes, OpClassExecInTurn)
		}
		command := execCommandPayload(toolName, args)
		commands := effectiveCommands(command)
		if network, _ := egressIn(command, commands); network {
			classes = append(classes, OpClassNetwork)
		}
		// A call that receives the daemon's network is a network operation,
		// whatever made it so: a package install or a git remote the capability
		// middleware recognised, a script that asked with `network: true`, a
		// trusted workspace. "Do not use the network" then reaches every call
		// that can, not only the ones that name an egress program.
		if shared, _ := args["_network_shared"].(bool); shared {
			classes = append(classes, OpClassNetwork)
		}
		for _, read := range commands {
			program := read.id.program()
			_, deletes := deleteExecBinaries[program.Name]
			if deletes || (program.Name == "find" && slices.Contains(program.Args, "-delete")) {
				classes = append(classes, OpClassDelete)
				break
			}
		}
	}
	if (toolName == "patch" || toolName == "apply_patch") && strings.Contains(stringArg(args, "patch"), "*** Delete File:") {
		classes = append(classes, OpClassDelete)
	}
	if dangerous {
		classes = append(classes, OpClassDangerous)
	}
	return uniqueOperationClasses(classes)
}

func uniqueOperationClasses(classes []OperationClass) []OperationClass {
	seen := map[OperationClass]bool{}
	out := make([]OperationClass, 0, len(classes))
	for _, class := range classes {
		if class == "" || seen[class] {
			continue
		}
		seen[class] = true
		out = append(out, class)
	}
	return out
}

// operationTargetsFor collects the literal objects this call acts on, so a
// prohibition that named a file constrains that file and not its neighbours.
func operationTargetsFor(args map[string]interface{}) []string {
	if args == nil {
		return nil
	}
	var targets []string
	for _, key := range []string{"path", "file_path", "filename", "target", "command", "code", "cmd"} {
		if value := strings.TrimSpace(stringArg(args, key)); value != "" {
			targets = append(targets, value)
		}
	}
	return targets
}

// approvalNeeded decides whether a tool call requires human approval under the
// given mode. dangerous is the dangerous-op heuristic result; contained is the
// sandbox-containment judgement (execSandboxContained) and is only ever true for
// an exec call proven to run isolated with no network.
func approvalNeeded(mode ApprovalMode, toolName string, dangerous, contained bool) bool {
	switch mode {
	case ApprovalFullAuto:
		return false
	case ApprovalSmart:
		// A dangerous op always asks: out-of-workspace targets, destructive
		// programs, and network egress keep their gate no matter what the
		// sandbox can do.
		if dangerous {
			return true
		}
		// Exec asks UNLESS the sandbox can contain it (batch C1). Arbitrary
		// commands/code are not approval-worthy because they are commands; they
		// are approval-worthy because of what they can reach. An isolated,
		// network-disabled run can only touch workspace files — the same blast
		// radius as the write tools smart mode already allows unprompted — so
		// asking about it was fatigue with no safety return. Everything the
		// sandbox cannot contain (host mode, missing sandbox platform, egress
		// policy on) still asks and is still triaged.
		//
		// The `dangerous` heuristic remains a gate, never an authorization: it
		// once inspected only args["command"], so execute_code's args["code"]
		// payload slid past it entirely and ran with NO approval.
		if isExecTool(toolName) {
			return !contained
		}
		return false
	case ApprovalReadOnly:
		return isWriteTool(toolName) || isExecTool(toolName) || dangerous
	case ApprovalAutoEdit:
		// Edits flow freely (unless the heuristic flags them, e.g. out of
		// workspace); command execution always asks.
		return isExecTool(toolName) || dangerous
	default: // on-request
		// Arbitrary-code exec (terminal/shell/execute_command/execute_code)
		// ALWAYS asks, even when the dangerous heuristic does not fire: the
		// heuristic is a read-side optimization, not an authorization for
		// unprompted code execution. Non-exec tools stay gated on `dangerous`.
		return isExecTool(toolName) || dangerous
	}
}

// ModeDecision is the outcome of evaluating a SINGLE known operation against an
// approval mode (plus the hard floor and, for smart mode, the LLM judge). It is
// returned by EvaluateModeDecision so callers outside the live middleware — most
// notably the /mode retro-resolution of already-pending approvals — can reuse the
// exact funnel layering without re-running a real tool call.
type ModeDecision int

const (
	// ModeAsk: leave the decision to the human ask (or, for a pending approval,
	// keep it pending). This is the fail-safe default: the hard floor, any
	// uncertainty, an escalating/absent judge, or an ask-always mode all land here.
	ModeAsk ModeDecision = iota
	// ModeApprove: the mode (or an APPROVE judge verdict) auto-approves this op.
	ModeApprove
	// ModeDeny: a smart-mode judge returned DENY — block as a user-style decision
	// (do not retry), never as a diagnosable failure.
	ModeDeny
)

// EvaluateModeDecision classifies ONE already-known operation under an approval
// mode, mirroring SmartApprovalMiddleware's layering for a single call: the hard
// floor first (authoritative — a hardline op is NEVER auto-approved, it returns
// ModeAsk), then the mode gate (approvalNeeded), then, for smart mode only, the
// cheap LLM judge. It deliberately does NOT consult class grants; the caller owns
// that. It is used by the /mode command to re-evaluate approvals that were
// already pending when the mode changed, so a switch to smart/full-auto/auto-edit
// unblocks a run that was stuck on a human ask.
//
// dangerousHint lets a caller preserve a dangerous classification the local
// recompute cannot reproduce — e.g. a pending approval whose out-of-workspace
// path check needed the original projectRoot. It only ever RAISES danger (it is
// OR'd with the recompute), so it can never silently downgrade a flagged op.
// A nil judge in smart mode falls through to ModeAsk (never auto-approves).
func EvaluateModeDecision(ctx context.Context, mode ApprovalMode, projectRoot, toolName string, args map[string]interface{}, reason string, dangerousHint bool, judge ApprovalJudge) ModeDecision {
	if args == nil {
		args = map[string]interface{}{}
	}
	args["_tool_name"] = toolName
	annotateEffectiveSandboxMode(args)
	// Layer 1: hard floor. Authoritative — no mode bypasses it, and it is never
	// auto-approved here. Left to a human (ModeAsk); in practice a hardline op
	// never reaches a pending-approval row, but stay conservative regardless.
	if blocked, _ := hardlineToolCall(projectRoot, toolName, args); blocked {
		return ModeAsk
	}
	dangerous, dreason := dangerousToolCall(projectRoot, toolName, args)
	dangerous = dangerous || dangerousHint
	if strings.TrimSpace(reason) == "" {
		reason = dreason
	}
	// External tools whose effects have not been classified remain a once-only
	// human decision even after a mode switch. The persisted approval path no
	// longer has the internal descriptor, so the stable locally-authored reason
	// preserves the classification without trusting the tool's name.
	if unclassifiedExternalToolCall(args) || strings.HasPrefix(reason, unclassifiedExternalApprovalReason) {
		return ModeAsk
	}
	// Layer 2: mode gate. If the mode would not require an ask for this op, it is
	// auto-approved (full-auto for everything; auto-edit for non-dangerous edits).
	//
	// Containment is deliberately passed as FALSE here. This classifier also
	// re-evaluates ALREADY-PENDING approvals from redacted display args, where a
	// host-mode call's effective-mode marker may be gone; claiming containment
	// from a policy inference would auto-approve a host escape that the person
	// was asked about. A pending op is therefore never released by containment —
	// only by an explicit mode that covers it, or by a judge/human decision.
	if !approvalNeeded(mode, toolName, dangerous, false) {
		return ModeApprove
	}
	// Layer 4: smart-mode LLM triage. APPROVE auto-runs, DENY blocks as a
	// decision, everything else (ESCALATE / no judge / error / timeout) falls
	// through to the human ask — fail SAFE, exactly like the middleware.
	if mode == ApprovalSmart && judge != nil {
		// No intent context here: this path re-judges an ALREADY-PENDING approval
		// from persisted args, where the run's live conversation is not available.
		// Missing intent reads as unknown authorization, which biases the judge
		// toward escalating — the safe direction for a retro decision.
		verdict, _, _ := triageApproval(ctx, judge, toolName, triageSubject(toolName, args), reason, "")
		switch verdict {
		case TriageApprove:
			return ModeApprove
		case TriageDeny:
			return ModeDeny
		}
	}
	// Layer 5: human ask.
	return ModeAsk
}

// SmartApprovalMiddleware is the layered approval funnel that gates dangerous
// tool calls. Layers, in order:
//
//  1. Hard floor (hardlineToolCall): an unbypassable deny set. It fires BEFORE
//     the mode bypass — full-auto included — because these ops have a blast
//     radius no session "yes" should authorize. A hardline hit returns
//     "operation blocked: ..." which is deliberately DISTINCT from the user
//     rejection contract: kernel's isUserRejectionErr must NOT match it, so the
//     model is told this is a safety-policy block (do not retry), not a user
//     decision it might reword.
//  2. Mode bypass (approvalNeeded): full-auto/auto-edit/etc. skip the ask.
//  3. Grants: exact unchanged model decisions may be reused. Version-3 smart
//     effects still require authorization review despite older capability grants.
//  4. LLM triage (H2), smart mode only: a cheap judge reviews the actual effect
//     (APPROVE auto-runs + records an exact run decision; DENY blocks as a
//     do-not-retry decision; ESCALATE / no judge / any error / timeout falls
//     through to the human ask). It fails SAFE — never auto-approves without a
//     clear APPROVE from an installed judge — and sits strictly below the hard
//     floor, which returned long before this point.
//  5. Human ask (scope.Approval / clarify). An "approve + remember" decision
//     records a grant for the next same-class call.
func SmartApprovalMiddleware(projectRoot string, prechecks ...func(map[string]interface{}) error) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)
			annotateEffectiveSandboxMode(args)
			scope, hasScope := currentExecutionScopeAny(args)
			defer yieldQueuedHostExecution(args)
			effectiveRoot := projectRoot
			if hasScope {
				effectiveRoot = approvalProjectRoot(projectRoot, scope, args)
			}

			// Layer 1: hard floor. Unconditional — no mode, not even full-auto,
			// can bypass it.
			if blocked, reason := hardlineToolCall(effectiveRoot, toolName, args); blocked {
				// Distinct from the rejection contract (see isUserRejectionErr):
				// this is a safety-policy block, so the model must not retry any
				// variant, but it is not a user decision.
				return "", fmt.Errorf("operation blocked by safety policy: %s (do not retry; this is a hard safety limit, not a user rejection)", reason)
			}
			// Isolation was demanded but cannot be enforced for this call: no
			// grant, judge or person is asked to approve a boundary that would
			// not be the one enforced.
			if refusal := strings.TrimSpace(stringArg(args, sandboxPlanRefusalArg)); refusal != "" && isExecTool(toolName) {
				return "", unenforceableBoundaryError(refusal)
			}

			dangerous, reason := dangerousToolCall(effectiveRoot, toolName, args)
			dangerousReason := reason
			externalUnknown := unclassifiedExternalToolCall(args)
			// A Run keeps the mode chosen at admission. A later person-level
			// /mode change applies to new Runs, not this one's authority.
			mode := ApprovalOnRequest
			if hasScope && scope.ApprovalMode != "" {
				mode = scope.ApprovalMode
			}

			// Durable watcher finalization is deliberately unattended. It consumes
			// authoritative watcher evidence and may update files inside the active
			// workspace, but it must never wait for a human or independently re-run
			// shell/network checks. Rejecting immediately uses the stable
			// user-decision contract, so the model cannot retry command variants.
			if hasScope && scope.ExecutionProfile == ExecutionProfileWatchFinalization {
				if isExecTool(toolName) {
					return "", rejectOperation(rejectionCodeCapability, "operation rejected: unattended watcher finalization cannot run shell or terminal commands; use recorded watcher evidence and file tools, or finish waiting_user if evidence is insufficient")
				}
				if dangerous {
					return "", rejectOperation(rejectionCodeCapability, "operation rejected: unattended watcher finalization cannot perform privileged or out-of-workspace operations; finish waiting_user instead")
				}
				return next(args)
			}

			// Read-only admission checks run after the hard floor and execution
			// classification, before grants, judge calls, or a human ask. They do
			// not reserve resources or authorize dispatch; the inner middleware
			// still claims atomically after approval.
			for _, check := range prechecks {
				if err := check(args); err != nil {
					return "", err
				}
			}

			// Layer 2: mode bypass, including sandbox containment (C1). A
			// contained exec call is recorded so /diag can show how much of the
			// old ask volume was pure fatigue rather than judgement.
			containment := assessExecContainment(toolName, args)
			if isExecTool(toolName) {
				// Whatever releases this call below covers exactly these
				// in-place credential writes; dispatch refuses a plan that
				// writes more.
				args[approvedHostWritesArg] = append([]string{}, containment.HostCredentialWrites...)
			}
			hostOnce := mode == ApprovalSmart && isExecTool(toolName) && effectiveSandboxModeArg(args) == SandboxHost
			hostWrites := hostCredentialWritesBinding(containment.HostCredentialWrites)
			proxyReason := configuredProxyOverrideReason(toolName, args)
			proxyOverride := proxyReason != ""
			intentSnapshot := RunIntentSnapshot{}
			if hasScope && scope.IntentSnapshot != nil {
				intentSnapshot = scope.IntentSnapshot()
			} else if hasScope && scope.TriageIntent != nil {
				intentSnapshot.RawUserText = scope.TriageIntent()
			}
			// A prohibition constrains the operation it points at, not every
			// side-effecting tool in the run. "Do not modify files" used to
			// stop a read-only `go test` probe and a durable-watch
			// registration alike, forcing a human ask that nobody was there
			// to answer.
			denyForcesHuman := intentSnapshot.DenyBlocks(
				operationClassesFor(toolName, args, dangerous),
				operationTargetsFor(args),
			)
			// Scope containment and older grants describe capabilities, not whether
			// this effect agrees with the person's current request. No language
			// parser chooses this review: every effect in the new smart contract
			// is judged, including an otherwise in-scope file write.
			//
			// Containment is the one exception, and it has to be, or the C1
			// release below is unreachable: the gateway marks every run
			// ModelAuthorization, so an unconditional semantic review made
			// `contained` dead for the whole deployment. The funnel proved it —
			// `contained 0` against 33 judged decisions in a day, while the
			// judge's own escalation rationales read "read-only ... but the
			// specific target wasn't named". That is the fatigue C1 exists to
			// remove, paid for with a model call each time.
			//
			// Nothing is widened here. Containment still means isolated,
			// ENFORCED, no egress and no credentials — or a declaratively proven
			// observation — and a dangerous op, an explicit deny, an unclassified
			// external effect, a write tool, and every uncontained exec all keep
			// their review. Only a call the runtime can already prove harmless
			// stops paying for a judgement about whether it was asked for.
			contained := containment.AutoApprove() && !denyForcesHuman && !proxyOverride
			// Parallel Runs approve shared-network and credential-bearing
			// commands exactly like a single Run (D7): full-auto runs them and
			// smart has the judge review them. Cross-Run safety comes from the
			// durable external-effect claim taken after this layer, whose only
			// human ask is another Run's left-uncertain effect on the same
			// target. When this layer asks anyway, that decision joins its ask.
			foreignClaims, _ := args[externalEffectForeignClaimsArg].([]control.ExternalEffectClaim)
			semanticReview := mode == ApprovalSmart && intentSnapshot.ModelAuthorization &&
				!contained && (isWriteTool(toolName) || isExecTool(toolName) || dangerous)
			if len(foreignClaims) == 0 && !semanticReview && !denyForcesHuman && !externalUnknown && !approvalNeeded(mode, toolName, dangerous, contained) {
				if contained && mode == ApprovalSmart && hasScope {
					recordScopeTriage(scope, toolName, "", TriageOutcomeContained, TriageAssessment{}, 0, nil)
					log.Debug("smart approval: sandbox-contained exec, no ask", "tool", toolName, "reason", containedExecReason, "assessment", containment.Summary())
				}
				return next(args)
			}
			// Review facts accumulate: a deny or a route change is added to what
			// made the operation dangerous, never substituted for it in the judge
			// prompt or the person's request.
			if denyForcesHuman {
				reason = appendApprovalReason("the current request contains an explicit deny; a person must confirm any override", reason)
			} else if !dangerous && reason == "" && !proxyOverride {
				reason = fmt.Sprintf("%s requires approval in %s mode", toolName, mode)
			}
			if proxyOverride {
				reason = appendApprovalReason(reason, proxyReason)
			}

			// Layer 3: class-level allowlist. A coarse pattern key identifies the
			// action CLASS; a matching task/person grant skips the human ask.
			// Hardline never reaches here, so a granted class can never cover a
			// hard-floor op. Every reusable key is bound to the call's in-place
			// credential writes: an approval covers the boundary it was given
			// for, so a changed write set asks again and an unchanged one does
			// not. The exact-run and decision keys carry them through the
			// containment summary.
			patternKey := bindHostCredentialWrites(approvalPatternKeyForScope(toolName, args, reason, scope, hasScope), hostWrites)
			exactRunKey := approvalExactRunKey(toolName, args, scope, hasScope, containment)
			declaredEffectKey := bindHostCredentialWrites(approvalDeclaredEffectKey(toolName, args, scope, hasScope), hostWrites)
			// Rule candidates (batch B2) are the narrow authorizations this call
			// could create — a command prefix, a host, a writable root — and the
			// keys a previously granted rule is looked up under.
			ruleCandidates := bindRuleCandidatesToHostWrites(approvalRuleCandidates(toolName, args, scope, reason), hostWrites,
				compactHostPaths(args, containment.HostCredentialWrites))
			targetKeys := approvalTargetRuleKeys(toolName, args, scope)
			resumeFingerprint := approvalResumeFingerprint(toolName, args, scope, containment.Summary())
			decisionKey := triageDecisionKey(resumeFingerprint, scope, intentSnapshot, mode)
			isRunGranted := func(key string) bool {
				return hasScope && scope.runGrants != nil && scope.runGrants.has(key)
			}
			switch {
			case denyForcesHuman:
			// A current deny outranks full-auto, containment, and every stored
			// grant. Continue directly to the human ask below.
			case len(foreignClaims) > 0:
				// No reusable grant or judge can resolve another Run's uncertain
				// effect. Join that mandatory one-time decision into this ask.
			case hostOnce:
				// D1: each host invocation needs a fresh decision in smart mode.
			case externalUnknown:
				// External unknown effects are deliberately once-only. Historical
				// broad grants and live run grants cannot release them.
			case isRunGranted(declaredEffectKey):
				// A person approved this byte-identical command as part of a bounded
				// phase. The hard floor and current deny already ran above; identity,
				// workspace, environment and effective sandbox are part of the key.
				recordScopeTriage(scope, toolName, declaredEffectKey, TriageOutcomeBundleHit, TriageAssessment{}, 0, nil)
				return next(args)
			case isRunGranted(decisionKey):
				recordScopeTriage(scope, toolName, decisionKey, TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
				return next(args)
			case semanticReview:
				// An old capability grant cannot interpret a new human restriction.
			case proxyOverride:
				// An observation grant does not decide whether to discard the
				// operator's route. Only an unchanged exact decision above can.
			case isRunGranted(patternKey):
				recordScopeTriage(scope, toolName, patternKey, TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
				return next(args)
			case isRunGranted(exactRunKey):
				recordScopeTriage(scope, toolName, exactRunKey, TriageOutcomeExactRunHit, TriageAssessment{}, 0, nil)
				return next(args)
			case len(targetKeys) > 0 && allApprovalKeysGranted(targetKeys, isRunGranted):
				recordScopeTriage(scope, toolName, strings.Join(targetKeys, ","), TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
				return next(args)
			case anyApprovalKeyGranted(approvalRuleKeys(ruleCandidates), isRunGranted):
				recordScopeTriage(scope, toolName, "rule", TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
				return next(args)
			}
			// A parked approval is a one-shot capability for one byte-identical
			// regenerated action. It sits below the hard floor and current explicit
			// deny, so stale approval evidence cannot override today's safety facts.
			// An ask that included other Runs' unresolved effects authorizes the
			// action only against that same set of claims.
			authorizationFingerprint := resumeFingerprint
			if len(foreignClaims) > 0 {
				authorizationFingerprint = foreignEffectAuthorizationFingerprint(toolName, args, scope, externalClaimIDs(firstClaimPerEffect(foreignClaims)))
			}
			if !denyForcesHuman && !externalUnknown && hasScope && scope.ResumeAuthorizations != nil && authorizationFingerprint != "" {
				grantCtx := contextFromArgs(args)
				approvalID, decisionID, grantKey, claimed, claimErr := scope.ResumeAuthorizations.ClaimApprovalResumeAuthorization(
					grantCtx, scope.TenantID, scope.PersonID, scope.RunID, authorizationFingerprint,
				)
				if claimErr != nil {
					return "", fmt.Errorf("claim parked approval authorization: %w", claimErr)
				}
				if claimed {
					if scope.runGrants != nil && !hostOnce {
						switch {
						case strings.TrimSpace(grantKey) != "":
							scope.runGrants.add(grantKey)
						case decisionID == "run" && patternKey != "":
							scope.runGrants.add(patternKey)
						case decisionID == "run_exact" && exactRunKey != "":
							scope.runGrants.add(exactRunKey)
						}
					}
					if len(foreignClaims) > 0 {
						args[externalEffectToleratedClaimsArg] = externalClaimIDs(foreignClaims)
					}
					recordScopeTriage(scope, toolName, approvalID, TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
					return next(args)
				}
			}
			if len(foreignClaims) == 0 && !hostOnce && !semanticReview && !denyForcesHuman && !externalUnknown && !proxyOverride && hasScope && scope.Grants != nil {
				grantCtx := contextFromArgs(args)
				isGranted := func(key string) bool {
					if key == "" || !scope.StandingGrants.Allowed {
						return false
					}
					granted, _ := scope.Grants.IsApprovalGranted(grantCtx, scope.TenantID, scope.PersonID, scope.WorkspaceID, key, scope.StandingGrants.NotAfter)
					return granted
				}
				switch {
				case isGranted(patternKey):
					recordScopeTriage(scope, toolName, patternKey, TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
					return next(args)
				// ALL targets must be covered (batch B3): a patch touching three
				// roots must not be released by a grant that covers one of them.
				case len(targetKeys) > 0 && allApprovalKeysGranted(targetKeys, isGranted):
					recordScopeTriage(scope, toolName, strings.Join(targetKeys, ","), TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
					return next(args)
				case anyApprovalKeyGranted(approvalRuleKeys(ruleCandidates), isGranted):
					recordScopeTriage(scope, toolName, "rule", TriageOutcomeGrantHit, TriageAssessment{}, 0, nil)
					return next(args)
				}
			}

			// Layer 4 (H2): LLM triage, smart mode only. Sits ABOVE the human ask
			// and BELOW the hard floor (hardline ops returned already). Version-3
			// smart effects reach this review unless the exact action, evidence,
			// and environment decision is unchanged. Fails SAFE: with no
			// judge, or on ESCALATE / any error / timeout, we fall through to the
			// human ask — never an auto-approval.
			// triageState travels to the human ask so the surface can say WHY it is
			// asking: a deliberate escalation and a broken judge are indistinguishable
			// at the prompt, and the second one silently degrades smart mode to
			// on-request.
			triageState := ""
			triageRationale := ""
			triageRisk := ""
			triageAuthorization := ""
			decisionPolicy := ""
			hostWithoutClass := false
			if isExecTool(toolName) && containment.Filesystem != containmentFilesystemIsolated {
				destructive := dangerous && strings.TrimSpace(dangerousReason) != HostEscapeApprovalReason
				if _, eligible := grantCommandPrefix(toolName, args); !eligible || destructive {
					hostWithoutClass = true
				}
			}
			// A credential CLI's resolved credential:read outside the observation
			// catalog is a once-only decision. Host-inventory state handed to an
			// undecidable payload is not: it makes the call uncontained (above)
			// but adds no remote reach, since without a network route it reaches
			// no target and sandbox reads already cover the host's credential
			// files.
			if semanticReview || externalUnknown || denyForcesHuman || hostOnce || hostWithoutClass || proxyOverride ||
				(credentialReadResolved(args) && !containment.ObservationOnly) {
				decisionPolicy = ApprovalDecisionPolicyOnceOnly
			}
			// The boundary facts lead the reason: every surface truncates it (an
			// IM ask to 120 characters, the terminal panel to two lines), and
			// these are the facts the judge and the person must not miss.
			if fact := approvalBoundaryFact(args, containment); fact != "" {
				reason = prependApprovalReason(fact, reason)
			}
			if mode == ApprovalSmart && hasScope && !denyForcesHuman && !externalUnknown && len(foreignClaims) == 0 {
				switch {
				case scope.Judge == nil:
					// No judge wired: smart mode cannot triage at all. Count it so
					// /diag can say the funnel is disabled rather than strict.
					triageState = TriageStateUnavailable
					decisionPolicy = ApprovalDecisionPolicyOnceOnly
					recordScopeTriage(scope, toolName, patternKey, TriageOutcomeUnavailable, TriageAssessment{}, 0, nil)
				case triageDenyBreakerTripped(scope.RunID):
					// The model is circling a denied action (batch C2). Stop
					// spending judge calls on it and put the person in the loop,
					// who is the only one who can end the loop. Never the reverse:
					// a tripped breaker cannot approve anything.
					triageState = TriageStateEscalated
					decisionPolicy = ApprovalDecisionPolicyOnceOnly
					recordScopeTriage(scope, toolName, patternKey, TriageOutcomeEscalated, TriageAssessment{}, 0, nil)
					log.Info("smart approval: triage stepped aside after repeated denials in this run", "tool", toolName, "run", scope.RunID)
				default:
					ctx := withParallelApprovalContext(contextFromArgs(args), scope)
					triageStarted := time.Now()
					verdict, assessment, terr := triageApprovalWithIntent(ctx, scope.Judge, toolName, triageSubject(toolName, approvalDisplayArgs(args)), reason, intentSnapshot, containment)
					assessment.Response.ToolCallID = stringArg(args, "_tool_call_id")
					triageLatency := time.Since(triageStarted)
					triageRationale = assessment.Rationale
					triageRisk = assessment.Risk
					triageAuthorization = assessment.Authorization
					if assessment.Risk == "high" || assessment.Risk == "critical" {
						decisionPolicy = ApprovalDecisionPolicyOnceOnly
					}
					switch verdict {
					case TriageApprove:
						// Reuse only this action under unchanged human evidence and
						// environment. A model verdict cannot grant a command class.
						if decisionKey != "" && !hostOnce {
							recordApprovalGrant(ctx, scope, "run", decisionKey, time.Time{})
						}
						recordScopeTriage(scope, toolName, patternKey, TriageOutcomeApproved, assessment, triageLatency, nil)
						clearTriageDenials(scope.RunID)
						log.Info("smart approval: auto-approved by triage", "tool", toolName, "reason", reason, "class", patternKey,
							"risk", assessment.Risk, "authorization", assessment.Authorization)
						return next(args)
					case TriageDeny:
						// Rejection contract: reuse the "operation rejected" prefix so
						// kernel's isUserRejectionErr treats it as a decision (do NOT
						// retry a variant), not a diagnosable failure. The rationale
						// travels with it so the model learns WHY instead of trying a
						// variant of the same idea.
						recordScopeTriage(scope, toolName, patternKey, TriageOutcomeDenied, assessment, triageLatency, nil)
						tripped := recordTriageDenial(scope.RunID)
						log.Warn("smart approval: blocked by safety triage", "tool", toolName, "reason", reason,
							"risk", assessment.Risk, "breaker_tripped", tripped)
						if assessment.Rationale != "" {
							return "", rejectOperation(rejectionCodeTriage, "operation rejected: blocked by safety triage: "+assessment.Rationale)
						}
						return "", rejectOperation(rejectionCodeTriage, "operation rejected: blocked by safety triage")
					default:
						// ESCALATE (and any error/timeout) → fall through to the human
						// ask. An error is NOT an escalation: it means the funnel did
						// not run, so it is reported separately.
						clearTriageDenials(scope.RunID)
						if terr != nil {
							triageState = TriageStateUnavailable
							triageRationale = "Automatic review did not produce a valid decision. Human confirmation is required."
							decisionPolicy = ApprovalDecisionPolicyOnceOnly
							recordScopeTriage(scope, toolName, patternKey, TriageOutcomeUnavailable, assessment, triageLatency, terr)
							log.Debug("smart approval: triage escalated on error", "tool", toolName, "error", terr)
						} else {
							triageState = TriageStateEscalated
							recordScopeTriage(scope, toolName, patternKey, TriageOutcomeEscalated, assessment, triageLatency, nil)
						}
					}
				}
			}

			// Layer 5: human ask.
			if hasScope && scope.Approval != nil {
				if len(foreignClaims) > 0 {
					// Passing another Run's unresolved effect is a one-time
					// decision about this exact call; it never becomes a grant.
					reason = appendApprovalReason(reason, foreignEffectApprovalReason(foreignClaims))
					triageRationale = foreignEffectApprovalReason(foreignClaims)
					decisionPolicy = ApprovalDecisionPolicyOnceOnly
				}
				recordScopeTriage(scope, toolName, patternKey, TriageOutcomeHumanAsk, TriageAssessment{
					Risk: triageRisk, Authorization: triageAuthorization, Rationale: triageRationale,
				}, 0, nil)
				grantClass := describeHostCredentialWritesGrant(grantClassForDecision(toolName, reason, args, patternKey), hostWrites,
					compactHostPaths(args, containment.HostCredentialWrites))
				runGrantClass := exactRunGrantDescription(patternKey, exactRunKey)
				askRules := ruleCandidates
				if decisionPolicy == ApprovalDecisionPolicyOnceOnly {
					grantClass = ""
					runGrantClass = ""
					askRules = nil
				}
				decision, err := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
					TenantID:                 scope.TenantID,
					PersonID:                 scope.PersonID,
					TaskID:                   scope.TaskID,
					RunID:                    scope.RunID,
					Channel:                  scope.Channel,
					ToolName:                 toolName,
					Reason:                   reason,
					Args:                     approvalDisplayArgs(args),
					GrantClass:               grantClass,
					RunGrantClass:            runGrantClass,
					ResourceFingerprint:      approvalResourceFingerprint(scope, toolName, args),
					Environment:              scope.EnvironmentSnapshotID,
					Cwd:                      approvalDisplayCwd(scope),
					ChangeSummary:            ApprovalChangeSummary(toolName, args),
					TriageState:              triageState,
					TriageRationale:          triageRationale,
					TriageRisk:               triageRisk,
					TriageAuthorization:      triageAuthorization,
					Containment:              containment.Summary(),
					RuleCandidates:           askRules,
					DecisionPolicy:           decisionPolicy,
					AuthorizationFingerprint: authorizationFingerprint,
					ExternalEffectClaimIDs:   externalClaimIDs(foreignClaims),
				})
				if err != nil {
					return "", err
				}
				if !decision.Approved {
					// A rejection and a TIMEOUT are different facts (batch B2):
					// the first is a decision the model must not work around, the
					// second means nobody answered. Both stop the call, but only
					// the rejection carries the do-not-retry user-decision
					// contract that kernel's isUserRejectionErr matches; a timeout
					// returns a typed pause so kernel parks before any further
					// dispatch, independently of the model reading the message.
					if decision.Outcome == ApprovalOutcomeTimedOut {
						return "", &runPauseError{
							cause:  fmt.Errorf("approval timed out with no answer: %s (nobody is at the keyboard; do not retry a variant, finish waiting_user)", fallbackReason(decision.Reason, "no answer before the approval expired")),
							reason: "waiting_user", message: "Work is paused because this action still needs your approval.", needApproval: true,
						}
					}
					if decision.Reason != "" {
						return "", rejectOperation(rejectionCodeApproval, "operation rejected: "+decision.Reason)
					}
					return "", rejectOperation(rejectionCodeApproval, "operation rejected by approval "+decision.ApprovalID)
				}
				if decision.Scope != "" && decision.Scope != "run" && decision.Scope != "workspace" {
					return "", rejectOperation(rejectionCodeScope, fmt.Sprintf("operation rejected: approval scope %q was not offered for this request", decision.Scope))
				}
				if decisionPolicy == ApprovalDecisionPolicyOnceOnly && (decision.Scope != "" || strings.TrimSpace(decision.GrantKey) != "") {
					return "", rejectOperation(rejectionCodeScope, "operation rejected: this sensitive request can only be approved once")
				}
				// Remember what the human actually chose (batch B2): a rule they
				// picked, else the action class. A rule key is honored ONLY when
				// it is one this call offered — a decision arriving from any
				// surface must not be able to mint an authorization of its own.
				if scope.Grants != nil || scope.runGrants != nil {
					grantCtx := contextFromArgs(args)
					if rule, ok := approvalRuleByKey(ruleCandidates, decision.GrantKey); ok {
						recordApprovalGrant(grantCtx, scope, decision.Scope, rule.Key, approvalGrantExpiry(decision.Scope, args))
						log.Info("approval rule granted", "kind", rule.Kind, "scope", decision.Scope, "tool", toolName)
					} else {
						if strings.TrimSpace(decision.GrantKey) != "" {
							log.Warn("approval rule refused: not offered for this call", "tool", toolName)
						}
						if patternKey != "" {
							recordApprovalGrant(grantCtx, scope, decision.Scope, patternKey, approvalGrantExpiry(decision.Scope, args))
						} else if decision.Scope == "run" && exactRunKey != "" {
							recordApprovalGrant(grantCtx, scope, "run", exactRunKey, time.Time{})
						}
						// A remembered decision on a multi-target write must cover
						// EVERY target, otherwise the next call re-asks for the
						// files this answer already authorized (batch B3).
						if decision.Scope != "" {
							for _, key := range targetKeys {
								recordApprovalGrant(grantCtx, scope, decision.Scope, key, approvalGrantExpiry(decision.Scope, args))
							}
						}
					}
				}
				if len(foreignClaims) > 0 {
					args[externalEffectToleratedClaimsArg] = externalClaimIDs(foreignClaims)
				}
				return next(args)
			}

			clarifyFn := clarifyHandlerFromArgs(args)
			if clarifyFn != nil {
				question := fmt.Sprintf("Dangerous operation detected.\nTool: %s\nArgs: %v\nReason: %s\nConfirm execution?", toolName, MarshalArgs(args), reason)
				response := clarifyFn(question, []string{"Yes", "No"})
				if strings.ToLower(response) != "yes" {
					return "", fmt.Errorf("operation cancelled by user")
				}
			}
			return next(args)
		}
	}
}

func recordScopeTriage(scope ExecutionScope, toolName, grantKey string, outcome TriageOutcome, assessment TriageAssessment, latency time.Duration, err error) {
	route := ""
	if routed, ok := scope.Judge.(ApprovalJudgeRoute); ok {
		route = strings.TrimSpace(routed.ApprovalJudgeRoute())
	}
	errorClass := ""
	var responseError interface{ ApprovalErrorClass() string }
	if err != nil {
		switch {
		case errors.As(err, &responseError):
			errorClass = responseError.ApprovalErrorClass()
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			errorClass = "timeout"
		default:
			errorClass = "provider_error"
		}
	} else if outcome == TriageOutcomeUnavailable && scope.Judge == nil {
		errorClass = "not_configured"
	}
	RecordTriageAuditEvent(TriageAuditEvent{
		TenantID: scope.TenantID, PersonID: scope.PersonID, TaskID: scope.TaskID, RunID: scope.RunID,
		ToolName: toolName, Outcome: outcome, RiskLevel: assessment.Risk,
		Authorization: assessment.Authorization, GrantKey: grantKey, ProviderRoute: route,
		Latency: latency, ErrorClass: errorClass, Rationale: assessment.Rationale,
		PolicyVersion: ApprovalTriagePolicyVersion, Response: assessment.Response,
	}, err)
}

// approvalExactRunKey fingerprints an exec action that the reusable-grant floor
// deliberately refuses to classify. The raw payload never leaves this function;
// the key is in-memory only and includes the environment/blast-radius identity,
// so changing code, sandbox, workspace, credentials, or network asks again.
// assessment is the call's containment, which the funnel has already derived
// from the whole command; deriving it again doubled the cost of a long one.
func approvalExactRunKey(toolName string, args map[string]interface{}, scope ExecutionScope, hasScope bool, assessment ContainmentAssessment) string {
	if !hasScope || !isExecTool(toolName) || strings.TrimSpace(scope.RunID) == "" {
		return ""
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return ""
	}
	material := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(toolName)), payload,
		strings.TrimSpace(scope.RunID), strings.TrimSpace(scope.WorkspaceID),
		strings.TrimSpace(scope.EnvironmentSnapshotID), fmt.Sprint(scope.EnvironmentGeneration),
		assessment.Summary(),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("run:exact:%x", sum[:16])
}

// approvalDeclaredEffectKey binds a command approved through
// request_permissions to this live run and execution identity. Public arguments
// are canonicalized, while credentials and raw commands never leave the hash.
// Network and credential capabilities remain independent runtime checks; the
// effective filesystem boundary is included so a sandbox fallback asks again.
func approvalDeclaredEffectKey(toolName string, args map[string]interface{}, scope ExecutionScope, hasScope bool) string {
	if !hasScope || !isExecTool(toolName) || strings.TrimSpace(scope.RunID) == "" {
		return ""
	}
	actualArgs, err := json.Marshal(approvalArgs(args))
	if err != nil {
		return ""
	}
	material := strings.Join([]string{
		"v1", strings.ToLower(strings.TrimSpace(toolName)), string(actualArgs),
		strings.TrimSpace(scope.TenantID), strings.TrimSpace(scope.PersonID), strings.TrimSpace(scope.TaskID),
		strings.TrimSpace(scope.RunID), strings.TrimSpace(scope.WorkspaceID),
		strings.TrimSpace(scope.EnvironmentSnapshotID), fmt.Sprint(scope.EnvironmentGeneration),
		strings.TrimSpace(scope.EnvironmentFingerprint), strings.TrimSpace(scope.PrincipalFingerprint),
		strings.TrimSpace(scope.CredentialSourceHash), string(effectiveSandboxModeArg(args)),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("run:declared-effect:v1:%x", sum[:16])
}

// approvalResumeFingerprint binds a parked decision to the actual action and
// stable execution identity without persisting raw command/code/credentials.
// Run id and snapshot generation are intentionally excluded: a continuation is
// a new run, while the environment/principal fingerprints prove equivalence.
func approvalResumeFingerprint(toolName string, args map[string]interface{}, scope ExecutionScope, containment string) string {
	if strings.TrimSpace(toolName) == "" || strings.TrimSpace(scope.TaskID) == "" {
		return ""
	}
	actualArgs, err := json.Marshal(approvalArgs(args))
	if err != nil {
		return ""
	}
	material := strings.Join([]string{
		"v1", strings.ToLower(strings.TrimSpace(toolName)), string(actualArgs),
		strings.TrimSpace(scope.TenantID), strings.TrimSpace(scope.PersonID),
		strings.TrimSpace(scope.TaskID), strings.TrimSpace(scope.WorkspaceID),
		strings.TrimSpace(scope.EnvironmentFingerprint), strings.TrimSpace(scope.PrincipalFingerprint),
		strings.TrimSpace(scope.CredentialSourceHash), strings.TrimSpace(containment),
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("resume:v1:%x", sum[:])
}

func exactRunGrantDescription(patternKey, exactRunKey string) string {
	if patternKey != "" || exactRunKey == "" {
		return ""
	}
	return "this exact action for this run"
}

// approvalDisplayCwd is the working root shown on the approval surface: where
// the operation would actually run. It reports the scope's own root, never the
// daemon process cwd (which is not the active workspace) and never a path taken
// from the model's args, so the displayed location cannot be spoofed by the call
// being approved.
func approvalDisplayCwd(scope ExecutionScope) string {
	if root := strings.TrimSpace(scope.WorkspaceRoot); root != "" {
		return filepath.Clean(root)
	}
	for _, root := range scope.AllowedRoots {
		if trimmed := strings.TrimSpace(root); trimmed != "" {
			return filepath.Clean(trimmed)
		}
	}
	return ""
}

// approvalProjectRoot aligns approval heuristics with the workspace scope that
// already authorized the call. The daemon process cwd is not necessarily the
// active workspace; using it caused harmless reads after /ws to ask again.
func approvalProjectRoot(fallback string, scope ExecutionScope, args map[string]interface{}) string {
	roots := append([]string{}, scope.AllowedRoots...)
	if len(roots) == 0 && strings.TrimSpace(scope.WorkspaceRoot) != "" {
		roots = []string{scope.WorkspaceRoot}
	}
	target := ""
	for _, key := range []string{"path", "cwd"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			target = strings.TrimSpace(value)
			break
		}
	}
	if target != "" && !filepath.IsAbs(target) && strings.TrimSpace(scope.WorkspaceRoot) != "" {
		target = filepath.Join(scope.WorkspaceRoot, target)
	}
	if target != "" {
		for _, root := range roots {
			absRoot, err := filepath.Abs(root)
			if err == nil && isWithin(filepath.Clean(absRoot), filepath.Clean(target)) {
				return filepath.Clean(absRoot)
			}
		}
	}
	if strings.TrimSpace(scope.WorkspaceRoot) != "" {
		return filepath.Clean(scope.WorkspaceRoot)
	}
	return fallback
}

// recordApprovalGrant remembers an "approve this class" decision. Scope "run"
// stays in memory for the live run; "task" persists for the current task;
// "person" persists across the person's tasks. "" (once) records nothing.
// Failures are swallowed: a lost grant only costs one extra ask, never
// correctness.
func recordApprovalGrant(ctx context.Context, scope ExecutionScope, decisionScope, patternKey string, expiresAt time.Time) {
	if patternKey == "" {
		return
	}
	switch decisionScope {
	case "run":
		if scope.runGrants != nil {
			scope.runGrants.add(patternKey)
		}
	case "workspace":
		// The key already carries the workspace fingerprint, so the scope row is
		// what makes the grant reviewable and revocable by workspace rather than
		// what bounds it.
		if scope.Grants != nil && scope.PersonID != "" && strings.TrimSpace(scope.WorkspaceID) != "" {
			_ = scope.Grants.GrantApproval(ctx, "workspace", scope.TenantID, scope.PersonID, scope.WorkspaceID, patternKey, expiresAt)
		}
	case "person":
		if scope.Grants != nil && scope.PersonID != "" {
			_ = scope.Grants.GrantApproval(ctx, "person", scope.TenantID, scope.PersonID, scope.PersonID, patternKey, expiresAt)
		}
	}
}

// appendApprovalReason adds one review fact to the reason a judge and a person
// see, keeping every earlier fact.
func appendApprovalReason(reason, fact string) string {
	reason, fact = strings.TrimSpace(reason), strings.TrimSpace(fact)
	switch {
	case reason == "":
		return fact
	case fact == "" || strings.Contains(reason, fact):
		return reason
	}
	return reason + "; " + fact
}

// approvalGrantPersonTTL bounds a PERSON-scope class, which outlives every
// workspace and every unit of work and is therefore the one scope a deadline
// is the right instrument for. The 8h window matches the execution-capability
// policy so the two ledgers cannot disagree about how long "remember this"
// lasts.
const approvalGrantPersonTTL = 8 * time.Hour

// approvalGrantExpiry leaves a workspace-scope class unbounded on purpose.
//
// Host execution used to force an 8h deadline on every remembered class. On a
// platform with no sandbox that made the feature useless rather than safer: the
// same release workflow re-asked the same questions the next morning, so a
// deadline shorter than the work it covers is a deadline the person pays for
// twice. What actually bounds a workspace class is that it is narrow (a
// verb-bounded prefix), scoped (one workspace), listed (`/approvals rules`),
// revocable, and re-checked against the floor whenever the floor changes.
func approvalGrantExpiry(decisionScope string, args map[string]interface{}) time.Time {
	if strings.EqualFold(strings.TrimSpace(decisionScope), "person") {
		return time.Now().Add(approvalGrantPersonTTL)
	}
	return time.Time{}
}

// approvalPatternKey returns a COARSE class identifier for an approval decision,
// deliberately NOT the exact command. Approving one `chmod` should approve the
// chmod CLASS for the granted scope, not just that byte-identical string.
//
// An exec key is the command class the floor derives (grantCommandPrefix) and
// nothing else: "exec:<tool>|class=<prefix>" for a sandboxed call, and the
// workspace-bound host shape for a host escape. It is never built from the
// approval reason. The reason is whatever made THIS call ask — the mode
// ("terminal requires approval in on-request mode"), or nothing at all in smart
// mode — so a key built from it named the whole tool: remembering one sandboxed
// `kubectl get pods` released `kubectl delete` and `terraform destroy`, while
// the prompt showed the mode text or a `kubectl get` class.
//
// For write/path tools the class is the tool name plus a bucket of the reason
// with the specific path stripped, so approving one out-of-workspace write does
// not silently approve a write to a different out-of-workspace path forever.
// Returns "" when there is nothing meaningful to remember.
func approvalPatternKey(toolName string, args map[string]interface{}, dangerousReason string) string {
	return approvalPatternKeyForScope(toolName, args, dangerousReason, ExecutionScope{}, false)
}

func approvalPatternKeyForScope(toolName string, args map[string]interface{}, dangerousReason string, scope ExecutionScope, hasScope bool) string {
	if isExecTool(toolName) {
		// Floor first: a payload whose class cannot bound what will run is
		// approvable once but never remembered (see grant_floor.go).
		prefix, eligible := grantCommandPrefix(toolName, args)
		if !eligible {
			return ""
		}
		if effectiveSandboxModeArg(args) == SandboxHost {
			if !hasScope {
				// A host escape without a durable workspace identity may be
				// approved once, but must never create a reusable broad grant.
				return ""
			}
			// A command dangerous for its own sake (chmod, kill, ssh) has no
			// class on the host, where no sandbox bounds it: the ask is
			// once-only (hostWithoutClass), so no stored key, however old,
			// releases it.
			if hostDangerousFamily(grantPrefixLabel(prefix)) {
				return ""
			}
			resource := approvalResourceFingerprint(scope, toolName, args)
			if resource == "" {
				return ""
			}
			return hostGrantKeyBase + grantKeyResourceMarker + resource
		}
		return "exec:" + toolName + grantKeyClassMarker + grantPrefixLabel(prefix)
	}
	return toolName + ":" + patternReasonBucket(dangerousReason)
}

// approvalResourceFingerprint scopes host-execution approval memory to both
// the active workspace and the effective command family. Raw paths, commands,
// tokens, and credential bytes never enter the persisted key.
func approvalResourceFingerprint(scope ExecutionScope, toolName string, args map[string]interface{}) string {
	if !isExecTool(toolName) || effectiveSandboxModeArg(args) != SandboxHost {
		return ""
	}
	workspace := strings.TrimSpace(scope.WorkspaceID)
	if workspace == "" {
		workspace = filepath.Clean(strings.TrimSpace(scope.WorkspaceRoot))
	}
	if workspace == "" || workspace == "." {
		return ""
	}
	sum := sha256.Sum256([]byte(workspace))
	return fmt.Sprintf("workspace:%x:command:%s", sum[:8], execCommandFamily(toolName, args))
}

// execCommandFamily names the command family for a host-execution resource
// fingerprint. It delegates to the grant floor so the fingerprint can never
// disagree with the eligibility decision: an ineligible payload has no family,
// and approvalPatternKeyForScope has already refused a reusable key by the time
// this is reached.
//
// The family is the whole verb-bounded prefix, so a remembered
// `aws codebuild batch-get-builds` cannot release `aws codebuild start-build`.
func execCommandFamily(toolName string, args map[string]interface{}) string {
	if prefix, ok := grantCommandPrefix(toolName, args); ok {
		return grantPrefixLabel(prefix)
	}
	return "unknown"
}

// patternReasonBucket collapses a dangerous/approval reason to its class by
// dropping everything after the first ":" (the variable part — a concrete path
// or mode name). "accesses restricted path: /etc/x" → "accesses restricted
// path".
func patternReasonBucket(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "generic"
	}
	if i := strings.Index(reason, ":"); i >= 0 {
		return strings.TrimSpace(reason[:i])
	}
	return reason
}

// hardlineProtectedRoots are directories a recursive delete must never touch.
// Kept to the tight, high-blast-radius set: the filesystem root and the core
// system/user trees whose loss bricks the host or wipes the user's home. Merely
// "dangerous" deletes (a project subdir) stay behind normal approval.
var hardlineProtectedRoots = map[string]struct{}{
	"/": {}, "/home": {}, "/etc": {}, "/usr": {}, "/var": {}, "/boot": {},
}

// hardlineDiskDevices are raw block-device path prefixes, including the macOS
// whole-disk nodes. Formatting one (mkfs), overwriting it (dd of=), or
// redirecting into it destroys partitions irrecoverably, so any such target is
// a hard block.
var hardlineDiskDevices = []string{"/dev/sd", "/dev/nvme", "/dev/hd", "/dev/vd", "/dev/xvd", "/dev/disk", "/dev/rdisk"}

// hardlineDiskDevice reports the device prefix a write target names, read
// as the shell passes it: escapes removed.
func hardlineDiskDevice(target string) (string, bool) {
	target = strings.ToLower(strings.ReplaceAll(strings.Trim(target, `"'`), `\`, ""))
	for _, dev := range hardlineDiskDevices {
		if strings.HasPrefix(target, dev) {
			return dev, true
		}
	}
	return "", false
}

// hardlineToolCall is the UNBYPASSABLE safety floor. It fires before any
// approval-mode bypass (full-auto included) because these operations have a
// blast radius no session-level "yes" should ever authorize:
//
//   - recursive delete of the filesystem root or a protected system/home root
//     (rm -rf /, /home, $HOME, /etc, /usr, /var, /boot), including a find
//     that amounts to one (find / -delete, find / -exec rm -rf {} +);
//   - raw-disk destruction: mkfs*, newfs*, diskutil's erase verbs,
//     dd of=/dev/sdX|/dev/nvmeX, or any redirect into a /dev/sdX|/dev/nvmeX
//     device;
//   - fork bomb (:(){ :|:& };:) which exhausts the process table;
//   - powering the host down or rebooting it out from under a running task
//     (shutdown, reboot, halt, poweroff, init or telinit 0|6, systemctl
//     reboot|poweroff|halt|kexec|soft-reboot).
//
// It is intentionally TIGHT: only patterns whose harm is effectively
// irreversible and never a legitimate agent action belong here. Everything
// merely "dangerous" stays in dangerousToolCall behind normal approval.
// Returns (true, reason) to hard-deny.
func hardlineToolCall(projectRoot, toolName string, args map[string]interface{}) (bool, string) {
	if !isExecTool(toolName) {
		return false, ""
	}
	cmd := execCommandPayload(toolName, args)
	if strings.TrimSpace(cmd) == "" {
		return false, ""
	}
	lower := strings.ToLower(cmd)

	// Fork bomb: match the self-replicator regardless of internal spacing.
	if strings.Contains(strings.ReplaceAll(cmd, " ", ""), ":(){") {
		return true, "fork bomb (process-table exhaustion)"
	}

	// Raw-disk destruction via dd of=/dev/... or a redirect into /dev/...
	// These substring checks on the FULL command pierce any wrapper and any
	// code a payload is quoted in (`bash -c "dd ... of=/dev/sda"`); reading it
	// once more with quotes and escapes removed catches `of="/dev/sda"`. The
	// commands below check the same targets as the shell passes them.
	unquoted := strings.ToLower(strings.NewReplacer(`"`, "", "'", "", `\`, "").Replace(cmd))
	for _, text := range []string{lower, unquoted} {
		for _, dev := range hardlineDiskDevices {
			if strings.Contains(text, "of="+dev) {
				return true, "overwrites raw disk device: " + dev + "*"
			}
			if strings.Contains(text, "> "+dev) || strings.Contains(text, ">"+dev) {
				return true, "redirects over raw disk device: " + dev + "*"
			}
		}
	}

	// Program + argument inspection of every command the payload runs, named
	// by identifyCommand: `bash -c "rm -rf /"`, `sudo rm -rf /`,
	// `FOO=a/b RM -rf /` and `if x; then \rm -rf /; fi` are classified by the
	// program the shell runs, not by how it is spelled. The floor stays TIGHT:
	// an unwrappable payload does NOT hard-block here (that only raises the
	// dangerous heuristic) — deny only what we can positively identify.
	home := strings.TrimRight(os.Getenv("HOME"), "/")
	for _, read := range effectiveCommands(cmd) {
		if reason, hit := hardlineCommand(read, home); hit {
			return true, reason
		}
	}
	return false, ""
}

// hardlineCommand is the floor for one command the payload runs.
func hardlineCommand(read readCommand, home string) (string, bool) {
	for _, target := range read.writes {
		if dev, hit := hardlineDiskDevice(target); hit {
			return "redirects over raw disk device: " + dev + "*", true
		}
	}
	program := read.id.program()
	base, rest := program.Name, program.Args
	if base == "dd" {
		for _, arg := range rest {
			if target, ok := strings.CutPrefix(arg, "of="); ok {
				if dev, hit := hardlineDiskDevice(target); hit {
					return "overwrites raw disk device: " + dev + "*", true
				}
			}
		}
	}
	switch {
	case base == "mkfs" || strings.HasPrefix(base, "mkfs.") || base == "newfs" || strings.HasPrefix(base, "newfs_"):
		return "formats a filesystem: " + base, true
	case base == "shutdown" || base == "reboot" || base == "halt" || base == "poweroff":
		return "powers down or reboots the host: " + base, true
	case (base == "init" || base == "telinit") && len(rest) > 0 && (rest[0] == "0" || rest[0] == "6"):
		return "changes runlevel to halt/reboot: " + base + " " + rest[0], true
	case base == "systemctl":
		if verb, hit := systemctlPowerVerb(rest); hit {
			return "powers down or reboots the host: systemctl " + verb, true
		}
	case base == "diskutil":
		if verb, hit := diskutilEraseVerb(rest); hit {
			return "erases a disk or volume: diskutil " + verb, true
		}
	case base == "rm":
		if target, hit := hardlineRmProtectedRoot(rest, home); hit {
			return "recursive delete of protected root: " + target, true
		}
	case base == "find":
		return hardlineFindSweep(rest, home)
	}
	return "", false
}

// hardlineRmProtectedRoot reports whether an rm invocation (rest = args after
// `rm`) recursively targets a protected root. Recursion is required (a
// non-recursive rm at a root cannot wipe a tree); force is not, because a
// recursive delete of a protected root is catastrophic with or without -f.
func hardlineRmProtectedRoot(rest []string, home string) (string, bool) {
	recursive := false
	var targets []string
	for _, f := range rest {
		switch {
		case f == "--recursive" || f == "--no-preserve-root":
			// --no-preserve-root only exists to disable the very guard we are
			// re-implementing; treat its presence as recursive intent.
			recursive = true
		case strings.HasPrefix(f, "--"):
			// other long options (e.g. --force) do not affect target detection
		case strings.HasPrefix(f, "-"):
			if strings.ContainsAny(f, "rR") {
				recursive = true
			}
		default:
			targets = append(targets, f)
		}
	}
	if !recursive {
		return "", false
	}
	for _, t := range targets {
		if root, ok := hardlineProtectedRootTarget(t, home); ok {
			return root, true
		}
	}
	return "", false
}

// hardlineProtectedRootTarget normalizes an rm target the way it resolves —
// the home directory however it is spelled, the path cleaned, trailing globs
// collapsed — and only then reports whether it is a protected root:
// `rm -rf ~/`, `rm -rf $HOME/..` and `rm -rf /*` are as catastrophic as
// `rm -rf ~`, `rm -rf /home` and `rm -rf /`.
func hardlineProtectedRootTarget(target, home string) (string, bool) {
	t := strings.Trim(target, `"'`)
	homeSpelled := false
	for _, spelling := range []string{"${HOME}", "$HOME", "~"} {
		if rest, ok := strings.CutPrefix(t, spelling); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			t, homeSpelled = "$HOME"+rest, true
			if home != "" {
				t = home + rest
			}
			break
		}
	}
	clean := cleanShellPathTarget(t)
	for {
		collapsed := strings.TrimSuffix(clean, "/*")
		if collapsed == clean {
			break
		}
		if collapsed == "" {
			collapsed = "/"
		}
		clean = collapsed
	}
	if _, bad := hardlineProtectedRoots[clean]; bad {
		return clean, true
	}
	if clean == "$HOME" || (home != "" && clean == cleanShellPathTarget(home)) {
		if homeSpelled {
			return "$HOME", true
		}
		return clean, true
	}
	return "", false
}

// cleanShellPathTarget normalizes shell command paths with slash semantics.
// Tool commands often run in WSL/Linux containers even when SelfMind is built
// or tested on Windows, so filepath.Clean would use the wrong separator rules
// for targets like "/" or "/etc".
func cleanShellPathTarget(target string) string {
	target = strings.ReplaceAll(target, "\\", "/")
	return path.Clean(target)
}

// dangerousBinaries are program names that warrant explicit approval before
// execution. Matched by programName so that `rm`, `/bin/rm`, `RM` and `\rm`
// are caught alike.
var dangerousBinaries = map[string]struct{}{
	"rm": {}, "rmdir": {}, "chmod": {}, "chown": {}, "kill": {}, "pkill": {},
	"killall": {}, "shutdown": {}, "reboot": {}, "halt": {}, "poweroff": {},
	"mkfs": {}, "dd": {}, "fdisk": {}, "shred": {}, "wipefs": {},
}

// destructiveSubstrings are raw patterns that are dangerous regardless of how
// the command is tokenized (e.g. redirecting over a device node).
var destructiveSubstrings = []string{"> /dev/", ":(){", "/dev/sd", "/dev/nvme"}

// egressBinaries are programs whose primary purpose is moving data across the
// network. They are classified dangerous (approval-gated) not because they are
// destructive but because they are the exfiltration half of the IM-injection
// threat: an untrusted message reaching the agent must not silently `curl` data
// out. Matched by basename over wrapper-unwrapped segments, like
// dangerousBinaries. ssh/scp/sftp/rsync are included because they move data to
// a remote host; git is intentionally NOT here (its network use is push/pull to
// configured remotes, and gating every git call would be pure fatigue).
var egressBinaries = map[string]struct{}{
	"curl": {}, "wget": {}, "nc": {}, "ncat": {}, "netcat": {}, "socat": {},
	"telnet": {}, "ftp": {}, "tftp": {}, "scp": {}, "sftp": {}, "rsync": {},
	"ssh": {},
}

// egressSubstrings catch network egress that has no distinct program name: the
// bash pseudo-device redirect (`>/dev/tcp/host/port`) and its UDP sibling. These
// are checked on the full payload so a wrapper cannot hide them.
var egressSubstrings = []string{"/dev/tcp/", "/dev/udp/"}

// egressCommand reports whether the exec payload performs network egress. It is
// a first-class safety classifier (its own function, not a buried list entry) so
// the egress threat can be reasoned about, tested, and tightened independently
// of the destructive-command heuristics. cmd is the raw payload and segs the
// caller's field reading of it; the shell's own reading of cmd is added, so a
// quoted assignment or a control structure cannot hide the program.
func egressCommand(cmd string, segs [][]string) (bool, string) {
	commands, _ := shellCommands(cmd, 0)
	for _, fields := range segs {
		commands = append(commands, readCommand{id: identifyCommand(fields), fallback: true})
	}
	return egressIn(cmd, commands)
}

func egressIn(cmd string, commands []readCommand) (bool, string) {
	lower := strings.ToLower(cmd)
	for _, sub := range egressSubstrings {
		if strings.Contains(lower, sub) {
			return true, "network egress via " + sub
		}
	}
	for _, read := range commands {
		base := read.id.program().Name
		if _, hit := egressBinaries[base]; hit {
			return true, egressCommandReason + base
		}
	}
	return false, ""
}

// Dangerous-operation reasons that name a program or variable. They explain an
// ask; a remembered class is never keyed on one (approvalPatternKeyForScope).
const (
	dangerousCommandReason = "invokes dangerous command: "
	egressCommandReason    = "network egress command: "
	codeInjectionReason    = "injects code through environment variable: "
	findDeleteReason       = "find deletes files: -delete"
)

// codeInjectionAssignment returns the first variable the commands assign that
// loads code into whatever program runs next, so no program, not even an
// observation, can vouch for the call. PYTHONPATH counts when the payload
// runs Python and ENV when it runs an interactive shell.
func codeInjectionAssignment(commands []readCommand) string {
	var readers injectionReaders
	for _, read := range commands {
		program := read.id.program()
		readers.python = readers.python || pythonProgram(program.Name)
		readers.interactiveShell = readers.interactiveShell || interactiveShell(program)
	}
	for _, read := range commands {
		if variable := read.id.codeInjection(readers); variable != "" {
			return variable
		}
	}
	return ""
}

func dangerousToolCall(projectRoot, toolName string, args map[string]interface{}) (bool, string) {
	if unclassifiedExternalToolCall(args) {
		return true, unclassifiedExternalApprovalReason
	}
	if isExecTool(toolName) {
		cmd := execCommandPayload(toolName, args)
		for _, pattern := range destructiveSubstrings {
			if strings.Contains(cmd, pattern) {
				return true, fmt.Sprintf("contains dangerous pattern: %s", pattern)
			}
		}
		// Every command the payload runs is named by identifyCommand, defeating
		// spelling tricks like /bin/rm, RM or `FOO=a/b rm` AND wrapper tricks like
		// `bash -c "rm ..."` / `sudo rm ...` / `exec rm ...`.
		commands := effectiveCommands(cmd)
		// Injected code runs inside whatever confines the program it is
		// injected into. Enforced containment (writes confined to scope, no
		// network or credential escape) already bounds arbitrary code, so the
		// injection does not widen it; anywhere else no program can vouch for
		// the call. It never backs a remembered class either way.
		if variable := codeInjectionAssignment(commands); variable != "" && !assessExecContainment(toolName, args).AutoApprove() {
			return true, codeInjectionReason + variable
		}
		for _, read := range commands {
			base := read.id.program().Name
			if _, bad := dangerousBinaries[base]; bad {
				return true, dangerousCommandReason + base
			}
		}
		// A command find runs is classified like any other command; -delete
		// is find's own deletion.
		for _, read := range commands {
			if program := read.id.program(); program.Name == "find" && slices.Contains(program.Args, "-delete") {
				return true, findDeleteReason
			}
		}
		// Network egress (data exfiltration half of the IM-injection threat) is
		// dangerous even when the command is otherwise harmless. The same
		// identification sees `sudo curl ...` through its wrapper.
		if hit, ereason := egressIn(cmd, commands); hit {
			return true, ereason
		}
		// A wrapper whose payload we could not extract (e.g. `bash script.sh`,
		// `sh -s`) is opaque: treat it as dangerous (needs approval) but never
		// hardline. This is the "can't parse → dangerous, not hard-blocked" rule.
		for _, read := range commands {
			if read.id.Opaque {
				return true, "invokes an opaque wrapped command"
			}
		}
		// Host execution remains approval-gated, but only after inspecting the
		// real payload. This preserves specific classes such as curl/chmod/rm
		// instead of collapsing every host request into one reusable grant.
		if effectiveSandboxModeArg(args) == SandboxHost {
			return true, HostEscapeApprovalReason
		}
	}

	path, _ := args["path"].(string)
	if path == "" {
		return false, ""
	}
	if strings.Contains(path, "/etc/") || strings.Contains(path, "/root/") || strings.Contains(path, "/dev/") {
		return true, fmt.Sprintf("accesses restricted path: %s", path)
	}
	if projectRoot != "" && filepath.IsAbs(path) && !isWithin(filepath.Clean(projectRoot), filepath.Clean(path)) {
		return true, fmt.Sprintf("accesses path outside project root: %s", path)
	}
	return false, ""
}

// execCommandPayload returns the shell/code payload an exec tool would run. The
// classifier heuristics were originally written for terminal-style tools that
// carry their command in args["command"], which silently exempted execute_code
// (payload in args["code"]) from BOTH the hard floor and the dangerous-op
// heuristic. This normalizes across the exec tools so the same string checks see
// the real payload regardless of which arg key holds it.
func execCommandPayload(toolName string, args map[string]interface{}) string {
	if !isExecTool(toolName) {
		return ""
	}
	for _, key := range []string{"command", "code", "script"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func contextFromArgs(args map[string]interface{}) context.Context {
	if args != nil {
		if ctx, ok := args["_context"].(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

func approvalArgs(args map[string]interface{}) map[string]interface{} {
	return publicToolArgs(args)
}

func approvalDisplayArgs(args map[string]interface{}) map[string]interface{} {
	out := approvalArgs(args)
	for key, value := range out {
		out[key] = redactApprovalNamedValue(key, value)
	}
	if effectiveSandboxModeArg(args) == SandboxHost {
		requested, _ := requestedSandboxMode(args)
		if requested != SandboxHost {
			why := strings.TrimSpace(stringArg(args, effectiveSandboxReasonArg))
			if why == "" {
				why = "isolated sandbox unavailable or disabled"
			}
			out["effective_sandbox"] = RedactSensitive("host (" + why + ")")
		}
	}
	return out
}

func redactApprovalValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case string:
		return RedactSensitive(typed)
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = RedactSensitive(item)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = redactApprovalValue(item)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			out[key] = redactApprovalNamedValue(key, item)
		}
		return out
	default:
		return value
	}
}

var sensitiveApprovalField = regexp.MustCompile(`(?i)(^|[_-])(api[_-]?key|token|secret|password|credential|authorization)($|[_-])`)

func redactApprovalNamedValue(key string, value interface{}) interface{} {
	if text, ok := value.(string); ok && sensitiveApprovalField.MatchString(strings.TrimSpace(key)) && !isWholeReference(text) {
		return "[REDACTED]"
	}
	return redactApprovalValue(value)
}
