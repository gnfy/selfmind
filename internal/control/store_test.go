package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreIdentityWorkspaceTaskFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount failed: %v", err)
	}
	if identity.TenantID != "tenant-a" || identity.PersonID == "" || identity.AccountID == "" {
		t.Fatalf("unexpected identity: %+v", identity)
	}

	again, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount second call failed: %v", err)
	}
	if again.PersonID != identity.PersonID || again.AccountID != identity.AccountID {
		t.Fatalf("identity was not stable: first=%+v second=%+v", identity, again)
	}

	ws, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "repo",
		LocalPath:     filepath.Join(t.TempDir(), "repo"),
	})
	if err != nil {
		t.Fatalf("RegisterWorkspace failed: %v", err)
	}
	currentWS, err := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatalf("CurrentWorkspace failed: %v", err)
	}
	if currentWS == nil || currentWS.ID != ws.ID {
		t.Fatalf("current workspace mismatch: %+v vs %+v", currentWS, ws)
	}

	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: ws.ID,
		Title:       "Implement sync",
		Channel:     "cli",
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	run, err := store.StartRun(ctx, task, "wechat", "continue task")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started"}); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}
	if err := store.RecordChannelMessage(ctx, *identity, "wechat", task.ID, "user", "progress?"); err != nil {
		t.Fatalf("RecordChannelMessage failed: %v", err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatalf("FinishRun failed: %v", err)
	}
	finishedTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask after FinishRun failed: %v", err)
	}
	if finishedTask.ActiveRunID != "" {
		t.Fatalf("active run was not cleared: %+v", finishedTask)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "running", "half done", []string{"finish tests"}); err != nil {
		t.Fatalf("UpdateTaskStatus failed: %v", err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{TaskID: task.ID, Summary: "handoff summary"}); err != nil {
		t.Fatalf("SaveHandoff failed: %v", err)
	}
	artifact, err := store.SaveArtifact(ctx, Artifact{
		TaskID: task.ID,
		RunID:  run.ID,
		Kind:   "file",
		Name:   "server.go",
		URI:    "internal/gateway/httpapi/server.go",
	})
	if err != nil {
		t.Fatalf("SaveArtifact failed: %v", err)
	}

	currentTask, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatalf("CurrentTask failed: %v", err)
	}
	if currentTask == nil || currentTask.ID != task.ID || currentTask.CurrentSummary != "half done" {
		t.Fatalf("current task mismatch: %+v", currentTask)
	}
	handoff, err := store.LatestHandoff(ctx, task.ID)
	if err != nil {
		t.Fatalf("LatestHandoff failed: %v", err)
	}
	if handoff == nil || handoff.Summary != "handoff summary" {
		t.Fatalf("handoff mismatch: %+v", handoff)
	}
	artifacts, err := store.ListTaskArtifacts(ctx, task.ID, 10)
	if err != nil {
		t.Fatalf("ListTaskArtifacts failed: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != artifact.ID {
		t.Fatalf("artifacts mismatch: %+v", artifacts)
	}
}

func TestStoreRuntimeDeliveryAndInterruptFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Long run",
		Channel:  "telegram",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "telegram", "do work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRunHeartbeat(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestRunCancel(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RunCancelRequested(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("expected cancel flag")
	}

	delivery, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       "weixin",
		PlatformUserID: "wx-user",
		Channel:        "wx-chat",
		TaskID:         task.ID,
		RunID:          run.ID,
		Content:        "done",
		MaxAttempts:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != delivery.ID {
		t.Fatalf("due deliveries = %+v", due)
	}
	if due[0].PlatformUserID != "wx-user" || due[0].Channel != "wx-chat" {
		t.Fatalf("delivery recipient was not preserved: %+v", due[0])
	}
	if err := store.MarkDeliveryAttempt(ctx, delivery.ID, false, "network token=secret", time.Now()); err != nil {
		t.Fatal(err)
	}

	count, err := store.MarkInterruptedRuns(ctx, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("interrupted count = %d, want 1", count)
	}
	updated, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ActiveRunID != "" || updated.Status != "interrupted" {
		t.Fatalf("task after interrupt = %+v", updated)
	}
}

func TestStoreApprovalFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Needs approval",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID:         identity.TenantID,
		PersonID:         identity.PersonID,
		TaskID:           task.ID,
		ActionType:       "shell",
		Payload:          []byte(`{"command":"rm file"}`),
		RequestedChannel: "cli",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest failed: %v", err)
	}
	if approval.ID == "" || approval.Status != "pending" {
		t.Fatalf("unexpected approval: %+v", approval)
	}
	pending, err := store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != approval.ID {
		t.Fatalf("pending approvals = %+v", pending)
	}
	approved, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "wechat", "")
	if err != nil {
		t.Fatalf("RespondApprovalRequest failed: %v", err)
	}
	if approved.Status != "approved" || approved.ApprovedChannel != "wechat" {
		t.Fatalf("unexpected approved request: %+v", approved)
	}
	pending, err = store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending approvals, got %+v", pending)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli", ""); err == nil {
		t.Fatal("expected duplicate response to fail")
	}
}

