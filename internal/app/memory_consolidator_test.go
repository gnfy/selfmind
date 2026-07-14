package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

func seedParaphrases(t *testing.T, mem *memory.MemoryManager, tenantID string) {
	t.Helper()
	ctx := context.Background()
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store required")
	}
	for _, content := range []string{
		"User prefers viewing code inline rather than written to files",
		"The user prefers code displayed inline rather than written to files",
		"User prefers code examples inline rather than written into files",
	} {
		if err := store.ApplyIntakeWrite(ctx, tenantID, memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "global",
			Source: memory.SourceFactExtractor, Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func activeCount(t *testing.T, mem *memory.MemoryManager, tenantID string) (active, archived int) {
	t.Helper()
	store, _ := mem.Canonical()
	all, err := store.ListCanonicalMemories(context.Background(), tenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive, memory.CanonicalArchived},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Status == memory.CanonicalActive {
			active++
		} else {
			archived++
		}
	}
	return active, archived
}

// TestConsolidatorShadowNeverWrites: shadow mode judges, checkpoints, and
// reports — and applies NOTHING, however confident the judge is.
func TestConsolidatorShadowNeverWrites(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.99}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "shadow"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	active, archived := activeCount(t, mgr, "person")
	if active != 3 || archived != 0 {
		t.Fatalf("shadow mode must not apply: active=%d archived=%d", active, archived)
	}
	store, _ := mgr.Canonical()
	judged, err := store.ListJudgedClusterIDs(context.Background(), "person")
	if err != nil || len(judged) == 0 {
		t.Fatalf("shadow judgement must be checkpointed: %v err=%v", judged, err)
	}

	// Second pass: the checkpoint suppresses re-judging the same cluster.
	before := judge.calls()
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if judge.calls() != before {
		t.Fatalf("already-judged cluster was re-judged: %d -> %d", before, judge.calls())
	}
}

// TestConsolidatorShadowAnnotatesWouldApply: the shadow report dry-runs the
// SAME apply gates merge-only uses, so would_apply marks exactly the writes
// a mode switch would perform — the human review gates real behavior.
func TestConsolidatorShadowAnnotatesWouldApply(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.99}`}
	reportDir := t.TempDir()
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "shadow"}, reportDir: reportDir}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if active, archived := activeCount(t, mgr, "person"); active != 3 || archived != 0 {
		t.Fatalf("shadow dry run must not write: active=%d archived=%d", active, archived)
	}
	raw, err := os.ReadFile(filepath.Join(reportDir, "shadow-person.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report consolidationReportFile
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.JudgedNow) != 1 || !report.JudgedNow[0].WouldApply || report.JudgedNow[0].Applied {
		t.Fatalf("shadow report must annotate would_apply without applying: %+v", report.JudgedNow)
	}
	if report.Summary.ActiveBefore != 3 || report.Summary.WouldApply != 1 || report.Summary.ProjectedActive != 1 {
		t.Fatalf("unexpected human calibration summary: %+v", report.Summary)
	}
	markdown, err := os.ReadFile(filepath.Join(reportDir, "shadow-person.md"))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(markdown); !strings.Contains(text, "## Calibration Summary") || !strings.Contains(text, "Would apply in merge-only") {
		t.Fatalf("human report is missing calibration guidance:\n%s", text)
	}
	if summary := c.PassSummary(context.Background(), "person"); !strings.Contains(summary, "would_apply=1") || !strings.Contains(summary, "projected_active=1") {
		t.Fatalf("diag summary did not use the report: %q", summary)
	}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(reportDir, "shadow-person.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.JudgedNow != 0 || report.Summary.WouldApply != 0 {
		t.Fatalf("a no-op pass must replace stale action counts: %+v", report.Summary)
	}
}

// TestConsolidatorReinforceApply: merge-only folds a REINFORCE cluster onto
// one member's VERBATIM text — the applied canonical never contains model
// wording, making reinforce strictly safer than merge.
func TestConsolidatorReinforceApply(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	verbatim := "The user prefers code displayed inline rather than written to files"
	judge := &capturingProviderStub{content: `{"action":"REINFORCE","canonical":"` + verbatim + `","confidence":0.92}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	active, archived := activeCount(t, mgr, "person")
	if active != 1 || archived != 3 {
		t.Fatalf("reinforce apply expected 1 active + 3 archived, got active=%d archived=%d", active, archived)
	}
	store, _ := mgr.Canonical()
	actives, _ := store.ListCanonicalMemories(context.Background(), "person", memory.CanonicalFilter{})
	if len(actives) != 1 || actives[0].Content != verbatim {
		t.Fatalf("reinforced canonical must be the member's verbatim text: %+v", actives)
	}
}

// TestConsolidatorReinforceRejectsNonVerbatim: a REINFORCE whose canonical is
// model-authored (matches no member) is kept, not applied.
func TestConsolidatorReinforceRejectsNonVerbatim(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"REINFORCE","canonical":"User strongly prefers inline code and dislikes file output","confidence":0.96}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if active, archived := activeCount(t, mgr, "person"); active != 3 || archived != 0 {
		t.Fatalf("non-verbatim reinforce must be kept, got active=%d archived=%d", active, archived)
	}
}

// TestConsolidatorArchiveApply: merge-only applies a confident ARCHIVE —
// members become archived (reversible), never deleted.
func TestConsolidatorArchiveApply(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"ARCHIVE","confidence":0.95,"reason":"transient debugging state"}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if active, archived := activeCount(t, mgr, "person"); active != 0 || archived != 3 {
		t.Fatalf("archive apply expected 0 active + 3 archived, got active=%d archived=%d", active, archived)
	}
}

