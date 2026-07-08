package tools

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"selfmind/internal/platform/log"
)

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
				if len(result) > 200 {
					return RedactSensitive(result[:200]) + "..."
				}
				return RedactSensitive(result)
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
	// ApprovalOnRequest (default) asks only when an op trips the dangerous-op
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
	"terminal": {}, "execute_command": {}, "shell": {}, "execute_code": {},
}

func isWriteTool(name string) bool { _, ok := writeTools[name]; return ok }
func isExecTool(name string) bool  { _, ok := execTools[name]; return ok }

// approvalNeeded decides whether a tool call requires human approval under the
// given mode. dangerous is the dangerous-op heuristic result.
func approvalNeeded(mode ApprovalMode, toolName string, dangerous bool) bool {
	switch mode {
	case ApprovalFullAuto:
		return false
	case ApprovalSmart:
		// Gates on any arbitrary-code exec tool OR a dangerous op. Running
		// arbitrary commands/code is inherently approval-worthy: the `dangerous`
		// heuristic is a NARROWING optimization for known-safe reads, never a
		// gate that lets arbitrary exec through unprompted (it historically only
		// inspected args["command"], so execute_code's args["code"] payload slid
		// past it entirely and ran with NO approval). In smart mode the exec ask
		// is then triaged by the LLM judge in SmartApprovalMiddleware; the gate
		// itself stays here.
		return isExecTool(toolName) || dangerous
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
	// Layer 2: mode gate. If the mode would not require an ask for this op, it is
	// auto-approved (full-auto for everything; auto-edit for non-dangerous edits).
	if !approvalNeeded(mode, toolName, dangerous) {
		return ModeApprove
	}
	// Layer 4: smart-mode LLM triage. APPROVE auto-runs, DENY blocks as a
	// decision, everything else (ESCALATE / no judge / error / timeout) falls
	// through to the human ask — fail SAFE, exactly like the middleware.
	if mode == ApprovalSmart && judge != nil {
		verdict, _ := triageApproval(ctx, judge, toolName, triageSubject(toolName, args), reason)
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
//  3. Class-level allowlist (scope.Grants): a prior "approve this class" for the
//     task (session) or person (persistent) suppresses the ask. This is the key
//     fatigue reducer — approving one chmod approves the chmod CLASS.
//  4. LLM triage (H2), smart mode only: a cheap judge triages the dangerous op
//     (APPROVE auto-runs + grants the class for the task; DENY blocks as a
//     do-not-retry decision; ESCALATE / no judge / any error / timeout falls
//     through to the human ask). It fails SAFE — never auto-approves without a
//     clear APPROVE from an installed judge — and sits strictly below the hard
//     floor, which returned long before this point.
//  5. Human ask (scope.Approval / clarify). An "approve + remember" decision
//     records a grant for the next same-class call.
func SmartApprovalMiddleware(projectRoot string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)

			// Layer 1: hard floor. Unconditional — no mode, not even full-auto,
			// can bypass it.
			if blocked, reason := hardlineToolCall(projectRoot, toolName, args); blocked {
				// Distinct from the rejection contract (see isUserRejectionErr):
				// this is a safety-policy block, so the model must not retry any
				// variant, but it is not a user decision.
				return "", fmt.Errorf("operation blocked by safety policy: %s (do not retry; this is a hard safety limit, not a user rejection)", reason)
			}

			dangerous, reason := dangerousToolCall(projectRoot, toolName, args)

			scope, hasScope := currentExecutionScopeAny(args)
			// Live mode lookup: the mode is resolved PER ASK, not frozen at run
			// start, so a /mode change from any endpoint governs the in-flight
			// run's later asks. ModeGetter carries the gateway's re-resolution
			// (explicit request mode wins, else current persisted preference);
			// the static snapshot is the fallback when no getter is installed.
			mode := ApprovalOnRequest
			if hasScope {
				switch {
				case scope.ModeGetter != nil:
					if live := scope.ModeGetter(); live != "" {
						mode = live
					} else if scope.ApprovalMode != "" {
						mode = scope.ApprovalMode
					}
				case scope.ApprovalMode != "":
					mode = scope.ApprovalMode
				}
			}

			// Layer 2: mode bypass.
			if !approvalNeeded(mode, toolName, dangerous) {
				return next(args)
			}
			if !dangerous && reason == "" {
				reason = fmt.Sprintf("%s requires approval in %s mode", toolName, mode)
			}

			// Layer 3: class-level allowlist. A coarse pattern key identifies the
			// action CLASS; a matching task/person grant skips the human ask.
			// Hardline never reaches here, so a granted class can never cover a
			// hard-floor op.
			patternKey := approvalPatternKey(toolName, approvalArgs(args), reason)
			if hasScope && scope.Grants != nil && patternKey != "" {
				if granted, _ := scope.Grants.IsApprovalGranted(contextFromArgs(args), scope.TenantID, scope.PersonID, scope.TaskID, patternKey); granted {
					return next(args)
				}
			}

			// Layer 4 (H2): LLM triage, smart mode only. Sits ABOVE the human ask
			// and BELOW the hard floor (hardline ops returned already) and the
			// class-grant allowlist (a granted class returned already), so triage
			// is asked at most once per class per task. Only a dangerous
			// (non-hardline) op reaches here in smart mode. Fails SAFE: with no
			// judge, or on ESCALATE / any error / timeout, we fall through to the
			// human ask — never an auto-approval.
			if mode == ApprovalSmart && hasScope && scope.Judge != nil {
				ctx := contextFromArgs(args)
				verdict, terr := triageApproval(ctx, scope.Judge, toolName, triageSubject(toolName, approvalArgs(args)), reason)
				switch verdict {
				case TriageApprove:
					// Record a TASK-scope class grant so the judge is consulted at
					// most once per class per task, then proceed.
					if scope.Grants != nil && patternKey != "" {
						recordApprovalGrant(ctx, scope, "task", patternKey)
					}
					log.Info("smart approval: auto-approved by triage", "tool", toolName, "reason", reason, "class", patternKey)
					return next(args)
				case TriageDeny:
					// Rejection contract: reuse the "operation rejected" prefix so
					// kernel's isUserRejectionErr treats it as a decision (do NOT
					// retry a variant), not a diagnosable failure.
					log.Warn("smart approval: blocked by safety triage", "tool", toolName, "reason", reason)
					return "", fmt.Errorf("operation rejected: blocked by safety triage")
				default:
					// ESCALATE (and any error/timeout) → fall through to the human ask.
					if terr != nil {
						log.Debug("smart approval: triage escalated on error", "tool", toolName, "error", terr)
					}
				}
			}

			// Layer 5: human ask.
			if hasScope && scope.Approval != nil {
				decision, err := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
					TenantID: scope.TenantID,
					PersonID: scope.PersonID,
					TaskID:   scope.TaskID,
					RunID:    scope.RunID,
					Channel:  scope.Channel,
					ToolName: toolName,
					Reason:   reason,
					Args:     approvalArgs(args),
				})
				if err != nil {
					return "", err
				}
				if !decision.Approved {
					// The "operation rejected" prefix is a stable contract:
					// kernel's isUserRejectionErr matches it to tell the model
					// a rejection is a user decision (never retry a variant),
					// not a diagnosable failure. Keep the wording in sync.
					if decision.Reason != "" {
						return "", fmt.Errorf("operation rejected: %s", decision.Reason)
					}
					return "", fmt.Errorf("operation rejected by approval %s", decision.ApprovalID)
				}
				// Remember an "approve this class" decision so the next same-class
				// call skips the ask.
				if scope.Grants != nil && patternKey != "" {
					recordApprovalGrant(contextFromArgs(args), scope, decision.Scope, patternKey)
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

// recordApprovalGrant persists an "approve this class" decision. Scope "task"
// grants for the current task (session memory, needs a task id); "person"
// grants across all of the person's tasks (persistent memory). "" (once) and an
// empty scope record nothing. Failures are swallowed: a lost grant only costs
// one extra ask, never correctness.
func recordApprovalGrant(ctx context.Context, scope ExecutionScope, decisionScope, patternKey string) {
	if scope.Grants == nil || patternKey == "" {
		return
	}
	switch decisionScope {
	case "task":
		if scope.TaskID != "" {
			_ = scope.Grants.GrantApproval(ctx, "task", scope.TenantID, scope.PersonID, scope.TaskID, patternKey)
		}
	case "person":
		if scope.PersonID != "" {
			_ = scope.Grants.GrantApproval(ctx, "person", scope.TenantID, scope.PersonID, scope.PersonID, patternKey)
		}
	}
}

// approvalPatternKey returns a COARSE class identifier for an approval decision,
// deliberately NOT the exact command. Approving one `chmod` should approve the
// chmod CLASS for the granted scope, not just that byte-identical string. For
// exec tools the class is "exec:"+the dangerous-op reason, which carries the
// invoked binary (so "chmod 777 a" and "chmod +x b" both key to
// "exec:invokes dangerous command: chmod", while an rm keys differently). For
// write/path tools the class is the tool name plus a bucket of the reason with
// the specific path stripped, so approving one out-of-workspace write does not
// silently approve a write to a different out-of-workspace path forever — it
// still buckets by REASON class, which is the intended coarseness. Returns ""
// when there is nothing meaningful to remember.
func approvalPatternKey(toolName string, args map[string]interface{}, dangerousReason string) string {
	if isExecTool(toolName) {
		if dangerousReason != "" {
			return "exec:" + dangerousReason
		}
		return "exec:" + toolName
	}
	return toolName + ":" + patternReasonBucket(dangerousReason)
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

// hardlineDiskDevices are raw block-device path prefixes. Formatting one
// (mkfs), overwriting it (dd of=), or redirecting into it destroys partitions
// irrecoverably, so any such target is a hard block.
var hardlineDiskDevices = []string{"/dev/sd", "/dev/nvme", "/dev/hd", "/dev/vd", "/dev/xvd"}

// hardlineToolCall is the UNBYPASSABLE safety floor. It fires before any
// approval-mode bypass (full-auto included) because these operations have a
// blast radius no session-level "yes" should ever authorize:
//
//   - recursive delete of the filesystem root or a protected system/home root
//     (rm -rf /, /home, $HOME, /etc, /usr, /var, /boot);
//   - raw-disk destruction: mkfs*, dd of=/dev/sdX|/dev/nvmeX, or any redirect
//     into a /dev/sdX|/dev/nvmeX device;
//   - fork bomb (:(){ :|:& };:) which exhausts the process table;
//   - powering the host down or rebooting it out from under a running task
//     (shutdown, reboot, halt, poweroff, init 0, init 6).
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
	// These are substring checks on the FULL command, so they already pierce
	// any shell/priv wrapper (`bash -c "dd ... of=/dev/sda"`).
	for _, dev := range hardlineDiskDevices {
		if strings.Contains(lower, "of="+dev) {
			return true, "overwrites raw disk device: " + dev + "*"
		}
		if strings.Contains(lower, "> "+dev) || strings.Contains(lower, ">"+dev) {
			return true, "redirects over raw disk device: " + dev + "*"
		}
	}

	// Per-segment program + argument inspection over the WRAPPER-UNWRAPPED
	// segments, so `bash -c "rm -rf /"`, `sudo rm -rf /`, and `env rm -rf $HOME`
	// are classified by their real inner program, not the wrapper. The floor
	// stays TIGHT: an unwrappable payload does NOT hard-block here (that only
	// raises the dangerous heuristic) — deny only what we can positively identify.
	home := strings.TrimRight(os.Getenv("HOME"), "/")
	segs, _ := expandCommandSegments(cmd, 0)
	for _, fields := range segs {
		progIdx, ok := segmentProgram(fields)
		if !ok {
			continue
		}
		base := filepath.Base(fields[progIdx])
		rest := fields[progIdx+1:]
		switch {
		case base == "mkfs" || strings.HasPrefix(base, "mkfs."):
			return true, "formats a filesystem: " + base
		case base == "shutdown" || base == "reboot" || base == "halt" || base == "poweroff":
			return true, "powers down or reboots the host: " + base
		case base == "init" && len(rest) > 0 && (rest[0] == "0" || rest[0] == "6"):
			return true, "changes runlevel to halt/reboot: init " + rest[0]
		case base == "rm":
			if target, hit := hardlineRmProtectedRoot(rest, home); hit {
				return true, "recursive delete of protected root: " + target
			}
		}
	}
	return false, ""
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

// hardlineProtectedRootTarget normalizes an rm target and reports whether it is
// a protected root. It handles $HOME/~ expansion and a trailing /* glob
// (rm -rf /* is as catastrophic as rm -rf /).
func hardlineProtectedRootTarget(target, home string) (string, bool) {
	t := strings.Trim(target, `"'`)
	if t == "$HOME" || t == "${HOME}" || t == "~" {
		return "$HOME", true
	}
	clean := cleanShellPathTarget(t)
	// Collapse a trailing glob so "/*" and "/home/*" map to their root.
	if strings.HasSuffix(clean, "/*") {
		clean = strings.TrimSuffix(clean, "/*")
		if clean == "" {
			clean = "/"
		}
	}
	if _, bad := hardlineProtectedRoots[clean]; bad {
		return clean, true
	}
	if home != "" && clean == cleanShellPathTarget(home) {
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

// segmentProgram returns the index of the invoked program in a segment's
// fields, skipping leading VAR=value environment assignments. ok=false when the
// segment is only assignments.
func segmentProgram(fields []string) (int, bool) {
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.ContainsAny(fields[i], "/\\") {
		i++
	}
	if i >= len(fields) {
		return 0, false
	}
	return i, true
}

// dangerousBinaries are program names that warrant explicit approval before
// execution. Matched by basename so that both `rm` and `/bin/rm` are caught.
var dangerousBinaries = map[string]struct{}{
	"rm": {}, "rmdir": {}, "chmod": {}, "chown": {}, "kill": {}, "pkill": {},
	"killall": {}, "shutdown": {}, "reboot": {}, "halt": {}, "poweroff": {},
	"mkfs": {}, "dd": {}, "fdisk": {}, "shred": {}, "wipefs": {},
}

// destructiveSubstrings are raw patterns that are dangerous regardless of how
// the command is tokenized (e.g. redirecting over a device node).
var destructiveSubstrings = []string{"> /dev/", ":(){", "/dev/sd", "/dev/nvme"}

func dangerousToolCall(projectRoot, toolName string, args map[string]interface{}) (bool, string) {
	if isExecTool(toolName) {
		cmd := execCommandPayload(toolName, args)
		for _, pattern := range destructiveSubstrings {
			if strings.Contains(cmd, pattern) {
				return true, fmt.Sprintf("contains dangerous pattern: %s", pattern)
			}
		}
		// Inspect each WRAPPER-UNWRAPPED segment's program by basename, defeating
		// both prefix tricks like /bin/rm AND wrapper tricks like
		// `bash -c "rm ..."` / `sudo rm ...` / `env rm ...`.
		segs, unparsed := expandCommandSegments(cmd, 0)
		for _, fields := range segs {
			progIdx, ok := segmentProgram(fields)
			if !ok {
				continue
			}
			base := filepath.Base(fields[progIdx])
			if _, bad := dangerousBinaries[base]; bad {
				return true, fmt.Sprintf("invokes dangerous command: %s", base)
			}
		}
		// A wrapper whose payload we could not extract (e.g. `bash script.sh`,
		// `sh -s`) is opaque: treat it as dangerous (needs approval) but never
		// hardline. This is the "can't parse → dangerous, not hard-blocked" rule.
		if unparsed {
			return true, "invokes an opaque wrapped command"
		}
	}

	path, _ := args["path"].(string)
	if path == "" {
		return false, ""
	}
	if strings.Contains(path, "/etc/") || strings.Contains(path, "/root/") || strings.Contains(path, "/dev/") {
		return true, fmt.Sprintf("accesses restricted path: %s", path)
	}
	if projectRoot != "" && filepath.IsAbs(path) && !strings.HasPrefix(filepath.Clean(path), filepath.Clean(projectRoot)) {
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

// maxWrapperDepth bounds how deep the wrapper unwrapper recurses (e.g.
// `sudo bash -c "env rm -rf /"`), so pathological nesting cannot spin the
// classifier.
const maxWrapperDepth = 3

// shellDashCWrappers invoke a shell that runs a `-c "<script>"` payload; the
// real command lives inside the quoted script, invisible to a first-token scan.
var shellDashCWrappers = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {},
}

// execPrefixWrappers prefix another command (optionally after their own flags
// and, for env, VAR=value assignments). The wrapped program is the real target.
var execPrefixWrappers = map[string]struct{}{
	"env": {}, "sudo": {}, "doas": {}, "xargs": {}, "nohup": {}, "timeout": {},
	"nice": {}, "ionice": {}, "setsid": {}, "stdbuf": {}, "command": {},
}

// wrapperValueFlags lists, per exec-prefix wrapper, the SEPARATED short flags
// that consume the following token as their value (e.g. `sudo -u root <cmd>`).
// Skipping the value is what stops `sudo -u root rm -rf /` from mis-identifying
// `root` as the command and missing the `rm`. Attached forms (`-uroot`,
// `--user=root`) are self-contained single tokens and need no entry.
var wrapperValueFlags = map[string]map[string]struct{}{
	"sudo":    {"-u": {}, "-g": {}, "-p": {}, "-C": {}, "-r": {}, "-t": {}, "-T": {}, "-U": {}, "-h": {}},
	"doas":    {"-u": {}, "-C": {}},
	"env":     {"-u": {}, "-C": {}, "-S": {}},
	"nice":    {"-n": {}},
	"ionice":  {"-c": {}, "-n": {}, "-p": {}, "-P": {}},
	"timeout": {"-s": {}, "-k": {}, "--signal": {}, "--kill-after": {}},
	"xargs":   {"-n": {}, "-I": {}, "-P": {}, "-L": {}, "-s": {}, "-d": {}, "-E": {}, "-a": {}},
	"stdbuf":  {"-i": {}, "-o": {}, "-e": {}},
}

// expandCommandSegments splits a command line into its EFFECTIVE segments,
// recursively unwrapping shell/priv/exec wrappers so downstream classifiers see
// the wrapped payload. Each returned segment starts at its real program (leading
// env assignments stripped). unparsed reports whether any wrapper's payload
// could not be extracted, so the caller can degrade to "dangerous, not hardline".
// Shared by hardlineToolCall (the floor) and dangerousToolCall (the heuristic).
func expandCommandSegments(cmd string, depth int) (segs [][]string, unparsed bool) {
	for _, seg := range splitTopLevelSegments(cmd) {
		fields := shellFields(seg)
		if len(fields) == 0 {
			continue
		}
		sub, u := expandSegment(fields, depth)
		segs = append(segs, sub...)
		unparsed = unparsed || u
	}
	return segs, unparsed
}

// expandSegment unwraps a single tokenized segment. A shell `-c` wrapper is
// replaced by the segments of its script; an exec-prefix wrapper is replaced by
// re-classifying from its wrapped program. A wrapper we cannot see through
// yields the wrapper segment itself plus unparsed=true.
func expandSegment(fields []string, depth int) (segs [][]string, unparsed bool) {
	progIdx, ok := segmentProgram(fields)
	if !ok {
		return nil, false
	}
	base := strings.ToLower(filepath.Base(fields[progIdx]))
	rest := fields[progIdx+1:]
	if depth < maxWrapperDepth {
		if _, isShell := shellDashCWrappers[base]; isShell {
			if script, found := dashCScript(rest); found {
				return expandCommandSegments(script, depth+1)
			}
			return [][]string{fields[progIdx:]}, true
		}
		if _, isExec := execPrefixWrappers[base]; isExec {
			if inner, found := execWrappedCommand(base, rest); found {
				return expandSegment(inner, depth+1)
			}
			return [][]string{fields[progIdx:]}, true
		}
	}
	return [][]string{fields[progIdx:]}, false
}

// dashCScript returns the script a shell `-c` runs. Because shellFields strips
// quotes and splits on whitespace, a multi-word script like "rm -rf /" arrives
// as several tokens; rejoining everything after the `-c` flag reconstructs it
// (a small over-capture of any positional $0/$1 args after the script is
// harmless — it only adds more tokens to classify). Matches `-c`, `--command`,
// and a combined short cluster ending in c (e.g. `-lc`).
func dashCScript(rest []string) (string, bool) {
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		isDashC := tok == "-c" || tok == "--command" ||
			(strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--") && len(tok) > 1 && strings.HasSuffix(tok, "c"))
		if isDashC {
			if i+1 < len(rest) {
				return strings.Join(rest[i+1:], " "), true
			}
			return "", false
		}
	}
	return "", false
}

// execWrappedCommand skips an exec-prefix wrapper's own flags, flag-values, and
// env assignments (and timeout's leading DURATION) to return the wrapped command
// fields. found=false when everything was consumed as wrapper options.
func execWrappedCommand(wrapper string, rest []string) ([]string, bool) {
	valueFlags := wrapperValueFlags[wrapper]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "--":
			if i+1 < len(rest) {
				return rest[i+1:], true
			}
			return nil, false
		case strings.HasPrefix(tok, "-"):
			if valueFlags != nil {
				if _, needsValue := valueFlags[tok]; needsValue && i+1 < len(rest) {
					i++ // consume the flag's separated value
				}
			}
		case strings.Contains(tok, "=") && !strings.ContainsAny(tok, "/\\"):
			// leading VAR=value assignment (env), skip
		case wrapper == "timeout" && isDurationToken(tok):
			// `timeout DURATION cmd...`: the duration precedes the command
		default:
			return rest[i:], true
		}
	}
	return nil, false
}

// isDurationToken reports whether tok looks like a timeout duration (5, 30s,
// 2m, 1.5h). Used only to skip timeout's leading argument; a real command name
// never matches this shape.
func isDurationToken(tok string) bool {
	if tok == "" {
		return false
	}
	if s := tok[len(tok)-1]; s == 's' || s == 'm' || s == 'h' || s == 'd' {
		tok = tok[:len(tok)-1]
	}
	if tok == "" {
		return false
	}
	seenDot := false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !seenDot:
			seenDot = true
		default:
			return false
		}
	}
	return true
}

// splitTopLevelSegments splits s on shell separators (; | & newline backtick
// parens). It is deliberately NOT quote-aware: a separator inside quotes still
// splits, which is strictly more CONSERVATIVE for approval classification (a
// command hidden after an in-quote `;` still surfaces as its own segment) and,
// combined with the quote-stripping in shellFields, lets the classifier see a
// shell command embedded inside a code payload — e.g. execute_code running
// `os.system('rm -rf /')`, where the parens/quotes are just delimiters.
func splitTopLevelSegments(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ';', '|', '&', '\n', '`', '(', ')':
			return true
		}
		return false
	})
}

// shellFields strips shell quote characters and splits the segment on
// whitespace. Real shells GROUP a quoted span into one argument; treating its
// words individually instead is intentional here — it can only over-split (a
// false "dangerous", which merely asks for approval), never hide a program,
// and it means a wrapped `-c "rm -rf /"` script re-tokenizes cleanly once
// dashCScript rejoins the tokens after `-c`.
func shellFields(seg string) []string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' {
			return -1
		}
		return r
	}, seg)
	return strings.Fields(cleaned)
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
	out := make(map[string]interface{})
	for k, v := range args {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}
	return out
}

func SkillMetricsMiddleware(skillStore interface {
	RecordResult(ctx context.Context, tenantID, skillName string, success bool) error
}) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)
			tenantID, _ := args["_tenant_id"].(string)
			if !strings.HasPrefix(toolName, "skill:") || skillStore == nil || tenantID == "" {
				return next(args)
			}

			skillName := strings.TrimPrefix(toolName, "skill:")
			result, err := next(args)
			success := err == nil
			_ = skillStore.RecordResult(context.Background(), tenantID, skillName, success)
			if success {
				_ = MarkSkillUsed(tenantID, skillName)
			}
			return result, err
		}
	}
}
