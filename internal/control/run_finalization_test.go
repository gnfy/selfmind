package control

import (
	"context"
	"testing"
	"time"
)

func TestMaterializeRunFinalizationIsAtomicAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "atomic finalization",
		Channel:  "cli-session",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli-session", "finish this")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	input := RunFinalization{
		Identity:           *identity,
		RunID:              run.ID,
		RunStatus:          "done",
		TaskID:             task.ID,
		TaskStatus:         "done",
		Summary:            "finished atomically",
		NextSteps:          []string{"ship it"},
		Channel:            "cli-session",
		AssistantContent:   "All work is complete.",
		AnalyzerVersion:    2,
		MaintenancePayload: `{"kind":"post-run"}`,
		Handoff: Handoff{
			Summary:      "finished atomically",
			DoneItems:    []string{"implemented"},
			NextSteps:    []string{"ship it"},
			ChangedFiles: []string{"main.go"},
			TestStatus:   "passed",
		},
		Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}
	first, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil {
		t.Fatalf("first finalization: %v", err)
	}
	second, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil {
		t.Fatalf("replayed finalization: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay returned event %q; want %q", second.ID, first.ID)
	}

	assertCount := func(table, where string, args ...any) {
		t.Helper()
		var count int
		query := "SELECT COUNT(*) FROM " + table
		if where != "" {
			query += " WHERE " + where
		}
		if err := store.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("%s count = %d; want 1", table, count)
		}
	}
	assertCount("task_events", "run_id = ? AND type = 'run.finished'", run.ID)
	assertCount("channel_messages", "task_id = ? AND role = 'assistant'", task.ID)
	assertCount("task_handoffs", "task_id = ?", task.ID)
	assertCount("maintenance_jobs", "run_id = ? AND analyzer_version = 2", run.ID)

	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "done" || storedRun.FinishedAt == nil {
		t.Fatalf("stored run = %+v, %v; want terminal done", storedRun, err)
	}
	storedTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || storedTask == nil {
		t.Fatalf("stored task = %+v, %v", storedTask, err)
	}
	if storedTask.Status != "done" || storedTask.ActiveRunID != "" || storedTask.CurrentSummary != input.Summary {
		t.Fatalf("stored task = %+v; want done with cleared active run", storedTask)
	}
}

