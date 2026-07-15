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

// ---- 内置工具实现 ----

// ListFilesTool 列出目录文件
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

// ReadFileTool 读取文件内容
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

	if limit > 0 {
		return readFileLineLimited(path, limit, maxBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
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
	return content, nil
}

func readFileLineLimited(path string, limit, maxBytes int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
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
	return strings.Join(lines, "\n"), nil
}

// WriteFileTool 写入文件内容
type WriteFileTool struct {
	BaseTool
}

func NewWriteFileTool() *WriteFileTool {
	return &WriteFileTool{
		BaseTool: BaseTool{
			name:        "write_file",
			description: "Write content to a file (overwrites)",
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

// ExecuteCommandTool 执行 Shell 命令
type ExecuteCommandTool struct {
	BaseTool
}

func NewExecuteCommandTool() *ExecuteCommandTool {
	return &ExecuteCommandTool{
		BaseTool: BaseTool{
			name:        "terminal",
			description: "Execute a system command and return output",
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
					"background": {
						Type:        "boolean",
						Description: "Whether to run in background",
						Default:     false,
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
	timeoutSeconds, _ := args["timeout"].(int)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	if background {
		registry := ProcessRegistryForArgs(args)
		id, err := registry.StartProcess(cmdStr, cwd)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Started background process with ID: %s", id), nil
	}

	return executeForegroundCommand(args, "terminal", timeoutSeconds)
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
				"command": {Type: "string", Description: "Full verification command to execute"},
				"cwd":     {Type: "string", Description: "Working directory", Default: "."},
				"timeout": {Type: "integer", Description: "Timeout in seconds", Default: 120},
				"kind": {
					Type:        "string",
					Description: "Verification category",
					Enum:        []string{"test", "build", "lint", "typecheck", "syntax", "smoke", "custom"},
					Default:     "custom",
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
	timeoutSeconds, _ := args["timeout"].(int)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	return executeForegroundCommand(args, "verify", timeoutSeconds)
}

func executeForegroundCommand(args map[string]interface{}, toolName string, timeoutSeconds int) (string, error) {
	cmdStr, _ := args["command"].(string)
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = "."
	}
	parentCtx := contextFromArgs(args)
	runCtx, cancel := context.WithTimeout(parentCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := shellCommandContext(runCtx, cmdStr)
	cmd.Dir = cwd

	out, err := runCommandStreaming(runCtx, cmd, cmdStr, toolName, stringArg(args, "_tool_call_id"))
	exitCode := 0
	if exitErr, ok := err.(interface{ ExitCode() int }); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		exitCode = -1
	}
	args["_command_exit_code"] = exitCode
	if runCtx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("command timed out after %d seconds", timeoutSeconds)
	}
	if runCtx.Err() == context.Canceled {
		return out, fmt.Errorf("command cancelled")
	}
	if err != nil {
		return out, fmt.Errorf("command failed: %v", err)
	}
	return out, nil
}

func runCommandStreaming(ctx context.Context, cmd commandRunner, command, toolName, toolCallID string) (string, error) {
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
		err := cmd.Wait()
		wg.Wait()
		done <- err
	}()

	ticker := time.NewTicker(time.Second)
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

// SearchFilesTool 搜索文件内容
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

	var matches []string
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
			return "", fmt.Errorf("search_files timed out after %d seconds", timeoutSeconds)
		}
		if err == context.Canceled {
			return "", fmt.Errorf("search_files cancelled")
		}
		return "", err
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

// GetCurrentTimeTool 获取当前时间
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

// RegisterBuiltins 将所有内置工具注册到 dispatcher
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
