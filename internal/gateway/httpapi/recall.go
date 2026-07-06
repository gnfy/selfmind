package httpapi

// Automatic semantic recall (Work Timeline P2, docs/work-timeline.md
// "Semantic recall", tier v1). At turn start the context selector asks this
// engine for up to a handful of "possibly related prior work" slices — indexed
// session fragments (FTS/BM25) and task label cards (control.db) — and attaches
// them to kernel.TaskRuntimeContext.RecallSlices. That is the SELECTOR layer of
// the durable-context contract: recall never appends prompt fragments in
// agent.go, gateway handlers, or IM adapters, and the slices are ephemeral —
// regenerated per turn, rendered only into this turn's system-prompt context
// block, never persisted into working history.
//
// Failure philosophy: recall must never block or fail the turn. Query
// expansion is bounded by a timeout and degrades to raw terms; a source error
// degrades to the other sources; no hits degrades to nothing. A recall miss is
// recoverable in-conversation (the agent asks or searches); that is the whole
// point of spine-first context (docs/work-timeline.md).

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

// Recall budget and skip thresholds. Kept in one place so tests and docs have
// a single truth. maxRecallSlices/maxRecallExcerptChars mirror the kernel-side
// render hard floors in kernel/task_runtime_context.go.
const (
	recallMaxSlices        = 3
	recallExcerptChars     = 400
	recallTotalExcerptCap  = 1300 // hard cap across all slices (~3 x 400 + headers)
	recallMinMessageRunes  = 6
	recallMaxTerms         = 10
	recallMaxSessionProbes = 5  // FTS searches per turn (one per top term)
	recallSessionProbeHits = 5  // hits fetched per FTS probe
	recallTaskCardWindow   = 20 // recent task cards scanned per turn
	defaultExpandTimeout   = 3 * time.Second
)

// RecallQueryExpander widens the raw user message into extra search keyword
// variants. Implemented by memory.SemanticExpander (semantic_recall role);
// tests inject fakes. Expansion is optional — nil means raw-term search only.
type RecallQueryExpander interface {
	Expand(ctx context.Context, query string) string
}

// RecallSessionSearcher is the FTS/BM25 session index boundary, satisfied by
// *memory.MemoryManager.
type RecallSessionSearcher interface {
	SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error)
}

// RecallTaskCardLister is the control-plane label-card boundary, satisfied by
// *control.Store (read-only query).
type RecallTaskCardLister interface {
	ListTaskCards(ctx context.Context, tenantID, personID string, limit int) ([]control.TaskCard, error)
}

// RecallQuery is the per-turn search request handed to every source.
type RecallQuery struct {
	TenantID string
	PersonID string
	// Terms are the significant search terms: raw-message terms first, then
	// expansion variants. Bounded by recallMaxTerms.
	Terms []string
	// RawTermCount marks the raw/expansion split: Terms[:RawTermCount] came
	// from the user message itself, the rest from semantic_recall expansion.
	// Sources with a per-term probe budget reserve slots for expansion terms —
	// the whole point of expansion is reaching vocabulary the raw message
	// lacks, so those terms must not starve behind raw ones.
	RawTermCount int
	// ExcludeWorkKey is the current turn's own work line ("task:<id>") — its
	// context is already in the bundle/history, echoing it back is noise.
	ExcludeWorkKey string
}

// probeTerms picks up to max terms for per-term probing: leading raw terms
// first, but at least two slots (when available) go to expansion variants.
func probeTerms(q RecallQuery, max int) []string {
	raw := q.Terms
	var expanded []string
	if q.RawTermCount >= 0 && q.RawTermCount <= len(q.Terms) {
		raw = q.Terms[:q.RawTermCount]
		expanded = q.Terms[q.RawTermCount:]
	}
	reserved := 2
	if len(expanded) < reserved {
		reserved = len(expanded)
	}
	out := make([]string, 0, max)
	rawTaken := 0
	for _, t := range raw {
		if len(out) >= max-reserved {
			break
		}
		out = append(out, t)
		rawTaken++
	}
	for _, t := range expanded {
		if len(out) >= max {
			break
		}
		out = append(out, t)
	}
	for _, t := range raw[rawTaken:] { // backfill unused expansion slots
		if len(out) >= max {
			break
		}
		out = append(out, t)
	}
	return out
}

// RecallHit is one scored candidate from a source. WorkKey identifies the
// work line for dedupe (one slice per task/session line); sources with richer
// summaries win ties (taskcard over raw session fragment).
type RecallHit struct {
	Slice    kernel.RecallSlice
	Score    float64
	WorkKey  string
	Priority int // lower wins inside one work line (taskcard=0, session=1)
}

