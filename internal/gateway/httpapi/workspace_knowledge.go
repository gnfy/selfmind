package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

const (
	workspaceKnowledgeMaxSections = 32
	workspaceKnowledgeExcerpt     = 1200
)

// refreshWorkspaceKnowledge projects the same authorized convention files
// used by the agent prompt into a queryable, bounded index. It is deliberately
// fail-open: project work continues when indexing cannot complete.
func (c *RunCoordinator) refreshWorkspaceKnowledge(ctx context.Context, task *control.Task, workspace *control.Workspace) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil || workspace == nil {
		return
	}
	if strings.TrimSpace(workspace.ID) == "" || strings.TrimSpace(workspace.LocalPath) == "" {
		return
	}
	files, err := kernel.NewContextScanner().ScanFrom(workspace.LocalPath)
	if err != nil {
		return
	}
	sections := workspaceKnowledgeProjection(files)
	_ = c.srv.Control.ReplaceWorkspaceKnowledge(ctx, task.TenantID, task.PersonID, workspace.ID, sections)
}

func workspaceKnowledgeProjection(files []kernel.ContextFile) []control.WorkspaceKnowledgeWrite {
	var out []control.WorkspaceKnowledgeWrite
	for _, file := range files {
		content := strings.TrimSpace(file.Content)
		if content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		mtime := int64(0)
		if info, err := os.Stat(file.Path); err == nil {
			mtime = info.ModTime().Unix()
		}
		for index, section := range splitWorkspaceKnowledgeSections(content, file.Name) {
			out = append(out, control.WorkspaceKnowledgeWrite{
				FilePath:    filepath.Clean(file.Path),
				FileName:    file.Name,
				ContentHash: hash,
				FileMTime:   mtime,
				Section:     index,
				Title:       section.title,
				Excerpt:     textutil.Truncate(strings.TrimSpace(section.body), workspaceKnowledgeExcerpt-3),
			})
		}
	}
	return out
}

type workspaceKnowledgeSection struct {
	title string
	body  string
}

func splitWorkspaceKnowledgeSections(content, fallbackTitle string) []workspaceKnowledgeSection {
	lines := strings.Split(content, "\n")
	sections := make([]workspaceKnowledgeSection, 0, 8)
	title := strings.TrimSpace(fallbackTitle)
	var body []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if text == "" || len(sections) >= workspaceKnowledgeMaxSections {
			body = body[:0]
			return
		}
		sections = append(sections, workspaceKnowledgeSection{title: title, body: text})
		body = body[:0]
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if heading, ok := markdownHeading(trimmed); ok {
			flush()
			title = heading
			body = append(body, trimmed)
			continue
		}
		body = append(body, line)
	}
	flush()
	if len(sections) == 0 {
		sections = append(sections, workspaceKnowledgeSection{title: fallbackTitle, body: content})
	}
	return sections
}

func markdownHeading(line string) (string, bool) {
	if line == "" || line[0] != '#' {
		return "", false
	}
	i := 0
	for i < len(line) && line[i] == '#' && i < 6 {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != ' ' {
		return "", false
	}
	heading := strings.TrimSpace(line[i+1:])
	return heading, heading != ""
}
