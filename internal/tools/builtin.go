package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

// ---- Built-in tool implementations ----

// ListFilesTool lists directory entries.
type ListFilesTool struct {
	BaseTool
}

func NewListFilesTool() *ListFilesTool {
	return &ListFilesTool{
		BaseTool: BaseTool{
			name:        "ls_r",
			description: "Recursively list files and directories in a path",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"path": {
						Type:        "string",
						Description: "Directory path, default to .",
						Default:     ".",
					},
					"recursive": {
						Type:        "boolean",
						Description: "Whether to list recursively",
						Default:     false,
					},
					"max_entries": {
						Type:        "integer",
						Description: "Maximum entries to return before truncating",
						Default:     300,
					},
					"timeout": {
						Type:        "integer",
						Description: "Timeout in seconds for recursive listing",
						Default:     10,
					},
				},
				Required: []string{},
			},
		},
	}
}

func (t *ListFilesTool) Execute(args map[string]interface{}) (string, error) {
	path := "."
	if p, ok := args["path"].(string); ok {
		path = p
	}
	recursive, _ := args["recursive"].(bool)
	maxEntries := intArg(args, "max_entries", 300)
	if maxEntries <= 0 {
		maxEntries = 300
	}
	timeoutSeconds := intArg(args, "timeout", 10)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 10
	}

	parentCtx := contextFromArgs(args)
	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	eventCh := kernel.EventChannelFromContext(ctx)

	var entries []string
	scanned := 0
	skippedDirs := 0
	truncated := false
	start := time.Now()
	lastHeartbeat := start

	if recursive {
		err := filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry.IsDir() && p != path && shouldSkipWalkDir(entry.Name()) {
				skippedDirs++
				return filepath.SkipDir
			}
			scanned++
			if len(entries) < maxEntries {
				entries = append(entries, p)
			} else {
				truncated = true
			}
			if time.Since(lastHeartbeat) >= time.Second {
				lastHeartbeat = time.Now()
				emitToolProgress(eventCh, "tool.heartbeat", map[string]interface{}{
					"tool_name":       "ls_r",
					"tool_call_id":    stringArg(args, "_tool_call_id"),
					"path":            path,
					"elapsed_seconds": time.Since(start).Seconds(),
					"scanned_entries": scanned,
					"entries":         len(entries),
					"skipped_dirs":    skippedDirs,
					"status":          fmt.Sprintf("scanned %d entries", scanned),
				}, "")
			}
			if truncated && scanned >= maxEntries {
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			if err == context.DeadlineExceeded {
				return "", fmt.Errorf("ls_r timed out after %d seconds", timeoutSeconds)
			}
			if err == context.Canceled {
				return "", fmt.Errorf("ls_r cancelled")
			}
			return "", err
		}
	} else {
		dir, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer dir.Close()
		files, err := dir.ReadDir(maxEntries + 1)
		if err != nil && err != io.EOF {
			return "", err
		}
		for _, f := range files {
			if ctx.Err() != nil {
				return "", fmt.Errorf("ls_r cancelled")
			}
			scanned++
			if len(entries) < maxEntries {
				name := f.Name()
				if f.IsDir() {
					name += string(os.PathSeparator)
				}
				entries = append(entries, name)
			} else {
				truncated = true
			}
		}
	}
	b, _ := json.Marshal(map[string]interface{}{
		"path":         path,
		"recursive":    recursive,
		"entries":      entries,
		"count":        len(entries),
		"scanned":      scanned,
		"truncated":    truncated,
		"max_entries":  maxEntries,
		"skipped_dirs": skippedDirs,
	})
	return string(b), nil
}

// ReadFileTool reads file contents.
type ReadFileTool struct {
	BaseTool
}

