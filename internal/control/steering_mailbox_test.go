package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

// TestSteeringMailboxLifecycle pins the durability contract: accepted before
// acknowledgement, claimed on channel hand-off, consumed only on kernel
// proof, and consumption matching is per-run + content hash, oldest first.
func TestSteeringMailboxLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "cli",
		Content: "focus on the failing test first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Status != SteeringAccepted || msg.ContentHash == "" {
		t.Fatalf("accepted row = %+v", msg)
	}
	if err := store.MarkSteeringClaimed(ctx, identity.TenantID, msg.ID); err != nil {
		t.Fatal(err)
	}

	// Consumption is exact by mailbox ID; a wrong ID is a no-op.
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, "steer-missing"); err != nil || ok {
		t.Fatalf("wrong-id consume = %v %v", ok, err)
	}
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, msg.ID); err != nil || !ok {
		t.Fatalf("consume = %v %v", ok, err)
	}
	// Second consume of the same content finds nothing (row already consumed).
	if ok, _ := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, msg.ID); ok {
		t.Fatal("consumed row must not be consumable twice")
	}
	if leftovers, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10); err != nil || len(leftovers) != 0 {
		t.Fatalf("unconsumed after consume = %+v err=%v", leftovers, err)
	}
}

// TestSteeringDeferralAndBootRecovery pins the crash-window healing: an
// accepted-but-unconsumed row survives as durable next-turn work without a
// guessed task edge, deferral is idempotent via the queue key, and acknowledged guidance
// never disappears merely because the daemon was offline for a long time.
func TestSteeringDeferralAndBootRecovery(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	fresh, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "wechat-room",
		Platform: "weixin", PlatformUserID: "wx-user", WorkspaceID: "ws-1", ApprovalMode: "auto-edit",
		Content: "also update the changelog",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "telegram-room",
		Platform: "telegram", PlatformUserID: "tg-user", WorkspaceID: "ws-2", ApprovalMode: "read-only",
		Content: "an instruction from last week",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE steering_mailbox SET created_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).Unix(), stale.ID); err != nil {
		t.Fatal(err)
	}

	deferred, expired, err := store.RecoverSteeringAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deferred != 2 || expired != 0 {
		t.Fatalf("recovery = deferred %d expired %d, want 2/0", deferred, expired)
	}

	// The deferred row became exactly one queued item. Main never saw it, so the
	// queue must not guess that it belongs to the finished task.
	queued, err := store.NextQueued(ctx, identity.TenantID, identity.PersonID)
	if err != nil || queued == nil {
		t.Fatalf("queued: %+v err=%v", queued, err)
	}
	if queued.TaskID != "" || queued.Content != "an instruction from last week" {
		t.Fatalf("queued row = %+v", queued)
	}
	if queued.IdempotencyKey != "steering:"+stale.ID || queued.Platform != "telegram" || queued.PlatformUserID != "tg-user" || queued.WorkspaceID != "ws-2" || queued.ApprovalMode != "read-only" {
		t.Fatalf("idempotency key = %q", queued.IdempotencyKey)
	}
	// Replayed recovery is a no-op: nothing live remains, the queue key blocks
	// a duplicate row.
	if deferred, expired, err := store.RecoverSteeringAtBoot(ctx); err != nil || deferred != 0 || expired != 0 {
		t.Fatalf("second recovery = %d/%d err=%v", deferred, expired, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_queue WHERE idempotency_key LIKE 'steering:%'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("queued steering rows = %d, want 2", rows)
	}
	// The stale accepted row was deferred rather than silently expired.
	var staleStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM steering_mailbox WHERE id = ?`, stale.ID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != SteeringDeferred {
		t.Fatalf("stale status = %q", staleStatus)
	}
	_ = fresh
}

func TestIndependentTransferCannotDuplicateFinalizationDeferral(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Content: "unseen input at finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeferSteering(ctx, *msg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueSteeringAsIndependent(ctx, identity.TenantID, identity.PersonID, run.ID, msg.ID); err == nil {
		t.Fatal("a generic finalization deferral must not be reclassified into a second queue row")
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].IdempotencyKey != "steering:"+msg.ID {
		t.Fatalf("queued rows = %+v", queued)
	}
}

func TestSteeringExactConsumptionWithDuplicateContent(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	first, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash {
		t.Fatal("duplicate text must share a hash")
	}
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, second.ID); err != nil || !ok {
		t.Fatalf("consume second = %v %v", ok, err)
	}
	left, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != first.ID {
		t.Fatalf("leftovers = %+v", left)
	}
}

// TestSteeringExpireOnBackpressure: a back-pressure rejection terminates the
// row so it can never replay as a surprise.
func TestSteeringExpireOnBackpressure(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "cli", Content: "rejected guidance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSteeringExpired(ctx, identity.TenantID, msg.ID); err != nil {
		t.Fatal(err)
	}
	if deferred, expired, err := store.RecoverSteeringAtBoot(ctx); err != nil || deferred != 0 || expired != 0 {
		t.Fatalf("expired row leaked into recovery: %d/%d err=%v", deferred, expired, err)
	}
}

func steeringRootPaths(roots []executionenv.RootBinding) string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	return strings.Join(paths, ",")
}

// Input steered into another run keeps the roots its own request froze. When
// Main queues it as separate work, or the run ends before consuming it, the
// queued work runs with those roots. It used to take the run's, so that run's
// --add-dir directories reached work whose request never named them.
func TestSteeredInputQueuesWithItsOwnRoots(t *testing.T) {
	ctx := context.Background()
	store, identity, task, _ := newRecoveryFixture(t)
	runRoots := []executionenv.RootBinding{
		{Path: "/work/a", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace},
		{Path: "/data/shared", Role: executionenv.RootRoleAdditional, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir},
	}
	inputRoots := []executionenv.RootBinding{
		{Path: "/work/b", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace},
	}
	active, err := store.StartRunWithOptions(ctx, task, "session-a", "long task", StartRunOptions{ExecutionRoots: runRoots})
	if err != nil {
		t.Fatal(err)
	}
	for name, queue := range map[string]func(*SteeringMessage) (*QueuedTask, error){
		"queued by Main": func(m *SteeringMessage) (*QueuedTask, error) {
			return store.QueueSteeringAsIndependent(ctx, identity.TenantID, identity.PersonID, active.ID, m.ID)
		},
		"deferred at run end": func(m *SteeringMessage) (*QueuedTask, error) {
			if err := store.DeferSteering(ctx, *m); err != nil {
				return nil, err
			}
			return store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, "steering:"+m.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			msg, err := store.AcceptSteering(ctx, SteeringMessage{
				TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: active.ID, TaskID: task.ID,
				Channel: "session-b", WorkspaceID: "ws-b", ExecutionRoots: inputRoots, Content: "separate work, " + name,
			})
			if err != nil {
				t.Fatal(err)
			}
			queued, err := queue(msg)
			if err != nil || queued == nil {
				t.Fatalf("queued=%+v err=%v", queued, err)
			}
			if got := steeringRootPaths(queued.ExecutionRoots); got != "/work/b" || queued.WorkspaceID != "ws-b" {
				t.Fatalf("queued work roots=%q workspace=%q, want the input's own", got, queued.WorkspaceID)
			}
		})
	}
}

// Input steered without roots keeps the steered run's roots when it becomes
// separate work. The thin steering endpoint sends none, and an empty list was
// stored as "this input has no roots", so the queued work lost --add-dir.
func TestSteeringWithoutRootsKeepsTheRunsRoots(t *testing.T) {
	ctx := context.Background()
	store, identity, task, _ := newRecoveryFixture(t)
	runRoots := []executionenv.RootBinding{
		{Path: "/work/a", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace},
		{Path: "/data/shared", Role: executionenv.RootRoleAdditional, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir},
	}
	active, err := store.StartRunWithOptions(ctx, task, "cli", "long task", StartRunOptions{ExecutionRoots: runRoots})
	if err != nil {
		t.Fatal(err)
	}
	for name, queue := range map[string]func(*SteeringMessage) (*QueuedTask, error){
		"queued by Main": func(m *SteeringMessage) (*QueuedTask, error) {
			return store.QueueSteeringAsIndependent(ctx, identity.TenantID, identity.PersonID, active.ID, m.ID)
		},
		"deferred at run end": func(m *SteeringMessage) (*QueuedTask, error) {
			if err := store.DeferSteering(ctx, *m); err != nil {
				return nil, err
			}
			return store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, "steering:"+m.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			msg, err := store.AcceptSteering(ctx, SteeringMessage{
				TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: active.ID, TaskID: task.ID,
				Channel: "cli", Content: "separate work, " + name,
			})
			if err != nil {
				t.Fatal(err)
			}
			if msg.RootsRecorded {
				t.Fatal("an input sent without roots was recorded as having its own")
			}
			queued, err := queue(msg)
			if err != nil || queued == nil {
				t.Fatalf("queued=%+v err=%v", queued, err)
			}
			if got, want := steeringRootPaths(queued.ExecutionRoots), steeringRootPaths(runRoots); got != want {
				t.Fatalf("queued work roots=%q, want the run's %q", got, want)
			}
		})
	}
}

func TestExactSteeringPreservesTargetAcrossFinalStepAndRestart(t *testing.T) {
	for _, status := range []string{"interrupted", "done"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "task A", WorkspaceID: "ws-a", Channel: "session-a"})
			if err != nil {
				t.Fatal(err)
			}
			roots := []executionenv.RootBinding{{Path: "/work/a", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite}}
			run, err := store.StartRunWithOptions(ctx, task, "session-a", "A", StartRunOptions{ExecutionRoots: roots})
			if err != nil {
				t.Fatal(err)
			}
			m, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, ExactTarget: true, Channel: "session-b", WorkspaceID: "ws-b", ExecutionRoots: []executionenv.RootBinding{{Path: "/work/b"}}, Content: "add the missing criterion"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, run.ID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if n, _, err := store.RecoverSteeringAtBoot(ctx); err != nil || n != 1 {
				t.Fatalf("recovery=%d err=%v", n, err)
			}
			q, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "steering:"+m.ID)
			if err != nil || q == nil || q.TaskID != task.ID || q.WorkspaceID != "ws-a" || steeringRootPaths(q.ExecutionRoots) != "/work/a" {
				t.Fatalf("exact target lost: %+v err=%v", q, err)
			}
			wantParent := ""
			if status == "interrupted" {
				wantParent = run.ID
			}
			if q.ReplyToRunID != wantParent {
				t.Fatalf("parent=%q want=%q", q.ReplyToRunID, wantParent)
			}
			if n, _, err := store.RecoverSteeringAtBoot(ctx); err != nil || n != 0 {
				t.Fatalf("duplicate recovery=%d err=%v", n, err)
			}
		})
	}
}

func TestExactSteeringCannotMintAnotherPersonsTarget(t *testing.T) {
	ctx := context.Background()
	store, owner, _, run := newRecoveryFixture(t)
	if _, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: owner.TenantID, PersonID: "another-person", RunID: run.ID, ExactTarget: true, Content: "continue"}); err == nil {
		t.Fatal("foreign Run acquired exact reply provenance")
	}
}
