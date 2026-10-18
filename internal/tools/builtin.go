package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

// ---- Built-in tool implementations ----

// WriteFileTool writes file contents.
type WriteFileTool struct {
	BaseTool
}

func NewWriteFileTool() *WriteFileTool {
	return &WriteFileTool{
		BaseTool: BaseTool{
			name:        "write_file",
			description: "Write content to a file (overwrites). This path is taken literally — env vars are not expanded. For a throwaway file (a scratch script, intermediate data) use terminal with mktemp instead, so it never lands in the workspace.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"path": {
						Type:        "string",
						Description: "File path",
					},
					"content": {
						Type:        "string",
						Description: "File content",
					},
				},
				Required: []string{"path", "content"},
			},
		},
	}
}

func (t *WriteFileTool) Execute(args map[string]interface{}) (string, error) {
	path, _ := args["path"].(string)
	content, contentOK := args["content"].(string)
	if path == "" || !contentOK {
		return "", fmt.Errorf("path and content are required")
	}
	_, release, err := acquireManagedExecution(contextFromArgs(args), args, nil, false)
	if err != nil {
		return "", err
	}
	defer release()
	// Capture the pre-image so an overwrite can be shown as a real diff instead
	// of an opaque "all-added" dump (W2d). Best-effort: a read error just means
	// we treat it as a new file.
	oldBytes, statErr := os.ReadFile(path)
	existed := statErr == nil
	if err := atomicWriteBytes(path, []byte(content), 0644); err != nil {
		return "", err
	}
	return writeFileResult(path, string(oldBytes), content, existed), nil
}

// maxWriteDiffLines bounds the diff included in the write_file result so a large
// write does not flood the model context (the TUI also bounds its preview).
const maxWriteDiffLines = 60

// writeFileResult renders the outcome of a write as a compact, diff-bearing
// message: "Created"/"Edited <path> (+A -B)" plus a bounded unified diff. The
// header verbs let the TUI recognize and colorize it.
func writeFileResult(path, oldText, newText string, existed bool) string {
	if existed && oldText == newText {
		return fmt.Sprintf("No change to %s", path)
	}
	diff, added, removed := unifiedLineDiff(oldText, newText, 3)
	verb := "Created"
	if existed {
		verb = "Edited"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s %s (+%d -%d)\n", verb, path, added, removed))
	shown := diff
	truncated := 0
	if len(diff) > maxWriteDiffLines {
		shown = diff[:maxWriteDiffLines]
		truncated = len(diff) - maxWriteDiffLines
	}
	sb.WriteString(strings.Join(shown, "\n"))
	if truncated > 0 {
		sb.WriteString(fmt.Sprintf("\n… +%d more diff line(s)", truncated))
	}
	return sb.String()
}

// ExecuteCommandTool runs a shell command.
type ExecuteCommandTool struct {
	BaseTool
}

func NewExecuteCommandTool() *ExecuteCommandTool {
	return &ExecuteCommandTool{
		BaseTool: BaseTool{
			name:        "terminal",
			description: "Execute a system command and return output. Linux commands run with Bash. $TMPDIR and $SELFMIND_RUN_TMP are this run's scratch space and persist across its commands, so `mktemp` keeps throwaway files out of the workspace.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"command": {
						Type:        "string",
						Description: "Full command to execute",
					},
					"cwd": {
						Type:        "string",
						Description: "Working directory",
						Default:     ".",
					},
					"timeout": {
						Type:        "integer",
						Description: "Timeout in seconds",
						Default:     30,
					},
					"execution_class": toolExecutionClassProperty(),
					"background": {
						Type:        "boolean",
						Description: "Whether to run in background",
						Default:     false,
					},
					"effect_claim_id": {
						Type:        "string",
						Description: "Optional unresolved external effect claim that this check observes. Only a proven read-only command or an owner-approved observation script may carry it. A successful check releases a claim of this work, or lets this run's next conflicting call pass another run's claim once; you judge whether the check shows the effect's result.",
					},
					"sandbox": {
						Type:        "string",
						Description: "Execution isolation: auto prefers an isolated filesystem sandbox that gets the daemon's network only for a command that reaches a remote service or sets network; isolated requires it; host uses host credentials/network under the current approval mode",
						Enum:        []string{"auto", "isolated", "host"},
						Default:     "auto",
					},
					"network": networkRequestProperty(),
				},
				Required: []string{"command"},
			},
		},
	}
}

func (t *ExecuteCommandTool) Execute(args map[string]interface{}) (string, error) {
	result, err := t.ExecuteResult(args)
	return result.Output, err
}

