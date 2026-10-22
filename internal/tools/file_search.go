package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

type SearchFilesTool struct{ BaseTool }

func NewSearchFilesTool() *SearchFilesTool {
	return &SearchFilesTool{BaseTool: BaseTool{
		name:        "search_files",
		description: "Search text files. For code lookups use output_mode=content to get path, line and matching text. match_mode=regex uses Go/RE2 syntax; the default is literal. Default output_mode=files preserves the file list. Skips generated/VCS directories, binary/oversized files and symlinks; partial results name coverage gaps.",
		schema: ToolSchema{Type: "object", Properties: map[string]PropertyDef{
			"pattern":        {Type: "string", Description: "Text to find; interpreted by match_mode"},
			"path":           {Type: "string", Description: "Directory or text file to search", Default: "."},
			"file_glob":      {Type: "string", Description: "Basename filter, e.g. *.go (filepath.Match syntax)", Default: "*"},
			"match_mode":     {Type: "string", Description: "literal (default) or regex (Go/RE2)", Enum: []string{"literal", "regex"}, Default: "literal"},
			"output_mode":    {Type: "string", Description: "files (default) or content with matching line numbers and text", Enum: []string{"files", "content"}, Default: "files"},
			"limit":          {Type: "integer", Description: "Maximum files in files mode or matching lines in content mode (content capped at 500)", Default: 50},
			"timeout":        {Type: "integer", Description: "Search timeout in seconds", Default: 15},
			"max_file_bytes": {Type: "integer", Description: "Skip files exceeding this size; reported as incomplete coverage", Default: 1048576},
		}, Required: []string{"pattern"}},
	}}
}

type searchHit struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type fileSearchResult struct {
	Path            string      `json:"path"`
	Pattern         string      `json:"pattern"`
	FileGlob        string      `json:"file_glob"`
	MatchMode       string      `json:"match_mode"`
	OutputMode      string      `json:"output_mode"`
	Matches         []string    `json:"matches"`
	Hits            []searchHit `json:"hits,omitempty"`
	Count           int         `json:"count"`
	HitCount        int         `json:"hit_count,omitempty"`
	Limit           int         `json:"limit"`
	Scanned         int         `json:"scanned_files"`
	SkippedDirs     int         `json:"skipped_dirs"`
	SkippedLarge    int         `json:"skipped_large"`
	SkippedSymlinks int         `json:"skipped_symlinks,omitempty"`
	SkippedBinary   int         `json:"skipped_binary,omitempty"`
	ReadErrors      int         `json:"read_errors,omitempty"`
	Errors          []string    `json:"errors,omitempty"`
	Partial         bool        `json:"partial,omitempty"`
	Truncated       bool        `json:"truncated"`
	ErrorClass      string      `json:"error_class,omitempty"`
	Message         string      `json:"message,omitempty"`
}

