package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageAttachmentsFromInput(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bare path as whole input (drag-to-terminal).
	got := imageAttachmentsFromInput(img, dir)
	if len(got) != 1 || got[0].MimeType != "image/png" || got[0].Kind != "image" {
		t.Fatalf("whole-path: %+v", got)
	}

	// Path embedded in a sentence, relative to cwd, with text around it.
	got = imageAttachmentsFromInput("看看 shot.png 这个报错", dir)
	if len(got) != 1 || got[0].Name != "shot.png" {
		t.Fatalf("embedded relative: %+v", got)
	}

	// Non-existent image path → ignored.
	if got := imageAttachmentsFromInput("missing.png", dir); got != nil {
		t.Fatalf("nonexistent should be ignored: %+v", got)
	}

	// Plain text, no image → nil.
	if got := imageAttachmentsFromInput("just a normal message", dir); got != nil {
		t.Fatalf("plain text should yield no attachments: %+v", got)
	}
}

func TestDeletedComposerImageTokenDoesNotCreateAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clipboard.png")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	model := NewController(nil, nil, nil, "").model
	token := model.editor.AttachImage(path)
	model.editor.SetValue("describe the remaining text")
	preview := model.editor.PreviewSubmission()

	if strings.Contains(preview.Expanded, path) || strings.Contains(preview.Display, token) {
		t.Fatalf("deleted token still resolved: display=%q expanded=%q", preview.Display, preview.Expanded)
	}
	if got := imageAttachmentsFromInput(preview.Expanded, dir); got != nil {
		t.Fatalf("deleted token created an attachment: %+v", got)
	}
}

func TestClipboardImageAttachmentDoesNotCommitAHistoryNotice(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.attachClipboardImage(filepath.Join(t.TempDir(), "clipboard.png"), "")

	if !strings.Contains(model.editor.Value(), "[Image #1 · clipboard.png]") {
		t.Fatalf("composer missing attached image token: %q", model.editor.Value())
	}
	for _, message := range model.messages {
		if strings.Contains(message.Content, "Image attached from the clipboard") {
			t.Fatalf("attachment status leaked into immutable transcript: %+v", message)
		}
	}

	model.editor.SetValue("")
	preview := model.editor.PreviewSubmission()
	if preview.Display != "" || preview.Expanded != "" {
		t.Fatalf("deleted token left an outgoing attachment: %+v", preview)
	}
}
