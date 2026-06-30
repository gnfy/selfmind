package memory

import (
	"math"
	"sort"
	"time"
)

// Memory governance: confidence scoring (W3b), scope derivation (W3c), and
// scope/confidence/freshness-aware fact selection (W3d). All functions are
// pure so they can be unit-tested in isolation; legacy facts (zero metadata)
// are treated as neutral and are never penalized to zero, so existing data
// keeps showing up.

// Fact sources, ordered by inherent trust.
const (
	SourceUser          = "user"           // user stated it directly
	SourceFactExtractor = "fact_extractor" // distilled at the end of a substantive turn
	SourceTurnExtractor = "turn_extractor" // lightweight per-turn extraction
)

// BaseConfidence is the prior confidence for a freshly written fact, by source.
func BaseConfidence(source string) float64 {
	switch source {
	case SourceUser:
		return 0.9
	case SourceFactExtractor:
		return 0.65
	case SourceTurnExtractor:
		return 0.5
	default:
		return 0.5
	}
}

// RepetitionBoost raises confidence when independent extractions corroborate a
// fact, with diminishing returns and capped below certainty.
func RepetitionBoost(base float64, occurrences int) float64 {
	if occurrences <= 1 {
		return base
	}
	boost := base + 0.1*float64(occurrences-1)
	if boost > 0.98 {
		boost = 0.98
	}
	return boost
}

// DecayHalfLife is the age at which an unreaffirmed fact's effective confidence
// halves during selection.
const DecayHalfLife = 90 * 24 * time.Hour

// EffectiveConfidence applies time decay for selection. Unscored (<=0, legacy)
// facts are treated as neutral (0.5). A floor keeps long-lived facts from
// decaying to nothing.
func EffectiveConfidence(stored float64, age time.Duration) float64 {
	if stored <= 0 {
		stored = 0.5
	}
	if age <= 0 {
		return stored
	}
	eff := stored * math.Pow(0.5, float64(age)/float64(DecayHalfLife))
	if floor := 0.2 * stored; eff < floor {
		eff = floor
	}
	return eff
}

// DeriveFactScope assigns a scope from the fact's target and the active
// workspace. User preferences apply everywhere ("global"); environment/
// convention facts are scoped to the workspace when one is active.
func DeriveFactScope(target, workspaceID string) string {
	if target == "user" {
		return "global"
	}
	if workspaceID != "" {
		return "workspace:" + workspaceID
	}
	return "global"
}

// scopeRelevance weights a fact by how relevant its scope is to the current
// turn. Global/legacy facts always apply; same-workspace facts get a boost;
// other-workspace facts are down-weighted but not excluded.
func scopeRelevance(factScope, currentScope string) float64 {
	if factScope == "" || factScope == "global" {
		return 1.0
	}
	if currentScope != "" && factScope == currentScope {
		return 1.25
	}
	return 0.6
}

// ScoreFact ranks a fact for selection by decayed confidence × scope relevance.
func ScoreFact(f Fact, currentScope string, now time.Time) float64 {
	ref := f.LastVerifiedAt
	if ref.IsZero() {
		ref = f.CreatedAt
	}
	var age time.Duration
	if !ref.IsZero() && now.After(ref) {
		age = now.Sub(ref)
	}
	return EffectiveConfidence(f.Confidence, age) * scopeRelevance(f.Scope, currentScope)
}

// SelectFacts returns the top `max` facts for the current scope, ranked by
// ScoreFact (ties broken toward more recently inserted). max<=0 returns all.
func SelectFacts(facts []Fact, currentScope string, now time.Time, max int) []Fact {
	if max <= 0 || len(facts) == 0 {
		return facts
	}
	type scored struct {
		f   Fact
		s   float64
		idx int
	}
	arr := make([]scored, len(facts))
	for i, f := range facts {
		arr[i] = scored{f: f, s: ScoreFact(f, currentScope, now), idx: i}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].s != arr[j].s {
			return arr[i].s > arr[j].s
		}
		return arr[i].idx > arr[j].idx
	})
	n := len(arr)
	if n > max {
		n = max
	}
	out := make([]Fact, n)
	for i := 0; i < n; i++ {
		out[i] = arr[i].f
	}
	return out
}