// RecallSource is one searchable store of prior work. The v2 embedding tier
// implements this same interface (vector index over spine entries + label
// cards + artifacts) and registers alongside the FTS sources — the selector
// and budget/dedupe logic do not change shape.
type RecallSource interface {
	Name() string
	Search(ctx context.Context, q RecallQuery) ([]RecallHit, error)
}

// RecallStats is the redacted observability summary for the context.recall
// task event: source counts and refs only, never excerpts.
type RecallStats struct {
	Sources  map[string]int
	Refs     []string
	Expanded bool
	Terms    int
	Skipped  string // non-empty when recall was skipped ("control_command", "short_message")
}

// RecallEngine runs bounded searches over its sources and selects the top
// slices for one turn. Nil engine (not wired) disables automatic recall.
type RecallEngine struct {
	sources  []RecallSource
	expander RecallQueryExpander
	// expandTimeout bounds the semantic_recall expansion call; tests shrink it.
	expandTimeout time.Duration
}

// NewRecallEngine wires the v1 sources: task label cards (control.db, live
// query) and indexed sessions (FTS). Either dependency may be nil — the
// matching source is simply not registered. expander may be nil (raw terms).
func NewRecallEngine(cards RecallTaskCardLister, sessions RecallSessionSearcher, expander RecallQueryExpander) *RecallEngine {
	engine := &RecallEngine{expander: expander, expandTimeout: defaultExpandTimeout}
	if cards != nil {
		engine.sources = append(engine.sources, &taskCardRecallSource{cards: cards})
	}
	if sessions != nil {
		engine.sources = append(engine.sources, &sessionRecallSource{sessions: sessions})
	}
	return engine
}

// Select builds the search query from the incoming user message, runs every
// source, and returns the deduped, budgeted top slices plus redacted stats.
// It never returns an error: recall degrades, the turn proceeds.
func (e *RecallEngine) Select(ctx context.Context, tenantID, personID, currentTaskID, message string) ([]kernel.RecallSlice, RecallStats) {
	stats := RecallStats{Sources: map[string]int{}}
	if e == nil || len(e.sources) == 0 {
		return nil, stats
	}
	trimmed := strings.TrimSpace(message)
	// Cheap skips before any work: control-command-shaped input never reaches
	// the agent anyway, and trivially short messages carry no searchable signal.
	if strings.HasPrefix(trimmed, "/") {
		stats.Skipped = "control_command"
		return nil, stats
	}
	if utf8.RuneCountInString(trimmed) < recallMinMessageRunes {
		stats.Skipped = "short_message"
		return nil, stats
	}

	terms := recallTerms(trimmed)
	rawCount := len(terms)
	if expansion, ok := e.expand(ctx, trimmed); ok {
		stats.Expanded = true
		terms = mergeTerms(terms, recallTerms(expansion))
	}
	if len(terms) == 0 {
		stats.Skipped = "no_terms"
		return nil, stats
	}
	if len(terms) > recallMaxTerms {
		terms = terms[:recallMaxTerms]
	}
	if rawCount > len(terms) {
		rawCount = len(terms)
	}
	stats.Terms = len(terms)

	query := RecallQuery{TenantID: tenantID, PersonID: personID, Terms: terms, RawTermCount: rawCount}
	if strings.TrimSpace(currentTaskID) != "" {
		query.ExcludeWorkKey = "task:" + strings.TrimSpace(currentTaskID)
	}

	// Dedupe by work line: keep the best hit per task/session, preferring the
	// richer source (label card over raw session fragment), then higher score.
	best := map[string]RecallHit{}
	for _, source := range e.sources {
		hits, err := source.Search(ctx, query)
		if err != nil {
			continue // recall degrades, never fails the turn
		}
		for _, hit := range hits {
			if hit.WorkKey == "" || hit.WorkKey == query.ExcludeWorkKey {
				continue
			}
			existing, ok := best[hit.WorkKey]
			if !ok || hit.Priority < existing.Priority ||
				(hit.Priority == existing.Priority && hit.Score > existing.Score) {
				best[hit.WorkKey] = hit
			}
		}
	}
	if len(best) == 0 {
		return nil, stats
	}

	ranked := make([]RecallHit, 0, len(best))
	for _, hit := range best {
		ranked = append(ranked, hit)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].WorkKey < ranked[j].WorkKey
	})

	var slices []kernel.RecallSlice
	usedChars := 0
	for _, hit := range ranked {
		if len(slices) >= recallMaxSlices {
			break
		}
		slice := hit.Slice
		slice.Excerpt = textutil.Truncate(strings.TrimSpace(slice.Excerpt), recallExcerptChars)
		if usedChars+len(slice.Excerpt) > recallTotalExcerptCap && len(slices) > 0 {
			break
		}
		usedChars += len(slice.Excerpt)
		slices = append(slices, slice)
		stats.Sources[slice.Source]++
		stats.Refs = append(stats.Refs, slice.Ref)
	}
	return slices, stats
}

