package control

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// WorkTimeline is the public behavioral seam for work-history presentation.
// These tests intentionally use the real SQLite Store: callers should not need
// to know which persisted facts produce a listed Thread.
func TestWorkTimelineKeepsOrdinaryInteractionSearchableButUnlisted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "你好，你是什么模型？",
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Fatalf("ordinary interaction appeared in work list: %+v", listed)
	}
	search, err := timeline.Search(ctx, identity.TenantID, identity.PersonID, "什么模型", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(search) != 1 || search[0].ID != thread.ID || search[0].Kind != ThreadKindInteraction || search[0].Visibility != ThreadVisibilityUnlisted {
		t.Fatalf("search=%+v, want unlisted interaction %s", search, thread.ID)
	}
}

func TestWorkTimelinePromotesInteractionWithoutChangingRunHistory(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "设计一个功能",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "设计一个功能")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := timeline.Promote(ctx, identity.TenantID, identity.PersonID, thread.ID); err != nil {
		t.Fatal(err)
	}

	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Threads) != 1 || listed.Threads[0].ID != thread.ID || listed.Threads[0].Kind != ThreadKindWork {
		t.Fatalf("listed=%+v, want promoted thread %s", listed, thread.ID)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "done" {
		t.Fatalf("promotion changed run history: run=%+v err=%v", storedRun, err)
	}
}

func TestWorkTimelineDerivesAndDismissesAttentionWithoutRewritingRun(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "发布服务",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "发布服务")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}

	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attention) != 1 || attention[0].Thread.ID != thread.ID || attention[0].RunID != run.ID || attention[0].Activity != ThreadActivityResumable {
		t.Fatalf("attention=%+v, want exact resumable run %s", attention, run.ID)
	}
	changed, err := timeline.DismissAttention(ctx, identity.TenantID, identity.PersonID, thread.ID)
	if err != nil || changed != 1 {
		t.Fatalf("dismiss changed=%d err=%v", changed, err)
	}
	attention, err = timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 0 {
		t.Fatalf("attention after dismiss=%+v err=%v", attention, err)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "waiting_user" {
		t.Fatalf("dismiss changed run history: run=%+v err=%v", storedRun, err)
	}
}

