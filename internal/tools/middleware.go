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
	// ApprovalFullAuto auto-approves everything (workspace scope still applies).
	ApprovalFullAuto ApprovalMode = "full-auto"
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
	default:
		return ApprovalOnRequest
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

func SmartApprovalMiddleware(projectRoot string) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)
			dangerous, reason := dangerousToolCall(projectRoot, toolName, args)

			mode := ApprovalOnRequest
			if scope, ok := currentExecutionScopeAny(args); ok && scope.ApprovalMode != "" {
				mode = scope.ApprovalMode
			}
			if !approvalNeeded(mode, toolName, dangerous) {
				return next(args)
			}
			if !dangerous && reason == "" {
				reason = fmt.Sprintf("%s requires approval in %s mode", toolName, mode)
			}

			if scope, ok := currentExecutionScopeAny(args); ok && scope.Approval != nil {
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
					if decision.Reason != "" {
						return "", fmt.Errorf("operation rejected: %s", decision.Reason)
					}
					return "", fmt.Errorf("operation rejected by approval %s", decision.ApprovalID)
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