func NewReadFileTool() *ReadFileTool {
	return &ReadFileTool{
		BaseTool: BaseTool{
			name:        "read_file",
			description: "Read the content of a text file",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"path": {
						Type:        "string",
						Description: "File path (absolute or relative)",
					},
					"limit": {
						Type:        "integer",
						Description: "Max lines to read, 0 for all",
						Default:     0,
					},
					"max_bytes": {
						Type:        "integer",
						Description: "Maximum bytes to read before truncating",
						Default:     1048576,
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

func (t *ReadFileTool) Execute(args map[string]interface{}) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}
	limit := intArg(args, "limit", 0)
	maxBytes := intArg(args, "max_bytes", 1024*1024)
	if maxBytes < 64*1024 {
		maxBytes = 64 * 1024
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("read_file requires a file, but %q is a directory; use list_dir or ls_r", path)
	}

	if limit > 0 {
		return readFileLineLimited(path, limit, maxBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", enrichToolFailure("read_file", err, "")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return "", err
	}
	content := string(data)
	if len(data) > maxBytes {
		content = textutil.TruncateBytes(content, maxBytes) + fmt.Sprintf("\n... (file truncated after %d bytes)", maxBytes)
	}
	if len(data) == 0 {
		return emptyFileResult(path), nil
	}
	return content, nil
}

func readFileLineLimited(path string, limit, maxBytes int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", enrichToolFailure("read_file", err, "")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBytes)
	var lines []string
	for scanner.Scan() {
		if len(lines) >= limit {
			return strings.Join(lines, "\n") + fmt.Sprintf("\n... (file truncated after %d lines)", limit), nil
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return strings.Join(lines, "\n"), err
	}
	if len(lines) == 0 {
		return emptyFileResult(path), nil
	}
	return strings.Join(lines, "\n"), nil
}

func emptyFileResult(path string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"path":  path,
		"empty": true,
		"bytes": 0,
		"lines": 0,
	})
	return string(b)
}

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
					"sandbox": {
						Type:        "string",
						Description: "Execution isolation: auto prefers an isolated filesystem sandbox whose network follows exec_sandbox.allow_network; isolated requires it; host uses host credentials/network and requires approval",
						Enum:        []string{"auto", "isolated", "host"},
						Default:     "auto",
					},
				},
				Required: []string{"command"},
			},
		},
	}
}

func (t *ExecuteCommandTool) Execute(args map[string]interface{}) (string, error) {
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return "", fmt.Errorf("command is required")
	}
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = "."
	}
	background, _ := args["background"].(bool)
	if background {
		mode, err := requestedSandboxMode(args)
		if err != nil {
			return "", err
		}
		if mode != SandboxHost {
			return "", fmt.Errorf("background commands require sandbox=host and approval; use watch_external for durable unattended waits")
		}
		// A background command belongs to its run: it gets the run's environment
		// binding, scratch space, and tool state overlays like any other command.
		// Skipping that made the same command behave differently depending only
		// on whether it was backgrounded.
		material := execMaterialForArgs(args, absoluteCWD(cwd))
		if material.ProfileError != nil {
			return "", enrichToolFailure("terminal", fmt.Errorf("prepare execution environment: %w", material.ProfileError), "")
		}
		runCtx := contextFromArgs(args)
		emitProfilePreparation(runCtx, "terminal", stringArg(args, "_tool_call_id"), material)
		registry := ProcessRegistryForArgs(args)
		// The long-running class ceiling bounds a leaked background process. It
		// is not a work limit: watch_external is the durable, observable way to
		// wait a long time for something external.
		ceiling := backgroundProcessCeiling(args)
		id, err := registry.StartProcess(cmdStr, cwd, material.Env, ceiling)
		if err != nil {
			return "", err
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
			"background":   true,
			"profiles":     material.Profiles,
			// The plan is recorded even though a detached process cannot report a
			// streamed result: an execution with no auditable boundary is exactly
			// what made background commands invisible.
			"snapshot_id":        plan.SnapshotID,
			"generation":         plan.Generation,
			"scratch_handle":     plan.ScratchHandle,
			"ceiling_seconds":    int(ceiling / time.Second),
			"host_escape_reason": HostEscapeHostWrite,
		}, string(decision.Mode))
		return fmt.Sprintf("Started background process with ID: %s", id), nil
	}

	return executeForegroundCommand(args, "terminal", 30)
}

