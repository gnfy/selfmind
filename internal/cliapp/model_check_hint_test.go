package cliapp

import (
	"strings"
	"testing"
)

func TestModelCheckFailureHintForAuxiliary(t *testing.T) {
	hint := modelCheckFailureHint("auxiliary")
	if !strings.Contains(hint, "selfmind setup") || !strings.Contains(hint, "models.auxiliary") {
		t.Fatalf("auxiliary hint = %q", hint)
	}
	if strings.Contains(hint, "selfmind model`") {
		t.Fatalf("auxiliary hint must not redirect to the primary model command: %q", hint)
	}
}

func TestModelCheckFailureHintForExplicitRole(t *testing.T) {
	hint := modelCheckFailureHint("vision")
	if !strings.Contains(hint, "models.roles.vision") {
		t.Fatalf("role hint = %q", hint)
	}
}