func (t *SearchFilesTool) Execute(args map[string]interface{}) (string, error) {
	pattern := stringArg(args, "pattern")
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	path := stringArg(args, "path")
	if path == "" {
		path = "."
	}
	glob := stringArg(args, "file_glob")
	if glob == "" {
		glob = "*"
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return "", fmt.Errorf("invalid file_glob: %w", err)
	}
	mode := stringArg(args, "match_mode")
	if mode == "" {
		mode = "literal"
	}
	output := stringArg(args, "output_mode")
	if output == "" {
		output = "files"
	}
	if output != "files" && output != "content" {
		return "", fmt.Errorf("output_mode must be files or content")
	}
	var find func(string) []int
	switch mode {
	case "literal":
		find = func(line string) []int {
			i := strings.Index(line, pattern)
			if i < 0 {
				return nil
			}
			return []int{i, i + len(pattern)}
		}
	case "regex":
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", fmt.Errorf("invalid regex: %w", err)
		}
		find = re.FindStringIndex
	default:
		return "", fmt.Errorf("match_mode must be literal or regex")
	}
	limit := intArg(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if output == "content" {
		limit = min(limit, 500)
	}
	maxBytes := intArg(args, "max_file_bytes", 1048576)
	if maxBytes <= 0 {
		maxBytes = 1048576
	}
	seconds := intArg(args, "timeout", 15)
	if seconds <= 0 {
		seconds = 15
	}
	ctx, cancel := context.WithTimeout(contextFromArgs(args), time.Duration(seconds)*time.Second)
	defer cancel()
	result := fileSearchResult{Path: path, Pattern: pattern, FileGlob: glob, MatchMode: mode, OutputMode: output, Matches: []string{}, Limit: limit}
	info, err := os.Lstat(path)
	if err != nil {
		return "", enrichToolFailure("search_files", err, "")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("search root must be a directory or regular file, not a symlink or special file")
	}
	directory, start := path, "."
	if !info.IsDir() {
		directory, start = filepath.Dir(path), filepath.Base(path)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", enrichToolFailure("search_files", err, "")
	}
	defer root.Close()
	lastBeat := time.Now()
	err = fs.WalkDir(root.FS(), start, func(relative string, entry fs.DirEntry, walkErr error) error {
		p := filepath.Join(directory, relative)
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if p == path {
				return walkErr
			}
			result.recordError(p, walkErr)
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			result.SkippedSymlinks++
			return nil
		}
		if entry.IsDir() {
			if p != path && shouldSkipWalkDir(entry.Name()) {
				result.SkippedDirs++
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		result.Scanned++
		if output == "files" && len(result.Matches) >= limit || output == "content" && len(result.Hits) >= limit {
			result.Truncated = true
			return filepath.SkipAll
		}
		ok, _ := filepath.Match(glob, entry.Name())
		if !ok {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if p == path {
				return err
			}
			result.recordError(p, err)
			return nil
		}
		if info.Size() > int64(maxBytes) {
			result.SkippedLarge++
			result.Partial = true
			return nil
		}
		// Anchor opens to the searched root even if an entry is swapped for a
		// symlink between enumeration and open.
		file, err := root.Open(relative)
		if err != nil {
			if p == path {
				return err
			}
			result.recordError(p, err)
			return nil
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
		_ = file.Close()
		if readErr != nil {
			if p == path {
				return readErr
			}
			result.recordError(p, readErr)
			return nil
		}
		if len(data) > maxBytes {
			result.SkippedLarge++
			result.Partial = true
			return nil
		}
		if bytes.IndexByte(data, 0) >= 0 {
			result.SkippedBinary++
			return nil
		}
		if output == "files" && mode == "literal" {
			// Preserve the published literal whole-file match, including newlines.
			if find(string(data)) != nil {
				result.Matches = append(result.Matches, p)
			}
		} else {
			fileAdded := false
			lines := strings.Split(string(data), "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			for i, line := range lines {
				if err := ctx.Err(); err != nil {
					return err
				}
				line = strings.TrimSuffix(line, "\r")
				where := find(line)
				if where == nil {
					continue
				}
				if output == "content" && len(result.Hits) >= limit {
					result.Truncated = true
					return filepath.SkipAll
				}
				if !fileAdded {
					result.Matches = append(result.Matches, p)
					fileAdded = true
				}
				if output == "files" {
					break
				}
				start := max(0, where[0]-200)
				for start > 0 && !utf8.RuneStart(line[start]) {
					start--
				}
				text := textutil.TruncateBytes(line[start:], 2000)
				result.Hits = append(result.Hits, searchHit{Path: p, Line: i + 1, Text: text, Truncated: start > 0 || len(text) < len(line)})
			}
		}
		if time.Since(lastBeat) >= time.Second {
			lastBeat = time.Now()
			emitToolProgress(kernel.EventChannelFromContext(ctx), "tool.heartbeat", map[string]interface{}{
				"tool_name": "search_files", "tool_call_id": stringArg(args, "_tool_call_id"), "path": path,
				"scanned_files": result.Scanned, "matches": len(result.Matches), "status": fmt.Sprintf("scanned %d files, %d matches", result.Scanned, len(result.Matches)),
			}, "")
		}
		return nil
	})
	if err == context.Canceled {
		return "", fmt.Errorf("search_files cancelled")
	}
	if err == context.DeadlineExceeded {
		result.Partial = true
		result.ErrorClass = "timeout"
		result.Message = fmt.Sprintf("search timed out after %d seconds; partial results are preserved", seconds)
	} else if err != nil {
		return "", enrichToolFailure("search_files", err, "")
	}
	if result.Partial && result.ErrorClass == "" {
		result.ErrorClass = "partial_search"
		result.Message = "Search coverage is incomplete; inspect read_errors and skipped_large before concluding no matches."
	}
	result.Count = len(result.Matches)
	result.HitCount = len(result.Hits)
	data, _ := json.Marshal(result)
	return string(data), nil
}

func (r *fileSearchResult) recordError(path string, err error) {
	r.ReadErrors++
	r.Partial = true
	if len(r.Errors) < 4 {
		r.Errors = append(r.Errors, textutil.TruncateBytes(path+": "+err.Error(), 300))
	}
}