// VerifyTool runs a foreground check whose result is recorded as verification
// evidence. Keeping it separate from terminal lets finalization distinguish a
// deliberate test/build/lint check from an arbitrary shell command.
type VerifyTool struct{ BaseTool }

func NewVerifyTool() *VerifyTool {
	return &VerifyTool{BaseTool: BaseTool{
		name:        "verify",
		description: "Run a test, build, lint, typecheck, syntax, smoke, or custom verification command and record its exit status as durable run evidence",
		schema: ToolSchema{
			Type: "object",
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
					Description: "Execution isolation: auto prefers an isolated filesystem sandbox whose network follows exec_sandbox.allow_network; isolated requires it; host uses host credentials/network and requires approval",
					Enum:        []string{"auto", "isolated", "host"},
					Default:     "auto",
				},
			},
			Required: []string{"command"},
		},
	}}
}

func (t *VerifyTool) Execute(args map[string]interface{}) (string, error) {
	if strings.TrimSpace(stringArg(args, "command")) == "" {
		return "", fmt.Errorf("command is required")
	}
	return executeForegroundCommand(args, "verify", 120)
}

// executeForegroundCommand is the thin adapter between a tool's argument map and
// the execution engine. It builds an ExecutionRequest, runs it, and converts the
// ExecutionResult back into the (output, error) pair tools return.
//
// Everything the sandbox needs now travels in the request; args remains only the
// dispatcher-side envelope (approval decisions, event ids, and the fields the
// middleware writes back). That separation is what makes the engine movable: a
// remote execution node receives a request and returns a result, and nothing
// about this function's callers changes.
func executeForegroundCommand(args map[string]interface{}, toolName string, standardDefaultSeconds int) (string, error) {
	profile, profileErr := resolveToolProfile(args, standardDefaultSeconds)
	if profileErr != nil {
		return "", profileErr
	}
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = "."
	}
	requestedMode, modeErr := requestedSandboxMode(args)
	if modeErr != nil {
		return "", enrichToolFailure(toolName, modeErr, "")
	}
	timeoutSeconds := int(profile.Timeout / time.Second)
	args["_execution_class"] = string(profile.Class)
	args["_timeout_seconds"] = timeoutSeconds
	args["_timeout_clamped"] = profile.TimeoutClamped
	if profile.RequestedTimeout > 0 {
		args["_timeout_requested_seconds"] = int(profile.RequestedTimeout / time.Second)
	}

	scope, _ := currentExecutionScopeAny(args)
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
		NetworkShared:  networkSharedArg(args),
		Timeout:        profile.Timeout,
		ToolProfile:    profile,
	}
	result, err := Execute(contextFromArgs(args), request, args)
	args["_command_exit_code"] = result.ExitCode
	args["_recovery_outcome"] = result.RecoveryOutcome
	if result.Plan.Mode != "" {
		args["_sandbox_mode"] = string(result.Plan.Mode)
	}
	return result.Output, err
}

func runCommandStreaming(ctx context.Context, cmd commandRunner, command, toolName, toolCallID string, profiles ...ToolProfile) (string, error) {
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

	eventCh := kernel.EventChannelFromContext(ctx)
	var mu sync.Mutex
	var output strings.Builder
	var wg sync.WaitGroup

	appendLine := func(stream, line string) {
		mu.Lock()
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(line)
		mu.Unlock()
		emitToolProgress(eventCh, "tool.output", map[string]interface{}{
			"tool_name":    toolName,
			"tool_call_id": toolCallID,
			"stream":       stream,
			"command":      command,
			"line":         line,
		}, line)
	}

	wg.Add(2)
	go scanCommandOutput(stdout, "stdout", appendLine, &wg)
	go scanCommandOutput(stderr, "stderr", appendLine, &wg)

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
			result := output.String()
			mu.Unlock()
			return result, err
		case <-ticker.C:
			emitToolProgress(eventCh, "tool.heartbeat", map[string]interface{}{
				"tool_name":       toolName,
				"tool_call_id":    toolCallID,
				"command":         command,
				"elapsed_seconds": time.Since(start).Seconds(),
			}, "")
		case <-ctx.Done():
			err := <-done
			mu.Lock()
			result := output.String()
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

func scanCommandOutput(r io.Reader, stream string, appendLine func(stream, line string), wg *sync.WaitGroup) {
	defer wg.Done()
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

// SearchFilesTool searches file contents.
type SearchFilesTool struct {
	BaseTool
}

func NewSearchFilesTool() *SearchFilesTool {
	return &SearchFilesTool{
		BaseTool: BaseTool{
			name:        "search_files",
			description: "Search for a pattern in file contents",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"pattern": {
						Type:        "string",
						Description: "Regex pattern or keyword",
					},
					"path": {
						Type:        "string",
						Description: "Search directory",
						Default:     ".",
					},
					"file_glob": {
						Type:        "string",
						Description: "File glob filter, e.g. *.go",
						Default:     "*",
					},
					"limit": {
						Type:        "integer",
						Description: "Max results",
						Default:     50,
					},
					"timeout": {
						Type:        "integer",
						Description: "Timeout in seconds",
						Default:     15,
					},
					"max_file_bytes": {
						Type:        "integer",
						Description: "Skip files larger than this many bytes",
						Default:     1048576,
					},
				},
				Required: []string{"pattern"},
			},
		},
	}
}

