package memory

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Memory governance: confidence scoring (W3b), scope derivation (W3c), and
// scope/confidence/freshness-aware fact selection (W3d). All functions are
// pure so they can be unit-tested in isolation; legacy facts (zero metadata)
// are treated as neutral and are never penalized to zero, so existing data
// keeps showing up.

// Fact sources, ordered by inherent trust.
const (
	SourceUser          = "user"           // user stated it directly
	SourceAgent         = "agent"          // the agent deliberately chose to save it mid-run
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
	case SourceAgent:
		return 0.6
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
	return selectFacts(facts, currentScope, "", now, max)
}

// SelectFactsForPrompt preserves the confidence/scope/freshness ordering while
// giving facts related to the current turn a bounded boost. It is deliberately
// lexical and deterministic: memory selection must not add another model call
// to the foreground path. Unicode word runs and CJK bigrams make the same
// selector useful for both English and Chinese prompts.
func SelectFactsForPrompt(facts []Fact, currentScope, query string, now time.Time, max int) []Fact {
	return selectFacts(facts, currentScope, query, now, max)
}

func selectFacts(facts []Fact, currentScope, query string, now time.Time, max int) []Fact {
	if max <= 0 || len(facts) == 0 {
		return facts
	}
	queryTokens := memorySearchTokens(query)
	type scored struct {
		f   Fact
		s   float64
		idx int
	}
	arr := make([]scored, len(facts))
	for i, f := range facts {
		score := ScoreFact(f, currentScope, now)
		if len(queryTokens) > 0 {
			relevance := lexicalMemoryRelevance(queryTokens, f.Content+" "+f.Category)
			score *= 1 + 8*relevance
		}
		arr[i] = scored{f: f, s: score, idx: i}
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

func memorySearchTokens(value string) map[string]struct{} {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	out := make(map[string]struct{})
	var run []rune
	var runASCII bool
	flush := func() {
		if len(run) == 0 {
			return
		}
		token := string(run)
		if runASCII {
			if len(run) >= 2 {
				out[token] = struct{}{}
			}
		} else {
			out[token] = struct{}{}
			if len(run) > 1 {
				for i := 0; i+1 < len(run); i++ {
					out[string(run[i:i+2])] = struct{}{}
				}
			}
		}
		run = run[:0]
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		ascii := r <= unicode.MaxASCII
		if len(run) > 0 && ascii != runASCII {
			flush()
		}
		if len(run) == 0 {
			runASCII = ascii
		}
		run = append(run, r)
	}
	flush()
	return out
}

func lexicalMemoryRelevance(queryTokens map[string]struct{}, value string) float64 {
	factTokens := memorySearchTokens(value)
	if len(queryTokens) == 0 || len(factTokens) == 0 {
		return 0
	}
	matches := 0
	cjkBigrams := 0
	for token := range queryTokens {
		if _, ok := factTokens[token]; ok {
			matches++
			runes := []rune(token)
			if len(runes) == 2 && (runes[0] > unicode.MaxASCII || runes[1] > unicode.MaxASCII) {
				cjkBigrams++
			}
		}
	}
	if matches == 0 {
		return 0
	}
	denominator := math.Sqrt(float64(len(queryTokens) * len(factTokens)))
	if denominator <= 0 {
		return 0
	}
	relevance := float64(matches) / denominator
	if relevance > 1 {
		return 1
	}
	if cjkBoost := 0.15 * float64(cjkBigrams); cjkBoost > relevance {
		if cjkBoost > 1 {
			return 1
		}
		return cjkBoost
	}
	return relevance
}
