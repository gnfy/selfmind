package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// Transient-content classification (two tiers). A single broad regex cannot
// separate a transient FACT ("RUQX-224 当前状态为 QUEUED") from a durable RULE
// that merely mentions status vocabulary ("QUEUED means the task is waiting
// for dispatch"), so destructive decisions key off a CONFIRMED verdict that
// requires all three signals — a concrete instance, current-state semantics,
// and a status token — while candidates are only ever flagged for review.
// Shared by the intake durability gate (internal/app) and the offline memory
// audit (selfmind maintenance memory-audit).
type TransientVerdict int

const (
	// TransientNone: no run-state vocabulary at all.
	TransientNone TransientVerdict = iota
	// TransientCandidate: status vocabulary without a confirmed instance
	// context, or with explanatory rule semantics. Never auto-dropped or
	// auto-archived — worst case it is stored time-bounded and reviewed.
	TransientCandidate
	// TransientConfirmed: a concrete instance (ticket/build/run id) described
	// in current-state terms. Safe for automatic run-state handling.
	TransientConfirmed
)

var (
	transientStatusTokens = regexp.MustCompile(
		`(?i)\b(IN_PROGRESS|QUEUED|PRE_BUILD|PREPARED_NOT_EXECUTED)\b|尚未执行|待执行|正在等待|正在执行|当前状态`)
	// Explanatory semantics mark a probable long-term rule; they veto the
	// confirmed tier no matter what else matches.
	transientRuleCues = regexp.MustCompile(
		`表示|意味着|说明|规则|流程|转为|(?i)\bmeans\b|(?i)\bindicates\b|(?i)\brule\b|(?i)\btransition`)
	// A concrete work instance: ticket key, run id, build id, or a UUID-ish
	// identifier.
	transientInstanceCues = regexp.MustCompile(
		`\b[A-Z][A-Z0-9]{1,9}-\d{1,6}\b|\brun_[A-Za-z0-9-]{6,}|(?i)\bbuild id\b|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}`)
	// Current-state semantics: this specific moment/run, not a general claim.
	transientTemporalCues = regexp.MustCompile(
		`当前|目前|现在|刚刚|本次|已按|状态标记为|状态[:：]?\s*(?:为|是)|(?i)\bcurrently\b|(?i)\bthis run\b|(?i)\bright now\b`)
)

// ClassifyTransientContent grades content for run-state transience.
func ClassifyTransientContent(content string) TransientVerdict {
	if !transientStatusTokens.MatchString(content) {
		return TransientNone
	}
	if transientRuleCues.MatchString(content) {
		return TransientCandidate
	}
	if transientInstanceCues.MatchString(content) && transientTemporalCues.MatchString(content) {
		return TransientConfirmed
	}
	return TransientCandidate
}

// Layered memory model (docs/memory-governance.zh-CN.md §2):
//
//   memory_observations  — immutable evidence. One row per extraction event;
//                          content is never rewritten after insert.
//   canonical_memories   — the current revisable understanding (read model).
//   memory_evidence      — relations between the two (supports/contradicts/
//                          supersedes).
//   memory_events        — the audit trail for every governance mutation,
//                          carrying prior-state snapshots for undo.
//
// The legacy `facts` table stays untouched during the transition; opening a
// tenant database incrementally imports its rows as legacy observations (see
// importLegacyFacts). Repetition across runs is represented as MULTIPLE
// observations supporting ONE canonical memory — that is the substrate for
// REINFORCE — while retries of the same run are deduplicated upstream by the
// maintenance-job layer, not here.

// Observation statuses.
const (
	ObservationCandidate = "candidate"
	ObservationAccepted  = "accepted"
	ObservationRetracted = "retracted"
	ObservationForgotten = "forgotten"
)

// Canonical memory statuses.
const (
	CanonicalActive     = "active"
	CanonicalConflicted = "conflicted"
	CanonicalSuperseded = "superseded"
	CanonicalArchived   = "archived"
	CanonicalForgotten  = "forgotten"
)