// TestConsolidatorJudgeCheckpointIsVersioned: checkpoints carry the judge
// version, so bumping consolidationJudgeVersion re-judges cached clusters
// instead of letting a newer apply gate consume stale decisions.
func TestConsolidatorJudgeCheckpointIsVersioned(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"KEEP","confidence":0.9}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "shadow"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	store, _ := mgr.Canonical()
	judged, err := store.ListJudgedClusterIDs(context.Background(), "person")
	if err != nil || len(judged) == 0 {
		t.Fatalf("expected a judgement checkpoint: %v err=%v", judged, err)
	}
	for key := range judged {
		if !strings.HasPrefix(key, consolidationJudgeVersion+":") {
			t.Fatalf("checkpoint key %q must carry judge version %q", key, consolidationJudgeVersion)
		}
	}
}

// TestConsolidatorMergeApply: merge-only mode applies a high-confidence MERGE
// — one new active canonical, members archived (never deleted), audit event.
func TestConsolidatorMergeApply(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.97}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	active, archived := activeCount(t, mgr, "person")
	if active != 1 || archived != 3 {
		t.Fatalf("merge apply expected 1 active + 3 archived, got active=%d archived=%d", active, archived)
	}
	store, _ := mgr.Canonical()
	actives, _ := store.ListCanonicalMemories(context.Background(), "person", memory.CanonicalFilter{})
	if len(actives) != 1 || actives[0].EvidenceCount != 3 {
		t.Fatalf("merged canonical must carry member evidence: %+v", actives)
	}
	events, _ := store.ListMemoryEvents(context.Background(), "person", 20)
	var sawMerge bool
	for _, e := range events {
		if e.Action == "merge" && e.Snapshot != "" {
			sawMerge = true
		}
	}
	if !sawMerge {
		t.Fatal("merge must write a snapshot-bearing audit event")
	}
}

// TestConsolidatorMergeGateRejectsLowConfidence: below auto_merge_confidence
// nothing is applied even in merge-only mode.
func TestConsolidatorMergeGateRejectsLowConfidence(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	seedParaphrases(t, mgr, "person")

	judge := &capturingProviderStub{content: `{"action":"MERGE","canonical":"User prefers code shown inline rather than written to files","confidence":0.80}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{Enabled: true, Mode: "merge-only"}, reportDir: t.TempDir()}
	if err := c.RunOnce(context.Background(), "person"); err != nil {
		t.Fatal(err)
	}
	if active, archived := activeCount(t, mgr, "person"); active != 3 || archived != 0 {
		t.Fatalf("under-confident merge must be kept, got active=%d archived=%d", active, archived)
	}
}

// TestConsolidatorCaps: full mode archives the weakest beyond
// max_active_global; pinned/user-confirmed rows are immune.
func TestConsolidatorCaps(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	ctx := context.Background()
	store, _ := mgr.Canonical()
	for _, seed := range []struct {
		content string
		source  string
	}{
		{"Repo builds with Go modules", memory.SourceFactExtractor},
		{"Deploy target runs Ubuntu server", memory.SourceFactExtractor},
		{"Owner name is Zero", memory.SourceUser}, // user-confirmed: immune
	} {
		if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "global", Source: seed.source, Content: seed.content,
		}); err != nil {
			t.Fatal(err)
		}
	}

	judge := &capturingProviderStub{content: `{"action":"KEEP","confidence":0.9}`}
	c := &MemoryConsolidator{provider: judge, mem: mgr, gov: config.MemoryGovernanceConfig{
		Enabled: true, Mode: "full", MaxActiveGlobal: 1, ArchiveAfter: "8760h",
	}, reportDir: t.TempDir()}
	if err := c.RunOnce(ctx, "person"); err != nil {
		t.Fatal(err)
	}
	actives, _ := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{})
	var confirmedSurvives bool
	for _, a := range actives {
		if strings.Contains(a.Content, "Owner name is Zero") {
			confirmedSurvives = true
		}
	}
	if !confirmedSurvives {
		t.Fatalf("user-confirmed memory must never be auto-archived: %+v", actives)
	}
	if len(actives) > 2 { // 1 allowed + the immune confirmed row
		t.Fatalf("caps did not archive overflow: %d actives", len(actives))
	}
}

func TestConsolidatorWorkspaceCapAndPauseDefault(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mgr := memory.NewMemoryManager(provider)
	ctx := context.Background()
	store, _ := mgr.Canonical()
	for i := 0; i < 3; i++ {
		if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "workspace:alpha",
			Content: fmt.Sprintf("Alpha project convention %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
		Decision: "ADD", Target: "memory", Scope: "workspace:beta", Content: "Beta project convention",
	}); err != nil {
		t.Fatal(err)
	}
	c := &MemoryConsolidator{provider: &capturingProviderStub{content: `{"action":"KEEP","confidence":0.9}`}, mem: mgr, gov: config.MemoryGovernanceConfig{
		Enabled: true, Mode: "full", MaxActiveGlobal: 20, MaxActivePerWorkspace: 2, ArchiveAfter: "8760h",
	}, reportDir: t.TempDir()}
	if !c.PauseWhileRunActive() {
		t.Fatal("foreground pause must default to true")
	}
	if err := c.RunOnce(ctx, "person"); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{})
	counts := map[string]int{}
	for _, item := range active {
		counts[item.Scope]++
	}
	if counts["workspace:alpha"] != 2 || counts["workspace:beta"] != 1 {
		t.Fatalf("workspace caps not enforced independently: %+v", counts)
	}
	pause := false
	c.gov.PauseWhileRunActive = &pause
	if c.PauseWhileRunActive() {
		t.Fatal("explicit pause_while_run_active=false ignored")
	}
}
