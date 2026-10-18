package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"selfmind/internal/kernel"
	"time"
)

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
					"file_glob": {Type: "string", Description: "Filter returned entry basenames (filepath.Match syntax), e.g. *.go; directories are still traversed", Default: "*"},
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
	glob := stringArg(args, "file_glob")
	if glob == "" {
		glob = "*"
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return "", fmt.Errorf("invalid file_glob: %w", err)
	}
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
			if matched, _ := filepath.Match(glob, entry.Name()); !matched {
				return nil
			}
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
		for !truncated {
			files, err := dir.ReadDir(128)
			if err != nil && err != io.EOF {
				return "", err
			}
			for _, f := range files {
				if ctx.Err() != nil {
					return "", fmt.Errorf("ls_r cancelled")
				}
				scanned++
				if matched, _ := filepath.Match(glob, f.Name()); !matched {
					continue
				}
				if len(entries) < maxEntries {
					name := f.Name()
					if f.IsDir() {
						name += string(os.PathSeparator)
					}
					entries = append(entries, name)
				} else {
					truncated = true
					break
				}
			}
			if err == io.EOF {
				break
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