func (t *ExecuteCommandTool) ExecuteResult(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return kernel.ToolDispatchResult{}, fmt.Errorf("command is required")
	}
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = "."
	}
	background, _ := args["background"].(bool)
	if background {
		// Every refusal before the process launches reports that nothing started,
		// so the external-effect claim this call holds is released rather than
		// left uncertain for work that never ran.
		notStarted := kernel.ToolDispatchResult{Process: &kernel.ToolProcessResult{Started: false}}
		mode, err := requestedSandboxMode(args)
		if err != nil {
			return notStarted, err
		}
		if mode != SandboxHost {
			return notStarted, fmt.Errorf("background commands require sandbox=host explicitly under the current approval mode; use watch_external for durable unattended waits")
		}
		// Host execution by contract, so the foreground boundary rule applies: a
		// policy that requires isolation forbids the start, whatever approved it.
		var policy *ExecSandboxPolicy
		if scope, ok := currentExecutionScopeAny(args); ok {
			policy = scope.SandboxPolicy
		}
		approved, approvedReason := approvedSandboxBoundary(args)
		if _, err := resolveSandboxBoundaryForPolicy(mode, approved, approvedReason, runtime.GOOS, ExecSandboxAvailable(), policy); err != nil {
			return notStarted, enrichToolFailure("terminal", err, "")
		}
		managed, release, laneErr := acquireManagedExecution(contextFromArgs(args), args, nil, true)
		if laneErr != nil {
			return notStarted, laneErr
		}
		defer release()
		args["_context"] = managed
		// A background command belongs to its run: it gets the run's environment
		// binding, scratch space, and tool state overlays like any other command.
		// Skipping that made the same command behave differently depending only
		// on whether it was backgrounded.
		material := execMaterialForArgs(args, absoluteCWD(cwd))
		if material.ProfileError != nil {
			return notStarted, enrichToolFailure("terminal", fmt.Errorf("prepare execution environment: %w", material.ProfileError), "")
		}
		runCtx := contextFromArgs(args)
		emitProfilePreparation(runCtx, "terminal", stringArg(args, "_tool_call_id"), material)
		registry := ProcessRegistryForArgs(args)
		// The long-running class ceiling bounds a leaked background process. It
		// is not a work limit: watch_external is the durable, observable way to
		// wait a long time for something external.
		ceiling := backgroundProcessCeiling(args)
		// A claimed external effect stays held until this process's observed
		// exit settles it (exit 0) or leaves it uncertain.
		var onExit func(string, int, error)
		if observer, ok := args[effectProcessObserverArg].(*effectProcessObserver); ok {
			onExit = observer.exitHook()
		}
		id, err := registry.StartObservedProcessInContext(WithExecutionRuntime(contextFromArgs(args), executionRuntimeFromArgs(args)), cmdStr, cwd, material.Env, ceiling, onExit)
		if err != nil {
			return notStarted, err
		}
		// Background execution is host execution by contract above, so it is
		// recorded as such: an unattributed host escape is invisible to the
		// avoidable-escape metric.
		decision := SandboxDecision{Mode: SandboxHost, Reason: "background execution requires host mode", NetworkShared: true}
		plan := planFromMaterial(material, decision)
		emitToolProgress(kernel.EventChannelFromContext(runCtx), "tool.sandbox", map[string]interface{}{
			"tool_name":    "terminal",
			"tool_call_id": stringArg(args, "_tool_call_id"),
			"mode":         string(decision.Mode),
			"reason":       decision.Reason,
			"network":      decision.NetworkShared,
			// Host execution carries the daemon's network by construction.
			"network_decision": executionenv.NetworkDecisionShared,
			"background":       true,
			"profiles":         material.Profiles,
			// The plan is recorded even though a detached process cannot report a
			// streamed result: an execution with no auditable boundary is exactly
			// what made background commands invisible.
			"snapshot_id":        plan.SnapshotID,
			"generation":         plan.Generation,
			"scratch_handle":     plan.ScratchHandle,
			"ceiling_seconds":    int(ceiling / time.Second),
			"host_escape_reason": HostEscapeHostWrite,
		}, string(decision.Mode))
		return kernel.ToolDispatchResult{Output: fmt.Sprintf("Started background process with ID: %s\nWarning: the host execution lane is released after startup; this process may still affect files used by other Runs.", id), Process: &kernel.ToolProcessResult{Started: true, SandboxMode: string(plan.Mode), NetworkDecision: executionenv.NetworkDecisionShared}}, nil
	}

	return executeForegroundCommand(args, "terminal", 30)
}

