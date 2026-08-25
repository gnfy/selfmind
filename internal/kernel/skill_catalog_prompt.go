package kernel

import (
	"fmt"
	"strings"

	"selfmind/internal/platform/textutil"
)

const SkillCatalogCandidateLimit = 256

// SkillCatalogRenderReport describes the exact bounded discovery surface. It
// intentionally counts metadata presentation only; full Skill bodies remain
// behind skill_view/skill_select.
type SkillCatalogRenderReport struct {
	Total       int
	Included    int
	Full        int
	Shortened   int
	Omitted     int
	Bytes       int
	Budget      int
	Tokens      int
	TokenBudget int
}

// WithinBudget reports the same dual byte/token contract enforced by the
// renderer. A zero token budget is the compatibility form for byte-only
// callers.
func (r SkillCatalogRenderReport) WithinBudget() bool {
	return r.Bytes <= r.Budget && (r.TokenBudget <= 0 || r.Tokens <= r.TokenBudget)
}

// renderSkillCandidateCatalog preserves existence before description detail:
// every candidate first receives its minimum line and a fair short-description
// baseline. Remaining bytes then complete descriptions in ranking order.
// Ranking is represented by input order and is therefore also the deterministic
// omission order when even minimum lines do not fit.
func renderSkillCandidateCatalog(candidates []SkillCandidateContext, maxBytes int) (string, SkillCatalogRenderReport) {
	report := SkillCatalogRenderReport{Total: len(candidates), Budget: maxBytes}
	if len(candidates) == 0 || maxBytes <= 0 {
		return "", report
	}
	header := "## Skill Candidates for Current Work Unit\n" +
		"Metadata only. Select at most one with skill_select(candidate_ref) when it clearly fits; otherwise continue without a skill. skill_view only inspects and does not count as use.\n"
	// Reserve a worst-case status line up front so included/shortened/omitted is
	// never itself lost to the budget it explains.
	statusReserve := len(fmt.Sprintf("\n[Skill catalog: included %d/%d, full descriptions %d, shortened %d, omitted %d.]\n",
		len(candidates), len(candidates), len(candidates), len(candidates), len(candidates)))
	available := maxBytes - len(header) - statusReserve
	if available <= 0 {
		raw := textutil.TruncateBytes(header, maxBytes)
		report.Bytes = len(raw)
		report.Omitted = len(candidates)
		return raw, report
	}

	type candidateLine struct {
		base        string
		description string
		rendered    string
	}
	lines := make([]candidateLine, 0, len(candidates))
	nameCounts := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		nameCounts[strings.ToLower(strings.TrimSpace(candidate.Name))]++
	}
	used := 0
	for _, candidate := range candidates {
		if len(lines) >= SkillCatalogCandidateLimit {
			break
		}
		name := trimLine(candidate.Name, 180)
		candidateRef := trimLine(candidate.CandidateRef, 40)
		base := "- " + name
		if candidateRef != "" {
			base = fmt.Sprintf("- %s %s", candidateRef, name)
		}
		// Scope and source are model-visible only when they disambiguate an
		// otherwise identical name. Stable keys, paths, hashes, and lifecycle
		// state remain on the control/diagnostic surface.
		if nameCounts[strings.ToLower(strings.TrimSpace(candidate.Name))] > 1 {
			scope := trimLine(candidate.Scope, 40)
			source := trimLine(candidate.Source, 60)
			if scope != "" || source != "" {
				base += fmt.Sprintf(" [%s/%s]", scope, source)
			}
		}
		cost := len(base) + 1
		if used+cost > available {
			break
		}
		lines = append(lines, candidateLine{
			base:        base,
			description: trimLine(candidate.Description, len(candidate.Description)),
		})
		used += cost
	}

	remaining := available - used
	described := make([]int, 0, len(lines))
	for i := range lines {
		if lines[i].description != "" {
			described = append(described, i)
		}
	}
	fullDescriptionCost := 0
	for _, index := range described {
		fullDescriptionCost += 2 + len(lines[index].description)
	}
	if fullDescriptionCost <= remaining {
		for _, index := range described {
			lines[index].rendered = lines[index].description
		}
		used += fullDescriptionCost
	} else if len(described) > 0 && remaining >= len(described)*3 {
		// Give every described entry the same baseline allocation. When the fair
		// share is generous, cap the baseline at 48 description bytes so the
		// remaining budget can preserve full detail for the highest-ranked rows.
		// Under pressure the cap never applies and allocation remains purely fair.
		share := remaining / len(described)
		if share > 50 { // 2 bytes for ": ", then up to 48 description bytes.
			share = 50
		}
		for _, index := range described {
			bodyBudget := share - 2 // ": "
			lines[index].rendered = textutil.TruncateBytes(lines[index].description, bodyBudget)
			if lines[index].rendered != "" {
				used += 2 + len(lines[index].rendered)
			}
		}
		remaining = available - used
		for _, index := range described {
			if remaining <= 0 || lines[index].rendered == lines[index].description {
				continue
			}
			want := len(lines[index].description) - len(lines[index].rendered)
			if lines[index].rendered == "" {
				if remaining <= 2 {
					continue
				}
				remaining -= 2
				used += 2
			}
			if want > remaining {
				want = remaining
			}
			expanded := textutil.TruncateBytes(lines[index].description, len(lines[index].rendered)+want)
			added := len(expanded) - len(lines[index].rendered)
			lines[index].rendered = expanded
			remaining -= added
			used += added
		}
	}

	var out strings.Builder
	out.WriteString(header)
	for _, line := range lines {
		out.WriteString(line.base)
		if line.rendered != "" {
			out.WriteString(": ")
			out.WriteString(line.rendered)
		}
		out.WriteByte('\n')
		if line.description != "" && line.rendered == line.description {
			report.Full++
		} else if line.description != "" {
			report.Shortened++
		}
	}
	report.Included = len(lines)
	report.Omitted = report.Total - report.Included
	fmt.Fprintf(&out, "\n[Skill catalog: included %d/%d, full descriptions %d, shortened %d, omitted %d.]\n",
		report.Included, report.Total, report.Full, report.Shortened, report.Omitted)
	raw := out.String()
	if len(raw) > maxBytes {
		// The status reservation uses maximum-width counts, so this is only a
		// defensive guard against future formatting changes.
		raw = textutil.TruncateBytes(raw, maxBytes)
	}
	report.Bytes = len(raw)
	return raw, report
}

