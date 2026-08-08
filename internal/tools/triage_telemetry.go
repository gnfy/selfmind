package tools

import (
	"strings"
	"sync"
	"time"
)

// Smart-mode triage telemetry. It answers ONE question that was previously
// unanswerable from any surface: when a person says "why is it asking me so
// much?", was the funnel working as designed (the judge deliberately escalated)
// or broken (no judge configured, provider unreachable, timeout)? Those two look
// identical at the prompt, and the second one silently turns smart mode into
// on-request.
//
// Ownership and partitioning: entries are keyed by tenant+person, exactly like
// the execution-scope registry in this package (ExecutionScopeDiagnostics), so
// no counter is ever shared across tenants or people. Records are in-memory,
// bounded per person, and pruned to the reporting window — this is a diagnostic
// read model. A gateway may additionally install a narrow durable sink so the
// same recent counters survive daemon restarts; the sink receives no command
// text or arguments.

// triageWindow bounds how far back TriageDiagnostics reports.
const triageWindow = 24 * time.Hour

// triageMaxEntriesPerPerson bounds memory per person. A person generating more
// triage decisions than this inside the window still gets accurate recent
// counts; only the oldest entries fall off.
const triageMaxEntriesPerPerson = 256

// TriageOutcome is what automatic triage did with one dangerous op.
type TriageOutcome string

const (
	// TriageOutcomeApproved: the judge auto-approved, so no human was asked.
	TriageOutcomeApproved TriageOutcome = "approved"
	// TriageOutcomeDenied: the judge blocked the op as a user-style decision.
	TriageOutcomeDenied TriageOutcome = "denied"
	// TriageOutcomeEscalated: the judge deliberately handed the call to a human.
	TriageOutcomeEscalated TriageOutcome = "escalated"
	// TriageOutcomeUnavailable: triage could not rule at all — no judge wired,
	// or the judge errored/timed out — so the human ask is a fail-safe fallback.
	TriageOutcomeUnavailable TriageOutcome = "unavailable"
	// TriageOutcomeContained: the sandbox could contain the call (isolated, no
	// network), so no ask and no judge call happened at all (batch C1). Counted
	// because it answers "how much of the old prompt volume was fatigue?".
	TriageOutcomeContained TriageOutcome = "contained"
	// TriageOutcomeGrantHit is a reusable class/rule grant hit.
	TriageOutcomeGrantHit TriageOutcome = "grant_hit"
	// TriageOutcomeExactRunHit is a byte-identical action explicitly approved
	// for this run. It is separated from broader grants for auditability.
	TriageOutcomeExactRunHit TriageOutcome = "exact_run_hit"
	// TriageOutcomeHumanAsk counts calls that reached a person.
	TriageOutcomeHumanAsk TriageOutcome = "human_ask"
)

type triageEntry struct {
	at      time.Time
	outcome TriageOutcome
	err     string
}

const ApprovalTriagePolicyVersion = "smart-v2"

// TriageAuditEvent is the non-secret decision envelope persisted by the
// gateway. It intentionally excludes command text and arguments; those remain
// in the separately redacted approval request when a human decision is needed.
type TriageAuditEvent struct {
	TenantID      string
	PersonID      string
	TaskID        string
	RunID         string
	ToolName      string
	Outcome       TriageOutcome
	RiskLevel     string
	Authorization string
	GrantKey      string
	ProviderRoute string
	Latency       time.Duration
	ErrorClass    string
	Rationale     string
	PolicyVersion string
	RedactedError string
	At            time.Time
}

var triageRecords = struct {
	mu     sync.Mutex
	byKey  map[string][]triageEntry
	nowFn  func() time.Time
	people int
}{byKey: map[string][]triageEntry{}}

var triageDurableSink = struct {
	mu sync.RWMutex
	fn func(TriageAuditEvent)
}{}

// SetTriageTelemetrySink installs the gateway-owned durable projection. The
// tools package stays independent of control.db; personal/in-process uses may
// leave the sink nil. The returned cleanup restores the previous sink.
func SetTriageTelemetrySink(fn func(TriageAuditEvent)) func() {
	triageDurableSink.mu.Lock()
	previous := triageDurableSink.fn
	triageDurableSink.fn = fn
	triageDurableSink.mu.Unlock()
	return func() {
		triageDurableSink.mu.Lock()
		triageDurableSink.fn = previous
		triageDurableSink.mu.Unlock()
	}
}

// triageMaxPeople bounds how many partitions are retained. Well past any
// personal-scale install; it only exists so a pathological id churn cannot grow
// the map without bound.
const triageMaxPeople = 64

func triageNow() time.Time {
	if triageRecords.nowFn != nil {
		return triageRecords.nowFn()
	}
	return time.Now()
}

func triageKey(tenantID, personID string) string {
	return strings.TrimSpace(tenantID) + "/" + strings.TrimSpace(personID)
}