// A newer Run supersedes its Thread's older parked state: the older Run stops
// being resumable Attention and the Thread settles, while a pending control
// object on that older Run still surfaces through its own signal.
func TestWorkTimelineNewerRunSupersedesOlderParkedRun(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release then follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.StartRun(ctx, thread.legacyTask(), "cli", "confirm release")
	if err != nil {
		t.Fatal(err)
	}
	setRunStartedAtForTest(t, store, older.ID, time.Now().Add(-2*time.Minute))
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: older.ID, RunStatus: "waiting_user", TaskID: thread.ID,
		TaskStatus: "waiting_user", Summary: "waiting for confirmation", Channel: "cli",
		Event: Event{Type: "run.waiting_user", Payload: []byte(`{"status":"waiting_user"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].RunID != older.ID || attention[0].Activity != ThreadActivityResumable ||
		attention[0].RunStatus != "waiting_user" || attention[0].Channel != "cli" {
		t.Fatalf("parked run attention=%+v err=%v", attention, err)
	}

	newer, err := store.StartRun(ctx, thread.legacyTask(), "cli", "never mind, it shipped")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, newer.ID, "done"); err != nil {
		t.Fatal(err)
	}
	attention, err = timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 0 {
		t.Fatalf("superseded parked run stayed resumable: %+v err=%v", attention, err)
	}
	settled, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewSettled})
	if err != nil || settled.Total != 1 || settled.Threads[0].ID != thread.ID {
		t.Fatalf("superseded thread is not settled: %+v err=%v", settled, err)
	}
	storedOlder, err := store.GetRun(ctx, identity.TenantID, older.ID)
	if err != nil || storedOlder == nil || storedOlder.Status != "waiting_user" {
		t.Fatalf("supersession rewrote run history: %+v err=%v", storedOlder, err)
	}

	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: older.ID,
	}); err != nil {
		t.Fatal(err)
	}
	attention, err = timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].RunID != older.ID || attention[0].Activity != ThreadActivityNeedsAttention {
		t.Fatalf("pending approval on an older run must still surface: %+v err=%v", attention, err)
	}
}

func TestRunPlanDeterministicallyPromotesItsInteractionThread(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "设计并实现功能",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "设计并实现功能")
	if err != nil {
		t.Fatal(err)
	}
	// A lone snapshotted step is not evidence of ongoing work: update_plan is
	// for genuinely multi-step work, and a one-shot answer that records its
	// single step must not enter the work list.
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "implementation", []RunPlanStepInput{{
		Step: "implement behavior", Status: "in_progress",
	}}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Fatalf("a single-step plan must not promote its interaction: %+v", listed)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "implementation", []RunPlanStepInput{
		{Step: "design the change", Status: "completed"},
		{Step: "implement behavior", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	listed, err = timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || listed.Threads[0].ID != thread.ID {
		t.Fatalf("multi-step planned work was not promoted: %+v", listed)
	}
}

func TestAutomaticPromotionNeverReopensArchivedThread(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "archived work",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := timeline.Archive(ctx, identity.TenantID, identity.PersonID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := timeline.Promote(ctx, identity.TenantID, identity.PersonID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "late evidence", []RunPlanStepInput{{
		Step: "do not reopen", Status: "in_progress",
	}}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, identity.TenantID, thread.ID)
	if err != nil || stored == nil || stored.Visibility != ThreadVisibilityArchived {
		t.Fatalf("automatic promotion reopened archive: %+v err=%v", stored, err)
	}
}

func TestResumableFinalizationPromotesItsInteractionThread(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release after approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "waiting_user",
		TaskID: thread.ID, TaskStatus: "waiting_user", Summary: "waiting for approval",
		Channel: "cli", Event: Event{Type: "run.waiting_user", Payload: []byte(`{"status":"waiting_user"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || listed.Threads[0].ID != thread.ID {
		t.Fatalf("resumable work was not promoted: %+v", listed)
	}
}

func TestDirectAnswerFinalizationKeepsItsInteractionUnlisted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "你是什么模型？",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: thread.ID,
		TaskStatus: "done", Summary: "I am SelfMind.", Channel: "cli",
		Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Fatalf("direct answer inflated work list: %+v", listed)
	}
	search, err := timeline.Search(ctx, identity.TenantID, identity.PersonID, "什么模型", 10)
	if err != nil || len(search) != 1 || search[0].ID != thread.ID {
		t.Fatalf("direct answer history search=%+v err=%v", search, err)
	}
}

func TestWorkTimelineInterruptedRunNeedsWorkEvidenceForAttention(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	startInterrupted := func(title string, evidence bool) *Run {
		thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, thread.legacyTask(), "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if evidence {
			if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
				RunID: run.ID, ToolCallID: "call-1", ToolName: "terminal", RetryClass: "side_effect",
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
			t.Fatal(err)
		}
		return run
	}
	startInterrupted("crashed while answering", false)
	worked := startInterrupted("crashed while editing", true)

	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].RunID != worked.ID ||
		attention[0].Activity != ThreadActivityResumable || attention[0].RunStatus != "interrupted" {
		t.Fatalf("attention=%+v err=%v, want only the interrupted run with work evidence %s", attention, err, worked.ID)
	}
}

func TestWorkTimelineAttentionForChannelPrefersSameChannel(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	park := func(title, channel string, startedAt time.Time) *Run {
		thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: channel,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, thread.legacyTask(), channel, title)
		if err != nil {
			t.Fatal(err)
		}
		setRunStartedAtForTest(t, store, run.ID, startedAt)
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		return run
	}
	now := time.Now()
	cliRun := park("cli work", "cli", now.Add(-2*time.Minute))
	imRun := park("telegram work", "telegram", now)

	plain, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(plain) != 2 || plain[0].RunID != imRun.ID || plain[1].RunID != cliRun.ID {
		t.Fatalf("plain attention=%+v err=%v, want newest first", plain, err)
	}
	preferred, err := timeline.AttentionForChannel(ctx, identity.TenantID, identity.PersonID, "cli", 10)
	if err != nil || len(preferred) != 2 || preferred[0].RunID != cliRun.ID || preferred[0].Channel != "cli" ||
		preferred[1].RunID != imRun.ID || preferred[1].Channel != "telegram" {
		t.Fatalf("cli-preferred attention=%+v err=%v", preferred, err)
	}
	unpreferred, err := timeline.AttentionForChannel(ctx, identity.TenantID, identity.PersonID, "", 10)
	if err != nil || len(unpreferred) != 2 || unpreferred[0].RunID != imRun.ID {
		t.Fatalf("empty channel preference must equal plain attention: %+v err=%v", unpreferred, err)
	}
}