// Evidence relations.
const (
	RelationSupports    = "supports"
	RelationContradicts = "contradicts"
	RelationSupersedes  = "supersedes"
)

// Observation is one immutable piece of extracted evidence.
type Observation struct {
	ID              string    `json:"id"`
	RunID           string    `json:"run_id,omitempty"`
	AnalyzerVersion int       `json:"analyzer_version,omitempty"`
	WorkspaceID     string    `json:"workspace_id,omitempty"`
	Target          string    `json:"target"`
	Scope           string    `json:"scope,omitempty"`
	Source          string    `json:"source,omitempty"`
	Content         string    `json:"content"`
	NormalizedHash  string    `json:"normalized_hash"`
	ConfidencePrior float64   `json:"confidence_prior,omitempty"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// CanonicalMemory is one current belief, backed by observations.
type CanonicalMemory struct {
	ID             string    `json:"id"`
	Target         string    `json:"target"`
	Scope          string    `json:"scope,omitempty"`
	Category       string    `json:"category,omitempty"`
	Content        string    `json:"content"`
	NormalizedHash string    `json:"normalized_hash"`
	Status         string    `json:"status"`
	Pinned         bool      `json:"pinned,omitempty"`
	UserConfirmed  bool      `json:"user_confirmed,omitempty"`
	Confidence     float64   `json:"confidence,omitempty"`
	EvidenceCount  int       `json:"evidence_count,omitempty"`
	Occurrences    int       `json:"occurrences,omitempty"`
	LastVerifiedAt time.Time `json:"last_verified_at,omitempty"`
	LastAccessedAt time.Time `json:"last_accessed_at,omitempty"`
	ValidFrom      time.Time `json:"valid_from,omitempty"`
	ValidUntil     time.Time `json:"valid_until,omitempty"`
	SupersededBy   string    `json:"superseded_by,omitempty"`
	Revision       int       `json:"revision,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// MemoryEvent is one audit record of a governance mutation.
type MemoryEvent struct {
	ID            string    `json:"id"`
	Actor         string    `json:"actor"`  // intake | consolidator | user | import
	Action        string    `json:"action"` // create|reinforce|supersede|conflict|correct|archive|forget|restore|undo|import
	MemoryID      string    `json:"memory_id,omitempty"`
	ObservationID string    `json:"observation_id,omitempty"`
	Confidence    float64   `json:"confidence,omitempty"`
	Snapshot      string    `json:"snapshot,omitempty"` // JSON of the prior state, for undo
	Detail        string    `json:"detail,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// DedupUndoSnapshot captures the exact evidence and counters changed by the
// one-time legacy-import deduplicator. Observations are immutable evidence, so
// cleanup must remain reversible through the ordinary memory event undo path.
type DedupUndoSnapshot struct {
	Canonical    DedupCanonicalSnapshot     `json:"canonical"`
	Observations []DedupObservationSnapshot `json:"observations"`
	Evidence     []DedupEvidenceSnapshot    `json:"evidence"`
}

type DedupCanonicalSnapshot struct {
	ID            string  `json:"id"`
	Confidence    float64 `json:"confidence"`
	EvidenceCount int     `json:"evidence_count"`
	Occurrences   int     `json:"occurrences"`
	UpdatedAt     int64   `json:"updated_at"`
}

type DedupObservationSnapshot struct {
	ID              string  `json:"id"`
	RunID           string  `json:"run_id"`
	AnalyzerVersion int     `json:"analyzer_version"`
	WorkspaceID     string  `json:"workspace_id"`
	Target          string  `json:"target"`
	Scope           string  `json:"scope"`
	Source          string  `json:"source"`
	Content         string  `json:"content"`
	NormalizedHash  string  `json:"normalized_hash"`
	ConfidencePrior float64 `json:"confidence_prior"`
	Status          string  `json:"status"`
	CreatedAt       int64   `json:"created_at"`
}

type DedupEvidenceSnapshot struct {
	MemoryID      string `json:"memory_id"`
	ObservationID string `json:"observation_id"`
	Relation      string `json:"relation"`
	CreatedAt     int64  `json:"created_at"`
}

// CanonicalFilter bounds canonical-memory reads.
type CanonicalFilter struct {
	Target   string   // optional: user | memory | pinned-era imports keep their target
	Statuses []string // optional; nil means active only
	Limit    int      // optional; <=0 means no explicit limit
}

// IntakeWrite is one intake ruling applied to the layered store: the
// observation is always recorded; the canonical effect depends on Decision.
// RefContent locates the ruled-against canonical via NormalizedContentHash —
// the intake policy layer refs legacy facts, so the mapping is by statement
// identity, not by id.
type IntakeWrite struct {
	Decision        string // ADD | REINFORCE | SUPERSEDE | CONFLICT
	Target          string
	Scope           string
	Source          string
	Content         string // the statement observed this run
	RefContent      string // existing statement being ruled against (empty for ADD)
	RunID           string
	WorkspaceID     string
	Confidence      float64
	AnalyzerVersion int
	DecisionKey     string // stable per frozen proposal item; used for replay idempotency
	// Category is the analyzer-declared fact category (optional).
	Category string
	// ValidUntil marks a time-bounded fact's expiry. Zero means no expiry
	// (durable). Episodic observations never reach ApplyIntakeWrite — the
	// intake policy layer drops them before the canonical write.
	ValidUntil time.Time
}

// MergeWrite folds a judged cluster into one canonical memory. Members are
// archived (never deleted); their evidence re-points to the new canonical.
type MergeWrite struct {
	MemberIDs  []string // canonical ids being folded
	Canonical  string   // judge-provided canonical text
	Target     string
	Scope      string
	Confidence float64
	ClusterID  string
	Actor      string // consolidator
}

// CanonicalStore is the layered-memory surface. It is an OPTIONAL capability
// of a StorageProvider (SQLite implements it; test fakes usually do not), so
// callers obtain it via MemoryManager.Canonical() and must tolerate absence.
type CanonicalStore interface {
	ListCanonicalMemories(ctx context.Context, tenantID string, filter CanonicalFilter) ([]CanonicalMemory, error)
	ObservationsForMemory(ctx context.Context, tenantID, memoryID string) ([]Observation, error)
	ListMemoryEvents(ctx context.Context, tenantID string, limit int) ([]MemoryEvent, error)
	ApplyIntakeWrite(ctx context.Context, tenantID string, w IntakeWrite) error
	ApplyMerge(ctx context.Context, tenantID string, w MergeWrite) error
	// SetCanonicalStatusByHash maps a legacy-fact mutation (forget/correct)
	// onto the canonical layer by statement identity. Empty newContent only
	// flips status; otherwise the canonical is revised in place.
	SetCanonicalStatusByHash(ctx context.Context, tenantID, target, scope, content, status, actor string) error
	// SetCanonicalStatus flips one canonical row by id (the read model hands
	// canonical ids to the management path once the cutover is active).
	SetCanonicalStatus(ctx context.Context, tenantID, id, status, actor string) error
	// SetCanonicalPinned changes the user's unconditional-injection bit by id.
	// Pinning also confirms the row as user authority; unpinning keeps that
	// confirmation while returning the row to ordinary ranked injection.
	SetCanonicalPinned(ctx context.Context, tenantID, id string, pinned bool, actor string) error
	TouchCanonicalAccess(ctx context.Context, tenantID string, ids []string) error
	ArchiveCanonicals(ctx context.Context, tenantID string, ids []string, actor, reason string) error
	ListJudgedClusterIDs(ctx context.Context, tenantID string) (map[string]bool, error)
	RecordConsolidationJudgement(ctx context.Context, tenantID, clusterID, action string, confidence float64, detail string) error
	// UndoMemoryEvent reverses a reversible canonical governance event. Merge
	// and archive events are reversible; evidence-only judgements are not.
	UndoMemoryEvent(ctx context.Context, tenantID, eventID, actor string) error
}

// Canonical exposes the layered store when the underlying provider supports
// it, or (nil, false) when it does not (e.g. lightweight test fakes).
func (m *MemoryManager) Canonical() (CanonicalStore, bool) {
	if m == nil || m.provider == nil {
		return nil, false
	}
	cs, ok := m.provider.(CanonicalStore)
	return cs, ok
}

// ReadModelFacts is the TRANSITION read model shared by prompt injection and
// the /memory views: canonical rows (active + conflicted) rendered as facts,
// UNION legacy facts whose statement identity the canonical layer has never
// seen. The union prevents the split brain where one mid-session canonical
// write hides every not-yet-imported legacy fact; a canonical row in ANY
// status (forgotten/superseded/archived included) suppresses its legacy
// shadow, so forget/supersede decisions always win over stale legacy rows.
// Returned ids are the canonical rows that were served (for access touching).
func ReadModelFacts(ctx context.Context, m *MemoryManager, tenantID string) (facts []Fact, servedCanonicalIDs []string) {
	legacy := func() []Fact {
		var out []Fact
		for _, target := range []string{"pinned", "user", "memory"} {
			fs, err := m.GetFacts(ctx, tenantID, target)
			if err != nil {
				continue
			}
			out = append(out, fs...)
		}
		return out
	}
	store, ok := m.Canonical()
	if !ok {
		return legacy(), nil
	}
	all, err := store.ListCanonicalMemories(ctx, tenantID, CanonicalFilter{
		Statuses: []string{CanonicalActive, CanonicalConflicted, CanonicalSuperseded, CanonicalArchived, CanonicalForgotten},
	})
	if err != nil || len(all) == 0 {
		return legacy(), nil
	}
	suppressed := make(map[string]bool, len(all))
	for _, c := range all {
		suppressed[canonicalStatementKey(c.Target, c.Scope, c.NormalizedHash)] = true
	}
	for _, c := range all {
		if c.Status != CanonicalActive && c.Status != CanonicalConflicted {
			continue
		}
		f := Fact{
			ID: c.ID, Target: c.Target, Content: c.Content, Scope: c.Scope,
			Confidence: c.Confidence, CreatedAt: c.CreatedAt, LastVerifiedAt: c.LastVerifiedAt,
		}
		if c.Pinned {
			f.Target = "pinned"
		}
		if c.UserConfirmed {
			f.Source = SourceUser
		}
		if c.Status == CanonicalConflicted {
			// Both sides of an unresolved contradiction stay visible, but
			// neither ranks as settled truth.
			f.Confidence = c.Confidence * 0.5
		}
		facts = append(facts, f)
		servedCanonicalIDs = append(servedCanonicalIDs, c.ID)
	}
	for _, f := range legacy() {
		if !suppressed[canonicalStatementKey(f.Target, normalizedConsolidationScope(f), NormalizedContentHash(f.Content))] {
			facts = append(facts, f)
		}
	}
	return facts, servedCanonicalIDs
}

func canonicalStatementKey(target, scope, hash string) string {
	return strings.ToLower(strings.TrimSpace(target)) + "\x00" + strings.TrimSpace(scope) + "\x00" + hash
}

// NormalizedContentHash is the identity key for the deterministic dedup net:
// two texts with the same hash are the same statement after normalization
// (case, punctuation, boilerplate prefixes). It reuses the consolidation
// normalizer so intake dedup and background consolidation agree on what
// "identical" means, then additionally strips per-token trailing periods:
// the normalizer keeps '.' for file paths, but for IDENTITY a sentence-final
// period must not distinguish "prefers Chinese." from "prefers Chinese".
func NormalizedContentHash(content string) string {
	fields := strings.Fields(normalizeConsolidationText(content))
	for i, field := range fields {
		fields[i] = strings.TrimRight(field, ".")
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, " ")))
	return hex.EncodeToString(digest[:])
}
