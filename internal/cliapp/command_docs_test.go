package cliapp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gatewaycommand "selfmind/internal/gateway/command"
)

func TestCommandReferencesCoverEveryUserFacingCommand(t *testing.T) {
	root := commandDocsRepoRoot(t)
	references := []string{
		filepath.Join(root, "docs", "command-reference.md"),
		filepath.Join(root, "docs", "command-reference.zh-CN.md"),
	}

	for _, path := range references {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(content)
		for _, usage := range documentedCLIUsages {
			if !strings.Contains(text, usage) {
				t.Errorf("%s is missing CLI usage %q", path, usage)
			}
		}
		for _, entry := range gatewaycommand.All() {
			if !strings.Contains(text, entry.Usage) {
				t.Errorf("%s is missing slash-command usage %q", path, entry.Usage)
			}
		}
	}
}

func TestTopLevelHelpListsEveryDocumentedCLIUsage(t *testing.T) {
	var output bytes.Buffer
	printTopLevelHelp(&output)
	for _, usage := range documentedCLIUsages {
		if !strings.Contains(output.String(), usage) {
			t.Errorf("top-level help is missing %q", usage)
		}
	}
}

func commandDocsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root")
		}
		dir = parent
	}
}