// RecordTriageOutcome files one triage decision for later /diag reporting. err
// is the judge failure, when there was one; it is stored redacted and bounded
// because a provider error can echo request material.
func RecordTriageOutcome(tenantID, personID string, outcome TriageOutcome, err error) {
	RecordTriageAuditEvent(TriageAuditEvent{
		TenantID: tenantID, PersonID: personID, Outcome: outcome,
		PolicyVersion: ApprovalTriagePolicyVersion,
	}, err)
}

// RecordTriageAuditEvent records a structured, bounded approval-funnel event.
// It preserves the historical aggregate API while giving the durable sink the
// evidence needed to explain a particular run without storing raw commands.
func RecordTriageAuditEvent(event TriageAuditEvent, err error) {
	tenantID := event.TenantID
	personID := event.PersonID
	outcome := event.Outcome
	if strings.TrimSpace(personID) == "" {
		return
	}
	message := ""
	if err != nil {
		message = truncateRunes(RedactSensitive(toSingleLine(err.Error())), 160)
	}
	key := triageKey(tenantID, personID)
	now := event.At
	if now.IsZero() {
		now = triageNow()
	}
	event.At = now
	event.RedactedError = message
	event.Rationale = truncateRunes(toSingleLine(RedactSensitive(event.Rationale)), 240)
	if event.PolicyVersion == "" {
		event.PolicyVersion = ApprovalTriagePolicyVersion
	}

	triageRecords.mu.Lock()
	if _, known := triageRecords.byKey[key]; !known && len(triageRecords.byKey) >= triageMaxPeople {
		pruneEmptyTriagePartitionsLocked(now)
		if len(triageRecords.byKey) < triageMaxPeople {
			triageRecords.byKey[key] = nil
		}
	}
	if _, known := triageRecords.byKey[key]; known || len(triageRecords.byKey) < triageMaxPeople {
		entries := append(triageRecords.byKey[key], triageEntry{at: now, outcome: outcome, err: message})
		if len(entries) > triageMaxEntriesPerPerson {
			entries = entries[len(entries)-triageMaxEntriesPerPerson:]
		}
		triageRecords.byKey[key] = entries
	}
	triageRecords.mu.Unlock()

	triageDurableSink.mu.RLock()
	sink := triageDurableSink.fn
	triageDurableSink.mu.RUnlock()
	if sink != nil {
		sink(event)
	}
}

// TriageStats is the bounded 24h view of automatic triage for one person.
type TriageStats struct {
	Approved    int
	Denied      int
	Escalated   int
	Unavailable int
	// Contained counts exec calls the sandbox could contain, which skip both the
	// ask and the judge.
	Contained    int
	GrantHits    int
	ExactRunHits int
	HumanAsks    int
	// LastError is the most recent judge failure in the window, already
	// redacted and one-lined. Empty when triage never failed.
	LastError   string
	LastErrorAt time.Time
}

// Total returns how many gated ops the funnel resolved in the window, including
// the ones containment settled before triage.
func (s TriageStats) Total() int {
	return s.Approved + s.Denied + s.Escalated + s.Unavailable + s.Contained + s.GrantHits + s.ExactRunHits + s.HumanAsks
}

// Judged returns how many ops actually reached the judge, which is the
// denominator for "is triage working" (containment never calls it).
func (s TriageStats) Judged() int {
	return s.Approved + s.Denied + s.Escalated + s.Unavailable
}

// TriageDiagnostics returns the person's triage counts for the reporting window.
// A zero value means triage never ran — which for a person in smart mode is
// itself the answer (nothing is being auto-approved).
func TriageDiagnostics(tenantID, personID string) TriageStats {
	key := triageKey(tenantID, personID)
	cutoff := triageNow().Add(-triageWindow)

	triageRecords.mu.Lock()
	defer triageRecords.mu.Unlock()
	entries := triageRecords.byKey[key]
	var stats TriageStats
	for _, entry := range entries {
		if entry.at.Before(cutoff) {
			continue
		}
		switch entry.outcome {
		case TriageOutcomeApproved:
			stats.Approved++
		case TriageOutcomeDenied:
			stats.Denied++
		case TriageOutcomeEscalated:
			stats.Escalated++
		case TriageOutcomeUnavailable:
			stats.Unavailable++
		case TriageOutcomeContained:
			stats.Contained++
		case TriageOutcomeGrantHit:
			stats.GrantHits++
		case TriageOutcomeExactRunHit:
			stats.ExactRunHits++
		case TriageOutcomeHumanAsk:
			stats.HumanAsks++
		}
		if entry.err != "" && !entry.at.Before(stats.LastErrorAt) {
			stats.LastError = entry.err
			stats.LastErrorAt = entry.at
		}
	}
	return stats
}

// pruneEmptyTriagePartitionsLocked drops partitions whose entries all aged out.
func pruneEmptyTriagePartitionsLocked(now time.Time) {
	cutoff := now.Add(-triageWindow)
	for key, entries := range triageRecords.byKey {
		kept := entries[:0]
		for _, entry := range entries {
			if !entry.at.Before(cutoff) {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(triageRecords.byKey, key)
			continue
		}
		triageRecords.byKey[key] = kept
	}
}

func toSingleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}