func TestMaterializeRunFinalizationRollsBackMismatchedTask(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}
	first, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, first, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity:   *identity,
		RunID:      run.ID,
		TaskID:     second.ID,
		RunStatus:  "done",
		TaskStatus: "done",
		Summary:    "must roll back",
	})
	if err == nil {
		t.Fatal("mismatched run/task finalization unexpectedly succeeded")
	}
	storedRun, getErr := store.GetRun(ctx, identity.TenantID, run.ID)
	if getErr != nil || storedRun == nil || storedRun.Status != "running" || storedRun.FinishedAt != nil {
		t.Fatalf("run changed after rollback: %+v, %v", storedRun, getErr)
	}
	storedSecond, getErr := store.GetTask(ctx, identity.TenantID, second.ID)
	if getErr != nil || storedSecond == nil || storedSecond.Status != "new" || storedSecond.CurrentSummary != "" {
		t.Fatalf("task changed after rollback: %+v, %v", storedSecond, getErr)
	}
	var sideEffects int
	if err := store.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM task_events WHERE run_id = ?) +
		        (SELECT COUNT(*) FROM maintenance_jobs WHERE run_id = ?)`,
		run.ID, run.ID).Scan(&sideEffects); err != nil {
		t.Fatalf("count rollback side effects: %v", err)
	}
	if sideEffects != 0 {
		t.Fatalf("rollback left %d side effects; want 0", sideEffects)
	}
}

func TestMaterializeRunFinalizationSuppressesDuplicateLogicalEffect(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "watch closure"})
	firstRun, _ := store.StartRun(ctx, task, "cli", "first finalization")
	effectKey := "external-watch:watch_1:r1:finalization"
	makeInput := func(run *Run, summary string) RunFinalization {
		return RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID,
			TaskStatus: "done", Summary: summary, Channel: "cli", AssistantContent: summary,
			Handoff: Handoff{Summary: summary}, AnalyzerVersion: 1,
			MaintenancePayload: `{}`, EffectKey: effectKey,
			Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)},
		}
	}
	if _, err := store.MaterializeRunFinalization(ctx, makeInput(firstRun, "first result")); err != nil {
		t.Fatal(err)
	}
	secondRun, err := store.StartRun(ctx, task, "cli", "duplicate finalization")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, makeInput(secondRun, "duplicate result")); err != nil {
		t.Fatal(err)
	}
	for table, query := range map[string]string{
		"handoffs":    `SELECT COUNT(*) FROM task_handoffs WHERE task_id = ?`,
		"messages":    `SELECT COUNT(*) FROM channel_messages WHERE task_id = ? AND role = 'assistant'`,
		"maintenance": `SELECT COUNT(*) FROM maintenance_jobs WHERE run_id IN (?, ?)`,
		"receipt":     `SELECT COUNT(*) FROM effect_receipts WHERE effect_key = ?`,
	} {
		var count int
		var err error
		switch table {
		case "maintenance":
			err = store.db.QueryRowContext(ctx, query, firstRun.ID, secondRun.ID).Scan(&count)
		case "receipt":
			err = store.db.QueryRowContext(ctx, query, effectKey).Scan(&count)
		default:
			err = store.db.QueryRowContext(ctx, query, task.ID).Scan(&count)
		}
		if err != nil || count != 1 {
			t.Fatalf("%s count = %d, %v; want 1", table, count, err)
		}
	}
	storedTask, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if storedTask == nil || storedTask.CurrentSummary != "first result" {
		t.Fatalf("duplicate effect overwrote task: %+v", storedTask)
	}
	owned, err := store.EffectOwnedByRun(ctx, identity.TenantID, effectKey, secondRun.ID)
	if err != nil || owned {
		t.Fatalf("duplicate run owns effect = %v, %v", owned, err)
	}
	storedRun, _ := store.GetRun(ctx, identity.TenantID, secondRun.ID)
	if storedRun == nil || storedRun.Status != "done" {
		t.Fatalf("duplicate run was not settled: %+v", storedRun)
	}
}

// TestMaterializeRunFinalizationAlwaysCommitsDerivedCard pins the P2 rule that
// replaced weak-attach card protection: every finalization writes the run's
// summary/next steps and the REDUCED status. There is no deferred lifecycle
// left — each root run owns its task, so no unrelated run can be "weakly
// attached" to an established card anymore.
func TestMaterializeRunFinalizationAlwaysCommitsDerivedCard(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Work line"})
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "in_progress", "old summary", []string{"old next"}); err != nil {
		t.Fatal(err)
	}
	run, _ := store.StartRun(ctx, task, "cli", "finish the work")
	_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID,
		TaskStatus: "done", Summary: "new run summary", NextSteps: []string{"new next"},
		Handoff:         Handoff{Summary: "new run summary"},
		AnalyzerVersion: 1, MaintenancePayload: `{}`, Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if stored.Status != "done" {
		t.Fatalf("finalization must commit the derived status: %+v", stored)
	}
	if stored.CurrentSummary != "new run summary" || len(stored.NextSteps) != 1 || stored.NextSteps[0] != "new next" {
		t.Fatalf("finalization must update the task card: %+v", stored)
	}
}

func TestMaterializeRunFinalizationSynthesizesOutcomeBeforeTerminal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "direct answer"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "answer this")
	if err != nil {
		t.Fatal(err)
	}
	input := RunFinalization{
		Identity:   *identity,
		RunID:      run.ID,
		TaskID:     task.ID,
		RunStatus:  "done",
		TaskStatus: "done",
		Summary:    "answered",
		Event: Event{Type: "run.finished", Payload: []byte(
			`{"outcome":{"status":"done","completion_reason":"completed","summary":"answered"}}`)},
	}
	if _, err := store.MaterializeRunFinalization(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, input); err != nil {
		t.Fatalf("replay: %v", err)
	}

	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var outcome, terminal *Event
	for i := range events {
		switch events[i].Type {
		case "run.outcome":
			outcome = &events[i]
		case "run.finished":
			terminal = &events[i]
		}
	}
	if outcome == nil || terminal == nil {
		t.Fatalf("events = %#v; want outcome and terminal", events)
	}
	if outcome.Cursor >= terminal.Cursor {
		t.Fatalf("outcome cursor %d must precede terminal cursor %d", outcome.Cursor, terminal.Cursor)
	}
	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_events WHERE run_id = ? AND type = 'run.outcome'`, run.ID,
	).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outcome count = %d, err=%v", count, err)
	}
}

