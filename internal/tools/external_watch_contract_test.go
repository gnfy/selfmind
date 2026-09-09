package tools

import (
	"strings"
	"testing"
)

// The command field said "Read-only command that checks the external state",
// which promised a wider input than the evaluator accepts: read-only here means
// provable without running it, so a loop or a runtime variable is refused. The
// model could only learn that by spending a call on a rejection. A tool
// description that is narrower than its enforcement is a call the model has to
// waste to discover the contract.
func TestExternalWatchDescribesTheInputItActuallyAccepts(t *testing.T) {
	tool := NewExternalWatchTool(nil)
	command, ok := tool.Schema().Properties["command"]
	if !ok {
		t.Fatal("watch_external has no command field")
	}
	described := strings.ToLower(command.Description + " " + tool.Description())

	// The property the evaluator enforces, named where the model reads first.
	if !strings.Contains(described, "proven read-only") && !strings.Contains(described, "provably read-only") {
		t.Errorf("the static-proof requirement is not stated:\n%s", described)
	}
	// The shapes that are actually rejected, so the first attempt can be legal.
	for _, rejected := range []string{"loops", "subshells", "command substitution", "sudo", "xargs"} {
		if !strings.Contains(described, rejected) {
			t.Errorf("the description does not rule out %q:\n%s", rejected, described)
		}
	}
	// The supported way to check something the proof layer cannot admit.
	if !strings.Contains(described, "observation script") {
		t.Errorf("the alternative for more involved checks is missing:\n%s", described)
	}
}