// RenderSkillCandidateCatalog exposes the one allocator to gateway selection,
// runtime rendering, telemetry, and tests. Callers use Included to persist refs
// only for entries that can actually become model-visible. The returned text is
// also the canonical bounded catalogue for a work unit created by update_plan.
func RenderSkillCandidateCatalog(candidates []SkillCandidateContext, budget RuntimeContextBudget) (string, SkillCatalogRenderReport) {
	return renderSkillCandidateCatalogWithinBudget(candidates, budget.SkillCatalogBytes, budget.SkillCatalogTokens)
}

// PreviewSkillCandidateCatalog reports the canonical allocation without
// introducing a second rendering path.
func PreviewSkillCandidateCatalog(candidates []SkillCandidateContext, budget RuntimeContextBudget) SkillCatalogRenderReport {
	_, report := RenderSkillCandidateCatalog(candidates, budget)
	return report
}

// renderSkillCandidateCatalogWithinBudget applies the exact UTF-8 byte ceiling
// and the shared estimated-token ceiling. It reuses the allocator itself while
// reducing only the byte allowance, so Doctor/runtime rendering cannot drift
// into a second catalogue implementation.
func renderSkillCandidateCatalogWithinBudget(candidates []SkillCandidateContext, maxBytes, maxTokens int) (string, SkillCatalogRenderReport) {
	if maxTokens <= 0 {
		prompt, report := renderSkillCandidateCatalog(candidates, maxBytes)
		report.Tokens = estimateTokens(prompt)
		return prompt, report
	}
	low, high := 0, maxBytes
	var best string
	bestReport := SkillCatalogRenderReport{Total: len(candidates), Budget: maxBytes, TokenBudget: maxTokens, Omitted: len(candidates)}
	for low <= high {
		mid := low + (high-low)/2
		prompt, report := renderSkillCandidateCatalog(candidates, mid)
		tokens := estimateTokens(prompt)
		if tokens <= maxTokens {
			best, bestReport = prompt, report
			bestReport.Tokens = tokens
			bestReport.TokenBudget = maxTokens
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	bestReport.Budget = maxBytes
	bestReport.TokenBudget = maxTokens
	return best, bestReport
}
