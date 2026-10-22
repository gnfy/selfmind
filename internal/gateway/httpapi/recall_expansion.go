package httpapi

import (
	"context"
	"time"
)

type recallExpansionResult struct {
	text, status string
	duration     time.Duration
}

// Expansion can overlap lexical selection but never extend its critical path.
// A late result is discarded, not injected by a second prompt path. The role's
// existing timeout and health policy still bound the optional provider call.
func (e *RecallEngine) startExpansion(ctx context.Context, message string) (<-chan recallExpansionResult, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if e.expander == nil {
		return nil, cancel
	}
	done := make(chan recallExpansionResult, 1)
	go func() {
		started := time.Now()
		text, status := e.expand(ctx, message)
		done <- recallExpansionResult{text: text, status: status, duration: time.Since(started)}
	}()
	return done, cancel
}
