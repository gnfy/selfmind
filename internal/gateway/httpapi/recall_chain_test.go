package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

// chainSessionSearcher returns one fixed hit per requested session id.
type chainSessionSearcher struct{ sessions []memory.FTS5Session }

func (c *chainSessionSearcher) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) {
	return c.sessions, nil
}

// chainCards satisfies the card boundary with nothing to list, and resolves
// resume chains — the shape *control.Store presents in production.
type chainCards struct{ roots map[string]string }

func (c *chainCards) ListTaskCards(ctx context.Context, tenantID, personID string, limit int) ([]control.TaskCard, error) {
	return nil, nil
}

func (c *chainCards) ResumeChainRoot(ctx context.Context, tenantID, runID string) (string, error) {
	if root, ok := c.roots[runID]; ok {
		return root, nil
	}
	return runID, nil
}

// TestRecallGroupsRunSessionsByResumeChain pins the read-time replacement for
// what Task used to provide. Sessions are keyed per run, so one continued line
// of work has several sessions; they must compete as ONE line, and the turn's
// own line — the run plus everything it resumes — must not be recalled back
// into itself.
func TestRecallGroupsRunSessionsByResumeChain(t *testing.T) {
	ctx := context.Background()
	const term = "aurora gate release checklist"
	sessions := &chainSessionSearcher{sessions: []memory.FTS5Session{
		{SessionID: "run:run_root", Summary: "first attempt at " + term, Timestamp: 10},
		{SessionID: "run:run_second", Summary: "continued " + term, Timestamp: 20},
		{SessionID: "run:run_unrelated", Summary: "unrelated " + term, Timestamp: 30},
	}}
	cards := &chainCards{roots: map[string]string{
		"run_root":   "run_root",
		"run_second": "run_root",
		"run_third":  "run_root",
	}}
	engine := NewRecallEngine(cards, sessions, nil)

	slices, _ := engine.SelectForWorkspace(ctx, "default", "person_a", "", "", "", term)
	refs := map[string]bool{}
	for _, slice := range slices {
		refs[slice.Ref] = true
	}
	if refs["run:run_root"] && refs["run:run_second"] {
		t.Fatalf("two runs of one chain competed as separate lines: %+v", slices)
	}
	if !refs["run:run_unrelated"] {
		t.Fatalf("an unrelated run must remain its own line: %+v", slices)
	}

	// The current turn's own line is excluded through the chain, not just by
	// its own run id: run_third resumes run_root, so neither may come back.
	slices, _ = engine.SelectForWorkspace(ctx, "default", "person_a", "", "", "run_third", term)
	for _, slice := range slices {
		if strings.HasPrefix(slice.Ref, "run:run_root") || strings.HasPrefix(slice.Ref, "run:run_second") {
			t.Fatalf("the turn's own chain was recalled into itself: %+v", slices)
		}
	}
	found := false
	for _, slice := range slices {
		if slice.Ref == "run:run_unrelated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("excluding the own chain must not drop unrelated work: %+v", slices)
	}
}
