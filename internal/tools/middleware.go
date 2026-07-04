package tools

import (
	"context"
	"fmt"
	"os"
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
		// Gates on a dangerous op like on-request. The smart-specific behavior
		// (LLM triage before the human ask) lives in SmartApprovalMiddleware,
		// layered after this gate; the gate itself stays here.
		return dangerous
	case ApprovalReadOnly:
		return isWriteTool(toolName) || isExecTool(toolName) || dangerous
	case ApprovalAutoEdit:
		// Edits flow freely (unless the heuristic flags them, e.g. out of
		// workspace); command execution always asks.
		return isExecTool(toolName) || dangerous
	default: // on-request
		return dangerous
	}
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
			mode := ApprovalOnRequest
			if hasScope && scope.ApprovalMode != "" {
				mode = scope.ApprovalMode
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
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return false, ""
	}
	lower := strings.ToLower(cmd)

	// Fork bomb: match the self-replicator regardless of internal spacing.
	if strings.Contains(strings.ReplaceAll(cmd, " ", ""), ":(){") {
		return true, "fork bomb (process-table exhaustion)"
	}

	// Raw-disk destruction via dd of=/dev/... or a redirect into /dev/...
	for _, dev := range hardlineDiskDevices {
		if strings.Contains(lower, "of="+dev) {
			return true, "overwrites raw disk device: " + dev + "*"
		}
		if strings.Contains(lower, "> "+dev) || strings.Contains(lower, ">"+dev) {
			return true, "redirects over raw disk device: " + dev + "*"
		}
	}

	// Per-segment program + argument inspection.
	home := strings.TrimRight(os.Getenv("HOME"), "/")
	for _, fields := range shellSegments(cmd) {
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
	clean := filepath.Clean(t)
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
	if home != "" && clean == filepath.Clean(home) {
		return clean, true
	}
	return "", false
}

// shellSegments splits a command line into segments on shell separators and
// returns the fields of each non-empty segment. It is a heuristic for approval
// gating (like tokenizeCommand), not a real shell parser.
func shellSegments(cmd string) [][]string {
	raw := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ';', '|', '&', '\n', '`', '(', ')':
			return true
		}
		return false
	})
	out := make([][]string, 0, len(raw))
	for _, seg := range raw {
		if fs := strings.Fields(seg); len(fs) > 0 {
			out = append(out, fs)
		}
	}
	return out
}

// segmentProgram returns the index of the invoked program in a segment's
// fields, skipping leading VAR=value environment assignments (mirrors
// tokenizeCommand). ok=false when the segment is only assignments.
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
	if toolName == "execute_command" || toolName == "terminal" {
		cmd, _ := args["command"].(string)
		for _, pattern := range destructiveSubstrings {
			if strings.Contains(cmd, pattern) {
				return true, fmt.Sprintf("contains dangerous pattern: %s", pattern)
			}
		}
		// Tokenize on shell separators so each invoked program in a pipeline or
		// chain is inspected by basename, defeating prefix tricks like /bin/rm.
		for _, tok := range tokenizeCommand(cmd) {
			base := filepath.Base(tok)
			if _, bad := dangerousBinaries[base]; bad {
				return true, fmt.Sprintf("invokes dangerous command: %s", base)
			}
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

// tokenizeCommand splits a shell command line into the program names it would
// invoke. It breaks on shell separators (; | & newlines) and, for each
// segment, returns the first word that is not a leading VAR=value assignment.
// This is a heuristic for approval gating, not a shell parser.
func tokenizeCommand(cmd string) []string {
	segments := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ';', '|', '&', '\n', '`', '(', ')':
			return true
		}
		return false
	})
	var progs []string
	for _, seg := range segments {
		for _, field := range strings.Fields(seg) {
			if strings.Contains(field, "=") && !strings.ContainsAny(field, "/\\") {
				continue // leading environment assignment, skip to the real program
			}
			progs = append(progs, field)
			break
		}
	}
	return progs
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
