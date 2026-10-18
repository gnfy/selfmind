package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestSearchFilesExplicitModesPreserveLiteralDefault(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "lookup.go")
	if err := os.WriteFile(path, []byte("alpha\nbeta\nalpha|beta\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchFilesTool()
	out, err := tool.Execute(map[string]interface{}{"path": root, "pattern": "alpha|beta"})
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal([]byte(out), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Matches) != 1 || old.Matches[0] != path {
		t.Fatalf("legacy literal lookup: %s", out)
	}
	out, err = tool.Execute(map[string]interface{}{"path": root, "pattern": "[alpha]"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Matches) != 0 {
		t.Fatalf("literal default interpreted regex metacharacters: %s", out)
	}
	out, err = tool.Execute(map[string]interface{}{"path": root, "pattern": "^(alpha|beta)$", "match_mode": "regex", "output_mode": "content"})
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Hits []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Hits) != 2 || found.Hits[0].Path != path || found.Hits[0].Line != 1 || found.Hits[1].Line != 2 || found.Hits[1].Text != "beta" {
		t.Fatalf("regex content: %s", out)
	}
}

func TestSearchFilesUnavailableIsNotEmptySuccess(t *testing.T) {
	tool := NewSearchFilesTool()
	for _, args := range []map[string]interface{}{
		{"path": filepath.Join(t.TempDir(), "missing"), "pattern": "needle"},
		{"path": t.TempDir(), "pattern": "[", "match_mode": "regex"},
		{"path": t.TempDir(), "pattern": "needle", "file_glob": "["},
	} {
		if out, err := tool.Execute(args); err == nil {
			t.Fatalf("invalid/unavailable search returned success: %s", out)
		}
	}
}

func TestSearchFilesDoesNotFollowFileSymlinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("EXTERNAL-ONLY"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skip(err)
	}
	out, err := NewSearchFilesTool().Execute(map[string]interface{}{"path": root, "pattern": "EXTERNAL-ONLY"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 {
		t.Fatalf("followed symlink outside search root: %s", out)
	}
}

func TestReadFileWindowNamesExactLinesAndNextOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "window.txt")
	if err := os.WriteFile(path, []byte("first\n第二行\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := NewReadFileTool().Execute(map[string]interface{}{"path": path, "offset": 2, "limit": 1, "line_numbers": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "first") || !strings.Contains(out, "2: 第二行") || !strings.Contains(out, "offset=3") {
		t.Fatalf("wrong read window: %s", out)
	}
	if _, err := NewReadFileTool().Execute(map[string]interface{}{"path": path, "offset": 99}); err == nil {
		t.Fatal("past-EOF window must not masquerade as empty file")
	}
}

func TestReadFileLineLimitAlsoBoundsAggregateBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many-lines.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat(strings.Repeat("x", 79)+"\n", 2000)), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := NewReadFileTool().Execute(map[string]interface{}{"path": path, "limit": 1500, "max_bytes": 65536})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 65536+512 || !strings.Contains(out, "truncated") {
		t.Fatalf("read ignored aggregate byte limit: %d bytes", len(out))
	}
}

func TestListFilesFiltersReturnedNamesWithoutStoppingTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.go", "b.txt", "nested/c.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := NewListFilesTool().Execute(map[string]interface{}{"path": root, "recursive": true, "file_glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Entries []string `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("wrong glob coverage: %s", out)
	}
	for _, entry := range result.Entries {
		if !strings.HasSuffix(entry, ".go") {
			t.Fatalf("unexpected entry %s", entry)
		}
	}
}

type factChainSink struct {
	content string
	fail    bool
}

func (s *factChainSink) SaveToolOutput(_ context.Context, _, content string) (kernel.ToolArtifactRef, error) {
	if s.fail {
		return kernel.ToolArtifactRef{}, fmt.Errorf("unavailable")
	}
	s.content = content
	return kernel.ToolArtifactRef{ID: "art_factchain", Bytes: len(content)}, nil
}

