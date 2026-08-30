package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"selfmind/internal/tools"
	"selfmind/internal/ui/components"
)

// MsgSkillCompletionLoaded carries a refreshed `$` completion inventory.
type MsgSkillCompletionLoaded struct {
	Candidates []tools.SkillCompletionCandidate
	Err        error
}

// loadSkillCompletion refreshes the completion inventory through the dispatch
// seam. It runs daemon-side because usage recency for a Skill on a read-only
// root lives in the control store, which a client does not have. Discovery walks
// the filesystem, so this is a refresh rather than a per-keystroke call.
func (m *uiModel) loadSkillCompletion() tea.Cmd {
	return func() tea.Msg {
		raw, err := m.dispatch("skill_manage", map[string]interface{}{
			"action": "completion", "_tenant_id": m.tenantID,
		})
		if err != nil {
			return MsgSkillCompletionLoaded{Err: err}
		}
		var candidates []tools.SkillCompletionCandidate
		if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
			return MsgSkillCompletionLoaded{Err: fmt.Errorf("decode Skill completion inventory: %w", err)}
		}
		return MsgSkillCompletionLoaded{Candidates: candidates}
	}
}

// skillCompletionHints matches the typed query against the cached inventory.
//
// Matching goes through the shared metadata ranker rather than a prefix test, so
// completion inherits its ASCII-token and CJK-bigram behaviour and agrees with
// the catalog on relevance. With nothing typed yet the inventory is ordered by
// usage recency, which is where implicit use earns its keep: a Skill this person
// read floats up here without ever touching catalog ranking.
func (m *uiModel) skillCompletionHints(query string) []components.CommandHint {
	ranked := tools.RankSkillCompletionCandidates(m.skillCompletion, query)
	hints := make([]components.CommandHint, 0, len(ranked))
	for _, candidate := range ranked {
		hints = append(hints, components.CommandHint{
			Name:        "$" + candidate.Label,
			Description: skillHintDescription(candidate),
			Insert:      "/" + candidate.Reference,
		})
	}
	return hints
}

func skillHintDescription(candidate tools.SkillCompletionCandidate) string {
	description := strings.TrimSpace(candidate.Description)
	if description == "" {
		description = "(no description)"
	}
	origin := strings.TrimSpace(candidate.Provenance)
	if origin == "" {
		origin = strings.TrimSpace(candidate.Scope)
	}
	if origin == "" {
		return description
	}
	return fmt.Sprintf("[%s] %s", origin, description)
}

// skillCommandMayChangeInventory reports whether a slash command can add, remove,
// or rename a Skill. The popup promises the whole inventory, so a package
// installed mid-session must not stay invisible until restart.
func skillCommandMayChangeInventory(command string) bool {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "/skills", "/reload-skills", "/bundles":
		return true
	}
	return false
}
