package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

func newRoutedChoice(t *testing.T, store *Store, action string, task *Task, run *Run) (*PendingTurnChoice, TurnChoiceWorkRoute) {
	t.Helper()
	ctx := context.Background()
	option := TurnChoiceOption{Key: "1", Label: "selected", Action: action}
	if run != nil {
		option.TaskID, option.RunID = task.ID, run.ID
	}
	choice, err := store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{
		TenantID: "default", PersonID: "person", Channel: "im", RequestJSON: `{"kind":"multi_active","content":"original"}`,
		Options: []TurnChoiceOption{option, {Key: "2", Label: "new", Action: "new"}}, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	route := TurnChoiceWorkRoute{
		TenantID: "default", PersonID: "person", ChoiceID: choice.ID, OptionKey: "1", RequestJSON: choice.RequestJSON,
		Queued: QueuedTask{TenantID: "default", PersonID: "person", Channel: "im", Platform: "weixin", PlatformUserID: "wx", Content: "original"},
	}
	if run != nil {
		route.Steering = SteeringMessage{TenantID: "default", PersonID: "person", RunID: run.ID, TaskID: task.ID,
			Channel: "im", Platform: "weixin", PlatformUserID: "wx", Content: "original"}
	}
	return choice, route
}

func TestRouteTurnChoiceRollsBackClaimWhenDestinationWriteFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	choice, route := newRoutedChoice(t, store, "new", nil, nil)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_choice_queue BEFORE INSERT ON task_queue
		WHEN NEW.idempotency_key LIKE 'choice:%' BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RoutePendingTurnChoice(ctx, route); err == nil {
		t.Fatal("expected injected queue failure")
	}
	peek, err := store.PeekPendingTurnChoice(ctx, "default", "person", choice.ID, time.Now(), time.Hour)
	if err != nil || peek.RequestJSON != choice.RequestJSON {
		t.Fatalf("accepted input lost after failed write: %+v err=%v", peek, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_choice_queue`); err != nil {
		t.Fatal(err)
	}
	result, err := store.RoutePendingTurnChoice(ctx, route)
	if err != nil || result.Queued == nil || result.Queued.Content != "original" {
		t.Fatalf("retry did not route exactly once: %+v err=%v", result, err)
	}
	if _, err := store.RoutePendingTurnChoice(ctx, route); !errors.Is(err, ErrTurnChoiceNotFound) {
		t.Fatalf("duplicate answer err=%v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_queue WHERE idempotency_key = ?`, "choice:"+choice.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("queued copies=%d err=%v", count, err)
	}
}

func TestRouteTurnChoiceTracksTargetStateAtomically(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", Title: "A", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "work A")
	if err != nil {
		t.Fatal(err)
	}
	_, liveRoute := newRoutedChoice(t, store, "steer", task, run)
	live, err := store.RoutePendingTurnChoice(ctx, liveRoute)
	if err != nil || live.Steering == nil || live.Steering.RunID != run.ID || live.Queued != nil {
		t.Fatalf("live target route=%+v err=%v", live, err)
	}
	if err := store.FinishRun(ctx, "default", run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	_, lateRoute := newRoutedChoice(t, store, "steer", task, run)
	late, err := store.RoutePendingTurnChoice(ctx, lateRoute)
	if err != nil || late.Queued == nil || late.Queued.TaskID != task.ID || late.Queued.ReplyToRunID != "" || late.Steering != nil {
		t.Fatalf("finished target route=%+v err=%v", late, err)
	}
}

func TestRouteTurnChoiceRollsBackWhenSteeringWriteFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", Title: "A", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "work A")
	if err != nil {
		t.Fatal(err)
	}
	choice, route := newRoutedChoice(t, store, "steer", task, run)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_choice_steer BEFORE INSERT ON steering_mailbox
		WHEN NEW.id LIKE 'steer_choice_%' BEGIN SELECT RAISE(ABORT, 'injected mailbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RoutePendingTurnChoice(ctx, route); err == nil {
		t.Fatal("expected injected mailbox failure")
	}
	if pending, err := store.PeekPendingTurnChoice(ctx, "default", "person", choice.ID, time.Now(), time.Hour); err != nil || pending.RequestJSON != choice.RequestJSON {
		t.Fatalf("choice consumed without mailbox: %+v err=%v", pending, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_choice_steer`); err != nil {
		t.Fatal(err)
	}
	result, err := store.RoutePendingTurnChoice(ctx, route)
	if err != nil || result.Steering == nil || result.Steering.Content != "original" {
		t.Fatalf("retry lost steering: %+v err=%v", result, err)
	}
}

func TestRouteTurnChoiceSingleWinnerAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	choice, route := newRoutedChoice(t, first, "new", nil, nil)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, store := range []*Store{first, second} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, routeErr := store.RoutePendingTurnChoice(context.Background(), route)
			results <- routeErr
		}(store)
	}
	wg.Wait()
	close(results)
	success := 0
	for routeErr := range results {
		if routeErr == nil {
			success++
		} else if !errors.Is(routeErr, ErrTurnChoiceNotFound) {
			t.Fatalf("route err=%v", routeErr)
		}
	}
	if success != 1 {
		t.Fatalf("successful routes=%d", success)
	}
	var count int
	if err := first.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_queue WHERE idempotency_key = ?`, "choice:"+choice.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("queued copies=%d err=%v", count, err)
	}
}

func TestChoiceBackedSteeringKeepsExactTargetAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", WorkspaceID: "ws-a", Title: "A", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	rootsA := []executionenv.RootBinding{{Path: "/work/a", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite}}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "work A", StartRunOptions{ExecutionRoots: rootsA})
	if err != nil {
		t.Fatal(err)
	}
	choice, route := newRoutedChoice(t, store, "steer", task, run)
	route.Steering.WorkspaceID = "ws-b"
	route.Steering.ExecutionRoots = []executionenv.RootBinding{{Path: "/work/b", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite}}
	result, err := store.RoutePendingTurnChoice(ctx, route)
	if err != nil || result.Steering == nil {
		t.Fatalf("accept exact choice: %+v err=%v", result, err)
	}
	if err := store.FinishRun(ctx, "default", run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	deferred, _, err := restarted.RecoverSteeringAtBoot(ctx)
	if err != nil || deferred != 1 {
		t.Fatalf("boot recovery deferred=%d err=%v", deferred, err)
	}
	queued, err := restarted.GetQueuedByIdempotencyKey(ctx, "default", "steering:"+result.Steering.ID)
	if err != nil || queued == nil || queued.TaskID != task.ID || queued.ReplyToRunID != run.ID || queued.WorkspaceID != "ws-a" ||
		len(queued.ExecutionRoots) != 1 || queued.ExecutionRoots[0].Path != "/work/a" {
		t.Fatalf("exact choice lost target or scope after restart: %+v err=%v", queued, err)
	}
	receipt, err := restarted.RoutedTurnChoice(ctx, "default", "person", choice.ID, "1")
	if err != nil || receipt.Queued == nil || receipt.Queued.ID != queued.ID || receipt.Steering != nil {
		t.Fatalf("repeat answer did not report deferred destination: %+v err=%v", receipt, err)
	}
	deferred, _, err = restarted.RecoverSteeringAtBoot(ctx)
	if err != nil || deferred != 0 {
		t.Fatalf("repeated recovery replayed choice: deferred=%d err=%v", deferred, err)
	}
}
