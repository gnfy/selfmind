package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestWorkspaceKnowledgeProjectionSplitsAndBoundsSections(t *testing.T) {
	content := "preamble rule\n\n# Build\nuse go test\n\n## Deploy\n" + strings.Repeat("verify first ", 300)
	rows := workspaceKnowledgeProjection([]kernel.ContextFile{{
		Path: "/repo/AGENTS.md", Name: "AGENTS.md", Content: content,
	}})
	if len(rows) != 3 {
		t.Fatalf("expected preamble plus two headings, got %+v", rows)
	}
	if rows[1].Title != "Build" || !strings.Contains(rows[1].Excerpt, "go test") {
		t.Fatalf("build section=%+v", rows[1])
	}
	if len([]rune(rows[2].Excerpt)) > workspaceKnowledgeExcerpt {
		t.Fatalf("excerpt exceeded bound: %d", len([]rune(rows[2].Excerpt)))
	}
}
