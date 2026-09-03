package memory

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteProvider_FTS5(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	tenantID := "test-user"
	traj := []byte(`{"messages":[{"role":"user","content":"hello world"},{"role":"assistant","content":"hi there"}]}`)

	if err := p.SaveTrajectory(nil, tenantID, "cli", traj); err != nil {
		t.Fatalf("SaveTrajectory: %v", err)
	}
	if err := p.IndexMessagesFromTrajectory(nil, tenantID, "cli", "sess-001", traj); err != nil {
		t.Fatalf("IndexMessagesFromTrajectory: %v", err)
	}

	sessions, err := p.SearchSessions(tenantID, "hello", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("expected at least 1 result for 'hello'")
	}
	if sessions[0].SessionID != "sess-001" {
		t.Errorf("expected session_id sess-001, got %s", sessions[0].SessionID)
	}

	recent, err := p.ListRecentSessions(tenantID, 5)
	if err != nil {
		t.Fatalf("ListRecentSessions: %v", err)
	}
	if len(recent) == 0 || recent[0].SessionID != "sess-001" {
		t.Fatalf("expected recent session sess-001, got %#v", recent)
	}

	messages, err := p.GetSessionMessages(tenantID, "sess-001", 1, 1)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if len(messages) == 0 || messages[0].Role != "user" || messages[0].MessageID != 1 {
		t.Fatalf("expected indexed user message, got %#v", messages)
	}

	sessions, err = p.SearchSessions(tenantID, "xyznonexistent", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 results for 'xyznonexistent', got %d", len(sessions))
	}
}

func TestSQLiteProvider_FTS5TreatsUserQueryAsLiterals(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	const tenantID = "literal-search"
	trajectory := []byte(`{"messages":[{"role":"user","content":"Build 625 uses release v1.2.3 at /srv/foo-bar and NEAR is ordinary text"}]}`)
	if err := p.IndexMessagesFromTrajectory(nil, tenantID, "cli", "sess-literals", trajectory); err != nil {
		t.Fatalf("IndexMessagesFromTrajectory: %v", err)
	}

	queries := []string{
		"625", "08", "foo:bar", "foo-bar", `"quoted"`, "OR", "NEAR",
		"v1.2.3", "/srv/foo-bar", "中文 625", "session_id:sess-literals",
		// Former stop-list entries and a punctuation-only query, which used to
		// compile to nothing and return before the LIKE fallback ran.
		"summary", "content", "session", "and", "not", "--- ???",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			if _, err := p.SearchSessions(tenantID, query, 5); err != nil {
				t.Fatalf("SearchSessions(%q): %v", query, err)
			}
		})
	}

	results, err := p.SearchSessions(tenantID, "625", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SessionID != "sess-literals" {
		t.Fatalf("numeric literal results = %#v", results)
	}

	// A word that only the stop list would have removed must still match the
	// indexed text. This is the user-visible symptom: searching an ordinary word
	// returned zero results with no error.
	ordinary, err := p.SearchSessions(tenantID, "ordinary", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 1 {
		t.Fatalf("ordinary word results = %#v", ordinary)
	}
	near, err := p.SearchSessions(tenantID, "NEAR", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(near) != 1 {
		t.Fatalf("the literal NEAR appears in the indexed text and must match: %#v", near)
	}
}

// stripQuotedSpans removes every "..." phrase, leaving only the structural
// syntax the compiler emitted itself. Anything user-supplied that survives has
// escaped the quoting boundary and can be parsed by FTS5 as an operator.
func stripQuotedSpans(query string) string {
	var out strings.Builder
	inQuotes := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '"' {
			// "" is an escaped quote inside a phrase, not a boundary.
			if inQuotes && i+1 < len(query) && query[i+1] == '"' {
				i++
				continue
			}
			inQuotes = !inQuotes
			continue
		}
		if !inQuotes {
			out.WriteByte(c)
		}
	}
	return out.String()
}

func TestSessionFTSQueryQuotesEveryTerm(t *testing.T) {
	// "or" and "near" are ordinary words a person may search for. Quoting is the
	// operator boundary, so they must survive as literals rather than being
	// filtered out — a stop list made real queries return silently empty.
	got := sessionFTSQuery(`content:625 foo-bar OR NEAR "quoted"`)
	for _, want := range []string{
		`content:"625"*`, `content:"foo"*`, `content:"bar"*`, `content:"quoted"*`,
		`content:"content"*`, `content:"OR"*`, `content:"NEAR"*`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sessionFTSQuery() = %q; missing %q", got, want)
		}
	}

	// Only the compiler's own syntax may appear outside quotes.
	residue := stripQuotedSpans(got)
	for _, structural := range []string{"session_id:", "content:", "summary:", " OR ", " AND ", "(", ")", "*"} {
		residue = strings.ReplaceAll(residue, structural, "")
	}
	if strings.TrimSpace(residue) != "" {
		t.Fatalf("user text escaped the quoting boundary: residue %q from %q", residue, got)
	}
}

