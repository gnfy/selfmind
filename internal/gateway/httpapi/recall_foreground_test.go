package httpapi

import (
	"context"
	"selfmind/internal/modelchange"
	"testing"
	"time"
)

type emptyForegroundRecallSource struct{}

func (emptyForegroundRecallSource) Name() string { return "empty" }
func (emptyForegroundRecallSource) Search(context.Context, RecallQuery) ([]RecallHit, error) {
	return nil, nil
}

func TestOptionalExpansionNeverHoldsCompletedLexicalRecall(t *testing.T) {
	blocked := &scriptedExpander{block: make(chan struct{})}
	defer close(blocked.block)
	e := &RecallEngine{sources: []RecallSource{emptyForegroundRecallSource{}}, expander: blocked, expandTimeout: time.Second}
	done := make(chan RecallStats, 1)
	go func() {
		_, stats := e.Select(context.Background(), "default", "person", "", "inspect remembered project work")
		done <- stats
	}()
	select {
	case stats := <-done:
		if stats.Expanded {
			t.Fatal("late expansion was presented as injected")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("optional provider expansion held the foreground selector")
	}
}

type canceledExpansionAttempt struct{ returned chan struct{} }

func (e canceledExpansionAttempt) ExpandWithError(ctx context.Context, query string) (string, error) {
	<-ctx.Done()
	close(e.returned)
	return query, ctx.Err()
}

func TestFinishedForegroundDoesNotDegradeCanceledOptionalRole(t *testing.T) {
	attempt := canceledExpansionAttempt{returned: make(chan struct{})}
	health := NewSemanticRecallHealth(SemanticRecallHealthOptions{Expander: attempt, Initial: modelchange.RouteReadiness{Ready: true}})
	e := &RecallEngine{sources: []RecallSource{emptyForegroundRecallSource{}}, expander: health, expandTimeout: time.Second}
	slices, stats := e.Select(context.Background(), "default", "person", "", "inspect remembered project work")
	if len(slices) != 0 || stats.Expanded {
		t.Fatal("late output entered the selected context")
	}
	select {
	case <-attempt.returned:
	case <-time.After(time.Second):
		t.Fatal("optional call was not canceled")
	}
	if !health.Ready() {
		t.Fatal("a completed foreground selection marked its canceled role unhealthy")
	}
}