func TestApprovalGrantsScopes(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	tenant, person := identity.TenantID, identity.PersonID
	const pk = "exec:invokes dangerous command: chmod"

	// Nothing granted yet.
	if ok, err := store.IsApprovalGranted(ctx, tenant, person, "task-1", pk); err != nil || ok {
		t.Fatalf("expected no grant initially, ok=%v err=%v", ok, err)
	}

	// Task grant applies only to that task.
	if err := store.GrantApproval(ctx, "task", tenant, person, "task-1", pk); err != nil {
		t.Fatalf("GrantApproval task: %v", err)
	}
	if ok, _ := store.IsApprovalGranted(ctx, tenant, person, "task-1", pk); !ok {
		t.Fatalf("task-1 grant should be visible for task-1")
	}
	if ok, _ := store.IsApprovalGranted(ctx, tenant, person, "task-2", pk); ok {
		t.Fatalf("task-1 grant must NOT apply to task-2")
	}

	// Person grant applies across all tasks.
	if err := store.GrantApproval(ctx, "person", tenant, person, person, pk); err != nil {
		t.Fatalf("GrantApproval person: %v", err)
	}
	if ok, _ := store.IsApprovalGranted(ctx, tenant, person, "task-2", pk); !ok {
		t.Fatalf("person grant should apply to any task")
	}
	if ok, _ := store.IsApprovalGranted(ctx, tenant, person, "", pk); !ok {
		t.Fatalf("person grant should apply even with no task id")
	}

	// A different pattern key is still ungranted.
	if ok, _ := store.IsApprovalGranted(ctx, tenant, person, "task-2", "exec:invokes dangerous command: rm"); ok {
		t.Fatalf("unrelated class must not be granted")
	}

	// Grants are idempotent.
	if err := store.GrantApproval(ctx, "task", tenant, person, "task-1", pk); err != nil {
		t.Fatalf("re-grant should be idempotent: %v", err)
	}
}

// TestRespondApprovalRecordsDecisionScope pins that the grant scope round-trips
// on an approval and is dropped on a rejection.
func TestRespondApprovalRecordsDecisionScope(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	mk := func() *ApprovalRequest {
		a, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			ActionType: "tool_call", RequestedChannel: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a1 := mk()
	got, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, a1.ID, "approved", "cli", "task")
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionScope != "task" {
		t.Fatalf("approve should keep task scope, got %q", got.DecisionScope)
	}
	a2 := mk()
	got, err = store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, a2.ID, "rejected", "cli", "person")
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionScope != "" {
		t.Fatalf("reject must drop grant scope, got %q", got.DecisionScope)
	}
}

func TestCurrentTaskForChannelIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const tenant, person = "t1", "p1"
	taskA, err := store.CreateTask(ctx, TaskCreate{TenantID: tenant, PersonID: person, Title: "A", Channel: "cli-A"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: tenant, PersonID: person, Title: "B", Channel: "cli-B"})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Each channel resolves to its OWN task — no cross-session bleed, even
	// though the single per-person current_task pointer now points at B.
	gotA, err := store.CurrentTaskForChannel(ctx, tenant, person, "cli-A")
	if err != nil || gotA == nil || gotA.ID != taskA.ID {
		t.Fatalf("channel cli-A should resolve to task A, got %+v (err %v)", gotA, err)
	}
	gotB, err := store.CurrentTaskForChannel(ctx, tenant, person, "cli-B")
	if err != nil || gotB == nil || gotB.ID != taskB.ID {
		t.Fatalf("channel cli-B should resolve to task B, got %+v (err %v)", gotB, err)
	}

	// A finished task is no longer the channel's current task.
	if err := store.UpdateTaskStatus(ctx, tenant, taskA.ID, "done", "", nil); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if got, _ := store.CurrentTaskForChannel(ctx, tenant, person, "cli-A"); got != nil {
		t.Fatalf("finished task should not be the channel's current task, got %+v", got)
	}
}

func TestListAccountsByPerson(t *testing.T) {
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
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_1", "Alice WX"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "telegram", "42", "Alice TG"); err != nil {
		t.Fatal(err)
	}
	// A different person's account must not leak in.
	if _, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wxid_other", "Bob"); err != nil {
		t.Fatal(err)
	}

	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %+v", accounts)
	}
	platforms := map[string]string{}
	for _, a := range accounts {
		platforms[a.Platform] = a.PlatformUserID
		if a.PersonID != identity.PersonID {
			t.Fatalf("account for wrong person: %+v", a)
		}
	}
	if platforms["weixin"] != "wxid_1" || platforms["telegram"] != "42" || platforms["cli"] != "local" {
		t.Fatalf("platforms = %+v", platforms)
	}
}