func TestSQLiteProvider_MultiTermNaturalQueryUsesRankedOR(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	const tenantID = "natural-query"
	trajectory := []byte(`{"messages":[{"role":"user","content":"The deployment approval timed out while the release was pending"}]}`)
	if err := p.IndexMessagesFromTrajectory(nil, tenantID, "cli", "sess-approval-timeout", trajectory); err != nil {
		t.Fatalf("IndexMessagesFromTrajectory: %v", err)
	}

	results, err := p.SearchSessions(tenantID, "please investigate yesterday's approval timeout incident", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "sess-approval-timeout" {
		t.Fatalf("multi-term natural query results = %#v", results)
	}
}

// TestSessionFTSTermsKeepsIdentifiersAndLiterals pins the tokenizer contract
// that the stop list used to break.
func TestSessionFTSTermsKeepsIdentifiersAndLiterals(t *testing.T) {
	cases := map[string][]string{
		"summary":                    {"summary"},
		"OR":                         {"OR"},
		"session_id":                 {"session_id"},
		"session_id:sess-literals":   {"session_id", "sess", "literals"},
		"tool_search memory_extract": {"tool_search", "memory_extract"},
	}
	for query, want := range cases {
		got := sessionFTSTerms(query)
		if len(got) != len(want) {
			t.Errorf("sessionFTSTerms(%q) = %v, want %v", query, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("sessionFTSTerms(%q) = %v, want %v", query, got, want)
				break
			}
		}
	}
	if terms := sessionFTSTerms("--- ??? ..."); len(terms) != 0 {
		t.Errorf("punctuation-only query should compile to no terms, got %v", terms)
	}
}

// TestSQLiteProvider_TaskSessionRecall verifies the second leg of the light task
// layer: a task's turns indexed under a stable task-derived session id are
// retrievable by content (cross-endpoint recall via session_search) and that
// re-indexing the growing trajectory each turn is idempotent — one session, not
// a duplicate per turn.
func TestSQLiteProvider_TaskSessionRecall(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	const tenant = "person1"
	const sessionID = "task:order-sys"

	// Turn 1 (arrived on WeChat), then turn 2 (arrived on CLI) — same task, so
	// both re-index the SAME session id as the trajectory grows.
	turn1 := []byte(`{"messages":[{"role":"user","content":"design the order module"}]}`)
	turn2 := []byte(`{"messages":[{"role":"user","content":"design the order module"},{"role":"assistant","content":"added invoice pricing rules"}]}`)
	if err := p.IndexMessagesFromTrajectory(nil, tenant, "wechat", sessionID, turn1); err != nil {
		t.Fatalf("index turn1: %v", err)
	}
	if err := p.IndexMessagesFromTrajectory(nil, tenant, "cli", sessionID, turn2); err != nil {
		t.Fatalf("index turn2: %v", err)
	}

	// Content from the later turn is retrievable ("what we did on the order system").
	sessions, err := p.SearchSessions(tenant, "invoice", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("expected the task session to be retrievable by later-turn content")
	}
	// Idempotent: re-indexing the same session id must not create duplicate rows.
	for _, s := range sessions {
		if s.SessionID != sessionID {
			t.Fatalf("unexpected session id %q", s.SessionID)
		}
	}
	all, err := p.SearchSessions(tenant, "order", 10)
	if err != nil {
		t.Fatalf("SearchSessions order: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 session row for the task after re-index, got %d", len(all))
	}
}

func TestSQLiteProvider_ChineseRecallFallback(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	const tenant = "person-cn"
	const sessionID = "task:kof97"
	traj := []byte(`{"messages":[{"role":"user","content":"九七对战游戏需要增加跳跃攻击和打击反馈"},{"role":"assistant","content":"已修改 arcade-fury-97.html"}]}`)
	if err := p.IndexMessagesFromTrajectory(nil, tenant, "weixin", sessionID, traj); err != nil {
		t.Fatalf("IndexMessagesFromTrajectory: %v", err)
	}

	sessions, err := p.SearchSessions(tenant, "跳跃攻击", 5)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("expected Chinese LIKE fallback to retrieve the task session")
	}
	if sessions[0].SessionID != sessionID {
		t.Fatalf("session id = %q, want %q", sessions[0].SessionID, sessionID)
	}
}