func TestMaterializeRunFinalizationReducesDurableTaskBlockers(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(context.Context, *Store, IdentityContext, Task, Run)
		status string
	}{
		{
			name: "pending approval",
			setup: func(ctx context.Context, store *Store, identity IdentityContext, task Task, run Run) {
				if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID,
					TaskID: task.ID, RunID: run.ID, ActionType: "tool_call",
				}); err != nil {
					t.Fatal(err)
				}
			},
			status: "waiting_user",
		},
		{
			name: "pending external watch",
			setup: func(ctx context.Context, store *Store, identity IdentityContext, task Task, run Run) {
				if _, err := store.CreateExternalWatch(ctx, ExternalWatch{
					TenantID: identity.TenantID, PersonID: identity.PersonID,
					TaskID: task.ID, RunID: run.ID, Channel: "cli",
					CWD: t.TempDir(), Command: "check", SuccessPattern: "done",
					TimeoutAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			},
			status: "waiting_external",
		},
		{
			name: "queued watch finalization",
			setup: func(ctx context.Context, store *Store, identity IdentityContext, task Task, run Run) {
				if _, err := store.EnqueueQueued(ctx, QueuedTask{
					TenantID: identity.TenantID, PersonID: identity.PersonID,
					TaskID: task.ID, Channel: "cli", Platform: "cli",
					Content: "finalize watch", IdempotencyKey: "external-watch:test:r1:finalization",
				}); err != nil {
					t.Fatal(err)
				}
			},
			status: "waiting_finalization",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID, Title: tt.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "work")
			if err != nil {
				t.Fatal(err)
			}
			tt.setup(ctx, store, *identity, *task, *run)
			if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
				Identity: *identity, RunID: run.ID, RunStatus: "done",
				TaskID: task.ID, TaskStatus: "done", Summary: "run finished",
			}); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != tt.status {
				t.Fatalf("task status=%q want %q", stored.Status, tt.status)
			}
		})
	}
}

func TestUpdateTaskStatusUsesReducerAndExplicitCancellationWins(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "derived setter",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, ActionType: "tool_call",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "done", "attempted completion", nil); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "waiting_user" {
		t.Fatalf("task status=%q want waiting_user", stored.Status)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "cancelled", "Cancelled by user.", nil); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "cancelled" {
		t.Fatalf("task status=%q want cancelled", stored.Status)
	}
}

func TestMaterializeRunFinalizationPreservesUnresumedPriorRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "resume safety",
	})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.StartRun(ctx, task, "cli", "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: prior.ID, RunStatus: "interrupted",
		TaskID: task.ID, TaskStatus: "interrupted", Summary: "needs continuation",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartRun(ctx, task, "cli", "unrelated follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: current.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "follow-up done",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "interrupted" {
		t.Fatalf("task status=%q want interrupted", stored.Status)
	}
}

