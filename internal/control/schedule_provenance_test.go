package control

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A tool receipt names its recorder only for the exact tool, event, text, and
// window, only from a Run its person started, and in any tenant.
func TestPersonsRecordingToolResultMatchesOnlyTheExactReceipt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	alice, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-bob", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := store.ResolveOrCreateAccount(ctx, "other", "telegram", "77", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	record := func(person *IdentityContext, origin, eventType, tool, result string) {
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "t", Channel: "c"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "c", "work")
		if err != nil {
			t.Fatal(err)
		}
		started, _ := json.Marshal(map[string]string{"origin": origin})
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started", Payload: started}); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]string{"tool": tool, "result": result})
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: eventType, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	record(alice, "", "tool.completed", "cron", "Cron job 7 (daily) added.")
	record(bob, "", "tool.completed", "terminal", "Cron job 8 (echo) added.")
	record(bob, "", "tool.output", "cron", "Cron job 9 (shaped) added.")
	record(bob, "cron", "tool.completed", "cron", "Cron job 10 (from a schedule) added.")
	record(elsewhere, "", "tool.completed", "cron", "Cron job 11 (other tenant) added.")
	now := time.Now()
	window := func(result string, from, to time.Time) []string {
		persons, err := store.PersonsRecordingToolResult(ctx, "cron", result, from, to)
		if err != nil {
			t.Fatal(err)
		}
		return persons
	}
	around := func(result string) []string { return window(result, now.Add(-time.Minute), now.Add(time.Minute)) }
	if got := around("Cron job 7 (daily) added."); len(got) != 1 || got[0] != alice.PersonID {
		t.Fatalf("receipt of job 7 = %v, want Alice", got)
	}
	for _, result := range []string{
		"Cron job 8 (echo) added.",             // another tool's identical text
		"Cron job 9 (shaped) added.",           // an event that is not a tool's result
		"Cron job 10 (from a schedule) added.", // a Run the daemon started
		"Cron job 7 (other) added.",            // a different job
	} {
		if got := around(result); len(got) != 0 {
			t.Fatalf("%q named %v", result, got)
		}
	}
	if got := window("Cron job 7 (daily) added.", now.Add(time.Hour), now.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("a receipt outside the window named %v", got)
	}
	if got := around("Cron job 11 (other tenant) added."); len(got) != 1 || got[0] != elsewhere.PersonID {
		t.Fatalf("a receipt from another tenant = %v, want Carol", got)
	}
	persons, err := store.PersonsWithAccounts(ctx, 5)
	if err != nil || len(persons) != 3 {
		t.Fatalf("persons with accounts = %v %v", persons, err)
	}
}

// The first recorded decision about an older schedule row is kept; only an
// explicit replacement of the expected decision changes it, once.
func TestLegacyScheduleVerdictKeepsTheFirstDecision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if got, err := store.RecordLegacyScheduleVerdict(ctx, "default", "legacy_schedule:1:100", "owner:p1"); err != nil || got != "owner:p1" {
		t.Fatalf("first record = %q %v", got, err)
	}
	if got, err := store.RecordLegacyScheduleVerdict(ctx, "default", "legacy_schedule:1:100", "owner:p2"); err != nil || got != "owner:p1" {
		t.Fatalf("second record = %q %v, want the first kept", got, err)
	}
	if ok, err := store.ReplaceLegacyScheduleVerdict(ctx, "default", "legacy_schedule:1:100", "unclaimed", "adopted:p3"); err != nil || ok {
		t.Fatalf("replacing a decision that is not the expected one = %v %v", ok, err)
	}
	if got, err := store.LegacyScheduleVerdict(ctx, "default", "legacy_schedule:1:100"); err != nil || got != "owner:p1" {
		t.Fatalf("after a refused replacement = %q %v", got, err)
	}
	if _, err := store.RecordLegacyScheduleVerdict(ctx, "default", "legacy_schedule:3:100", "unclaimed"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		if ok, err := store.ReplaceLegacyScheduleVerdict(ctx, "default", "legacy_schedule:3:100", "unclaimed", "adopted:p4"); err != nil || ok != want {
			t.Fatalf("replacement %d = %v %v, want %v", i, ok, err, want)
		}
	}
	if got, err := store.LegacyScheduleVerdict(ctx, "default", "legacy_schedule:1:101"); err != nil || got != "" {
		t.Fatalf("another row's decision = %q %v", got, err)
	}
}

// Of replacements racing for one decision, exactly one wins.
func TestConcurrentLegacyScheduleReplacementsHaveOneWinner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.RecordLegacyScheduleVerdict(ctx, "default", "legacy_schedule:2:100", "unclaimed"); err != nil {
		t.Fatal(err)
	}
	results := make(chan bool, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.ReplaceLegacyScheduleVerdict(ctx, "default", "legacy_schedule:2:100", "unclaimed", fmt.Sprintf("adopted:p%d", i))
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)
	won := 0
	for ok := range results {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d replacements won, want exactly one", won)
	}
}