func TestSQLiteProvider_MultiTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	itraj := []byte(`{"messages":[{"role":"user","content":"secret data for alice"}]}`)
	ibtraj := []byte(`{"messages":[{"role":"user","content":"secret data for bob"}]}`)

	p.IndexMessagesFromTrajectory(nil, "alice", "cli", "alice-sess", itraj)
	p.IndexMessagesFromTrajectory(nil, "bob", "cli", "bob-sess", ibtraj)

	results, _ := p.SearchSessions("alice", "alice", 5)
	if len(results) == 0 {
		t.Error("alice should find her own session")
	}

	results, _ = p.SearchSessions("alice", "bob", 5)
	if len(results) != 0 {
		t.Error("alice should not find bob's session")
	}
}

func TestSQLiteProvider_TenantScopedPath(t *testing.T) {
	path := TenantScopedPath("/data", "user1", "memory.db")
	expected := "/data/user1/memory.db"
	if path != expected {
		t.Errorf("expected %s, got %s", expected, path)
	}
}

func TestSQLiteProvider_DBDirCreation(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	err = p.SaveTrajectory(nil, "brand-new-tenant", "cli", []byte(`{}`))
	if err != nil {
		t.Fatalf("SaveTrajectory: %v", err)
	}

	expectedPath := filepath.Join(dir, "brand-new-tenant", "memory.db")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected DB file at %s: %v", expectedPath, err)
	}
}

