package httpapi

import (
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
)

// W3d (cross-source relevance): when more task events exist than fit the
// per-turn budget, keep the most RELEVANT ones (outcomes, handoffs, errors,
// plan/tool changes) rather than just the most recent — a burst of low-value
// thinking/stream events should not crowd out an earlier run.outcome. Selected
// events are returned in chronological order for display.

// eventTypeWeight rates how valuable an event type is to retain in prompt
// context. Higher = more important to keep.
func eventTypeWeight(t string) float64 {
	t = strings.ToLower(t)
	switch {
	case strings.Contains(t, "outcome"), strings.Contains(t, "handoff"), strings.Contains(t, "finish"):
		return 1.0
	case strings.Contains(t, "error"), strings.Contains(t, "fail"), strings.Contains(t, "approval"):
		return 0.95
	case strings.Contains(t, "plan"):
		return 0.7
	case strings.Contains(t, "tool"):
		return 0.6
	case strings.Contains(t, "started"), strings.Contains(t, "completed"):
		return 0.55
	case strings.Contains(t, "thinking"), strings.Contains(t, "step"), strings.Contains(t, "stream"):
		return 0.2
	default:
		return 0.5
	}
}

// rankTaskEvents selects the top `max` events by type-weight plus a recency
// bonus, then returns them in chronological order. With <= max events it just
// sorts chronologically.
func rankTaskEvents(events []control.Event, max int) []control.Event {
	if max <= 0 || len(events) == 0 {
		return nil
	}
	filtered := make([]control.Event, 0, len(events))
	for _, event := range events {
		// Recall adoption is diagnostic telemetry, not task evidence. Feeding it
		// back into the next prompt would turn an observability improvement into
		// permanent context growth and expose internal scoring details to models.
		if event.Type == "context.recall_usage" {
			continue
		}
		filtered = append(filtered, event)
	}
	events = filtered
	if len(events) == 0 {
		return nil
	}
	if len(events) <= max {
		out := append([]control.Event{}, events...)
		sortEventsChronological(out)
		return out
	}

	var newest, oldest time.Time
	for i, e := range events {
		if i == 0 || e.CreatedAt.After(newest) {
			newest = e.CreatedAt
		}
		if i == 0 || e.CreatedAt.Before(oldest) {
			oldest = e.CreatedAt
		}
	}
	span := newest.Sub(oldest)

	type scored struct {
		e   control.Event
		s   float64
		idx int
	}
	arr := make([]scored, len(events))
	for i, e := range events {
		rec := 1.0
		if span > 0 {
			rec = float64(e.CreatedAt.Sub(oldest)) / float64(span)
		}
		arr[i] = scored{e: e, s: eventTypeWeight(e.Type) + 0.3*rec, idx: i}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].s != arr[j].s {
			return arr[i].s > arr[j].s
		}
		return arr[i].idx < arr[j].idx // tie: earlier in the newest-first list = more recent
	})

	top := make([]control.Event, max)
	for i := 0; i < max; i++ {
		top[i] = arr[i].e
	}
	sortEventsChronological(top)
	return top
}

func sortEventsChronological(ev []control.Event) {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].CreatedAt.Before(ev[j].CreatedAt) })
}