func TestMaterializeRunFinalizationOnlyClaimReleasesUnresolvedRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "claim releases waits"})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.StartRun(ctx, task, "cli", "needs verification")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: prior.ID, RunStatus: "verification_partial",
		TaskID: task.ID, TaskStatus: "verification_partial", Summary: "verification remains",
	}); err != nil {
		t.Fatal(err)
	}
	// An UNRELATED completion cannot silently release the parked run: the
	// unclaimed verification_partial run stays the wait authority (§10.3).
	unrelated, err := store.StartRun(ctx, task, "cli", "separate follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: unrelated.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "unrelated answer",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "verification_partial" {
		t.Fatalf("task status=%q, unclaimed run must keep verification_partial", stored.Status)
	}
	// The CLAIMING child's completion releases the wait atomically.
	claiming, err := store.StartRunWithOptions(ctx, task, "cli", "verified", StartRunOptions{ParentRunID: prior.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: claiming.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "verified",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "done" {
		t.Fatalf("task status=%q want done after the claim completed", stored.Status)
	}
}

func TestMaterializeRunFinalizationPrefersActiveLifecycleOverPriorInterruption(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(context.Context, *Store, IdentityContext, Task, Run)
		status string
	}{
		{
			name: "external watch",
			setup: func(ctx context.Context, store *Store, identity IdentityContext, task Task, run Run) {
				if _, err := store.CreateExternalWatch(ctx, ExternalWatch{
					TenantID: identity.TenantID, PersonID: identity.PersonID,
					TaskID: task.ID, RunID: run.ID, Channel: "cli",
					CWD: t.TempDir(), Command: "check", SuccessPattern: "done",
					TimeoutAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			},
			status: "waiting_external",
		},
		{
			name: "watch finalization",
			setup: func(ctx context.Context, store *Store, identity IdentityContext, task Task, run Run) {
				if _, err := store.EnqueueQueued(ctx, QueuedTask{
					TenantID: identity.TenantID, PersonID: identity.PersonID,
					TaskID: task.ID, Channel: "cli", Platform: "cli",
					Content: "finalize watch", IdempotencyKey: "external-watch:combined:r1:finalization",
				}); err != nil {
					t.Fatal(err)
				}
			},
			status: "waiting_finalization",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID, Title: tt.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			prior, err := store.StartRun(ctx, task, "cli", "prior attempt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
				Identity: *identity, RunID: prior.ID, RunStatus: "interrupted",
				TaskID: task.ID, TaskStatus: "interrupted", Summary: "needs continuation",
			}); err != nil {
				t.Fatal(err)
			}
			current, err := store.StartRun(ctx, task, "cli", "current work")
			if err != nil {
				t.Fatal(err)
			}
			tt.setup(ctx, store, *identity, *task, *current)
			if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
				Identity: *identity, RunID: current.ID, RunStatus: "done",
				TaskID: task.ID, TaskStatus: "done", Summary: "current run finished",
			}); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != tt.status {
				t.Fatalf("task status=%q want %q", stored.Status, tt.status)
			}
		})
	}
}

func TestMaterializeRunFinalizationClosesDeliberatelyResumedRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "resume completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.StartRun(ctx, task, "cli", "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: prior.ID, RunStatus: "interrupted",
		TaskID: task.ID, TaskStatus: "interrupted", Summary: "needs continuation",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ParentRunID: prior.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current.ParentRunID != prior.ID {
		t.Fatalf("parent edge = %q want %s", current.ParentRunID, prior.ID)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: current.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "done" {
		t.Fatalf("task status=%q want done", stored.Status)
	}
}

