package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// verifyOnlyCronAdmission is the authority a scheduled, verification-only
// recovery was admitted with: every field a continuation used to lose.
func verifyOnlyCronAdmission() AdmissionSnapshot {
	return AdmissionSnapshot{
		Version: AdmissionSnapshotVersion,
		Strategy: AdmissionStrategy{
			Class: "external_lookup", ToolMode: "local_read", PlanPolicy: "optional",
			WebPolicy: "enabled", ChannelMode: "im", VerificationOnly: true,
		},
		RecoveryMode:               RunRecoveryModeVerifyOnly,
		ExecutionProfile:           "watch-finalization",
		WatchID:                    "watch_frozen",
		ApprovalMode:               "on-request",
		Origin:                     AdmissionOrigin{Kind: "cron", ID: "cron_job_nightly"},
		StandingGrantsAuthorizedAt: 1790000000,
		AdmittedAt:                 1789999000,
		Input:                      AdmissionInput{Kind: AdmissionInputChannelMessage, ID: "msg_original", Summary: "check the nightly export"},
	}
}

func admissionOwnerAndThread(t *testing.T, store *Store, platformUserID string) (*IdentityContext, *Task) {
	t.Helper()
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", platformUserID, platformUserID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "admission", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return owner, task
}

func TestAdmissionSnapshotIsStoredWithRunAndQueueRow(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	want := verifyOnlyCronAdmission()

	run, err := store.StartRunWithOptions(ctx, task, "cli", "Recover the interrupted run in verification-only mode.", StartRunOptions{Admission: want})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
	if err != nil || got != want {
		t.Fatalf("run admission = %+v err=%v, want %+v", got, err, want)
	}
	parent, err := store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, owner.PersonID, run.ID)
	if err != nil || parent != want {
		t.Fatalf("exact parent admission = %+v err=%v, want %+v", parent, err, want)
	}

	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Content: "summarize the overnight alerts", Class: QueueClassCron, Admission: want})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Admission != want {
		t.Fatalf("enqueue returned admission %+v", queued.Admission)
	}
	stored, err := store.GetQueued(ctx, owner.TenantID, queued.ID)
	if err != nil || stored == nil || stored.Admission != want {
		t.Fatalf("queue row admission = %+v err=%v", stored, err)
	}
	next, err := store.NextQueuedWhere(ctx, owner.TenantID, owner.PersonID, nil)
	if err != nil || next == nil || next.ID != queued.ID || next.Admission != want {
		t.Fatalf("drain candidate = %+v err=%v", next, err)
	}
}

// A person's own turn freezes a different, smaller authority. The same storage
// keeps it exactly, including an IM event reference and a summary of
// multi-byte text that is bounded without splitting a character.
func TestAdmissionSnapshotKeepsPersonTurnAndBoundsSummary(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	long := strings.Repeat("部署预览环境并核对结果", 40)
	admission := AdmissionSnapshot{
		Version:      AdmissionSnapshotVersion,
		Strategy:     AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"},
		ApprovalMode: "smart",
		Input:        AdmissionInput{Kind: AdmissionInputEvent, ID: "event_inbound", Summary: "  " + long + "  "},
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "ignored", StartRunOptions{Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got.Input.Summary) || utf8.RuneCountInString(got.Input.Summary) != admissionSummaryMaxRunes ||
		!strings.HasPrefix(long, got.Input.Summary) {
		t.Fatalf("summary not bounded on a rune boundary: %q", got.Input.Summary)
	}
	got.Input.Summary = ""
	admission.Input.Summary = ""
	if got != admission || got.Origin != (AdmissionOrigin{}) || got.StandingGrantsAuthorizedAt != 0 {
		t.Fatalf("person-turn admission = %+v, want %+v", got, admission)
	}

	// A replayed system enqueue returns the first row and its snapshot; the
	// second writer cannot replace the authority the first one froze.
	first, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Content: "continue", IdempotencyKey: "external-resource:run:effect", Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Content: "continue", IdempotencyKey: "external-resource:run:effect", Admission: verifyOnlyCronAdmission()})
	if err != nil || replay.ID != first.ID || replay.Admission != first.Admission {
		t.Fatalf("replayed enqueue = %+v err=%v, want first row %+v", replay, err, first)
	}
}

