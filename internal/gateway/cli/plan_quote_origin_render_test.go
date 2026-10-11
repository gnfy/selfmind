package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A cancelled step names the person input that authorized it, in the person's
// terms, and never shows the recorded source id or any of the input's text.
func TestPlanCancellationNamesTheInputThatAuthorizedIt(t *testing.T) {
	at := time.Date(2025, time.March, 4, 14, 2, 0, 0, time.Local).Format(time.RFC3339)
	plan := func(origin string) string {
		return `{"plan":[{"step":"Rotate the keys","status":"cancelled","cancellation_disposition":"user_takeover",` +
			`"cancellation_reason":"you rotate them","user_takeover_quote":"I will rotate the keys myself",` +
			`"cancellation_quote_source":"steering:steer_abc"` + origin + `},{"step":"Prepare the release","status":"in_progress"}]}`
	}
	for _, tc := range []struct{ name, origin, want string }{
		{"guidance", `,"cancellation_authorized_by":{"input":"guidance","received_at":"` + at + `"}`, "authorized by your guidance at Mar 4 14:02"},
		{"request", `,"cancellation_authorized_by":{"input":"request","received_at":"` + at + `"}`, "authorized by your request that started this run"},
		{"earlier request", `,"cancellation_authorized_by":{"input":"request","received_at":"` + at + `","earlier_run":true}`, "authorized by your earlier request at Mar 4 14:02"},
		{"earlier guidance", `,"cancellation_authorized_by":{"input":"guidance","received_at":"` + at + `","earlier_run":true}`, "authorized by your earlier guidance at Mar 4 14:02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := stripANSI(renderPlanCell(plan(tc.origin), 0, 140))
			for _, want := range []string{"Cancelled · user takeover: you rotate them", tc.want} {
				if !strings.Contains(rendered, want) {
					t.Errorf("plan lost %q:\n%s", want, rendered)
				}
			}
			for _, leaked := range []string{"steer_abc", "steering:", "I will rotate the keys myself"} {
				if strings.Contains(rendered, leaked) {
					t.Errorf("plan shows %q:\n%s", leaked, rendered)
				}
			}
			for _, line := range strings.Split(renderPlanCell(plan(tc.origin), 0, 30), "\n") {
				if width := ansi.StringWidth(line); width > 30 {
					t.Errorf("plan line has %d columns at width 30: %q", width, line)
				}
			}
		})
	}
	// A cancellation without a recorded source renders as it always has.
	without := stripANSI(renderPlanCell(strings.Replace(plan(""), `,"cancellation_quote_source":"steering:steer_abc"`, "", 1), 0, 140))
	if got := stripANSI(renderPlanCell(plan(""), 0, 140)); got != without || strings.Contains(got, "authorized by") {
		t.Fatalf("a cancellation without an authorizing input changed:\n%s\nwant:\n%s", got, without)
	}
}
