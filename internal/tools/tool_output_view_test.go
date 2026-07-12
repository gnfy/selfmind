package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeViewArtifact(t *testing.T, baseDir, personID, artifactID, content string) {
	t.Helper()
	dir := filepath.Join(baseDir, personID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifactID+".txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestToolOutputViewReadsChunks(t *testing.T) {
	base := t.TempDir()
	content := strings.Repeat("0123456789", 5000) // 50KB
	writeViewArtifact(t, base, "person_a", "art_11112222", content)
	tool := NewToolOutputViewTool(base)

	out, err := tool.Execute(map[string]interface{}{
		"artifact_id": "art_11112222",
		"_tenant_id":  "person_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bytes 0..24000 of 50000") || !strings.Contains(out, "offset_bytes=24000") {
		t.Fatalf("first chunk header wrong: %q", out[:120])
	}

	out, err = tool.Execute(map[string]interface{}{
		"artifact_id":  "art_11112222",
		"offset_bytes": 48000,
		"_tenant_id":   "person_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bytes 48000..50000 of 50000") || strings.Contains(out, "call again") {
		t.Fatalf("final chunk header wrong: %q", out[:120])
	}

	out, err = tool.Execute(map[string]interface{}{
		"artifact_id":  "art_11112222",
		"offset_bytes": 60000,
		"_tenant_id":   "person_a",
	})
	if err != nil || !strings.Contains(out, "beyond the end") {
		t.Fatalf("past-end read must be a friendly notice: %q err=%v", out, err)
	}
}

// TestToolOutputViewPersonScoped: another person's artifact resolves to a
// different partition directory and must not be readable.
func TestToolOutputViewPersonScoped(t *testing.T) {
	base := t.TempDir()
	writeViewArtifact(t, base, "person_a", "art_33334444", "secret output")
	tool := NewToolOutputViewTool(base)

	if _, err := tool.Execute(map[string]interface{}{
		"artifact_id": "art_33334444",
		"_tenant_id":  "person_b",
	}); err == nil || !strings.Contains(err.Error(), "not found in this person's partition") {
		t.Fatalf("cross-person read must fail: %v", err)
	}
}

// TestToolOutputViewRejectsBadIDs: the id is the file name, so anything
// outside the issued alphabet is rejected before touching the filesystem.
func TestToolOutputViewRejectsBadIDs(t *testing.T) {
	tool := NewToolOutputViewTool(t.TempDir())
	for _, id := range []string{"", "../../etc/passwd", "art_../x", "art_short", "no_prefix_12345678"} {
		if _, err := tool.Execute(map[string]interface{}{
			"artifact_id": id,
			"_tenant_id":  "person_a",
		}); err == nil {
			t.Fatalf("id %q must be rejected", id)
		}
	}
}

func TestToolOutputViewRequiresScope(t *testing.T) {
	tool := NewToolOutputViewTool(t.TempDir())
	if _, err := tool.Execute(map[string]interface{}{
		"artifact_id": "art_55556666",
	}); err == nil || !strings.Contains(err.Error(), "no person scope") {
		t.Fatalf("missing scope must fail closed: %v", err)
	}
}