func TestWorkTimelineAttentionHonorsLargeLimit(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	for i := 0; i < 25; i++ {
		thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: fmt.Sprintf("parked %02d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
	}
	all, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 200)
	if err != nil || len(all) != 25 {
		t.Fatalf("limit 200 returned %d items err=%v, want every parked run", len(all), err)
	}
	defaulted, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 0)
	if err != nil || len(defaulted) != 20 {
		t.Fatalf("default limit returned %d items err=%v, want 20", len(defaulted), err)
	}
}

func TestWorkTimelineDismissRefusesLiveControlObjects(t *testing.T) {
	tests := []struct {
		name     string
		create   func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run)
		activity string
	}{
		{
			name: "pending approval",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
				}); err != nil {
					t.Fatal(err)
				}
			},
			activity: ThreadActivityNeedsAttention,
		},
		{
			name: "pending clarification",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
					Question: "continue?",
				}); err != nil {
					t.Fatal(err)
				}
			},
			activity: ThreadActivityNeedsAttention,
		},
		{
			name: "live watcher",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateExternalWatch(ctx, ExternalWatch{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
					Channel: "cli", CWD: t.TempDir(), Command: "check", SuccessPattern: "done",
					TimeoutAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			},
			activity: ThreadActivityMonitoring,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, timeline := newTimelineFixture(t)
			thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID, Title: tt.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, thread.legacyTask(), "cli", tt.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			tt.create(ctx, t, store, identity, thread, run)
			before, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
			if err != nil || len(before) != 1 || before[0].RunID != run.ID || before[0].Activity != tt.activity {
				t.Fatalf("attention before dismissal=%+v err=%v", before, err)
			}
			if changed, err := timeline.DismissAttention(ctx, identity.TenantID, identity.PersonID, thread.ID); changed != 0 || !errors.Is(err, ErrAttentionPendingControl) {
				t.Fatalf("thread dismissal changed=%d err=%v, want ErrAttentionPendingControl", changed, err)
			}
			if dismissed, err := timeline.DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, thread.ID, run.ID); dismissed || !errors.Is(err, ErrAttentionPendingControl) {
				t.Fatalf("run dismissal dismissed=%v err=%v, want ErrAttentionPendingControl", dismissed, err)
			}
			after, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
			if err != nil || len(after) != 1 || after[0].RunID != run.ID || after[0].Activity != tt.activity {
				t.Fatalf("refused dismissal changed attention: before=%+v after=%+v err=%v", before, after, err)
			}
			var dismissedAt int64
			if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(attention_dismissed_at, 0) FROM runs WHERE id = ?`, run.ID).Scan(&dismissedAt); err != nil || dismissedAt != 0 {
				t.Fatalf("refused dismissal stamped the run: dismissed_at=%d err=%v", dismissedAt, err)
			}
		})
	}
}

func TestFinalizationPromotionIgnoresNonWorkLedgerRows(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "which work should continue?",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	for i, tool := range []string{"finish_run", "work_select"} {
		if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
			RunID: run.ID, ToolCallID: fmt.Sprintf("call-%d", i), ToolName: tool, RetryClass: "idempotent",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: thread.ID,
		TaskStatus: "done", Summary: "selected the earlier work", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil || listed.Total != 0 {
		t.Fatalf("lifecycle-only ledger rows promoted the interaction: %+v err=%v", listed, err)
	}
}

func TestFinalizationPromotesRunWithSideEffectLedgerRow(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "run the migration",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-1", ToolName: "terminal", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: thread.ID,
		TaskStatus: "done", Summary: "migration applied", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil || listed.Total != 1 || listed.Threads[0].ID != thread.ID || listed.Threads[0].Kind != ThreadKindWork {
		t.Fatalf("side-effect run was not promoted: %+v err=%v", listed, err)
	}
}

func TestInterruptedFinalizationWithoutEvidenceStaysUnlistedAndQuiet(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "你是谁？",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "interrupted", TaskID: thread.ID,
		TaskStatus: "interrupted", Summary: "daemon restarted", Channel: "cli",
		Event: Event{Type: "run.interrupted", Payload: []byte(`{"status":"interrupted"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil || listed.Total != 0 {
		t.Fatalf("evidence-free interrupted run promoted the interaction: %+v err=%v", listed, err)
	}
	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 0 {
		t.Fatalf("evidence-free interrupted run demands attention: %+v err=%v", attention, err)
	}
	search, err := timeline.Search(ctx, identity.TenantID, identity.PersonID, "你是谁", 10)
	if err != nil || len(search) != 1 || search[0].ID != thread.ID {
		t.Fatalf("interrupted interaction left retained history: %+v err=%v", search, err)
	}
}

func TestControlObjectCreationPromotesRunningInteraction(t *testing.T) {
	tests := []struct {
		name   string
		create func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run)
	}{
		{
			name: "approval",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
					ActionType: "tool_call",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "clarification",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
					Question: "which environment?",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "external watch",
			create: func(ctx context.Context, t *testing.T, store *Store, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateExternalWatch(ctx, ExternalWatch{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
					Channel: "cli", CWD: t.TempDir(), Command: "check", SuccessPattern: "done",
					TimeoutAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, timeline := newTimelineFixture(t)
			thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID, Title: tt.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, thread.legacyTask(), "cli", tt.name)
			if err != nil {
				t.Fatal(err)
			}
			if listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed}); err != nil || listed.Total != 0 {
				t.Fatalf("interaction listed before any evidence: %+v err=%v", listed, err)
			}
			tt.create(ctx, t, store, identity, thread, run)
			listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
			if err != nil || listed.Total != 1 || listed.Threads[0].ID != thread.ID ||
				listed.Threads[0].Kind != ThreadKindWork || listed.Threads[0].Visibility != ThreadVisibilityListed {
				t.Fatalf("control object did not promote the running interaction: %+v err=%v", listed, err)
			}
			stored, err := store.GetRun(ctx, identity.TenantID, run.ID)
			if err != nil || stored == nil || stored.Status != "running" {
				t.Fatalf("promotion touched run execution state: %+v err=%v", stored, err)
			}
		})
	}
}

