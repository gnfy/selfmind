package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

type evidenceFileSnapshot struct {
	path         string
	resolvedPath string
	operation    string
	before       string
}

// EvidenceMiddleware records facts observed by the runtime. It does not decide
// whether a run succeeded and it never asks a model for a verdict.
func EvidenceMiddleware() ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			toolName, _ := args["_tool_name"].(string)
			if toolName != "write_file" && toolName != "patch" && toolName != "terminal" && toolName != "verify" {
				return next(args)
			}
			var binding *verification.Binding
			if toolName == "verify" {
				var err error
				binding, err = prepareVerificationBinding(args)
				if err != nil {
					return kernel.ToolDispatchResult{Invoked: new(bool)}, newStableToolError(err, "verification_reference_invalid", "stale_precondition", err.Error(), "Use replaces and reason to inherit an eligible recorded obligation. Preserve its working directory. The runtime supplies omitted criterion, target and dependencies; historical unbound checks cannot acquire replacement authority.")
				}
			}

			started := time.Now()
			files := evidenceSnapshots(toolName, args)
			for i := range files {
				if canonical, err := canonicalContainmentPath(files[i].path); err == nil {
					files[i].resolvedPath = canonical
				}
			}
			result, err := next(args)
			finished := time.Now()
			evidence := kernel.RunEvidence{
				ToolCallID: stringArg(args, "_tool_call_id"),
				ToolName:   toolName,
				Kind:       evidenceKind(toolName),
				Status:     evidenceStatus(err),
				StartedAt:  started.UnixNano(),
				FinishedAt: finished.UnixNano(),
			}
			if err != nil {
				evidence.Error = RedactSensitive(err.Error())
			}
			for _, snapshot := range files {
				evidence.Files = append(evidence.Files, kernel.FileEffect{
					Path:         snapshot.path,
					ResolvedPath: snapshot.resolvedPath,
					Operation:    snapshot.operation,
					BeforeSHA256: snapshot.before,
					AfterSHA256:  fileSHA256(snapshot.path),
				})
			}
			if toolName == "terminal" || toolName == "verify" {
				exitCode := -1
				if result.Process != nil && result.Process.ExitCode != nil {
					exitCode = *result.Process.ExitCode
				}
				kind := stringArg(args, "kind")
				if kind == "" {
					kind = evidenceKind(toolName)
				}
				evidence.Command = &kernel.CommandEvidence{Binding: binding,
					Command:  RedactSensitive(stringArg(args, "command")),
					CWD:      stringArg(args, "cwd"),
					Kind:     kind,
					ExitCode: exitCode,
				}
			}
			result.Evidence = append(result.Evidence, evidence)
			emitEvidence(args, evidence)
			if evidence.Command != nil && evidence.Command.Binding != nil {
				result.Output += "\nVerification evidence: " + evidence.ToolCallID
				if err != nil && strings.TrimSpace(binding.StepID) != "" {
					result.Output += "\nThis verification_required obligation remains open. Keep exactly its existing plan step in_progress, keep diagnostic and report steps pending, and correct this same check with replaces. Cancel any separate retry step for this same check."
				}
				if err == nil && strings.TrimSpace(binding.Replaces) != "" && strings.TrimSpace(binding.StepID) != "" {
					result.Output += "\nThis replacement remains bound to the original verification obligation. Before completing the plan, keep the original obligation and cancel any separate step created only for this retry; verify another verification_required step only when it represents a distinct condition."
				}
				if len(binding.UnresolvedLocalDependencies) > 0 {
					result.Output += "\nDependency identity unresolved: " + strings.Join(binding.UnresolvedLocalDependencies, ", ") + ". Relative paths use tool cwd, not shell-internal cd. This check conservatively depends on all local changes; inspect and correct the declared inputs before relying on independence."
				}
			}
			return result, err
		}
	}
}

func emitEvidence(args map[string]interface{}, evidence kernel.RunEvidence) {
	ctx, _ := args["_context"].(context.Context)
	if ctx == nil {
		return
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
		Type:    "evidence.recorded",
		Payload: map[string]interface{}{"evidence": evidence},
	})
}

func evidenceKind(toolName string) string {
	if toolName == "verify" {
		return "verification"
	}
	if toolName == "terminal" {
		return "command"
	}
	return "mutation"
}

func evidenceStatus(err error) string {
	if err == nil {
		return "succeeded"
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "approval") || strings.Contains(lower, "denied") || strings.Contains(lower, "blocked") {
		return "blocked"
	}
	return "failed"
}

func evidenceSnapshots(toolName string, args map[string]interface{}) []evidenceFileSnapshot {
	switch toolName {
	case "write_file":
		path := stringArg(args, "path")
		if path != "" {
			return []evidenceFileSnapshot{{path: path, operation: "write", before: fileSHA256(path)}}
		}
	case "patch":
		ops, err := parseV4APatch(stringArg(args, "patch"))
		if err != nil {
			return nil
		}
		out := make([]evidenceFileSnapshot, 0, len(ops)*2)
		for _, op := range ops {
			switch op.Operation {
			case OpAdd:
				out = append(out, evidenceFileSnapshot{path: op.FilePath, operation: "create", before: fileSHA256(op.FilePath)})
			case OpUpdate:
				out = append(out, evidenceFileSnapshot{path: op.FilePath, operation: "update", before: fileSHA256(op.FilePath)})
			case OpDelete:
				out = append(out, evidenceFileSnapshot{path: op.FilePath, operation: "delete", before: fileSHA256(op.FilePath)})
			case OpMove:
				out = append(out,
					evidenceFileSnapshot{path: op.FilePath, operation: "move_from", before: fileSHA256(op.FilePath)},
					evidenceFileSnapshot{path: op.NewPath, operation: "move_to", before: fileSHA256(op.NewPath)},
				)
			}
		}
		return out
	}
	return nil
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