func (t *SearchFilesTool) Execute(args map[string]interface{}) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	glob, _ := args["file_glob"].(string)
	if glob == "" {
		glob = "*"
	}
	limit := intArg(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	timeoutSeconds := intArg(args, "timeout", 15)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	maxFileBytes := intArg(args, "max_file_bytes", 1024*1024)
	if maxFileBytes <= 0 {
		maxFileBytes = 1024 * 1024
	}

	matches := make([]string, 0)
	parentCtx := contextFromArgs(args)
	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	eventCh := kernel.EventChannelFromContext(ctx)
	start := time.Now()
	lastHeartbeat := start
	scanned := 0
	skippedDirs := 0
	skippedLarge := 0
	truncated := false

	err := filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			if p != path && shouldSkipWalkDir(entry.Name()) {
				skippedDirs++
				return filepath.SkipDir
			}
			return nil
		}
		scanned++
		if len(matches) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		matched, _ := filepath.Match(glob, filepath.Base(p))
		if !matched {
			return nil
		}
		info, statErr := entry.Info()
		if statErr == nil && info.Size() > int64(maxFileBytes) {
			skippedLarge++
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), pattern) {
			matches = append(matches, p)
		}
		if time.Since(lastHeartbeat) >= time.Second {
			lastHeartbeat = time.Now()
			emitToolProgress(eventCh, "tool.heartbeat", map[string]interface{}{
				"tool_name":       "search_files",
				"tool_call_id":    stringArg(args, "_tool_call_id"),
				"path":            path,
				"pattern":         pattern,
				"elapsed_seconds": time.Since(start).Seconds(),
				"scanned_files":   scanned,
				"matches":         len(matches),
				"skipped_dirs":    skippedDirs,
				"skipped_large":   skippedLarge,
				"status":          fmt.Sprintf("scanned %d files, %d matches", scanned, len(matches)),
			}, "")
		}
		return nil
	})
	if err != nil {
		if err == context.DeadlineExceeded {
			b, _ := json.Marshal(map[string]interface{}{
				"path":          path,
				"pattern":       pattern,
				"file_glob":     glob,
				"matches":       matches,
				"count":         len(matches),
				"scanned_files": scanned,
				"partial":       true,
				"error_class":   "timeout",
				"message":       fmt.Sprintf("search timed out after %d seconds; partial results are preserved", timeoutSeconds),
			})
			return string(b), nil
		}
		if err == context.Canceled {
			return "", fmt.Errorf("search_files cancelled")
		}
		return "", enrichToolFailure("search_files", err, "")
	}
	b, _ := json.Marshal(map[string]interface{}{
		"path":          path,
		"pattern":       pattern,
		"file_glob":     glob,
		"matches":       matches,
		"count":         len(matches),
		"scanned_files": scanned,
		"truncated":     truncated,
		"limit":         limit,
		"skipped_dirs":  skippedDirs,
		"skipped_large": skippedLarge,
	})
	return string(b), nil
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