func TestProjectInteractionTaskNeverLowersPromotedVisibility(t *testing.T) {
	tests := []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, store *Store, timeline *WorkTimeline, identity *IdentityContext, thread *Thread, run *Run)
	}{
		{
			name: "pinned thread",
			setup: func(ctx context.Context, t *testing.T, store *Store, timeline *WorkTimeline, identity *IdentityContext, thread *Thread, run *Run) {
				if err := store.SetTaskPinned(ctx, identity.TenantID, thread.ID, true); err != nil {
					t.Fatal(err)
				}
				if err := timeline.Promote(ctx, identity.TenantID, identity.PersonID, thread.ID); err != nil {
					t.Fatal(err)
				}
				if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "run with work evidence",
			setup: func(ctx context.Context, t *testing.T, store *Store, timeline *WorkTimeline, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
					TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: thread.ID, RunID: run.ID,
				}); err != nil {
					t.Fatal(err)
				}
				if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "listed by parked finalization",
			setup: func(ctx context.Context, t *testing.T, store *Store, timeline *WorkTimeline, identity *IdentityContext, thread *Thread, run *Run) {
				if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
					Identity: *identity, RunID: run.ID, RunStatus: "waiting_user", TaskID: thread.ID,
					TaskStatus: "waiting_user", Summary: "needs a decision", Channel: "cli",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, timeline := newTimelineFixture(t)
			thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID, Title: tt.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, thread.legacyTask(), "cli", tt.name)
			if err != nil {
				t.Fatal(err)
			}
			tt.setup(ctx, t, store, timeline, identity, thread, run)
			if err := store.ProjectInteractionTask(ctx, identity.TenantID, identity.PersonID, thread.ID, run.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetTask(ctx, identity.TenantID, thread.ID)
			if err != nil || stored == nil || stored.Kind != ThreadKindWork || stored.Visibility != ThreadVisibilityListed {
				t.Fatalf("projection lowered promoted visibility: %+v err=%v", stored, err)
			}
		})
	}
	t.Run("tool-free answer stays unlisted", func(t *testing.T) {
		ctx := context.Background()
		store, identity, timeline := newTimelineFixture(t)
		thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "你好",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if err := store.ProjectInteractionTask(ctx, identity.TenantID, identity.PersonID, thread.ID, run.ID); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetTask(ctx, identity.TenantID, thread.ID)
		if err != nil || stored == nil || stored.Kind != ThreadKindInteraction || stored.Visibility != ThreadVisibilityUnlisted {
			t.Fatalf("tool-free interaction = %+v err=%v", stored, err)
		}
	})
}

