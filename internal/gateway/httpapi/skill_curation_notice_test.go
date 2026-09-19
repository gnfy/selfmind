package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func curationEvent(t *testing.T, eventType, name, hash string, promoted bool, blocked string) control.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"name": name, "version_hash": hash, "promoted": promoted, "blocked_reason": blocked,
	})
	if err != nil {
		t.Fatal(err)
	}
	return control.Event{Type: eventType, Payload: payload}
}

// A published Skill writes two events carrying the same payload. The person
// must be told once.
func TestCurationNoticeReportsOnePublication(t *testing.T) {
	notice := skillCurationNotice([]control.Event{
		curationEvent(t, "skill.candidate.created", "release-ledger", "abc123def456789", true, ""),
		curationEvent(t, "skill.version.promoted", "release-ledger", "abc123def456789", true, ""),
	})
	if strings.Count(notice, "release-ledger") != 1 {
		t.Fatalf("one publication must be reported once: %q", notice)
	}
	if !strings.Contains(notice, "now active") {
		t.Fatalf("notice must say the Skill is active: %q", notice)
	}
}

// A candidate the policy withheld is the case that was invisible: the person
// was never told it existed, so the reason it stalled could not be acted on.
func TestCurationNoticeNamesTheWithholdingReason(t *testing.T) {
	notice := skillCurationNotice([]control.Event{
		curationEvent(t, "skill.candidate.created", "release-ledger", "abc123def456789", false,
			"class-specific repair evidence threshold is not met"),
	})
	if !strings.Contains(notice, "release-ledger") {
		t.Fatalf("notice must name the candidate: %q", notice)
	}
	if !strings.Contains(notice, "repair evidence threshold") {
		t.Fatalf("notice must carry the reason, or it is not actionable: %q", notice)
	}
	if !strings.Contains(notice, "abc123def456") {
		t.Fatalf("notice must carry the version to review: %q", notice)
	}
}

// The constraints that must change the result: a pass that changed nothing, and
// a candidate withheld for no recorded reason, say nothing at all. A background
// job that speaks on every sweep trains the person to ignore it.
func TestCurationNoticeStaysSilentWithoutAnOutcome(t *testing.T) {
	if notice := skillCurationNotice(nil); notice != "" {
		t.Fatalf("no events must produce no notice: %q", notice)
	}
	if notice := skillCurationNotice([]control.Event{
		{Type: "run.finished"},
		curationEvent(t, "skill.candidate.created", "release-ledger", "abc123", false, ""),
	}); notice != "" {
		t.Fatalf("an unexplained non-publication must stay silent: %q", notice)
	}
}

// A candidate that was withheld earlier in the same pass and then published
// must not be reported as both.
func TestCurationNoticePrefersThePublishedOutcome(t *testing.T) {
	notice := skillCurationNotice([]control.Event{
		curationEvent(t, "skill.candidate.created", "release-ledger", "abc123", false, "name collision with an active Skill"),
		curationEvent(t, "skill.version.promoted", "release-ledger", "abc123", true, ""),
	})
	if strings.Contains(notice, "not published") {
		t.Fatalf("a published Skill must not also be reported as withheld: %q", notice)
	}
	if !strings.Contains(notice, "now active") {
		t.Fatalf("notice must report the publication: %q", notice)
	}
}

// One notice reports what changed, not a catalog listing.
func TestCurationNoticeIsBounded(t *testing.T) {
	var events []control.Event
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		events = append(events, curationEvent(t, "skill.version.promoted", name, "h"+name, true, ""))
	}
	notice := skillCurationNotice(events)
	if !strings.Contains(notice, "and 2 more") {
		t.Fatalf("notice must bound the listing: %q", notice)
	}
	if strings.Contains(notice, "five") {
		t.Fatalf("notice must not list every Skill: %q", notice)
	}
}

// The renderer tests above exercise a pure function; they cannot see whether
// the events ever reach it. This one goes through the real store, because the
// first version of this path read the run EVIDENCE stream, whose query
// hardcodes `type='evidence.recorded'` and never selects the type column — so
// every curation outcome rendered nothing and the notice silently never fired.
// A wiring defect in the surface that exists to end invisibility is invisible
// by construction.
func TestCurationNoticeReadsTheRunsOwnCurationEvents(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: control.DefaultTenantID, PersonID: "person_1",
		Title: "release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "release")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"name": "release-ledger", "version_hash": "abc123def456789",
		"promoted": false, "blocked_reason": "the Skill is yours to change; apply it with /skills promote",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.candidate.created",
		Visibility: "task", Payload: payload, IdempotencyKey: "skill-candidate:evidence-1",
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store}
	notice, resolved := server.skillCurationNoticeForRun(ctx, control.DefaultTenantID, run.ID)
	if resolved == nil {
		t.Fatal("the run must resolve")
	}
	if !strings.Contains(notice, "release-ledger") || !strings.Contains(notice, "/skills promote") {
		t.Fatalf("the curation event must reach the notice: %q", notice)
	}

	// The constraint that must change the result: a run with no curation event
	// says nothing.
	quiet, err := store.StartRun(ctx, task, "cli", "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if notice, _ := server.skillCurationNoticeForRun(ctx, control.DefaultTenantID, quiet.ID); notice != "" {
		t.Fatalf("a run with no curation outcome must stay silent: %q", notice)
	}
}

// The prefix line already reported THAT the prompt prefix moved, which is the
// part a person can see in the bill. It could not say why, and the exposed tool
// set is the one prefix block that actually varies mid-run.
func TestPrefixChangeNamesTheToolCatalogDelta(t *testing.T) {
	breakdown := func(prefix string, added, removed []string) control.Event {
		p := map[string]interface{}{"provider_prefix_hash": prefix}
		if added != nil {
			p["tool_catalog_added"] = added
		}
		if removed != nil {
			p["tool_catalog_removed"] = removed
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return control.Event{Type: "provider.call.context_breakdown", Payload: raw}
	}
	// Newest first, matching the order the diag reader consumes.
	events := []control.Event{
		breakdown("hash_new", nil, []string{"web_search"}),
		breakdown("hash_old", nil, nil),
	}
	line := promptPrefixStabilityLine(events)
	if !strings.Contains(line, "changed between the last two calls") {
		t.Fatalf("unexpected line: %q", line)
	}
	if !strings.Contains(line, "-web_search") {
		t.Fatalf("the line must name what left the catalog: %q", line)
	}

	// The constraint that must change the result: a stable prefix says nothing
	// about the catalog, and a change with no recorded delta stays bare rather
	// than inventing a cause.
	stable := []control.Event{breakdown("same", nil, nil), breakdown("same", nil, nil)}
	if line := promptPrefixStabilityLine(stable); strings.Contains(line, "tool catalog") {
		t.Fatalf("a stable prefix must not report a catalog change: %q", line)
	}
	bare := []control.Event{breakdown("hash_new", nil, nil), breakdown("hash_old", nil, nil)}
	if line := promptPrefixStabilityLine(bare); strings.Contains(line, "tool catalog") {
		t.Fatalf("a change with no recorded delta must stay bare: %q", line)
	}
}
