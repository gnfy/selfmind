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
	timeoutSecs int
}

func NewExecuteCodeTool() *ExecuteCodeTool {
	return &ExecuteCodeTool{
		BaseTool: BaseTool{
			name:        "execute_code",
			description: "Execute one isolated Python script for bounded local computation. This tool does not expose SelfMind tools inside Python; use normal tool calls for files or external actions. The script runs offline; set network: true when it needs network access, such as calling a remote service.",
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
						Description: "Execution isolation: auto prefers an isolated filesystem sandbox that gets the daemon's network only when network is set; isolated requires it; host uses host credentials/network under the current approval mode",
						Enum:        []string{"auto", "isolated", "host"},
						Default:     "auto",
					},
					"network": codeNetworkRequestProperty(),
				},
				Required: []string{"code"},
			},
		},
		timeoutSecs: 300,
	}
}

func (t *ExecuteCodeTool) Execute(args map[string]interface{}) (string, error) {
	result, err := t.ExecuteResult(args)
	return result.Output, err
}

func (t *ExecuteCodeTool) ExecuteResult(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	code, ok := args["code"].(string)
	if !ok || code == "" {
		return kernel.ToolDispatchResult{}, fmt.Errorf("code is required")
	}

	language, _ := args["language"].(string)
	if language == "" {
		language = "python"
	}

	profile, profileErr := resolveToolProfile(args, 300)
	if profileErr != nil {
		return kernel.ToolDispatchResult{}, profileErr
	}
	timeout := int(profile.Timeout / time.Second)
	args["_execution_class"] = string(profile.Class)
	args["_timeout_seconds"] = timeout
	args["_timeout_clamped"] = profile.TimeoutClamped
	if profile.RequestedTimeout > 0 {
		args["_timeout_requested_seconds"] = int(profile.RequestedTimeout / time.Second)
	}

	if language != "python" {
		return kernel.ToolDispatchResult{}, fmt.Errorf("only python is supported currently")
	}

	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		home = "."
	}
	sandboxRoot, err := filepath.Abs(filepath.Join(home, ".selfmind", "code_sandbox"))
	if err != nil {
		return kernel.ToolDispatchResult{}, fmt.Errorf("resolve sandbox dir: %w", err)
	}
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		return kernel.ToolDispatchResult{}, fmt.Errorf("create sandbox dir: %w", err)
	}
	// Each call gets its own private directory, and only that directory is a
	// writable root. The shared parent is just a container: as a root it let a
	// concurrent Run's sandboxed code replace this call's script between the write
	// and python opening it, running that code inside this Run's boundary.
	tmpDir, err := os.MkdirTemp(sandboxRoot, "call-")
	if err != nil {
		return kernel.ToolDispatchResult{}, fmt.Errorf("create sandbox dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	scriptPath := filepath.Join(tmpDir, "script.py")

	// Write the user code verbatim to a script file and run it directly. Do NOT
	// wrap it in exec('''...''') string interpolation: that allowed code
	// containing triple quotes to break out of the literal and inject arbitrary
	// Python. Running the file directly is both safer and gives accurate
	// tracebacks/line numbers.
	if err := os.WriteFile(scriptPath, []byte(code), 0o600); err != nil {
		return kernel.ToolDispatchResult{}, fmt.Errorf("write script: %w", err)
	}

	ctx, cancel := context.WithTimeout(contextFromArgs(args), profile.Timeout)
	defer cancel()

	requestedMode, modeErr := requestedSandboxMode(args)
	if modeErr != nil {
		return kernel.ToolDispatchResult{}, enrichToolFailure("execute_code", modeErr, "")
	}
	// Same engine entry as the shell tools: an argv request instead of a shell
	// one. Routing it separately is how execute_code previously missed the run's
	// environment binding and scratch space.
	scope, _ := currentExecutionScopeAny(args)
	boundary, boundaryReason := approvedSandboxBoundary(args)
	request := ExecutionRequest{
		ToolName:       "execute_code",
		ToolCallID:     stringArg(args, "_tool_call_id"),
		RunID:          scope.RunID,
		LeaseID:        scope.LeaseID,
		Command:        []string{"python3", "-I", scriptPath},
		CWD:            tmpDir,
		WorkspaceRoots: append(append([]string{}, scope.AllowedRoots...), tmpDir),
		Sandbox:        requestedMode,
		Boundary:       boundary,
		BoundaryReason: boundaryReason,
		NetworkShared:  networkSharedArg(args),
		Timeout:        profile.Timeout,
		ToolProfile:    profile,
	}
	request.NetworkDecision, request.NetworkPermission = stringArg(args, networkDecisionArg), stringArg(args, networkPermissionArg)
	result, execErr := Execute(ctx, request, args)
	decision := SandboxDecision{Mode: result.Plan.Mode, NetworkShared: result.Plan.NetworkMode == "shared"}
	args["_sandbox_mode"] = string(decision.Mode)
	if decision.Mode == SandboxHost {
		sandboxWarnOnce.Do(func() {
			log.Warn("execute_code is running on the host without OS isolation", "reason", decision.Reason)
		})
	}
	if execErr != nil {
		return processToolResult(result), execErr
	}
	output := result.Output
	if len(output) > 50*1024 {
		output = output[:50*1024] + "\n... (output truncated)"
	}
	result.Output = output
	return processToolResult(result), nil
}