func newTimelineFixture(t *testing.T) (*Store, *IdentityContext, *WorkTimeline) {
	t.Helper()
	store := newTestStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	return store, identity, NewWorkTimeline(store)
}

// setRunStartedAtForTest gives Runs created within one second a deterministic
// causal order; production Runs are far enough apart for started_at to decide.
func setRunStartedAtForTest(t *testing.T, store *Store, runID string, at time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `UPDATE runs SET started_at = ? WHERE id = ?`, at.Unix(), runID); err != nil {
		t.Fatal(err)
	}
}

// A ledger row is written when a call id is claimed and only then dispatched.
// A row still sitting in the claimed-but-never-dispatched state touched
// nothing, so it is not work evidence; a dispatched one is.
func TestWorkEvidenceIgnoresNeverDispatchedLedgerRow(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "claimed but never dispatched",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "claimed but never dispatched")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tool_ledger
		(tenant_id, run_id, tool_call_id, tool_name, args_hash, retry_class, effect_id, plan_version,
		 plan_step_id, strategy, effect_class, environment_generation, status, created_at, updated_at)
		VALUES (?, ?, 'call-planned', 'terminal', 'hash', 'side_effect', '', 0, '', '', '', 0, 'planned', 1, 1)`,
		identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, TaskID: thread.ID, RunStatus: "interrupted", Summary: "cut off",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, ThreadQuery{View: ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Fatalf("a never-dispatched claim must not be work evidence: %+v", listed)
	}
	items, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("an evidence-free interruption must not be attention: %+v", items)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tool_ledger SET status = 'completed' WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	items, err = timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RunID != run.ID {
		t.Fatalf("a dispatched side-effect row is work evidence: %+v", items)
	}
}

// Explicit /resume is the person saying this work is live again, so pinning it
// also lifts that Run's Attention dismissal (docs/work-timeline.md).
func TestPinResumeSelectionClearsAttentionDismissal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release check",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "release check")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := timeline.DismissAttentionRun(ctx, identity.TenantID, identity.PersonID, thread.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if items, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10); err != nil || len(items) != 0 {
		t.Fatalf("dismissal did not take effect: %+v err=%v", items, err)
	}
	if err := store.PinResumeSelection(ctx, identity.TenantID, identity.PersonID, thread.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	items, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RunID != run.ID {
		t.Fatalf("explicit resume must restore the dismissed run's attention: %+v", items)
	}
	var dismissedAt int64
	var dismissedBy string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(attention_dismissed_at, 0), COALESCE(attention_dismissed_by, '')
		FROM runs WHERE id = ?`, run.ID).Scan(&dismissedAt, &dismissedBy); err != nil {
		t.Fatal(err)
	}
	if dismissedAt != 0 || dismissedBy != "" {
		t.Fatalf("dismissal marker survived an explicit resume: at=%d by=%q", dismissedAt, dismissedBy)
	}
}

