package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
)

// TestEditorAttachImageTokenAndExpand pins the image-placeholder contract
// (mirroring [[ paste:N ]]): the composer shows a compact token — never the
// raw path, which starts with "/" and used to be routed as a slash command —
// and ExpandValue substitutes the real path back for the submit pipeline.
func TestEditorAttachImageTokenAndExpand(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.textarea.SetValue("看一下这张图")

	token := e.AttachImage("/mnt/c/Users/u/AppData/Local/Temp/selfmind-paste-1.png")
	if !strings.Contains(token, "image:0") || !strings.Contains(token, "selfmind-paste-1.png") {
		t.Fatalf("token = %q, want an [[ image:0 …name… ]] placeholder", token)
	}
	if display := e.Value(); strings.Contains(display, "/mnt/c/") {
		t.Fatalf("display value leaks the raw path: %q", display)
	}
	if display := e.Value(); !strings.Contains(display, token) {
		t.Fatalf("display value %q missing token %q", display, token)
	}

	expanded := e.ExpandValue()
	if !strings.Contains(expanded, "/mnt/c/Users/u/AppData/Local/Temp/selfmind-paste-1.png") {
		t.Fatalf("expanded = %q, want the real path substituted back", expanded)
	}
	if strings.Contains(expanded, "[[ image:") {
		t.Fatalf("expanded = %q, token must not survive expansion", expanded)
	}

	// A second image gets its own index and both expand independently.
	e.AttachImage("/tmp/selfmind-paste-2.png")
	expanded = e.ExpandValue()
	if !strings.Contains(expanded, "selfmind-paste-1.png") || !strings.Contains(expanded, "/tmp/selfmind-paste-2.png") {
		t.Fatalf("expanded = %q, want both image paths", expanded)
	}

	// Reset clears the attachment registry with the text.
	e.Reset()
	if e.ExpandValue() != "" {
		t.Fatalf("after Reset, ExpandValue = %q, want empty", e.ExpandValue())
	}
}

// TestEditorDeletedImageTokenDetaches: deleting the token from the composer IS
// detaching the image — the path must not reappear in the expanded submission.
func TestEditorDeletedImageTokenDetaches(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.AttachImage("/tmp/pic.png")
	e.textarea.SetValue("just text, token deleted")

	expanded := e.ExpandValue()
	if strings.Contains(expanded, "/tmp/pic.png") {
		t.Fatalf("expanded = %q, deleted token must not expand", expanded)
	}
	if expanded != "just text, token deleted" {
		t.Fatalf("expanded = %q, want the text untouched", expanded)
	}
}
