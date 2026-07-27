package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestImportAttachmentsCopiesIntoPersonPartition pins the attachment scope
// channel: an out-of-workspace file (e.g. a clipboard paste in the OS temp
// dir) is copied into <AttachmentsDir>/<personID>/<runID>/ and the attachment
// path is rewritten to the managed copy, which lives under the root that
// installExecutionScope adds to AllowedRoots.
func TestImportAttachmentsCopiesIntoPersonPartition(t *testing.T) {
	tempSrc := filepath.Join(t.TempDir(), "selfmind-paste-1.png")
	if err := os.WriteFile(tempSrc, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	attDir := t.TempDir()
	c := &RunCoordinator{srv: &Server{AttachmentsDir: attDir}}
	identity := &control.IdentityContext{TenantID: "t1", PersonID: "person_a"}
	run := &control.Run{ID: "run_123"}

	out := c.importAttachments(identity, run, []api.MessageAttachment{
		{Kind: "image", Path: tempSrc, Name: "selfmind-paste-1.png", MimeType: "image/png"},
	})
	if len(out) != 1 {
		t.Fatalf("got %d attachments, want 1", len(out))
	}
	root := c.personAttachmentsRoot(identity)
	if root == "" || !strings.HasPrefix(out[0].Path, root+string(filepath.Separator)) {
		t.Fatalf("rewritten path %q not under person root %q", out[0].Path, root)
	}
	if !strings.Contains(out[0].Path, "run_123") {
		t.Fatalf("rewritten path %q missing run partition", out[0].Path)
	}
	data, err := os.ReadFile(out[0].Path)
	if err != nil || string(data) != "png-bytes" {
		t.Fatalf("managed copy unreadable or corrupted: %v %q", err, data)
	}
	if out[0].Size != int64(len("png-bytes")) {
		t.Fatalf("size = %d, want %d", out[0].Size, len("png-bytes"))
	}
}

// TestImportAttachmentsDegradesNeverDrops: a missing source file or a disabled
// store keeps the original attachment untouched instead of dropping it.
func TestImportAttachmentsDegradesNeverDrops(t *testing.T) {
	identity := &control.IdentityContext{TenantID: "t1", PersonID: "person_a"}
	run := &control.Run{ID: "run_1"}

	// Missing source file.
	c := &RunCoordinator{srv: &Server{AttachmentsDir: t.TempDir()}}
	out := c.importAttachments(identity, run, []api.MessageAttachment{{Path: "/nonexistent/x.png"}})
	if len(out) != 1 || out[0].Path != "/nonexistent/x.png" {
		t.Fatalf("missing file must keep original path, got %+v", out)
	}

	// Disabled store (no AttachmentsDir).
	c = &RunCoordinator{srv: &Server{}}
	src := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = c.importAttachments(identity, run, []api.MessageAttachment{{Path: src}})
	if len(out) != 1 || out[0].Path != src {
		t.Fatalf("disabled store must keep original path, got %+v", out)
	}
	if c.personAttachmentsRoot(identity) != "" {
		t.Fatal("disabled store must yield no scope root")
	}
}

// TestSanitizeAttachmentName strips separators and hostile runes so a
// client-supplied name cannot traverse out of the partition.
func TestSanitizeAttachmentName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"a b?.png":         "a_b_.png",
		"截图.png":           "__.png",
		"":                 "attachment",
	}
	for in, want := range cases {
		if got := sanitizeAttachmentName(in, "/tmp/fallback.bin"); in != "" && got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeAttachmentName("", "/tmp/fallback.bin"); got != "fallback.bin" {
		t.Errorf("empty name should fall back to path base, got %q", got)
	}
}
