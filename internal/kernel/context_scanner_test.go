package kernel

import (
	"os"
	"path/filepath"
	"strings"
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

	// Create a README.md file — route 3 removed README from the default list,
	// so it must NOT be injected (human-facing, low signal).
	readmeContent := "# My Project\nThis is a Go project."
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte(readmeContent), 0644)

	// Create a subdirectory to scan from.
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

	foundSelfmind := false
	for _, f := range files {
		if f.Name == ".selfmind.md" {
			foundSelfmind = true
			if !strings.Contains(f.Content, "pnpm") {
				t.Errorf("Expected .selfmind.md to contain 'pnpm', got: %s", f.Content)
			}
		}
		if f.Name == "README.md" {
			t.Error("README.md must NOT be injected (removed from default filenames)")
		}
	}
	if !foundSelfmind {
		t.Error("Expected to find .selfmind.md")
	}
}

func TestContextScanner_ScanFromWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	other := t.TempDir()
	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(other)

	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("Use workspace instructions."), 0644); err != nil {
		t.Fatal(err)
	}

	scanner := NewContextScanner()
	files, err := scanner.ScanFrom(workspace)
	if err != nil {
		t.Fatalf("ScanFrom failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected context files from workspace root")
	}
	if files[0].Name != "AGENTS.md" || !strings.Contains(files[0].Content, "workspace instructions") {
		t.Fatalf("unexpected files: %+v", files)
	}
}

func TestContextScannerScanRootsKeepsEachBoundRoot(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "AGENTS.md"), []byte("first root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "AGENTS.md"), []byte("second root"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewContextScanner().ScanRoots([]string{first, second, first})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %#v", files)
	}
	if files[0].ScopeRoot != first || files[1].ScopeRoot != second {
		t.Fatalf("scope roots = %q, %q", files[0].ScopeRoot, files[1].ScopeRoot)
	}
}

// TestContextScanner_RootToLeafOrder pins Codex-style hierarchical discovery:
// an AGENTS.md at the git root and one in a nested dir are BOTH collected, and
// the deeper (more local) one is emitted LAST so it overrides on conflict.
func TestContextScanner_RootToLeafOrder(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0755)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT rule: two-space indent."), 0644)

	sub := filepath.Join(root, "service", "api")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("LOCAL rule: tabs here."), 0644)

	scanner := NewContextScanner()
	files, err := scanner.ScanFrom(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 hierarchical AGENTS.md, got %d: %+v", len(files), files)
	}
	// Root first (depth 0), leaf last (higher depth).
	if !strings.Contains(files[0].Content, "ROOT rule") || files[0].Depth != 0 {
		t.Fatalf("first file should be root (depth 0): %+v", files[0])
	}
	if !strings.Contains(files[len(files)-1].Content, "LOCAL rule") {
		t.Fatalf("last file should be the deepest/local one: %+v", files[len(files)-1])
	}
	if files[len(files)-1].Depth <= files[0].Depth {
		t.Fatalf("leaf depth must exceed root depth: %+v", files)
	}
}

// TestContextScanner_LargeFileTruncatedNotDropped is the core route-3 fix: a
// >8KB AGENTS.md used to be skipped WHOLE. It must now be injected (head+tail)
// with a read_file pointer to the full path.
func TestContextScanner_LargeFileTruncatedNotDropped(t *testing.T) {
	big := "HEAD-MARKER rule one.\n" + strings.Repeat("padding line to inflate size.\n", 3000) + "TAIL-MARKER rule last."
	if len(big) <= 8*1024 {
		t.Fatalf("test fixture must exceed the old 8KB limit, got %d", len(big))
	}
	scanner := NewContextScanner()
	scanner.SetContextWindowTokens(8000) // small window → floor budget, forces truncation
	files := []ContextFile{
		{Name: "AGENTS.md", Path: "/proj/AGENTS.md", Content: big},
	}
	out := scanner.BuildContextPrompt(files)
	if out == "" {
		t.Fatal("large file must not be dropped (got empty prompt)")
	}
	if !strings.Contains(out, "HEAD-MARKER") {
		t.Error("expected the head of the file to be kept")
	}
	if !strings.Contains(out, "read_file: /proj/AGENTS.md") {
		t.Error("expected a read_file pointer to the full path in the truncation marker")
	}
}

// TestContextScanner_DynamicBudget verifies the byte budget scales with the
// model window (Hermes-style) and is clamped to the floor for tiny windows.
func TestContextScanner_DynamicBudget(t *testing.T) {
	cs := NewContextScanner()
	// Unset window → floor.
	if got := cs.totalBudget(); got != contextTotalFloor {
		t.Errorf("unset window budget = %d, want floor %d", got, contextTotalFloor)
	}
	// Large window → scales up (200k tokens × 4 × 0.06 = 48k > floor).
	cs.SetContextWindowTokens(200_000)
	if got := cs.totalBudget(); got <= contextTotalFloor {
		t.Errorf("large-window budget = %d, want > floor", got)
	}
	// Huge window → clamped to ceiling.
	cs.SetContextWindowTokens(100_000_000)
	if got := cs.totalBudget(); got != contextTotalCeiling {
		t.Errorf("huge-window budget = %d, want ceiling %d", got, contextTotalCeiling)
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
	if !strings.Contains(prompt, "Use pnpm.") {
		t.Errorf("Expected prompt to contain 'Use pnpm.', got: %s", prompt)
	}
	if !strings.Contains(prompt, "Always write tests.") {
		t.Errorf("Expected prompt to contain 'Always write tests.', got: %s", prompt)
	}
	// Untrusted-data fence: operator/user instructions must be stated to outrank
	// these files (IM-injection defense).
	if !strings.Contains(prompt, "take precedence") {
		t.Errorf("expected an untrusted-data fence noting operator precedence, got: %s", prompt)
	}
}