// VerifyTool runs a foreground check whose result is recorded as verification
// evidence. Keeping it separate from terminal lets finalization distinguish a
// deliberate test/build/lint check from an arbitrary shell command.
type VerifyTool struct{ BaseTool }

func NewVerifyTool() *VerifyTool {
	schema := ToolSchema{
		Type:                 "object",
		AdditionalProperties: rejectAdditionalProperties(),
		Properties: map[string]PropertyDef{
			"command":         {Type: "string", Description: "Full verification command to execute"},
			"cwd":             {Type: "string", Description: "Working directory", Default: "."},
			"timeout":         {Type: "integer", Description: "Timeout in seconds", Default: 120},
			"execution_class": toolExecutionClassProperty(),
			"kind": {
				Type:        "string",
				Description: "Verification category",
				Enum:        []string{"test", "build", "lint", "typecheck", "syntax", "smoke", "custom"},
				Default:     "custom",
			},
			"sandbox": {
				Type:        "string",
				Description: "Execution isolation: auto prefers an isolated filesystem sandbox that gets the daemon's network only for a command that reaches a remote service or sets network; isolated requires it; host uses host credentials/network under the current approval mode",
				Enum:        []string{"auto", "isolated", "host"},
				Default:     "auto",
			},
			"network": networkRequestProperty(),
		},
		Required: []string{"command"},
	}
	for name, property := range verificationBindingProperties() {
		schema.Properties[name] = property
	}
	schema.Properties["effect_claim_id"] = NewExecuteCommandTool().Schema().Properties["effect_claim_id"]
	return &VerifyTool{BaseTool: BaseTool{
		name: "verify",
		description: "Run a verification command and record evidence. For a Plan check, supply command and step_id; the stored acceptance condition is inherited. To correct an earlier check, supply command, replaces and reason with the same cwd; no need to repeat its condition. A standalone check uses criterion and target. A nonzero exit records a failed check; use the output to distinguish a negative result from a broken method. Bash uses -e and pipefail: handle expected nonzero exits explicitly with conditionals. Bind effect_claim_id only when this same proven read-only check observes an unresolved effect. " +
			"A check runs offline unless it reaches a remote service; set network: true when it needs its dependencies downloaded. A pass that ran offline although the workspace permits network is reported as such.",
		schema: schema,
	}}
}

// networkRequestProperty is the `network` parameter of the execution tools that
// take one (terminal, verify). It asks; it never grants.
func networkRequestProperty() PropertyDef {
	return PropertyDef{
		Type: "boolean",
		Description: "Set true when this command needs network access that its name does not show, such as a script that calls a remote service or a build that downloads dependencies on a cold cache. " +
			"Permission still comes from the workspace's network grant or an approval; this never grants anything. npm install, git push and cloud CLIs get the network by themselves; builds and tests run offline.",
		Default: false,
	}
}

// codeNetworkRequestProperty is the same parameter on execute_code, whose
// payload is the model's own program: nothing but the model can tell that it
// calls a remote service, so the program is offline unless it asks.
func codeNetworkRequestProperty() PropertyDef {
	return PropertyDef{
		Type: "boolean",
		Description: "Set true when this code needs network access, such as calling a remote service or downloading data. " +
			"Permission still comes from the workspace's network grant or an approval; this never grants anything. Without it the code runs offline.",
		Default: false,
	}
}

func (t *VerifyTool) NormalizeArguments(args map[string]interface{}) (map[string]interface{}, error) {
	return normalizeVerificationArgs(args)
}

func (t *VerifyTool) Execute(args map[string]interface{}) (string, error) {
	result, err := t.ExecuteResult(args)
	return result.Output, err
}

func (t *VerifyTool) ExecuteResult(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	var err error
	args, err = normalizeVerificationArgs(args)
	if err != nil {
		return kernel.ToolDispatchResult{}, err
	}
	if strings.TrimSpace(stringArg(args, "command")) == "" {
		return kernel.ToolDispatchResult{}, fmt.Errorf("command is required")
	}
	if _, err := prepareVerificationBinding(args); err != nil {
		return kernel.ToolDispatchResult{}, err
	}
	return executeForegroundCommand(args, "verify", 120)
}

