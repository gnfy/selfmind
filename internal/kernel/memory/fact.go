package memory

import (
	"time"
)

// Fact represents a durable piece of information about the user or environment.
//
// The governance metadata (W3) is optional and backward-compatible: facts
// written before the migration read back with zero-value metadata, and the
// legacy AddFact path leaves it empty. Confidence scoring, scope-aware
// selection, and memory eval build on these fields.
type Fact struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"` // 'user' (preferences) or 'memory' (environment/conventions)
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`

	// Governance metadata (optional).
	Source         string    `json:"source,omitempty"`           // e.g. "fact_extractor", "turn_extractor", "user"
	Scope          string    `json:"scope,omitempty"`            // e.g. "global", "workspace:<id>", "channel:<name>"
	Category       string    `json:"category,omitempty"`         // broad browsing and retrieval category
	Confidence     float64   `json:"confidence,omitempty"`       // 0..1; 0 = unscored (legacy/unknown)
	CreatedFromRun string    `json:"created_from_run,omitempty"` // run id this fact was extracted from
	LastVerifiedAt time.Time `json:"last_verified_at,omitempty"` // last time the fact was reaffirmed
	Canonical      bool      `json:"-"`                          // served from canonical_memories, not legacy facts
}
