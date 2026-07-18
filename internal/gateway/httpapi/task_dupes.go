package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/log"
)

// taskDuplicateSimilarity is the deterministic suggestion threshold over
// title+summary signatures. Suggestion-only by design (execution-quality W3):
// similarity may propose, only the user's explicit /task merge may fold —
// the same "similarity never merges" rule memory governance follows.
const taskDuplicateSimilarity = 0.8

// suggestDuplicateTasks scans each person's open labels for near-duplicate
// pairs and records ONE task.duplicate_suggested event on the newer label of
// a new pair. Deterministic and zero model calls; rides the periodic task
// governance sweep. Returns the number of new suggestions recorded.
func (d *Server) suggestDuplicateTasks(ctx context.Context) int {
	if d == nil || d.Control == nil {
		return 0
	}
	persons, err := d.Control.ListPersonIDs(ctx)
	if err != nil {
		return 0
	}
	suggested := 0
	for _, person := range persons {
		if ctx.Err() != nil {
			return suggested
		}
		suggested += d.suggestDuplicatesForPerson(ctx, d.DefaultTenantID, person)
	}
	if suggested > 0 {
		log.Info("gateway: duplicate-task suggestions recorded", "count", suggested)
	}
	return suggested
}

func (d *Server) suggestDuplicatesForPerson(ctx context.Context, tenantID, personID string) int {
	cards, err := d.Control.ListTaskCards(ctx, tenantID, personID, 50)
	if err != nil {
		return 0
	}
	open := make([]control.TaskCard, 0, len(cards))
	for _, card := range cards {
		if openTaskCardStatus(card.Status) {
			open = append(open, card)
		}
	}
	if len(open) < 2 {
		return 0
	}
	existing, err := d.Control.ListDuplicateSuggestions(ctx, tenantID, personID)
	if err != nil {
		existing = map[string]string{}
	}
	sigs := make([]memory.SimilaritySignature, len(open))
	for i, card := range open {
		sigs[i] = memory.BuildSimilaritySignature(strings.TrimSpace(card.Title + " " + card.Summary))
	}
	suggested := 0
	for i := 0; i < len(open); i++ {
		for j := i + 1; j < len(open); j++ {
			// Same-workspace only: identical titles across different
			// workspaces are usually the same chore on different projects,
			// not duplicates.
			if open[i].WorkspaceID != open[j].WorkspaceID {
				continue
			}
			if memory.SignatureSimilarity(sigs[i], sigs[j]) < taskDuplicateSimilarity {
				continue
			}
			// Cards are newest-first: open[i] is the newer of the pair.
			newer, older := open[i], open[j]
			if alreadySuggested(existing, newer.TaskID, older.TaskID) {
				continue
			}
			_, _ = d.Control.AppendEvent(ctx, control.Event{
				TaskID:     newer.TaskID,
				Type:       "task.duplicate_suggested",
				Visibility: "task",
				Payload: mustJSON(map[string]interface{}{
					"duplicate_of":       older.TaskID,
					"duplicate_of_title": truncate(toOneLine(older.Title), 60),
				}),
			})
			existing[newer.TaskID] = older.TaskID
			suggested++
		}
	}
	return suggested
}

// openTaskCardStatus keeps the suggester on genuinely open work: terminal and
// hidden labels can no longer be merged into, so pairing them is noise.
func openTaskCardStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "new", "in_progress", "interrupted", "blocked", "running", "waiting_external", "waiting_finalization", "waiting_user", "":
		return true
	default:
		return false
	}
}

func alreadySuggested(existing map[string]string, a, b string) bool {
	return existing[a] == b || existing[b] == a
}

// dupeSuggestionsForView filters recorded suggestions down to pairs whose
// BOTH sides are still in the rendered open set — merged or archived pairs
// disappear without any cleanup pass.
func dupeSuggestionsForView(suggestions map[string]string, tasks []control.Task) map[string]string {
	if len(suggestions) == 0 {
		return nil
	}
	visible := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		visible[t.ID] = true
	}
	out := map[string]string{}
	for taskID, otherID := range suggestions {
		if visible[taskID] && visible[otherID] {
			out[taskID] = otherID
		}
	}
	return out
}
