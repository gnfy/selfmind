package api

import (
	"strings"
	"time"

	"selfmind/internal/kernel"
)

// PlanCancellationDetail is the line every plan surface shows under a step
// whose cancellation Main assessed: the disposition, its reason, and the
// person input whose quote authorized it, as in "Cancelled · user takeover:
// you deploy it · authorized by your guidance at 14:02". A pending step the
// runtime kept open reads "Unfinished"; any other step has none. The input
// reads in now's location, never as an id or any of its text.
func PlanCancellationDetail(status, disposition, reason string, origin *kernel.PlanQuoteOrigin, now time.Time) string {
	var detail string
	switch {
	case status == "pending" && disposition == "unfinished":
		detail = "Unfinished"
	case status != "cancelled":
		return ""
	case disposition == "not_required":
		detail = "Cancelled · not required"
	case disposition == "user_takeover":
		detail = "Cancelled · user takeover"
	default:
		detail = "Cancelled"
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		detail += ": " + reason
	}
	if from := planQuoteOriginText(status, origin, now); from != "" {
		detail += " · authorized by " + from
	}
	return detail
}

// planQuoteOriginText names, in the person's terms, the input a cancelled
// step's quote came from: its kind and when it arrived.
func planQuoteOriginText(status string, origin *kernel.PlanQuoteOrigin, now time.Time) string {
	if status != "cancelled" || origin == nil {
		return ""
	}
	var input string
	switch origin.Input {
	case kernel.PlanQuoteFromGuidance:
		input = "guidance"
	case kernel.PlanQuoteFromRequest:
		if !origin.EarlierRun {
			return "your request that started this run"
		}
		input = "request"
	default:
		return ""
	}
	text := "your " + input
	if origin.EarlierRun {
		text = "your earlier " + input
	}
	if at, err := time.Parse(time.RFC3339, origin.ReceivedAt); err == nil {
		at = at.In(now.Location())
		layout := "Jan 2 15:04"
		if ay, am, ad := at.Date(); ay == now.Year() && am == now.Month() && ad == now.Day() {
			layout = "15:04"
		}
		text += " at " + at.Format(layout)
	}
	return text
}
