package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// sandboxWarnOnce keeps an auto-fallback warning from flooding the daemon log.
var sandboxWarnOnce sync.Once

// =============================================================================
// Code Execution Tool
// =============================================================================

// ExecuteCodeTool 代码执行沙箱
type ExecuteCodeTool struct {
	BaseTool
	allowedTools []string
	timeoutSecs  int
}

func NewExecuteCodeTool() *ExecuteCodeTool {
	return &ExecuteCodeTool{
		BaseTool: BaseTool{
			name:        "execute_code",
			description: "在沙箱中执行 Python 代码，可调用内置工具",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"code": {
						Type:        "string",
						Description: "要执行的 Python 代码",
					},
					"language": {
						Type:        "string",
						Description: "语言，默认 python",
						Default:     "python",
					},
					"timeout": {
						Type:        "integer",
						Description: "超时秒数，默认 300",
						Default:     300,
					},
					"execution_class": toolExecutionClassProperty(),
					"sandbox": {
						Type:        "string",
						Description: "Execution isolation: auto prefers an isolated filesystem sandbox whose network follows exec_sandbox.allow_network; isolated requires it; host uses host credentials/network and requires approval",
						Enum:        []string{"auto", "isolated", "host"},
						Default:     "auto",
					},
				},
				Required: []string{"code"},
			},
		},
		allowedTools: []string{"web_search", "web_extract", "read_file", "write_file", "search_files", "terminal"},
		timeoutSecs:  300,
	}
}

func (t *ExecuteCodeTool) Execute(args map[string]interface{}) (string, error) {
	code, ok := args["code"].(string)
	if !ok || code == "" {
		return "", fmt.Errorf("code is required")
	}

	language, _ := args["language"].(string)
	if language == "" {
		language = "python"
	}

	profile, profileErr := resolveToolProfile(args, 300)
	if profileErr != nil {
		return "", profileErr
	}
	timeout := int(profile.Timeout / time.Second)
	args["_execution_class"] = string(profile.Class)
	args["_timeout_seconds"] = timeout
	args["_timeout_clamped"] = profile.TimeoutClamped
	if profile.RequestedTimeout > 0 {
		args["_timeout_requested_seconds"] = int(profile.RequestedTimeout / time.Second)
	}

	if language != "python" {
		return "", fmt.Errorf("only python is supported currently")
	}

	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		home = "."
	}
	tmpDir, err := filepath.Abs(filepath.Join(home, ".selfmind", "code_sandbox"))
	if err != nil {
		return "", fmt.Errorf("resolve sandbox dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("create sandbox dir: %w", err)
	}

	scriptPath := filepath.Join(tmpDir, fmt.Sprintf("script_%d.py", time.Now().UnixNano()))

	// Write the user code verbatim to a script file and run it directly. Do NOT
	// wrap it in exec('''...''') string interpolation: that allowed code
	// containing triple quotes to break out of the literal and inject arbitrary
	// Python. Running the file directly is both safer and gives accurate
	// tracebacks/line numbers.
	if err := os.WriteFile(scriptPath, []byte(code), 0o600); err != nil {
		return "", fmt.Errorf("write script: %w", err)
	}
	defer os.Remove(scriptPath)

	ctx, cancel := context.WithTimeout(contextFromArgs(args), profile.Timeout)
	defer cancel()

	requestedMode, modeErr := requestedSandboxMode(args)
	if modeErr != nil {
		return "", enrichToolFailure("execute_code", modeErr, "")
	}
	cmd, decision, sandboxErr := sandboxedCommand(ctx, []string{"python3", "-I", scriptPath}, tmpDir, requestedMode, networkSharedArg(args))
	if sandboxErr != nil {
		return "", enrichToolFailure("execute_code", sandboxErr, "")
	}
	args["_sandbox_mode"] = string(decision.Mode)
	args["_sandbox_reason"] = decision.Reason
	emitToolProgress(kernel.EventChannelFromContext(ctx), "tool.sandbox", map[string]interface{}{
		"tool_name":    "execute_code",
		"tool_call_id": stringArg(args, "_tool_call_id"),
		"mode":         string(decision.Mode),
		"reason":       decision.Reason,
		"network":      decision.NetworkShared,
	}, string(decision.Mode))
	if decision.Mode == SandboxHost {
		sandboxWarnOnce.Do(func() {
			log.Warn("execute_code is running on the host without OS isolation", "reason", decision.Reason)
		})
	}
	cmd.Dir = tmpDir
	applySandboxLimits(cmd)

	output, err := runCommandStreaming(ctx, cmd, "python3 -I <script>", "execute_code", stringArg(args, "_tool_call_id"), profile)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			failure := fmt.Errorf("execution timed out after %s", timeoutSummary(profile))
			return output, enrichSandboxTimeout("execute_code", failure, output, decision)
		}
		return output, enrichToolFailure("execute_code", fmt.Errorf("execution error: %w", err), output)
	}

	result := output
	if len(result) > 50*1024 {
		result = result[:50*1024] + "\n... (output truncated)"
	}
	return result, nil
}