func TestDeliveryKindSurvivesQueue(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID:   "default",
		PersonID:   "person_x",
		Platform:   "telegram",
		Channel:    "42",
		Content:    "Approval required",
		Kind:       "approval",
		ApprovalID: "apr_abc",
	}); err != nil {
		t.Fatal(err)
	}
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	// Typed rendering (Telegram approval buttons) must survive retry, so the
	// row round-trips kind + approval_id, not just the text.
	if due[0].Kind != "approval" || due[0].ApprovalID != "apr_abc" {
		t.Fatalf("kind = %q approval_id = %q", due[0].Kind, due[0].ApprovalID)
	}
}

// TestAccountLastSeenMigrationOnExistingDB proves the ensureColumn migration
// adds accounts.last_seen_at to a database created before the column existed,
// and that the recency helpers work on the migrated rows.
func TestAccountLastSeenMigrationOnExistingDB(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Build a pre-G0-b database by hand: accounts without last_seen_at.
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE accounts (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT NOT NULL,
	display_name TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, platform, platform_user_id)
);
INSERT INTO accounts (id, tenant_id, person_id, platform, platform_user_id, display_name, status, created_at, updated_at)
VALUES ('acct_old', 'default', 'person_old', 'weixin', 'wxid_old', 'Old Row', 'active', 1000, 1000);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore on legacy db: %v", err)
	}
	defer store.Close()

	accounts, err := store.ListAccountsByPerson(ctx, "default", "person_old")
	if err != nil {
		t.Fatalf("ListAccountsByPerson after migration: %v", err)
	}
	if len(accounts) != 1 || accounts[0].LastSeenAt != 0 {
		t.Fatalf("legacy row must survive with last_seen_at=0: %+v", accounts)
	}
	if err := store.TouchAccountLastSeen(ctx, "default", "acct_old"); err != nil {
		t.Fatalf("TouchAccountLastSeen after migration: %v", err)
	}
	account, err := store.MostRecentIMAccount(ctx, "default", "person_old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if account == nil || account.ID != "acct_old" || account.LastSeenAt == 0 {
		t.Fatalf("MostRecentIMAccount after touch = %+v", account)
	}
}

func TestMostRecentIMAccount(t *testing.T) {
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
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_1", "Alice WX"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "telegram", "42", "Alice TG"); err != nil {
		t.Fatal(err)
	}

	// Nothing seen yet: bind order breaks the tie, and cli never qualifies.
	account, err := store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if account == nil || account.Platform != "weixin" {
		t.Fatalf("unseen accounts must resolve by bind order, got %+v", account)
	}

	// The most recently seen account wins.
	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.Platform == "telegram" {
			if err := store.TouchAccountLastSeen(ctx, identity.TenantID, a.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	account, err = store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if account == nil || account.Platform != "telegram" {
		t.Fatalf("most recently seen must win, got %+v", account)
	}

	// The platform filter (delivery capability) skips unsupported platforms.
	account, err = store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, func(platform string) bool {
		return platform == "weixin"
	})
	if err != nil {
		t.Fatal(err)
	}
	if account == nil || account.Platform != "weixin" {
		t.Fatalf("filter must skip unsupported platforms, got %+v", account)
	}

	// No qualifying account -> nil, nil (caller drops the push, no error).
	account, err = store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, func(string) bool { return false })
	if err != nil || account != nil {
		t.Fatalf("no qualifying account must be nil,nil; got %+v, %v", account, err)
	}
}

func TestPersonSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if value, err := store.GetPersonSetting(ctx, "default", "person_1", "notify_platform"); err != nil || value != "" {
		t.Fatalf("unset value = %q, %v", value, err)
	}
	if err := store.SetPersonSetting(ctx, "default", "person_1", "notify_platform", "telegram"); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.GetPersonSetting(ctx, "default", "person_1", "notify_platform"); value != "telegram" {
		t.Fatalf("value = %q", value)
	}
	// Overwrite, then reset with an empty value.
	if err := store.SetPersonSetting(ctx, "default", "person_1", "notify_platform", "weixin"); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.GetPersonSetting(ctx, "default", "person_1", "notify_platform"); value != "weixin" {
		t.Fatalf("value = %q", value)
	}
	if err := store.SetPersonSetting(ctx, "default", "person_1", "notify_platform", ""); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.GetPersonSetting(ctx, "default", "person_1", "notify_platform"); value != "" {
		t.Fatalf("reset value = %q", value)
	}
	// Settings are tenant/person scoped.
	if err := store.SetPersonSetting(ctx, "default", "person_2", "notify_platform", "telegram"); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.GetPersonSetting(ctx, "default", "person_1", "notify_platform"); value != "" {
		t.Fatalf("cross-person leak: %q", value)
	}
}