// executeForegroundCommand is the thin adapter between a tool's argument map and
// the execution engine. It builds an ExecutionRequest, runs it, and converts the
// ExecutionResult into typed output and observed process facts.
//
// Everything the sandbox needs now travels in the request; args remains only the
// dispatcher-side envelope (approval decisions, event ids, and the fields the
// middleware writes back). That separation is what makes the engine movable: a
// remote execution node receives a request and returns a result, and nothing
// about this function's callers changes.
func executeForegroundCommand(args map[string]interface{}, toolName string, standardDefaultSeconds int) (kernel.ToolDispatchResult, error) {
	profile, profileErr := resolveToolProfile(args, standardDefaultSeconds)
	if profileErr != nil {
		return kernel.ToolDispatchResult{}, profileErr
	}
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = "."
	}
	requestedMode, modeErr := requestedSandboxMode(args)
	if modeErr != nil {
		return kernel.ToolDispatchResult{}, enrichToolFailure(toolName, modeErr, "")
	}
	timeoutSeconds := int(profile.Timeout / time.Second)
	args["_execution_class"] = string(profile.Class)
	args["_timeout_seconds"] = timeoutSeconds
	args["_timeout_clamped"] = profile.TimeoutClamped
	if profile.RequestedTimeout > 0 {
		args["_timeout_requested_seconds"] = int(profile.RequestedTimeout / time.Second)
	}

	scope, _ := currentExecutionScopeAny(args)
	// The boundary the approval funnel resolved, not the requested mode: an
	// approval that assumed isolation must never run on the host.
	boundary, boundaryReason := approvedSandboxBoundary(args)
	request := ExecutionRequest{
		ToolName:       toolName,
		ToolCallID:     stringArg(args, "_tool_call_id"),
		RunID:          scope.RunID,
		LeaseID:        scope.LeaseID,
		Payload:        stringArg(args, "command"),
		Shell:          true,
		CWD:            cwd,
		WorkspaceRoots: append([]string{}, scope.AllowedRoots...),
		Sandbox:        requestedMode,
		Boundary:       boundary,
		BoundaryReason: boundaryReason,
		NetworkShared:  networkSharedArg(args),
		Timeout:        profile.Timeout,
		ToolProfile:    profile,
	}
	request.NetworkDecision, request.NetworkPermission = stringArg(args, networkDecisionArg), stringArg(args, networkPermissionArg)
	result, err := Execute(contextFromArgs(args), request, args)
	if result.Plan.Mode != "" {
		args["_sandbox_mode"] = string(result.Plan.Mode)
	}
	return processToolResult(result), err
}

func runCommandStreaming(ctx context.Context, cmd commandRunner, command, toolName, toolCallID string, profiles ...ToolProfile) (string, error) {
	if process, ok := cmd.(*exec.Cmd); ok {
		configureCommandCancellation(process)
		process.WaitDelay = 250 * time.Millisecond
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if process, ok := cmd.(*exec.Cmd); ok {
		recordSpawnedGroup(ctx, process)
	}

	eventCh := kernel.EventChannelFromContext(ctx)
	var mu sync.Mutex
	var output strings.Builder
	var wg sync.WaitGroup

	// The live stream reaches clients before the result middleware sees the
	// whole output, so it is masked here, line by line with state across
	// lines; the captured output stays raw for that middleware.
	// The command is masked once and marked, so that the event redactor does
	// not mask it again on every line of the output.
	var shownCommand kernel.RedactedText
	if eventCh != nil {
		shownCommand = kernel.RedactedText(RedactSensitive(command))
	}
	redactors := map[string]*lineStreamRedactor{"stdout": newLineStreamRedactor(), "stderr": newLineStreamRedactor()}
	emitLines := func(stream string, lines []string) {
		for _, line := range lines {
			emitToolProgress(eventCh, "tool.output", map[string]interface{}{
				"tool_name":    toolName,
				"tool_call_id": toolCallID,
				"stream":       stream,
				"command":      shownCommand,
				"line":         line,
			}, line)
		}
	}
	var captured []capturedLine
	appendLine := func(stream, line string) {
		mu.Lock()
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		start := output.Len()
		output.WriteString(line)
		captured = append(captured, capturedLine{stderr: stream == "stderr", start: start, end: output.Len()})
		mu.Unlock()
		if eventCh != nil {
			emitLines(stream, redactors[stream].push(line))
		}
	}
	endStream := func(stream string) {
		if eventCh != nil {
			emitLines(stream, redactors[stream].flush())
		}
	}

	wg.Add(2)
	go scanCommandOutput(stdout, "stdout", appendLine, endStream, &wg)
	go scanCommandOutput(stderr, "stderr", appendLine, endStream, &wg)

	done := make(chan error, 1)
	go func() {
		// Drain BEFORE reaping. os/exec closes the pipes returned by
		// StdoutPipe/StderrPipe as soon as the process exits, so calling Wait
		// while the scanners are still reading is documented as incorrect: the
		// close races the in-flight read and the output is truncated or lost
		// entirely. A short command that writes once and exits — `printf READY`
		// — has the widest window, and on a contended machine it surfaced as a
		// successful run with empty output and a nil error, which then read as
		// "the command produced nothing" to every caller above.
		//
		// This does not change how long the goroutine blocks: it already waited
		// on both the process and the scanners before signalling.
		wg.Wait()
		done <- cmd.Wait()
	}()

	heartbeat := 5 * time.Second
	if len(profiles) > 0 && profiles[0].HeartbeatInterval > 0 {
		heartbeat = profiles[0].HeartbeatInterval
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case err := <-done:
			mu.Lock()
			result := maskInterleavedStreams(output.String(), captured)
			mu.Unlock()
			return result, err
		case <-ticker.C:
			emitToolProgress(eventCh, "tool.heartbeat", map[string]interface{}{
				"tool_name":       toolName,
				"tool_call_id":    toolCallID,
				"command":         shownCommand,
				"elapsed_seconds": time.Since(start).Seconds(),
			}, "")
		case <-ctx.Done():
			// A descendant may detach from the process group while retaining a
			// pipe. Give buffered output a bounded drain, then close our readers
			// so cancellation cannot wait for an unrelated descendant's EOF.
			drain := time.AfterFunc(250*time.Millisecond, func() {
				_ = stdout.Close()
				_ = stderr.Close()
			})
			err := <-done
			drain.Stop()
			mu.Lock()
			result := maskInterleavedStreams(output.String(), captured)
			mu.Unlock()
			if err != nil {
				return result, err
			}
			return result, ctx.Err()
		}
	}
}

