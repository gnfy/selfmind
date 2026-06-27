package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// ProfileSynthesizer distills scattered user/environment facts into one concise,
// coherent "user profile" summary — a stable picture of who the user is (their
// stack, preferences, work style) rather than a growing list of raw facts. The
// profile is the primary "understands you" signal injected into the prompt, and
// being a short summary it is also more token-frugal than dumping every fact.
//
// Synthesis is gated and infrequent (it costs a model call): it only runs once
// enough facts have accumulated, and re-runs only after a meaningful number of
// new facts. It uses the memory_extract role provider (which falls back to the
// default model when no cheaper model is configured).
type ProfileSynthesizer struct {
	provider llm.Provider
	enabled  bool
	minFacts int // don't synthesize below this many total facts
	growth   int // re-synthesize after this many new facts since last profile
}

func NewProfileSynthesizer(provider llm.Provider, enabled bool) *ProfileSynthesizer {
	return &ProfileSynthesizer{
		provider: provider,
		enabled:  enabled && provider != nil,
		minFacts: 10,
		growth:   6,
	}
}

const profileTarget = "profile"

// pinnedTarget holds user-authored authoritative facts ("ground truth"). The
// synthesizer treats them as non-negotiable: it must reflect them and must not
// contradict or overwrite them, so a wrong inference can be corrected by pinning
// the truth. This is the human veto in the "learns about you" loop.
const pinnedTarget = "pinned"

type profilePayload struct {
	Summary   string `json:"summary"`
	FactCount int    `json:"fact_count"`
}

// MaybeSynthesize regenerates the profile when enough new facts have piled up.
// Best-effort: errors are swallowed. Run it in a goroutine — it makes a model
// call when due.
func (p *ProfileSynthesizer) MaybeSynthesize(ctx context.Context, tenantID string, mem *memory.MemoryManager) {
	if p == nil || !p.enabled || mem == nil {
		return
	}
	userFacts, _ := mem.GetFacts(ctx, tenantID, "user")
	memFacts, _ := mem.GetFacts(ctx, tenantID, "memory")
	total := len(userFacts) + len(memFacts)
	if total < p.minFacts {
		return
	}
	existing, _ := mem.GetFacts(ctx, tenantID, profileTarget)
	lastCount := 0
	for _, f := range existing {
		var pp profilePayload
		if json.Unmarshal([]byte(f.Content), &pp) == nil && pp.FactCount > lastCount {
			lastCount = pp.FactCount
		}
	}
	if len(existing) > 0 && total-lastCount < p.growth {
		return // not enough new facts since the last profile
	}
	pinnedFacts, _ := mem.GetFacts(ctx, tenantID, pinnedTarget)
	summary, err := p.synthesize(ctx, userFacts, memFacts, pinnedFacts)
	if err != nil || strings.TrimSpace(summary) == "" {
		return
	}
	payload, err := json.Marshal(profilePayload{Summary: strings.TrimSpace(summary), FactCount: total})
	if err != nil {
		return
	}
	// Replace any prior profile (single current profile per tenant).
	for _, f := range existing {
		_ = mem.RemoveFact(ctx, tenantID, f.ID)
	}
	_ = mem.AddFact(ctx, tenantID, profileTarget, string(payload))
}

func (p *ProfileSynthesizer) synthesize(ctx context.Context, userFacts, memFacts, pinnedFacts []memory.Fact) (string, error) {
	var b strings.Builder
	b.WriteString("Below are facts collected about a user across past sessions.\n\n")
	if len(pinnedFacts) > 0 {
		b.WriteString("AUTHORITATIVE facts the user confirmed — treat as ground truth, reflect them, and never contradict them:\n")
		for _, f := range pinnedFacts {
			fmt.Fprintf(&b, "- %s\n", f.Content)
		}
		b.WriteString("\n")
	}
	if len(userFacts) > 0 {
		b.WriteString("User preferences/details:\n")
		for _, f := range userFacts {
			fmt.Fprintf(&b, "- %s\n", f.Content)
		}
	}
	if len(memFacts) > 0 {
		b.WriteString("\nEnvironment/convention facts:\n")
		for _, f := range memFacts {
			fmt.Fprintf(&b, "- %s\n", f.Content)
		}
	}
	b.WriteString("\nWrite a concise user profile in 3-5 sentences: who they are, their stack/tools, " +
		"preferences and work style, and any notable recent shift. Be specific but brief. " +
		"Output only the profile prose, no headers or lists.")

	msgs := []llm.Message{
		{Role: "system", Content: "You distill user facts into a short, stable profile. Output only the profile prose."},
		{Role: "user", Content: b.String()},
	}
	return p.provider.ChatCompletion(ctx, msgs)
}

// ProfileSummary exposes the current synthesized profile summary (or "") so UI
// surfaces like /memory can show the user what SelfMind has inferred about them.
func (a *Agent) ProfileSummary(ctx context.Context, tenantID string) string {
	return a.latestProfileSummary(ctx, tenantID)
}

// latestProfileSummary returns the current synthesized profile summary, or "".
func (a *Agent) latestProfileSummary(ctx context.Context, tenantID string) string {
	if a == nil || a.memory == nil {
		return ""
	}
	facts, err := a.memory.GetFacts(ctx, tenantID, profileTarget)
	if err != nil || len(facts) == 0 {
		return ""
	}
	best := ""
	bestCount := -1
	for _, f := range facts {
		var pp profilePayload
		if json.Unmarshal([]byte(f.Content), &pp) == nil {
			if pp.FactCount >= bestCount && strings.TrimSpace(pp.Summary) != "" {
				best = strings.TrimSpace(pp.Summary)
				bestCount = pp.FactCount
			}
		}
	}
	return best
}
