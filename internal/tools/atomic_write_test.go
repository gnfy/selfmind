package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAcceptsEmptyContentAndReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := NewWriteFileTool()
	if _, err := tool.Execute(map[string]interface{}{"path": path, "content": ""}); err != nil {
		t.Fatalf("empty write failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty file, got %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("mode changed during replacement: got %o", got)
		}
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".selfmind-write-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}

func TestPatchUpdateUsesAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := applyUpdate(PatchOperation{
		Operation: OpUpdate,
		FilePath:  path,
		Hunks: []Hunk{{Lines: []HunkLine{
			{Prefix: "-", Content: "before"},
			{Prefix: "+", Content: "after"},
		}}},
	})
	if err != nil {
		t.Fatalf("patch update failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "after\n" {
		t.Fatalf("unexpected content: %q", data)
	}
}