// seedParkedAttentionRuns parks one Run per Thread with strictly increasing
// start times and returns their ids in Attention order (newest activity
// first), so a paging assertion can name the exact Run every ordinal means.
func seedParkedAttentionRuns(t *testing.T, store *Store, identity *IdentityContext, count int) []string {
	t.Helper()
	ctx := context.Background()
	timeline := NewWorkTimeline(store)
	base := time.Now().Add(-4 * time.Hour)
	ranked := make([]string, count)
	for i := 0; i < count; i++ {
		thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Title: fmt.Sprintf("parked release %03d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, thread.legacyTask(), "cli", thread.Title)
		if err != nil {
			t.Fatal(err)
		}
		setRunStartedAtForTest(t, store, run.ID, base.Add(time.Duration(i)*time.Minute))
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		ranked[count-1-i] = run.ID
	}
	return ranked
}

// Attention pages at the database. A person with more work than one page holds
// must still be able to reach item 101: the total is the real count, a later
// page carries the exact Runs the ranking names, and an unpaged caller that
// asks for more than one page is not silently truncated.
func TestWorkTimelineAttentionPagesPastOneHundredItems(t *testing.T) {
	ctx := context.Background()
	store, identity, timeline := newTimelineFixture(t)
	const parked = 130
	ranked := seedParkedAttentionRuns(t, store, identity, parked)

	var paged []string
	for offset := 0; offset < parked; offset += 100 {
		items, total, err := timeline.AttentionPage(ctx, identity.TenantID, identity.PersonID, "", 100, offset)
		if err != nil {
			t.Fatal(err)
		}
		if total != parked {
			t.Fatalf("offset %d reported total %d, want the true %d", offset, total, parked)
		}
		for _, item := range items {
			paged = append(paged, item.RunID)
		}
	}
	if len(paged) != parked {
		t.Fatalf("paging returned %d runs, want %d", len(paged), parked)
	}
	for i := range ranked {
		if paged[i] != ranked[i] {
			t.Fatalf("page ordering diverged at %d: got %s want %s", i, paged[i], ranked[i])
		}
	}
	second, total, err := timeline.AttentionPage(ctx, identity.TenantID, identity.PersonID, "", 100, 100)
	if err != nil || total != parked || len(second) != parked-100 || second[0].RunID != ranked[100] {
		t.Fatalf("page two=%d items total=%d err=%v, want %d items starting at %s",
			len(second), total, err, parked-100, ranked[100])
	}
	empty, total, err := timeline.AttentionPage(ctx, identity.TenantID, identity.PersonID, "", 100, parked)
	if err != nil || total != parked || len(empty) != 0 {
		t.Fatalf("offset past the end=%+v total=%d err=%v, want no items and the true total", empty, total, err)
	}
	all, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, parked)
	if err != nil || len(all) != parked {
		t.Fatalf("unpaged attention returned %d items err=%v, want the %d asked for", len(all), err, parked)
	}
}

// The open task view and /diag statistics read the same paged Attention set:
// the page is fetched, not sliced, so Total, HasMore, and the open count stay
// honest past one page.
func TestOpenTaskViewAndStatsReportTrueAttentionTotal(t *testing.T) {
	ctx := context.Background()
	store, identity, _ := newTimelineFixture(t)
	const parked = 130
	ranked := seedParkedAttentionRuns(t, store, identity, parked)

	first, err := store.QueryTasks(ctx, identity.TenantID, identity.PersonID, TaskQuery{View: "open", Limit: 100})
	if err != nil || first.Total != parked || len(first.Tasks) != 100 || !first.HasMore() {
		t.Fatalf("open page one=%d tasks total=%d hasMore=%v err=%v", len(first.Tasks), first.Total, first.HasMore(), err)
	}
	second, err := store.QueryTasks(ctx, identity.TenantID, identity.PersonID, TaskQuery{View: "open", Limit: 100, Offset: 100})
	if err != nil || second.Total != parked || len(second.Tasks) != parked-100 || second.HasMore() {
		t.Fatalf("open page two=%d tasks total=%d hasMore=%v err=%v", len(second.Tasks), second.Total, second.HasMore(), err)
	}
	for i, task := range second.Tasks {
		if task.ResumeRunID != ranked[100+i] {
			t.Fatalf("open page two item %d resumes %s, want %s", i, task.ResumeRunID, ranked[100+i])
		}
	}
	stats, err := store.ReadTaskGovernanceStats(ctx, identity.TenantID, identity.PersonID)
	if err != nil || stats.Open != parked {
		t.Fatalf("governance stats open=%d err=%v, want the true %d", stats.Open, err, parked)
	}
}