// Empty is "no snapshot": rows admitted without one keep today's behavior.
func TestMissingAdmissionSnapshotReadsAsNone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	run, err := store.StartRun(ctx, task, "cli", "legacy shape")
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (AdmissionSnapshot, error){
		func() (AdmissionSnapshot, error) { return store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID) },
		func() (AdmissionSnapshot, error) {
			return store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, owner.PersonID, run.ID)
		},
	} {
		got, err := read()
		if err != nil || !got.IsZero() {
			t.Fatalf("legacy run admission = %+v err=%v, want none", got, err)
		}
	}
	var raw string
	if err := store.db.QueryRowContext(ctx, `SELECT admission_json FROM runs WHERE id = ?`, run.ID).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("run without a snapshot stored %q err=%v", raw, err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "plain"})
	if err != nil || !queued.Admission.IsZero() {
		t.Fatalf("queue row without snapshot = %+v err=%v", queued, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT admission_json FROM task_queue WHERE id = ?`, queued.ID).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("queue row without a snapshot stored %q err=%v", raw, err)
	}
}

func TestExactParentAdmissionSnapshotNeverCrossesPersons(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	stranger, _ := admissionOwnerAndThread(t, store, "stranger")
	run, err := store.StartRunWithOptions(ctx, task, "cli", "work", StartRunOptions{Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() (AdmissionSnapshot, error){
		"another person": func() (AdmissionSnapshot, error) {
			return store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, stranger.PersonID, run.ID)
		},
		"another tenant": func() (AdmissionSnapshot, error) {
			return store.ExactParentAdmissionSnapshot(ctx, "elsewhere", owner.PersonID, run.ID)
		},
		"missing parent": func() (AdmissionSnapshot, error) {
			return store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, owner.PersonID, "run_missing")
		},
		"missing run": func() (AdmissionSnapshot, error) {
			return store.RunAdmissionSnapshot(ctx, owner.TenantID, "run_missing")
		},
	} {
		got, err := read()
		if !errors.Is(err, ErrAdmissionRunNotFound) || !got.IsZero() {
			t.Fatalf("%s: admission = %+v err=%v, want ErrAdmissionRunNotFound", name, got, err)
		}
	}
}

func TestAdmissionSnapshotRejectsMalformedWritesAtomically(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	valid := verifyOnlyCronAdmission()
	with := func(change func(*AdmissionSnapshot)) AdmissionSnapshot {
		a := valid
		change(&a)
		return a
	}
	for name, admission := range map[string]AdmissionSnapshot{
		"fields without a version":  with(func(a *AdmissionSnapshot) { a.Version = 0 }),
		"unknown version":           with(func(a *AdmissionSnapshot) { a.Version = AdmissionSnapshotVersion + 1 }),
		"bare version":              {Version: AdmissionSnapshotVersion},
		"no task class":             with(func(a *AdmissionSnapshot) { a.Strategy.Class = "" }),
		"no tool mode":              with(func(a *AdmissionSnapshot) { a.Strategy.ToolMode = "" }),
		"no plan policy":            with(func(a *AdmissionSnapshot) { a.Strategy.PlanPolicy = "" }),
		"no web policy":             with(func(a *AdmissionSnapshot) { a.Strategy.WebPolicy = "" }),
		"no channel mode":           with(func(a *AdmissionSnapshot) { a.Strategy.ChannelMode = " " }),
		"no approval mode":          with(func(a *AdmissionSnapshot) { a.ApprovalMode = "" }),
		"unknown task class":        with(func(a *AdmissionSnapshot) { a.Strategy.Class = "anything_goes" }),
		"unknown tool mode":         with(func(a *AdmissionSnapshot) { a.Strategy.ToolMode = "unrestricted" }),
		"unknown plan policy":       with(func(a *AdmissionSnapshot) { a.Strategy.PlanPolicy = "sometimes" }),
		"unknown web policy":        with(func(a *AdmissionSnapshot) { a.Strategy.WebPolicy = "always" }),
		"approval mode alias":       with(func(a *AdmissionSnapshot) { a.ApprovalMode = "yolo" }),
		"unknown execution profile": with(func(a *AdmissionSnapshot) { a.ExecutionProfile = "host" }),
		"unknown recovery mode":     with(func(a *AdmissionSnapshot) { a.RecoveryMode = "replay_everything" }),
		"unknown origin kind":       with(func(a *AdmissionSnapshot) { a.Origin.Kind = "webhook" }),
		"input kind without id":     with(func(a *AdmissionSnapshot) { a.Input.ID = "" }),
		"input id without kind":     with(func(a *AdmissionSnapshot) { a.Input.Kind = "" }),
		"unknown input kind":        with(func(a *AdmissionSnapshot) { a.Input.Kind = "prose" }),
		"origin id without kind":    with(func(a *AdmissionSnapshot) { a.Origin.Kind = "" }),
		"negative grant cutoff":     with(func(a *AdmissionSnapshot) { a.StandingGrantsAuthorizedAt = -1 }),
		"unbounded identifier":      with(func(a *AdmissionSnapshot) { a.WatchID = strings.Repeat("w", 257) }),
		"unbounded wide characters": with(func(a *AdmissionSnapshot) { a.Origin.ID = strings.Repeat("界", 86) }),
	} {
		if _, err := store.StartRunWithOptions(ctx, task, "cli", name, StartRunOptions{Admission: admission}); err == nil {
			t.Fatalf("%s: run accepted a malformed admission", name)
		}
		if _, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: name, Admission: admission}); err == nil {
			t.Fatalf("%s: queue accepted a malformed admission", name)
		}
	}
	var runs, rows int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM runs), (SELECT COUNT(*) FROM task_queue)`).Scan(&runs, &rows); err != nil || runs != 0 || rows != 0 {
		t.Fatalf("rejected admissions left runs=%d queue=%d err=%v", runs, rows, err)
	}
}