type commandRunner interface {
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

func scanCommandOutput(r io.Reader, stream string, appendLine func(stream, line string), endStream func(stream string), wg *sync.WaitGroup) {
	defer wg.Done()
	defer endStream(stream)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		appendLine(stream, scanner.Text())
	}
}

func emitToolProgress(eventCh chan string, eventType string, payload map[string]interface{}, content string) {
	if eventCh == nil {
		return
	}
	toolName := "terminal"
	toolCallID := ""
	if payload != nil {
		if name, ok := payload["tool_name"].(string); ok && strings.TrimSpace(name) != "" {
			toolName = strings.TrimSpace(name)
		}
		if id, ok := payload["tool_call_id"].(string); ok && strings.TrimSpace(id) != "" {
			toolCallID = strings.TrimSpace(id)
		}
	}
	kernel.EmitAgentEvent(eventCh, kernel.AgentEvent{
		Type:       eventType,
		Content:    content,
		ToolName:   toolName,
		ToolCallID: toolCallID,
		Payload:    payload,
	})
}

func shouldSkipWalkDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "target",
		".next", ".nuxt", ".cache", ".gocache", ".idea", ".vscode", "coverage",
		"tmp", "temp", "__pycache__":
		return true
	default:
		return false
	}
}

func stringArg(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	if value, ok := args[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// GetCurrentTimeTool returns the current time.
type GetCurrentTimeTool struct {
	BaseTool
}

func NewGetCurrentTimeTool() *GetCurrentTimeTool {
	return &GetCurrentTimeTool{
		BaseTool: BaseTool{
			name:        "get_current_time",
			description: "Get current system time",
			schema: ToolSchema{
				Type:       "object",
				Properties: map[string]PropertyDef{},
				Required:   []string{},
			},
		},
	}
}

func (t *GetCurrentTimeTool) Execute(args map[string]interface{}) (string, error) {
	return fmt.Sprintf("%s", Now().Format("2006-01-02 15:04:05 MST")), nil
}

// Now returns the current time (can be mocked in tests)
var Now = func() interface{ Format(string) string } {
	return &timeWrapper{}
}

type timeWrapper struct{}

func (t *timeWrapper) Format(layout string) string {
	return time.Now().Format(layout)
}

// RegisterBuiltins registers every built-in tool on the dispatcher.
func RegisterBuiltins(d *Dispatcher) {
	d.RegisterTool(NewListFilesTool())
	d.RegisterTool(NewReadFileTool())
	d.RegisterTool(NewWriteFileTool())
	d.RegisterTool(NewPatchTool())
	d.RegisterTool(NewExecuteCommandTool())
	d.RegisterTool(NewVerifyTool())
	d.RegisterTool(NewSearchFilesTool())
	d.RegisterTool(NewGetCurrentTimeTool())
	d.RegisterTool(NewProcessTool())
}
