package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"selfmind/internal/platform/textutil"
	"strings"
	"time"
)

// ReadFileTool reads file contents.
type ReadFileTool struct {
	BaseTool
}

func NewReadFileTool() *ReadFileTool {
	return &ReadFileTool{
		BaseTool: BaseTool{
			name:        "read_file",
			description: "Read a text file. Use offset (1-based) and limit for a specific window; line_numbers adds source line numbers. A truncated window gives the next offset. Omitted offset and line_numbers preserve plain text output.",
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
					"offset":       {Type: "integer", Description: "First line to read (1-based)", Default: 1},
					"line_numbers": {Type: "boolean", Description: "Prefix source line numbers", Default: false},
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
	offset := intArg(args, "offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	lineNumbers, _ := args["line_numbers"].(bool)
	maxBytes := intArg(args, "max_bytes", 1024*1024)
	if maxBytes < 64*1024 {
		maxBytes = 64 * 1024
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("read_file requires a file, but %q is a directory; use list_dir or ls_r", path)
	}

	if err := contextFromArgs(args).Err(); err != nil {
		return "", err
	}
	_, explicitOffset := args["offset"]
	if limit > 0 || explicitOffset || lineNumbers {
		return readFileWindow(contextFromArgs(args), path, offset, limit, maxBytes, lineNumbers, explicitOffset || lineNumbers)
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

func readFileWindow(ctx context.Context, path string, offset, limit, maxBytes int, numbered, observation bool) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", enrichToolFailure("read_file", err, "")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBytes)
	var content strings.Builder
	line, included, next := 0, 0, 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return content.String(), err
		}
		line++
		if line < offset {
			continue
		}
		text := scanner.Text()
		if numbered {
			text = fmt.Sprintf("%d: %s", line, text)
		}
		if limit > 0 && included == limit || content.Len()+len(text)+1 > maxBytes {
			if included == 0 {
				return "", fmt.Errorf("line %d exceeds max_bytes=%d including line numbers; increase max_bytes or disable line_numbers", line, maxBytes)
			}
			next = line
			break
		}
		if included > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(text)
		included++
	}
	if err := scanner.Err(); err != nil {
		return content.String(), fmt.Errorf("read_file could not read line %d within max_bytes=%d: %w", line+1, maxBytes, err)
	}
	if line < offset && !(line == 0 && offset == 1) {
		return "", fmt.Errorf("offset %d is beyond end of file (%d lines)", offset, line)
	}
	after, err := file.Stat()
	if err != nil {
		return content.String(), err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return content.String(), fmt.Errorf("file changed during read; returned content is a partial observation")
	}
	if included == 0 && next == 0 {
		return emptyFileResult(path), nil
	}
	if next > 0 {
		fmt.Fprintf(&content, "\n... (file truncated after %d lines / %d bytes; use offset=%d to continue)", included, content.Len(), next)
	}
	if observation {
		fmt.Fprintf(&content, "\n[Observed size=%d modified_at=%s; later windows may reflect file changes.]", before.Size(), before.ModTime().UTC().Format(time.RFC3339Nano))
	}
	return content.String(), nil
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
