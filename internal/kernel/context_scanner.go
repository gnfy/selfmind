package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContextFile represents a discovered project context file.
type ContextFile struct {
	Path     string
	Name     string
	Content  string
	Priority int // lower = higher priority
}

// ContextScanner scans the filesystem for project-specific context files
// (e.g. .selfmind.md, AGENTS.md) and injects them into the system prompt.
type ContextScanner struct {
	maxFileSize  int      // max bytes per file
	maxTotalSize int      // max bytes total injection
	filenames    []string // in priority order
}

// NewContextScanner creates a scanner with sensible defaults.
func NewContextScanner() *ContextScanner {
	return &ContextScanner{
		maxFileSize:  8 * 1024,  // 8 KB per file
		maxTotalSize: 16 * 1024, // 16 KB total
		filenames: []string{
			".selfmind.md",
			"AGENTS.md",
			".cursorrules",
			".claude.md",
			"CLAUDE.md",
			"README.md",
		},
	}
}

// Scan walks upward from the current working directory looking for context
// files. It stops at the first Git repository root or the user's home dir.
func (cs *ContextScanner) Scan() ([]ContextFile, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	home, _ := os.UserHomeDir()
	var results []ContextFile
	visited := make(map[string]bool)

	for dir := cwd; dir != "/" && dir != home; dir = filepath.Dir(dir) {
		if visited[dir] {
			break
		}
		visited[dir] = true

		for priority, name := range cs.filenames {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			if info.Size() > int64(cs.maxFileSize) {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			content := strings.TrimSpace(string(data))
			if content == "" {
				continue
			}
			results = append(results, ContextFile{
				Path:     path,
				Name:     name,
				Content:  content,
				Priority: priority,
			})
		}

		// Stop at git root — we don't want to leak context from parent dirs
		// that belong to a different project.
		if cs.isGitRoot(dir) {
			break
		}
	}

	return results, nil
}

// BuildContextPrompt formats discovered files into a system-prompt block.
// It respects maxTotalSize by truncating if necessary.
func (cs *ContextScanner) BuildContextPrompt(files []ContextFile) string {
	if len(files) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# PROJECT CONTEXT\n")
	sb.WriteString("The following files were found in the current project directory. " +
		"They contain conventions, preferences, and background that you MUST respect.\n\n")

	total := 0
	for _, f := range files {
		header := fmt.Sprintf("## %s (from %s)\n", f.Name, f.Path)
		block := header + f.Content + "\n\n"
		if total+len(block) > cs.maxTotalSize {
			remaining := cs.maxTotalSize - total
			if remaining > len(header)+50 {
				truncated := f.Content[:remaining-len(header)-50]
				block = header + truncated + "\n...[truncated]\n\n"
				sb.WriteString(block)
			}
			break
		}
		sb.WriteString(block)
		total += len(block)
	}

	return sb.String()
}

func (cs *ContextScanner) isGitRoot(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	return err == nil && info.IsDir()
}
