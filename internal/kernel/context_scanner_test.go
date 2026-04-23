package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextScanner_Scan(t *testing.T) {
	// Create a temporary directory structure
	tmpDir := t.TempDir()
	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)

	os.Chdir(tmpDir)

	// Create a .selfmind.md file
	selfmindContent := "Use pnpm instead of npm. Prefer TypeScript strict mode."
	os.WriteFile(filepath.Join(tmpDir, ".selfmind.md"), []byte(selfmindContent), 0644)

	// Create a README.md file
	readmeContent := "# My Project\nThis is a Go project."
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte(readmeContent), 0644)

	// Create a subdirectory with its own AGENTS.md (should be found from subdir)
	subDir := filepath.Join(tmpDir, "cmd", "app")
	os.MkdirAll(subDir, 0755)
	os.Chdir(subDir)

	// Create a git root marker in tmpDir so scanning stops there
	os.Mkdir(filepath.Join(tmpDir, ".git"), 0755)

	scanner := NewContextScanner()
	files, err := scanner.Scan()
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("Expected to find context files, got none")
	}

	// Should find .selfmind.md and README.md from parent dir
	foundSelfmind := false
	foundReadme := false
	for _, f := range files {
		if f.Name == ".selfmind.md" {
			foundSelfmind = true
			if !contains(f.Content, "pnpm") {
				t.Errorf("Expected .selfmind.md to contain 'pnpm', got: %s", f.Content)
			}
		}
		if f.Name == "README.md" {
			foundReadme = true
		}
	}

	if !foundSelfmind {
		t.Error("Expected to find .selfmind.md")
	}
	if !foundReadme {
		t.Error("Expected to find README.md")
	}
}

func TestContextScanner_BuildContextPrompt(t *testing.T) {
	scanner := NewContextScanner()
	files := []ContextFile{
		{Name: ".selfmind.md", Path: "/project/.selfmind.md", Content: "Use pnpm.", Priority: 0},
		{Name: "AGENTS.md", Path: "/project/AGENTS.md", Content: "Always write tests.", Priority: 1},
	}

	prompt := scanner.BuildContextPrompt(files)
	if prompt == "" {
		t.Fatal("Expected non-empty prompt")
	}

	if !contains(prompt, "Use pnpm.") {
		t.Errorf("Expected prompt to contain 'Use pnpm.', got: %s", prompt)
	}
	if !contains(prompt, "Always write tests.") {
		t.Errorf("Expected prompt to contain 'Always write tests.', got: %s", prompt)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
