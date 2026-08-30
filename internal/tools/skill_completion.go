package tools

import (
	"sort"
	"strings"
	"time"
)

// SkillCompletionCandidate is one entry of the local `$` completion inventory.
// Completion is UI-side: it spends no model context, so it keeps every
// discovered Skill, including one whose author kept it out of the model catalog.
type SkillCompletionCandidate struct {
	Name        string `json:"name"`
	Qualified   string `json:"qualified"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Provenance  string `json:"provenance"`
	Scope       string `json:"scope"`
	Reference   string `json:"reference"`
	LastUsed    string `json:"last_used,omitempty"`
}

// BuildSkillCompletionCandidates projects discovered Skills into completion
// entries. Label is the bare name unless it collides, in which case the row
// renders qualified so the entry the person picks is the package they meant.
func BuildSkillCompletionCandidates(skills []SkillInfo) []SkillCompletionCandidate {
	shortNameCounts := map[string]int{}
	qualifiedCounts := map[string]int{}
	for _, info := range skills {
		shortNameCounts[normalizeSkillCommandName(info.Name)]++
		qualifiedCounts[QualifiedSkillName(info)]++
	}
	out := make([]SkillCompletionCandidate, 0, len(skills))
	for _, info := range skills {
		qualified := QualifiedSkillName(info)
		label := info.Name
		if shortNameCounts[normalizeSkillCommandName(info.Name)] > 1 {
			label = qualified
		}
		out = append(out, SkillCompletionCandidate{
			Name: info.Name, Qualified: qualified, Label: label,
			Description: strings.TrimSpace(info.Description),
			Provenance:  info.Provenance, Scope: info.Scope,
			Reference: SkillCompletionReference(info, qualified, qualifiedCounts),
			LastUsed:  info.LastUsed,
		})
	}
	return out
}

// SkillCompletionReference is what completion writes after the leading slash.
//
// A qualified name is preferred: it is readable and, being built from sanitized
// parts, can never contain whitespace. Two roots can share both scope and
// source, so when the qualified form is not unique the discovery path is used
// instead — resolution accepts a path, and a path is always unique. A slash
// command is tokenized on whitespace, so a path containing whitespace cannot be
// carried this way; that case falls back to the qualified name and the person
// gets the ambiguity refusal, which lists the paths, rather than a silently
// wrong resolution.
func SkillCompletionReference(info SkillInfo, qualified string, qualifiedCounts map[string]int) string {
	if qualifiedCounts[qualified] == 1 {
		return qualified
	}
	if strings.ContainsAny(info.Path, " \t") {
		return qualified
	}
	return info.Path
}

// RankSkillCompletionCandidates orders the popup.
//
// With a query it reuses the metadata-only ranker the catalog uses, so the two
// agree on relevance and completion gains its ASCII-token and CJK-bigram
// matching instead of a second, weaker prefix test. With nothing to rank on it
// orders by usage recency, which is where implicit use earns its place: a Skill
// this person actually read floats up without ever influencing catalog ranking,
// which stays metadata-only and reproducible.
func RankSkillCompletionCandidates(candidates []SkillCompletionCandidate, query string) []SkillCompletionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	if strings.TrimSpace(query) == "" {
		return sortSkillCandidatesByRecency(candidates)
	}
	byName := make(map[string]SkillCompletionCandidate, len(candidates))
	metadata := make([]SkillInfo, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.Qualified
		byName[key] = candidate
		metadata = append(metadata, SkillInfo{
			Name: key, Description: candidate.Description,
		})
	}
	ranked := rankSkillsBM25F(query, metadata, len(metadata))
	out := make([]SkillCompletionCandidate, 0, len(ranked))
	for _, info := range ranked {
		if candidate, ok := byName[info.Name]; ok {
			out = append(out, candidate)
		}
	}
	return out
}

func sortSkillCandidatesByRecency(candidates []SkillCompletionCandidate) []SkillCompletionCandidate {
	out := append([]SkillCompletionCandidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		left, leftOK := parseSkillCompletionTime(out[i].LastUsed)
		right, rightOK := parseSkillCompletionTime(out[j].LastUsed)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !left.Equal(right) {
			return left.After(right)
		}
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	return out
}

func parseSkillCompletionTime(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