func TestBatchReadPreservesMiddleBeforePerItemClipping(t *testing.T) {
	raw := strings.Repeat("A", 6000) + "FACT-IN-MIDDLE" + strings.Repeat("Z", 6000)
	sink := &factChainSink{}
	ctx := kernel.WithToolArtifactSink(context.Background(), sink)
	tool := NewBatchReadTool(func(string, map[string]interface{}) (string, error) { return raw, nil })
	out, err := tool.ExecuteContext(ctx, map[string]interface{}{"operations": []interface{}{map[string]interface{}{"tool": "read_file", "path": "facts.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	if sink.content != raw || !strings.Contains(out, "art_factchain") || !strings.Contains(out, "tool_output_view") {
		t.Fatalf("middle is not addressable: saved=%d artifact reference=%t", len(sink.content), strings.Contains(out, "art_factchain"))
	}
}

func TestFileSearchCoverageAndSnippet(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("前", 1000) + "needle" + strings.Repeat("尾", 1000)
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(long+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := NewSearchFilesTool()
	out, err := tool.Execute(map[string]interface{}{"path": root, "pattern": "needle", "output_mode": "content"})
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Hits []struct {
			Text      string
			Truncated bool
		}
		Partial      bool
		SkippedLarge int `json:"skipped_large"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Hits) != 1 || !strings.Contains(r.Hits[0].Text, "needle") || !r.Hits[0].Truncated {
		t.Fatalf("matching part of long line is missing: %s", out)
	}
	out, err = tool.Execute(map[string]interface{}{"path": root, "pattern": "needle", "max_file_bytes": 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Partial || r.SkippedLarge != 1 {
		t.Fatalf("incomplete search looked conclusive: %s", out)
	}
}

func TestSearchFilesLegacyMultilineAndNoPhantomLine(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("alpha\nbeta\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pattern, mode, output string
		count                 int
	}{{"alpha\nbeta", "literal", "files", 1}, {"^$", "regex", "content", 0}} {
		out, err := NewSearchFilesTool().Execute(map[string]interface{}{"path": root, "pattern": tc.pattern, "match_mode": tc.mode, "output_mode": tc.output})
		if err != nil {
			t.Fatal(err)
		}
		var r struct{ Count int }
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		if r.Count != tc.count {
			t.Fatalf("search semantics changed: %s", out)
		}
	}
}

func TestBatchReadReportsSpoolFailureWithoutRetryingRead(t *testing.T) {
	for _, sink := range []*factChainSink{nil, {fail: true}} {
		ctx := context.Background()
		if sink != nil {
			ctx = kernel.WithToolArtifactSink(ctx, sink)
		}
		calls := 0
		tool := NewBatchReadTool(func(string, map[string]interface{}) (string, error) { calls++; return strings.Repeat("x", 12000), nil })
		out, err := tool.ExecuteContext(ctx, map[string]interface{}{"operations": []interface{}{map[string]interface{}{"tool": "read_file", "path": "one.txt"}}})
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Success bool
			Items   []struct {
				Success       bool
				ArtifactID    string `json:"artifact_id"`
				OutputWarning string `json:"output_warning"`
			}
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !r.Success || !r.Items[0].Success || r.Items[0].ArtifactID != "" || r.Items[0].OutputWarning == "" {
			t.Fatalf("spool failure changed execution meaning: calls=%d", calls)
		}
	}
}

func TestBatchReadForwardsFileWindowsAndSearchModes(t *testing.T) {
	var calls []map[string]interface{}
	tool := NewBatchReadTool(func(_ string, args map[string]interface{}) (string, error) {
		calls = append(calls, args)
		return "ok", nil
	})
	_, err := tool.Execute(map[string]interface{}{"operations": []interface{}{
		map[string]interface{}{"tool": "read_file", "path": "a.txt", "offset": 12, "line_numbers": true},
		map[string]interface{}{"tool": "search_files", "path": ".", "pattern": "a|b", "match_mode": "regex", "output_mode": "content"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls[0]["offset"] != 12 || calls[0]["line_numbers"] != true || calls[1]["match_mode"] != "regex" || calls[1]["output_mode"] != "content" {
		t.Fatalf("batch lost retrieval options: %#v", calls)
	}
}
