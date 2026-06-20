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

	var entries []string
	if recursive {
		err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			entries = append(entries, p)
			return nil
		})
		if err != nil {
			return "", err
		}
	} else {
		files, err := os.ReadDir(path)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			entries = append(entries, f.Name())
		}
	}
	b, _ := json.Marshal(entries)
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
	limit, _ := args["limit"].(int)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(data)
	if limit > 0 {
		lines := strings.Split(content, "\n")
		if len(lines) > limit {
			content = strings.Join(lines[:limit], "\n") + fmt.Sprintf("\n... (%d more lines)", len(lines)-limit)
		}
	}
	return content, nil
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
	content, _ := args["content"].(string)
	if path == "" || content == "" {
		return "", fmt.Errorf("path and content are required")
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Written %d bytes to %s", len(content), path), nil
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

	parentCtx := contextFromArgs(args)
	runCtx, cancel := context.WithTimeout(parentCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := shellCommandContext(runCtx, cmdStr)
	cmd.Dir = cwd

	out, err := runCommandStreaming(runCtx, cmd, cmdStr)
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

func runCommandStreaming(ctx context.Context, cmd commandRunner, command string) (string, error) {
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
			"tool_name": "terminal",
			"stream":    stream,
			"command":   command,
			"line":      line,
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
				"tool_name":       "terminal",
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
	kernel.EmitAgentEvent(eventCh, kernel.AgentEvent{
		Type:     eventType,
		Content:  content,
		ToolName: "terminal",
		Payload:  payload,
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

	var matches []string
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		matched, _ := filepath.Match(glob, filepath.Base(p))
		if !matched {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), pattern) {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(matches)
	return string(b), nil
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
	d.RegisterTool(NewSearchFilesTool())
	d.RegisterTool(NewGetCurrentTimeTool())
	d.RegisterTool(NewProcessTool())
}
