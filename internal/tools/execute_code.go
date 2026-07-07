package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

// sandboxWarnOnce ensures the "no real isolation" warning is emitted once per
// process, not on every execute_code call.
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

	// Residual-risk warning: applySandboxLimits is process-group containment
	// only, NOT real isolation (namespaces/seccomp/cgroups/container). Code that
	// reaches here has already cleared the approval funnel, but there is no OS
	// sandbox confining what it can touch on the host. Emitted once per process.
	sandboxWarnOnce.Do(func() {
		log.Warn("execute_code runs without OS isolation (process-group containment only); the executed code has full host access under this user")
	})

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

	tmpDir := filepath.Join(os.Getenv("HOME"), ".selfmind", "code_sandbox")
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// CommandContext ensures the process is actually killed when the timeout
	// fires. The previous code passed a context that was never wired to the
	// command, so long-running scripts ran unbounded.
	cmd := exec.CommandContext(ctx, "python3", "-I", scriptPath)
	cmd.Dir = tmpDir
	applySandboxLimits(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
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