func TestFactMetadataRoundTrip(t *testing.T) {
	p, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()

	verified := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := Fact{
		Target: "user", Content: "prefers Go", Source: "user",
		Scope: "global", Confidence: 0.9, CreatedFromRun: "run-1", LastVerifiedAt: verified,
	}
	if err := p.AddFactMeta(nil, "t1", want); err != nil {
		t.Fatalf("AddFactMeta: %v", err)
	}
	// Legacy metadata-less write must still work and read back with zero metadata.
	if err := p.AddFact(nil, "t1", "user", "plain fact"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	facts, err := p.GetFacts(nil, "t1", "user")
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	var meta, plain *Fact
	for i := range facts {
		switch facts[i].Content {
		case "prefers Go":
			meta = &facts[i]
		case "plain fact":
			plain = &facts[i]
		}
	}
	if meta == nil || plain == nil {
		t.Fatalf("expected both facts, got %+v", facts)
	}
	if meta.Source != "user" || meta.Scope != "global" || meta.Confidence != 0.9 || meta.CreatedFromRun != "run-1" {
		t.Fatalf("metadata not persisted: %+v", *meta)
	}
	if !meta.LastVerifiedAt.Equal(verified) {
		t.Fatalf("last_verified_at = %v, want %v", meta.LastVerifiedAt, verified)
	}
	if plain.Source != "" || plain.Confidence != 0 {
		t.Fatalf("plain fact should have zero metadata: %+v", *plain)
	}
}

func TestAddMissingColumnsBackwardCompat(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Simulate a pre-migration facts table + row.
	if _, err := db.Exec(`CREATE TABLE facts (id TEXT PRIMARY KEY, target TEXT, content TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO facts (id, target, content) VALUES ('1','user','legacy fact')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cols := []columnDef{
		{"source", "TEXT DEFAULT ''"},
		{"scope", "TEXT DEFAULT ''"},
		{"confidence", "REAL DEFAULT 0"},
		{"created_from_run", "TEXT DEFAULT ''"},
		{"last_verified_at", "DATETIME"},
	}
	if err := addMissingColumns(db, "facts", cols); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Idempotent: running again must not error.
	if err := addMissingColumns(db, "facts", cols); err != nil {
		t.Fatalf("migrate (idempotent): %v", err)
	}
	// The legacy row reads back with default metadata.
	var content, source string
	var confidence float64
	if err := db.QueryRow(`SELECT content, COALESCE(source,''), COALESCE(confidence,0) FROM facts WHERE id='1'`).Scan(&content, &source, &confidence); err != nil {
		t.Fatalf("select migrated: %v", err)
	}
	if content != "legacy fact" || source != "" || confidence != 0 {
		t.Fatalf("legacy row = %q/%q/%v", content, source, confidence)
	}
}

// TestSQLiteProvider_PurgeWorkHistoryReferences pins the memory side of a
// control work-history reset: Thread-keyed sessions disappear, run provenance
// is cleared on facts and observations, and ordinary sessions and preference
// content survive. A second purge is a no-op.
func TestSQLiteProvider_PurgeWorkHistoryReferences(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSQLiteProvider(dir)
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	defer p.Close()
	tenant := "person_reset"
	taskTrajectory := []byte(`{"messages":[{"role":"user","content":"release checklist"},{"role":"assistant","content":"prepared"}]}`)
	plainTrajectory := []byte(`{"messages":[{"role":"user","content":"plain chat about tabs"},{"role":"assistant","content":"noted"}]}`)
	if err := p.IndexMessagesFromTrajectory(nil, tenant, "cli", "task:thread-1", taskTrajectory); err != nil {
		t.Fatalf("index task session: %v", err)
	}
	if err := p.IndexMessagesFromTrajectory(nil, tenant, "cli", "session", plainTrajectory); err != nil {
		t.Fatalf("index plain session: %v", err)
	}
	if err := p.AddFactMeta(nil, tenant, Fact{Target: "user", Content: "prefers tabs", Source: "user", Scope: "global", CreatedFromRun: "run-1"}); err != nil {
		t.Fatalf("AddFactMeta: %v", err)
	}
	if err := p.AddFactMeta(nil, tenant, Fact{Target: "user", Content: "prefers short answers", Source: "user", Scope: "global"}); err != nil {
		t.Fatalf("AddFactMeta: %v", err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, tenant, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO memory_observations(id, run_id, target, content, normalized_hash, created_at) VALUES ('obs-1', 'run-1', 'user', 'prefers tabs', 'h1', 1)`); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	sessions, provenance, err := p.PurgeWorkHistoryReferences(nil, tenant)
	if err != nil || sessions != 1 || provenance != 2 {
		t.Fatalf("purge sessions=%d provenance=%d err=%v, want 1 session and 2 cleared references", sessions, provenance, err)
	}
	if hits, err := p.SearchSessions(tenant, "release", 5); err != nil || len(hits) != 0 {
		t.Fatalf("task session still searchable: %+v err=%v", hits, err)
	}
	if recent, err := p.ListRecentSessions(tenant, 5); err != nil || len(recent) != 1 || recent[0].SessionID != "session" {
		t.Fatalf("recent sessions after purge=%+v err=%v", recent, err)
	}
	if messages, err := p.GetSessionMessages(tenant, "task:thread-1", 1, 5); err != nil || len(messages) != 0 {
		t.Fatalf("task session messages survived: %+v err=%v", messages, err)
	}
	facts, err := p.GetFacts(nil, tenant, "user")
	if err != nil || len(facts) != 2 {
		t.Fatalf("facts after purge=%+v err=%v", facts, err)
	}
	for _, fact := range facts {
		if fact.CreatedFromRun != "" {
			t.Fatalf("fact %q still cites run %q", fact.Content, fact.CreatedFromRun)
		}
	}
	var runID string
	if err := raw.QueryRow(`SELECT run_id FROM memory_observations WHERE id = 'obs-1'`).Scan(&runID); err != nil || runID != "" {
		t.Fatalf("observation run_id=%q err=%v", runID, err)
	}
	if sessions, provenance, err := p.PurgeWorkHistoryReferences(nil, tenant); err != nil || sessions != 0 || provenance != 0 {
		t.Fatalf("second purge sessions=%d provenance=%d err=%v, want a no-op", sessions, provenance, err)
	}
}
