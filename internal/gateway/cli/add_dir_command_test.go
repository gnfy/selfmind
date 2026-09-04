package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddDirGrantsThisSessionAnotherDirectory pins the session command that
// closes the "I have to restart to reach another directory" gap. It carries the
// SAME name as the --add-dir flag on purpose: the flag already had to be
// learned, and a session command meaning the same thing should not cost a
// second name.
func TestAddDirGrantsThisSessionAnotherDirectory(t *testing.T) {
	model := NewController("", "", nil, "").model
	dir := t.TempDir()

	if cmd := model.handleAddDir(nil); cmd != nil {
		t.Fatal("bare /add-dir must render, not dispatch")
	}
	last := model.messages[len(model.messages)-1].Content
	if !strings.Contains(last, "No extra directories") {
		t.Fatalf("bare listing on an empty session: %q", last)
	}

	model.handleAddDir([]string{dir})
	if len(model.additionalRoots) != 1 || model.additionalRoots[0] != dir {
		t.Fatalf("roots = %v, want [%s]", model.additionalRoots, dir)
	}
	if last := model.messages[len(model.messages)-1].Content; !strings.Contains(last, dir) {
		t.Fatalf("reply must name the directory: %q", last)
	}

	// Adding it twice is a no-op, not a duplicate root.
	model.handleAddDir([]string{dir})
	if len(model.additionalRoots) != 1 {
		t.Fatalf("duplicate add changed the overlay: %v", model.additionalRoots)
	}

	// A path that is not a directory is refused with the reason, so a typo
	// does not become an unexplained tool refusal mid-turn.
	file := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	model.handleAddDir([]string{file})
	if last := model.messages[len(model.messages)-1].Content; !strings.Contains(last, "is a file, not a directory") {
		t.Fatalf("file path reply: %q", last)
	}
	model.handleAddDir([]string{filepath.Join(dir, "missing")})
	if last := model.messages[len(model.messages)-1].Content; !strings.Contains(last, "does not exist") {
		t.Fatalf("missing path reply: %q", last)
	}
	if len(model.additionalRoots) != 1 {
		t.Fatalf("a refused path must not enter the overlay: %v", model.additionalRoots)
	}
}

// TestAddDirRefusesBeyondTheGatewayBound stops at the same limit the gateway
// enforces, so the person is told which directory was rejected instead of
// having the whole turn refused later.
func TestAddDirRefusesBeyondTheGatewayBound(t *testing.T) {
	model := NewController("", "", nil, "").model
	for i := 0; i < maxSessionAdditionalRoots+1; i++ {
		model.handleAddDir([]string{t.TempDir()})
	}
	if len(model.additionalRoots) != maxSessionAdditionalRoots {
		t.Fatalf("roots = %d, want the gateway bound %d", len(model.additionalRoots), maxSessionAdditionalRoots)
	}
	if last := model.messages[len(model.messages)-1].Content; !strings.Contains(last, "maximum") {
		t.Fatalf("bound reply: %q", last)
	}
}
