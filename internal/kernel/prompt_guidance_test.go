package kernel

import (
	"strings"
	"testing"
)

func TestForegroundDeliveryGuidanceDefinesTerminalOutputContract(t *testing.T) {
	guidance := foregroundDeliveryGuidance()
	for _, want := range []string{
		"terminal client will style",
		"short descriptive headings",
		"lists flat",
		"file paths in inline code",
		"Do not manufacture a fixed Summary/Done/Tests/Files/Risks template",
	} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("foreground guidance missing %q:\n%s", want, guidance)
		}
	}
}

func TestInterfaceGuidanceIsSemanticAndProjectLed(t *testing.T) {
	// The always-on guidance must stay domain-agnostic.
	if strings.Contains(taskExecutionGuidance(), "INTERFACE") {
		t.Fatal("taskExecutionGuidance must not contain interface-specific content")
	}
	guidance := conditionalUserFacingInterfaceGuidance(userFacingInterfaceQualityGuidance())
	for _, want := range []string{"only when", "Ignore it for all other work", "existing design system", "accessibility", "Do not imply visual"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("interface guidance missing %q:\n%s", want, guidance)
		}
	}
	for _, unwanted := range []string{"CSS variables", "gradients", "animations", "system stacks", "AI slop"} {
		if strings.Contains(guidance, unwanted) {
			t.Fatalf("generic interface guidance must not impose an aesthetic default %q:\n%s", unwanted, guidance)
		}
	}
	// The universal floor stays capability-neutral; workspace implementation
	// guidance owns project-aware verification without a fixed language list.
	g := taskExecutionGuidance()
	if strings.Contains(g, "go.mod") || strings.Contains(g, "terminal") {
		t.Fatal("universal verification guidance must not assume a coding ecosystem or command capability")
	}
	workspace := workspaceImplementationGuidance()
	if !strings.Contains(workspace, "declared checks") || !strings.Contains(workspace, "when command execution is available") {
		t.Fatal("workspace guidance must make project verification capability-aware")
	}
}

// The always-on guidance states the same cancellation evidence the runtime
// checks; it must not ask a criteria-less step for an original criterion.
func TestTaskGuidanceStatesTheCancellationEvidenceRules(t *testing.T) {
	guidance := taskExecutionGuidance()
	for _, want := range []string{"established success_criteria", "planned without success_criteria", "states its own", "whole user message", "whole sentence", "punctuation", "neither grants nor vetoes"} {
		if !strings.Contains(guidance, want) {
			t.Errorf("guidance lacks %q", want)
		}
	}
	if strings.Contains(guidance, "covering the original success_criteria") {
		t.Error("guidance still demands an original criterion from every replacement")
	}
}
