package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestTerminalToolEmitsStreamingOutput(t *testing.T) {
	ch := make(chan string, 10)
	ctx := kernel.WithEventChannel(context.Background(), ch)

	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command":  "echo hello",
		"timeout":  5,
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q, want hello", out)
	}
	if !eventSeen(ch, "tool.output") {
		t.Fatalf("tool.output event was not emitted")
	}
}

func TestTerminalToolEmitsHeartbeatForLongCommand(t *testing.T) {
	ch := make(chan string, 20)
	ctx := kernel.WithEventChannel(context.Background(), ch)

	_, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command":  "sleep 2",
		"timeout":  5,
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eventSeen(ch, "tool.heartbeat") {
		t.Fatalf("tool.heartbeat event was not emitted")
	}
}

func TestReadFileToolTruncatesLargeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 80*1024)), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := NewReadFileTool().Execute(map[string]interface{}{
		"path":      path,
		"max_bytes": 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "file truncated after") {
		t.Fatalf("expected truncation marker, got %q", out[len(out)-80:])
	}
}

func TestListFilesToolRecursiveSkipsAndTruncates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "objects", "ignored"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "pkg", "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := NewListFilesTool().Execute(map[string]interface{}{
		"path":        root,
		"recursive":   true,
		"max_entries": 10,
		"timeout":     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Entries     []string `json:"entries"`
		SkippedDirs int      `json:"skipped_dirs"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SkippedDirs == 0 {
		t.Fatalf("expected ignored directories to be skipped: %s", out)
	}
	for _, entry := range payload.Entries {
		if strings.Contains(entry, ".git"+string(os.PathSeparator)+"objects") {
			t.Fatalf("ignored .git entry leaked into results: %v", payload.Entries)
		}
	}
}

func TestSearchFilesToolHonorsLimitAndSkipsIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("needle"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("needle"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "ignored.go"), []byte("needle"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := NewSearchFilesTool().Execute(map[string]interface{}{
		"path":      root,
		"pattern":   "needle",
		"file_glob": "*.go",
		"limit":     1,
		"timeout":   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Matches     []string `json:"matches"`
		SkippedDirs int      `json:"skipped_dirs"`
		Truncated   bool     `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Matches) != 1 {
		t.Fatalf("matches len = %d, want 1: %s", len(payload.Matches), out)
	}
	if payload.SkippedDirs == 0 {
		t.Fatalf("expected .git to be skipped: %s", out)
	}
	if !payload.Truncated {
		t.Fatalf("expected search result to be marked truncated: %s", out)
	}
}

func eventSeen(ch <-chan string, eventType string) bool {
	for {
		select {
		case raw := <-ch:
			event, ok := kernel.DecodeAgentEvent(raw)
			if ok && event.Type == eventType {
				return true
			}
		default:
			return false
		}
	}
}