// TestWorkKeysNeverResolveRunOwnership pins the ownership rule after the
// forward parent edge landed: display work keys (same or different) have no
// claim authority — an ambiguous label keeps both unfinished runs visible and
// unclaimed, and completing an unrelated run cannot erase them.
func TestWorkKeysNeverResolveRunOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "shared display label",
	})
	if err != nil {
		t.Fatal(err)
	}

	startInterrupted := func(summary, workKey string) *Run {
		run, startErr := store.StartRun(ctx, task, "cli", summary)
		if startErr != nil {
			t.Fatal(startErr)
		}
		if setErr := store.SetRunWorkKey(ctx, identity.TenantID, run.ID, workKey); setErr != nil {
			t.Fatal(setErr)
		}
		if _, finishErr := store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: "interrupted",
			TaskID: task.ID, TaskStatus: "interrupted", Summary: summary,
		}); finishErr != nil {
			t.Fatal(finishErr)
		}
		return run
	}
	matching := startInterrupted("RUQX-401 needs continuation", "RUQX-401")
	unrelated := startInterrupted("RUQX-402 needs continuation", "RUQX-402")
	// Both unfinished runs stay visible as candidates: nothing may auto-pick
	// between them by work key.
	unresolved, err := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("unresolved=%d want both ambiguous runs visible", len(unresolved))
	}
	current, err := store.StartRun(ctx, task, "cli", "continue RUQX-401")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunWorkKey(ctx, identity.TenantID, current.ID, "RUQX-401"); err != nil {
		t.Fatal(err)
	}

	var matchingOwner, unrelatedOwner string
	if err := store.db.QueryRowContext(ctx, `SELECT resumed_by_run_id FROM task_runs WHERE id = ?`, matching.ID).Scan(&matchingOwner); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT resumed_by_run_id FROM task_runs WHERE id = ?`, unrelated.ID).Scan(&unrelatedOwner); err != nil {
		t.Fatal(err)
	}
	if matchingOwner != "" || unrelatedOwner != "" {
		t.Fatalf("matching owner=%q unrelated owner=%q", matchingOwner, unrelatedOwner)
	}

	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: current.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "RUQX-401 completed",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "interrupted" {
		t.Fatalf("unrelated unfinished work was erased: task status=%q", stored.Status)
	}
}

// TestSameKeyUnfinishedWorkSurvivesCompletion: even when every run shares one
// display work key, completing a new run cannot close the label over the
// other unfinished, unclaimed runs — final reduction keeps the strongest
// remaining wait state.
func TestSameKeyUnfinishedWorkSurvivesCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-381 mixed work",
	})
	if err != nil {
		t.Fatal(err)
	}

	var unfinished []*Run
	for _, item := range []struct {
		summary string
		status  string
	}{
		{"ALTER TABLE remains unexecuted", "verification_partial"},
		{"GCP release still waiting", "waiting_user"},
	} {
		run, startErr := store.StartRunWithWorkKey(ctx, task, "cli", item.summary, "RUQX-381")
		if startErr != nil {
			t.Fatal(startErr)
		}
		if _, finishErr := store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: item.status,
			TaskID: task.ID, TaskStatus: item.status, Summary: item.summary,
		}); finishErr != nil {
			t.Fatal(finishErr)
		}
		unfinished = append(unfinished, run)
	}

	current, err := store.StartRunWithWorkKey(ctx, task, "cli", "continue RUQX-381", "RUQX-381")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range unfinished {
		var owner string
		if err := store.db.QueryRowContext(ctx, `SELECT resumed_by_run_id FROM task_runs WHERE id = ?`, run.ID).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			t.Fatalf("unfinished run %s was claimed by %s", run.ID, owner)
		}
	}

	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: current.ID, RunStatus: "done",
		TaskID: task.ID, TaskStatus: "done", Summary: "release completed",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "waiting_user" {
		t.Fatalf("same-key unfinished work was erased: task status=%q want waiting_user", stored.Status)
	}
}

func TestStartRunWithWorkKeyIsAtomicAndReadable(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-410"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithWorkKey(ctx, task, "cli", "continue", "ruqx-410")
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkKey != "RUQX-410" {
		t.Fatalf("created work key=%q", run.WorkKey)
	}
	stored, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.WorkKey != "RUQX-410" {
		t.Fatalf("stored run=%+v", stored)
	}
}
