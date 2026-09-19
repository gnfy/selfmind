package tools

import "time"

// StandingGrantPolicy bounds which remembered classes ONE run may consume.
//
// Authorization comes from the moment a person decides, not from the moment
// work executes. An interactive run and a 23:00 scheduled job are both
// authorised — the first as it happens, the second when the person created the
// schedule — so the question is never "is anyone watching" but "which decisions
// was this work authorised by".
//
// That distinction is why a scheduled job may USE a standing class and may
// never CREATE one: only a person answering an ask creates one, and automatic
// triage is already confined to a single run (see the run-scope grant recorded
// on an auto-approval).
type StandingGrantPolicy struct {
	// Allowed reports whether this run consumes remembered classes at all.
	// The zero value refuses them, so work that arrives without an explicit
	// policy behaves exactly as it did before standing classes existed.
	Allowed bool
	// NotAfter freezes the set to classes granted at or before this instant.
	// Zero means no bound. A schedule carries the instant it was authorised, so
	// a rule accepted months later for unrelated interactive work never widens
	// a job that has been running untouched.
	NotAfter time.Time
}

// InteractiveStandingGrants is the policy for a run a person is present for.
func InteractiveStandingGrants() StandingGrantPolicy {
	return StandingGrantPolicy{Allowed: true}
}

// ScheduledStandingGrants freezes a scheduled job to the classes that existed
// when it was authorised. A zero authorisation instant consumes nothing.
func ScheduledStandingGrants(authorizedAt time.Time) StandingGrantPolicy {
	if authorizedAt.IsZero() {
		return StandingGrantPolicy{}
	}
	return StandingGrantPolicy{Allowed: true, NotAfter: authorizedAt}
}
