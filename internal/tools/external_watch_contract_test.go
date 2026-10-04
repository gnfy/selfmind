package tools

import (
	"errors"
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

// A watcher refused for its specification says what is wrong. The refusal said
// only that the specification was invalid, so qwen had to guess at the one
// correction the hint allows.
func TestInvalidWatcherSpecSaysWhatIsWrong(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  map[string]interface{}
		want string
	}{
		{"group size", map[string]interface{}{"wait_group": "receipt-handoff", "wait_group_size": float64(1)}, "wait_group_size must be between 2 and 8"},
		{"group mode", map[string]interface{}{"wait_group": "receipt-handoff", "wait_group_size": float64(2), "wait_group_mode": "both"}, "wait_group_mode must be all or any"},
		{"pattern", map[string]interface{}{"success_pattern": "^READY("}, "invalid success_pattern"},
		{"target", map[string]interface{}{"target_pattern": "READY"}, "requires both terminal_success_pattern and terminal_failure_pattern"},
	} {
		args := map[string]interface{}{"command": "cat prerequisite.txt", "success_pattern": "^READY$"}
		for key, value := range tc.set {
			args[key] = value
		}
		err := validateExternalWatchStatic(args)
		var refusal interface {
			ToolErrorCode() string
			ModelSafeMessage() string
		}
		if !errors.As(err, &refusal) || refusal.ToolErrorCode() != "watch_spec_invalid" {
			t.Fatalf("%s: err=%v, want a watch_spec_invalid refusal", tc.name, err)
		}
		if !strings.Contains(refusal.ModelSafeMessage(), tc.want) {
			t.Errorf("%s: model sees %q, want it to say %q", tc.name, refusal.ModelSafeMessage(), tc.want)
		}
	}
	if err := validateExternalWatchStatic(map[string]interface{}{"command": "cat prerequisite.txt", "success_pattern": "^READY$"}); err != nil {
		t.Fatalf("a valid specification was refused: %v", err)
	}
}
