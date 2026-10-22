package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// cancellationQuoteSourceMaxBytes bounds the identity of the input a quote
// matched; every recognized identity is a short runtime id.
const cancellationQuoteSourceMaxBytes = 256

// cancellationQuoteSourceKinds name the inputs a quote can match: a consumed
// steering message, a Run's input, and a user-originated run.started event.
var cancellationQuoteSourceKinds = []string{"steering:", "run_input:", "run_started:"}

// The kinds of input a cancellation quote source names, without the colon.
const (
	CancellationQuoteFromSteering = "steering"
	CancellationQuoteFromRunInput = "run_input"
	CancellationQuoteFromRunStart = "run_started"
)

// CancellationQuoteOrigin is the input a recorded quote source names: its
// kind, the Run that received it, and when it arrived.
type CancellationQuoteOrigin struct {
	Kind       string
	RunID      string
	ReceivedAt time.Time
}

// CancellationQuoteOrigins resolves the recorded quote sources of steps,
// keyed by source, so a display can say which input authorized a cancellation
// without an id or any of that input's text. A Run's request arrived when the
// Run started; guidance arrived when the mailbox accepted it. A source whose
// input is no longer stored resolves to nothing, so no display guesses one.
func (s *Store) CancellationQuoteOrigins(ctx context.Context, tenantID string, steps []RunPlanStep) (map[string]CancellationQuoteOrigin, error) {
	origins := map[string]CancellationQuoteOrigin{}
	if s == nil || s.db == nil {
		return origins, nil
	}
	tenant := normalizeTenant(tenantID)
	for _, step := range steps {
		source := step.CancellationQuoteSource
		if _, seen := origins[source]; seen || source == "" {
			continue
		}
		kind, id, _ := strings.Cut(source, ":")
		var query string
		switch kind {
		case CancellationQuoteFromSteering:
			query = `SELECT run_id, created_at FROM steering_mailbox WHERE tenant_id=? AND id=?`
		case CancellationQuoteFromRunInput, CancellationQuoteFromRunStart:
			query = `SELECT id, started_at FROM runs WHERE tenant_id=? AND id=?`
		default:
			continue
		}
		origin := CancellationQuoteOrigin{Kind: kind}
		var receivedAt int64
		switch err := s.db.QueryRowContext(ctx, query, tenant, id).Scan(&origin.RunID, &receivedAt); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, err
		}
		origin.ReceivedAt = time.Unix(receivedAt, 0)
		origins[source] = origin
	}
	return origins, nil
}

// keepCancellationQuoteSources settles the quote source each step of a
// snapshot records. On entry a step carries the identity of the input its quote
// matched when this snapshot validated that quote, and nothing otherwise. A
// step whose validated quote is the one its previous record holds keeps that
// record's source, even an empty one from before sources were recorded, so
// restating a quote never re-points it at another input. A new or changed
// quote records the input it matched. A step that is not cancelled, or whose
// quote this snapshot did not validate, records none.
func keepCancellationQuoteSources(steps []RunPlanStep, previous *RunPlan) error {
	recorded := map[string]RunPlanStep{}
	if previous != nil {
		for _, step := range previous.Steps {
			recorded[step.StepID] = step
		}
	}
	for i := range steps {
		source, err := normalizeCancellationQuoteSource(steps[i].CancellationQuoteSource)
		if err != nil {
			return fmt.Errorf("plan step %s: %w", steps[i].StepID, err)
		}
		quote, quoted := validatedCancellationQuote(steps[i])
		if !quoted || source == "" {
			steps[i].CancellationQuoteSource = ""
			continue
		}
		if old, ok := recorded[steps[i].StepID]; ok {
			if oldQuote, oldQuoted := validatedCancellationQuote(old); oldQuoted && oldQuote == quote {
				if source, err = normalizeCancellationQuoteSource(old.CancellationQuoteSource); err != nil {
					return fmt.Errorf("plan step %s: %w", steps[i].StepID, err)
				}
			}
		}
		steps[i].CancellationQuoteSource = source
	}
	return nil
}

// cancellationQuote is the quote a cancelled step's disposition rests on and
// the field that holds it.
type cancellationQuote struct{ field, text string }

// validatedCancellationQuote is the quote the runtime validates for a
// cancelled step: a takeover's user_takeover_quote, or the scope_change_quote
// of a not_required cancellation.
func validatedCancellationQuote(step RunPlanStep) (cancellationQuote, bool) {
	if step.Status != "cancelled" {
		return cancellationQuote{}, false
	}
	switch step.CancellationDisposition {
	case "user_takeover":
		text := strings.TrimSpace(step.UserTakeoverQuote)
		return cancellationQuote{"user_takeover_quote", text}, text != ""
	case "not_required":
		text := strings.TrimSpace(step.ScopeChangeQuote)
		return cancellationQuote{"scope_change_quote", text}, text != ""
	}
	return cancellationQuote{}, false
}

func normalizeCancellationQuoteSource(raw string) (string, error) {
	source := strings.TrimSpace(raw)
	if source == "" {
		return "", nil
	}
	if len(source) <= cancellationQuoteSourceMaxBytes && utf8.ValidString(source) {
		for _, kind := range cancellationQuoteSourceKinds {
			id, ok := strings.CutPrefix(source, kind)
			if ok && id != "" && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0 {
				return source, nil
			}
		}
	}
	return "", fmt.Errorf("cancellation quote source must be steering:<id>, run_input:<id>, or run_started:<id> within %d bytes", cancellationQuoteSourceMaxBytes)
}
