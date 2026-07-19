package tools

import (
	"bytes"
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
					"sandbox": {
						Type:        "string",
						Description: "Execution isolation: auto prefers an isolated no-network sandbox; isolated requires it; host uses host credentials/network and requires approval",
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

	timeout := 300
	if to, ok := args["timeout"].(int); ok {
		timeout = to
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

	ctx, cancel := context.WithTimeout(contextFromArgs(args), time.Duration(timeout)*time.Second)
	defer cancel()

	requestedMode, modeErr := requestedSandboxMode(args)
	if modeErr != nil {
		return "", enrichToolFailure("execute_code", modeErr, "")
	}
	cmd, decision, sandboxErr := sandboxedCommand(ctx, []string{"python3", "-I", scriptPath}, tmpDir, requestedMode)
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
	}, string(decision.Mode))
	if decision.Mode == SandboxHost {
		sandboxWarnOnce.Do(func() {
			log.Warn("execute_code is running on the host without OS isolation", "reason", decision.Reason)
		})
	}
	cmd.Dir = tmpDir
	applySandboxLimits(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("execution timed out after %d seconds", timeout)
		}
		return "", fmt.Errorf("execution error: %v\n%s", err, stderr.String())
	}

	result := stdout.String()
	if stderr.Len() > 0 {
		result += stderr.String()
	}
	if len(result) > 50*1024 {
		result = result[:50*1024] + "\n... (output truncated)"
	}
	return result, nil
}
