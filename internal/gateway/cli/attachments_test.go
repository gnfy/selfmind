package cli

import (
	"os"
	"path/filepath"
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
