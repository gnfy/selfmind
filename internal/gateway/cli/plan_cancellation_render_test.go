package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPlanCancellationIsNotRenderedAsPending(t *testing.T) {
	cancelled := `{"plan":[{"step":"Observe current load","status":"cancelled","cancellation_disposition":"not_required","cancellation_reason":"An accepted replacement covers the observation"},{"step":"Inspect local records","status":"completed"},{"step":"Prepare collection commands","status":"in_progress"}]}`
	pending := strings.Replace(cancelled, `"status":"cancelled"`, `"status":"pending"`, 1)
	rendered := stripANSI(renderPlanCell(cancelled, 0, 100))
	if rendered == stripANSI(renderPlanCell(pending, 0, 100)) {
		t.Fatal("cancelled and pending steps have identical presentation")
	}
	for _, want := range []string{"Cancelled", "An accepted replacement covers the observation", "1/3", "1 cancelled"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("cancelled Plan lost %q:\n%s", want, rendered)
		}
	}
}

func TestPlanCancellationWrapsAtNarrowTerminalWidth(t *testing.T) {
	content := `{"plan":[{"step":"Inspect the current target","status":"cancelled","cancellation_disposition":"not_required","cancellation_reason":"An accepted alternative covers the original condition"}]}`
	rendered := renderPlanCell(content, 0, 24)
	for _, line := range strings.Split(rendered, "\n") {
		if width := ansi.StringWidth(line); width > 24 {
			t.Errorf("Plan line has %d columns at width 24: %q", width, line)
		}
	}
	if !strings.Contains(stripANSI(rendered), "Cancelled") {
		t.Fatal("narrow rendering lost cancellation meaning")
	}
}

func TestPlanRetainsUnfinishedCancellationMeaning(t *testing.T) {
	content := `{"plan":[{"step":"Inspect current target","status":"pending","cancellation_disposition":"unfinished","cancellation_reason":"Access is unavailable"},{"step":"Read independent local notes","status":"completed"},{"step":"Prepare next check","status":"in_progress"}]}`
	rendered := stripANSI(renderPlanCell(content, 0, 100))
	for _, want := range []string{"Unfinished", "Access is unavailable", "Inspect current target", "Read independent local notes", "Prepare next check"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("Plan lost %q:\n%s", want, rendered)
		}
	}
	if strings.Index(rendered, "Inspect current target") > strings.Index(rendered, "Read independent local notes") {
		t.Fatal("rendering reordered Main's plan instead of explaining its states")
	}
}