// expand runs the semantic_recall expansion with a hard deadline. The expander
// is trusted to degrade on provider errors (it returns the raw query); the
// goroutine+timer guards against a misbehaving/blocking implementation so the
// turn is never held hostage by recall.
func (e *RecallEngine) expand(ctx context.Context, message string) (string, bool) {
	if e.expander == nil {
		return "", false
	}
	timeout := e.expandTimeout
	if timeout <= 0 {
		timeout = defaultExpandTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- e.expander.Expand(tctx, message) }()
	select {
	case expanded := <-done:
		expanded = strings.TrimSpace(expanded)
		if expanded == "" || expanded == strings.TrimSpace(message) {
			return "", false
		}
		return expanded, true
	case <-tctx.Done():
		return "", false
	}
}

// --- query building ---------------------------------------------------------

// recallTerms extracts significant search terms from free text. ASCII words
// keep length >= 3 minus a tiny stopword list; CJK runs are kept whole (when
// short) and additionally split into bigrams — the standard poor-man's CJK
// tokenization, matching how a related phrase overlaps an indexed one.
func recallTerms(text string) []string {
	var terms []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] || recallStopwords[t] {
			return
		}
		seen[t] = true
		terms = append(terms, t)
	}
	var word, cjk []rune
	flush := func() {
		if len(word) >= 3 {
			add(string(word))
		}
		word = word[:0]
		if n := len(cjk); n > 0 {
			if n <= 4 {
				add(string(cjk))
			}
			for i := 0; i+2 <= n; i++ {
				add(string(cjk[i : i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			if len(word) > 0 {
				flush()
			}
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.':
			if len(cjk) > 0 {
				flush()
			}
			word = append(word, r)
		default:
			flush()
		}
		if len(terms) >= recallMaxTerms*2 {
			break
		}
	}
	flush()
	return terms
}

func mergeTerms(base, extra []string) []string {
	seen := map[string]bool{}
	for _, t := range base {
		seen[t] = true
	}
	for _, t := range extra {
		if !seen[t] {
			seen[t] = true
			base = append(base, t)
		}
	}
	return base
}

// recallStopwords drops the highest-frequency function words that would match
// everything. Deliberately tiny: this is noise control, not NLP.
var recallStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true,
	"that": true, "you": true, "your": true, "please": true, "can": true,
	"could": true, "would": true, "help": true, "make": true, "want": true,
	"need": true, "let": true, "使用": true, "我们": true, "一个": true,
	"帮我": true, "请你": true, "这个": true, "那个": true, "什么": true,
}

// containsTerm reports a case-insensitive substring match. haystack must
// already be lowercased by the caller.
func containsTerm(lowerHaystack, term string) bool {
	return term != "" && strings.Contains(lowerHaystack, term)
}

// --- task label-card source -------------------------------------------------

// taskCardRecallSource matches search terms against compact task cards
// (title + current summary + latest handoff summary/changed files) queried
// live from control.db. Changed files double as the artifact trail: an
// artifact belongs to a task's work line, so it surfaces through the card
// rather than as a competing slice (the dedupe rule would collapse them
// anyway).
type taskCardRecallSource struct {
	cards RecallTaskCardLister
}

func (s *taskCardRecallSource) Name() string { return "taskcard" }

func (s *taskCardRecallSource) Search(ctx context.Context, q RecallQuery) ([]RecallHit, error) {
	cards, err := s.cards.ListTaskCards(ctx, q.TenantID, q.PersonID, recallTaskCardWindow)
	if err != nil {
		return nil, err
	}
	var hits []RecallHit
	for _, card := range cards {
		title := strings.ToLower(card.Title)
		body := strings.ToLower(card.Summary + "\n" + card.HandoffSummary)
		files := strings.ToLower(strings.Join(card.ChangedFiles, "\n"))
		score := 0.0
		for _, term := range q.Terms {
			switch {
			case containsTerm(title, term):
				score += 2
			case containsTerm(body, term):
				score++
			case containsTerm(files, term):
				score++
			}
		}
		if score <= 0 {
			continue
		}
		hits = append(hits, RecallHit{
			Slice: kernel.RecallSlice{
				Source:  s.Name(),
				Title:   card.Title,
				Excerpt: taskCardExcerpt(card),
				Ref:     card.TaskID,
			},
			Score:    score,
			WorkKey:  "task:" + card.TaskID,
			Priority: 0,
		})
	}
	return hits, nil
}

func taskCardExcerpt(card control.TaskCard) string {
	var parts []string
	if card.Status != "" {
		parts = append(parts, "status "+card.Status)
	}
	if s := strings.TrimSpace(card.Summary); s != "" {
		parts = append(parts, s)
	}
	if h := strings.TrimSpace(card.HandoffSummary); h != "" && h != strings.TrimSpace(card.Summary) {
		parts = append(parts, "handoff: "+h)
	}
	if len(card.ChangedFiles) > 0 {
		files := card.ChangedFiles
		if len(files) > 5 {
			files = files[:5]
		}
		parts = append(parts, "files: "+strings.Join(files, ", "))
	}
	return textutil.Truncate(strings.Join(parts, "; "), recallExcerptChars)
}

// --- indexed-session FTS source ----------------------------------------------

// sessionRecallSource probes the FTS5 session index once per top term (the
// provider composes the column MATCH query and orders by BM25 rank) and scores
// hits by how many distinct terms matched, with a small rank bonus. Sessions
// indexed under a task-derived id ("task:<id>") share the task's work line so
// the dedupe step prefers the label card when both hit.
type sessionRecallSource struct {
	sessions RecallSessionSearcher
}

func (s *sessionRecallSource) Name() string { return "session" }

func (s *sessionRecallSource) Search(ctx context.Context, q RecallQuery) ([]RecallHit, error) {
	type candidate struct {
		session memory.FTS5Session
		matched int
		bonus   float64
	}
	found := map[string]*candidate{}
	probes := 0
	for _, term := range probeTerms(q, recallMaxSessionProbes) {
		if probes >= recallMaxSessionProbes {
			break
		}
		if ctx.Err() != nil {
			break
		}
		safe := ftsSafeTerm(term)
		if safe == "" {
			continue
		}
		probes++
		// The memory store partitions by PERSON: the agent runs with the
		// person id as its storage tenant (see gateway runConversation →
		// Agent.RunConversation(uid=person_id) and Agent.trajectoryKey "the
		// person is already the storage tenant"), so sessions are indexed —
		// and must be searched — under q.PersonID, not the control tenant.
		sessions, err := s.sessions.SearchSessions(q.PersonID, safe, recallSessionProbeHits)
		if err != nil {
			continue
		}
		for pos, sess := range sessions {
			c, ok := found[sess.SessionID]
			if !ok {
				c = &candidate{session: sess}
				found[sess.SessionID] = c
			}
			c.matched++
			c.bonus += 1.0 / float64(2+pos)
		}
	}
	var hits []RecallHit
	for _, c := range found {
		workKey := c.session.SessionID
		if !strings.HasPrefix(workKey, "task:") {
			workKey = "session:" + workKey
		}
		title := c.session.SessionID
		if strings.HasPrefix(c.session.SessionID, "task:") {
			title = "prior task session " + strings.TrimPrefix(c.session.SessionID, "task:")
		}
		excerpt := strings.TrimSpace(c.session.Summary)
		if excerpt == "" {
			excerpt = strings.TrimSpace(c.session.Content)
		}
		hits = append(hits, RecallHit{
			Slice: kernel.RecallSlice{
				Source:  s.Name(),
				Title:   title,
				Excerpt: textutil.Truncate(excerpt, recallExcerptChars),
				Ref:     c.session.SessionID,
			},
			Score:    float64(c.matched) + c.bonus*0.1,
			WorkKey:  workKey,
			Priority: 1,
		})
	}
	return hits, nil
}

// ftsSafeTerm reduces a term to its first alnum/CJK run so the provider's
// FTS5 MATCH composition stays syntactically valid (dots/dashes in e.g. file
// names would break the bareword query). "agent.go" probes as "agent", which
// the tokenizer also produced on the indexed side.
func ftsSafeTerm(term string) string {
	var b strings.Builder
	for _, r := range term {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 {
			break
		}
	}
	return b.String()
}