// An unreadable snapshot is never "no snapshot": falling back to today's
// reconstruction would hand a verify-only continuation its mutation tools.
func TestUnreadableAdmissionSnapshotFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	run, err := store.StartRunWithOptions(ctx, task, "cli", "work", StartRunOptions{Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "later", Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"newer version":    `{"version":2,"strategy":{"class":"general_task","tool_mode":"full","plan_policy":"optional","web_policy":"disabled","channel_mode":"im"},"approval_mode":"smart"}`,
		"unknown field":    `{"version":1,"strategy":{"class":"general_task","tool_mode":"local_read","plan_policy":"optional","web_policy":"disabled","channel_mode":"im","verification_only":true},"approval_mode":"smart","widen":true}`,
		"bare version":     `{"version":1}`,
		"no approval mode": `{"version":1,"strategy":{"class":"general_task","tool_mode":"local_read","plan_policy":"optional","web_policy":"disabled","channel_mode":"im"}}`,
		"no tool mode":     `{"version":1,"strategy":{"class":"general_task","plan_policy":"optional","web_policy":"disabled","channel_mode":"im"},"approval_mode":"smart"}`,
		"malformed":        `{"version":1`,
		"zero version":     `{"version":0}`,
		"wrong field type": `{"version":"1"}`,
		"trailing data":    `{"version":1} {"version":1,"strategy":{"tool_mode":"full"}}`,
	} {
		if _, err := store.db.ExecContext(ctx, `UPDATE runs SET admission_json = ? WHERE id = ?`, raw, run.ID); err != nil {
			t.Fatal(err)
		}
		for reader, read := range map[string]func() (AdmissionSnapshot, error){
			"run": func() (AdmissionSnapshot, error) { return store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID) },
			"exact parent": func() (AdmissionSnapshot, error) {
				return store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, owner.PersonID, run.ID)
			},
		} {
			got, err := read()
			if !errors.Is(err, ErrAdmissionSnapshotUnreadable) || !got.IsZero() {
				t.Fatalf("%s %s: admission = %+v err=%v, want ErrAdmissionSnapshotUnreadable", name, reader, got, err)
			}
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE task_queue SET admission_json = ? WHERE id = ?`, raw, queued.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := store.GetQueued(ctx, owner.TenantID, queued.ID); err != nil || got == nil ||
			!errors.Is(got.AdmissionError, ErrAdmissionSnapshotUnreadable) || !got.Admission.IsZero() {
			t.Fatalf("%s queue row = %+v err=%v, want a row marked unreadable", name, got, err)
		}
	}
}
