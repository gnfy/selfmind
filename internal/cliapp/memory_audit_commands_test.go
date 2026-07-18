package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

// The audit must classify with the SAME deterministic marker rule the intake
// gate enforces, report without mutating by default, and archive (never
// delete) with --archive.
func TestMaintenanceMemoryAudit(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	provider, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(content string) {
		t.Helper()
		if err := provider.ApplyIntakeWrite(ctx, "person_test", memory.IntakeWrite{
			Decision: "ADD", Target: "memory", Scope: "workspace:ws",
			Source: memory.SourceFactExtractor, Content: content, RunID: "run-1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("RUQX-222 已执行并审批，当前状态 IN_PROGRESS / QUEUED")
	seed("lid-tm-tracking 使用 _COMMIT 参数，不传 _IMG_TAG")
	// Candidate: status vocabulary without instance context — reported only.
	seed("构建已入队 QUEUED")
	// Pinned confirmed-transient content: user decisions outrank automation.
	if err := provider.ApplyIntakeWrite(ctx, "person_test", memory.IntakeWrite{
		Decision: "ADD", Target: "pinned", Scope: "global",
		Source: memory.SourceUser, Content: "RUQX-1 当前状态 IN_PROGRESS（用户置顶）", RunID: "run-1",
	}); err != nil {
		t.Fatal(err)
	}
	provider.Close()

	newApp := func(args ...string) (*App, *bytes.Buffer) {
		out := &bytes.Buffer{}
		return &App{ctx: ctx, args: append([]string{"selfmind", "maintenance"}, args...), stdout: out, stderr: out}, out
	}

	// Dry run: reports the transient row, archives nothing.
	app, out := newApp("memory-audit", "--data-dir", dataDir)
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("dry run handled=%v code=%d output=%s", handled, code, out.String())
	}
	if !strings.Contains(out.String(), "IN_PROGRESS") || strings.Contains(out.String(), "_IMG_TAG") {
		t.Fatalf("report selected wrong rows:\n%s", out.String())
	}
	provider, err = memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	active, err := provider.ListCanonicalMemories(ctx, "person_test", memory.CanonicalFilter{})
	if err != nil || len(active) != 4 {
		t.Fatalf("dry run mutated store: %d active err=%v", len(active), err)
	}
	provider.Close()

	// Archive: only the CONFIRMED transient row leaves the active set. The
	// durable row, the ambiguous candidate, and the pinned row all stay.
	app, out = newApp("memory-audit", "--data-dir", dataDir, "--archive-confirmed")
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
		t.Fatalf("archive handled=%v code=%d output=%s", handled, code, out.String())
	}
	provider, err = memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	active, err = provider.ListCanonicalMemories(ctx, "person_test", memory.CanonicalFilter{})
	if err != nil || len(active) != 3 {
		t.Fatalf("archive result wrong: %+v err=%v", active, err)
	}
	for _, m := range active {
		if strings.Contains(m.Content, "RUQX-222") {
			t.Fatalf("confirmed transient row survived archive: %+v", m)
		}
	}
	var pinnedKept, candidateKept bool
	for _, m := range active {
		if strings.Contains(m.Content, "用户置顶") {
			pinnedKept = true
		}
		if strings.Contains(m.Content, "构建已入队") {
			candidateKept = true
		}
	}
	if !pinnedKept || !candidateKept {
		t.Fatalf("pinned/candidate rows must never be auto-archived: pinned=%v candidate=%v %+v", pinnedKept, candidateKept, active)
	}
	archived, err := provider.ListCanonicalMemories(ctx, "person_test", memory.CanonicalFilter{Statuses: []string{"archived"}})
	if err != nil || len(archived) != 1 {
		t.Fatalf("archived row must remain recoverable: %+v err=%v", archived, err)
	}
}

func TestTruncateAuditLinePreservesUTF8(t *testing.T) {
	input := strings.Repeat("当前构建状态正常", 40)
	got := truncateAuditLine(input)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated line = %q, want ellipsis", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncated line contains replacement rune: %q", got)
	}
	if len([]rune(strings.TrimSuffix(got, "..."))) != 120 {
		t.Fatalf("truncated rune count = %d, want 120", len([]rune(strings.TrimSuffix(got, "..."))))
	}
}
