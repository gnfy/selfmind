package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

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
	os.MkdirAll(tmpDir, 0755)

	scriptPath := filepath.Join(tmpDir, fmt.Sprintf("script_%d.py", time.Now().UnixNano()))
	outputPath := filepath.Join(tmpDir, fmt.Sprintf("output_%d.txt", time.Now().UnixNano()))

	wrappedCode := fmt.Sprintf(`import sys
sys.stdout = open('%s', 'w')
sys.stderr = sys.stdout
exec('''%s''')
sys.stdout.flush()
`, outputPath, code)

	if err := os.WriteFile(scriptPath, []byte(wrappedCode), 0755); err != nil {
		return "", fmt.Errorf("write script: %w", err)
	}
	defer os.Remove(scriptPath)
	defer os.Remove(outputPath)

	cmd := exec.Command("python3", scriptPath)
	cmd.Dir = tmpDir

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

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

	output, _ := os.ReadFile(outputPath)
	result := string(output)
	if len(result) > 50*1024 {
		result = result[:50*1024] + "\n... (output truncated)"
	}
	return result, nil
}
