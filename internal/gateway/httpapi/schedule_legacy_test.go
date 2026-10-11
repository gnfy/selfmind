package httpapi

// Schedules stored by an older release. That release's cron tool stored what
// the model sent: a job with no endpoint became channel "cli" with no
// deliver_to, which its executor ran as the account cli:local — an account the
// command line no longer uses (it is cli:$USER), and one any Run, a chat's
// included, could produce. These tests write rows exactly as that tool did and
// fire them through the production executor with its scheduler.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/modelchange"
)

// installation is a daemon whose command line runs as cli:cwill, with that
// person's WeChat bound, its scheduler, and the directory of its databases.
type installation struct {
	dir    string
	daemon *Server
	store  *control.Store
	sender *syncedSender
	sched  *cron.Scheduler
	cronDB *sql.DB
	me     *control.IdentityContext
}

func newInstallation(t *testing.T) *installation {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	t.Cleanup(func() { store.Close() })
	provider := newSlowLLMProvider("Scheduled turn finished.")
	provider.releaseNow()
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test agent", 1, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	sender := &syncedSender{}
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{})
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sched := cron.NewScheduler(db, nil)
	if err := sched.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	me, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "cwill", "Me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, me.TenantID, me.PersonID, "weixin", "wx-me", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	return &installation{dir: dir, daemon: daemon, store: store, sender: sender, sched: sched, cronDB: db, me: me}
}

// legacyRow stores a job the way the older cron tool did: channel defaulted
// to the platform or "cli", deliver_to only when the model sent one, and no
// authorisation instant.
func (in *installation) legacyRow(t *testing.T, name, platform, deliverTo, channel string) cron.CronJob {
	t.Helper()
	if deliverTo != "" && (channel == "" || strings.EqualFold(channel, platform)) {
		channel = deliverTo
	}
	if channel == "" {
		channel = firstNonEmptyString(platform, "cli")
	}
	result, err := in.cronDB.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled)
		VALUES (?, '0 9 * * *', 'summarize my day', 'default', ?, ?, ?, 0, 0, 1)`, name, channel, platform, deliverTo)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return in.job(t, id)
}

func (in *installation) job(t *testing.T, id int64) cron.CronJob {
	t.Helper()
	jobs, err := in.sched.ListJobs(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == id {
			return job
		}
	}
	t.Fatalf("job %d is not stored", id)
	return cron.CronJob{}
}

// receipt records what the creating Run's cron tool answered, as that Run's
// event history keeps it.
func (in *installation) receipt(t *testing.T, creator *control.IdentityContext, job cron.CronJob) {
	t.Helper()
	in.receiptFrom(t, creator, job, "")
}

// receiptFrom is receipt from a Run with the given origin: "" for the
// person's own turn, "cron" for one a schedule started.
func (in *installation) receiptFrom(t *testing.T, creator *control.IdentityContext, job cron.CronJob, origin string) {
	t.Helper()
	ctx := context.Background()
	task, err := in.store.CreateTask(ctx, control.TaskCreate{TenantID: creator.TenantID, PersonID: creator.PersonID,
		Title: "set up a schedule", Channel: creator.PlatformUserID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := in.store.StartRun(ctx, task, creator.PlatformUserID, "remind me every morning")
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		started, _ := json.Marshal(map[string]string{"origin": origin})
		if _, err := in.store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "run.started",
			Visibility: "task", Payload: started}); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{"tool": "cron", "result": fmt.Sprintf("Cron job %d (%s) added.", job.ID, job.Name)})
	if _, err := in.store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed",
		Visibility: "task", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := in.store.FinishRun(ctx, creator.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
}

// stranger is another person who once messaged the WeChat bot and ran work.
func (in *installation) stranger(t *testing.T) *control.IdentityContext {
	t.Helper()
	ctx := context.Background()
	stranger, err := in.store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-stranger", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	task, err := in.store.CreateTask(ctx, control.TaskCreate{TenantID: stranger.TenantID, PersonID: stranger.PersonID,
		Title: "hello", Channel: "wx-stranger"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := in.store.StartRun(ctx, task, "wx-stranger", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.store.FinishRun(ctx, stranger.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	return stranger
}

func (in *installation) fire(t *testing.T, id int64) api.MessageResponse {
	t.Helper()
	resp, err := NewCronExecutor(in.daemon, in.sched).Fire(context.Background(), in.job(t, id))
	if err != nil {
		t.Fatalf("fire job %d: %v", id, err)
	}
	return resp
}

func (in *installation) runs(t *testing.T, person *control.IdentityContext) int {
	t.Helper()
	runs, err := in.store.ListRecentRunsForPerson(context.Background(), person.TenantID, person.PersonID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

// cronTool answers one cron tool call made by a Run of the person.
func (in *installation) cronTool(t *testing.T, person *control.IdentityContext, args map[string]interface{}) (string, error) {
	t.Helper()
	args["_context"] = context.Background()
	args["_invocation_scope"] = kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID,
		AccountID: person.AccountID, RunID: "run-of-" + person.PersonID}
	tool := &cron.ToolAdapter{CronTool: cron.NewCronTool(in.sched, NewScheduleAuthority(in.store))}
	return tool.Execute(args)
}

func (in *installation) listed(t *testing.T, person *control.IdentityContext, job cron.CronJob) string {
	t.Helper()
	out, err := in.cronTool(t, person, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, fmt.Sprintf("[%d] ", job.ID)) {
			return line
		}
	}
	return ""
}

// P1: a job the older release stored with no endpoint runs as the person the
// command line acts as now, and is that person's to list and remove. No
// cli:local account is created for it.
func TestLegacyCommandLineJobRunsAsTheCommandLinePerson(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	job := in.legacyRow(t, "standup notes", "", "", "")
	before := in.runs(t, in.me)
	resp := in.fire(t, job.ID)
	if in.runs(t, in.me) != before+1 || resp.Identity == nil || resp.Identity.PersonID != in.me.PersonID {
		t.Fatalf("the legacy job did not run as the command-line person: runs %d -> %d, resp=%+v", before, in.runs(t, in.me), resp)
	}
	if local, err := in.store.ResolveAccount(ctx, "default", "cli", "local"); err != nil || local != nil {
		t.Fatalf("firing created cli:local: %+v %v", local, err)
	}
	if !in.job(t, job.ID).Enabled {
		t.Fatal("a job that ran was turned off")
	}
	if line := in.listed(t, in.me, job); line == "" || strings.Contains(line, "does not run") {
		t.Fatalf("the person's list shows %q for their legacy job", line)
	}
	if _, err := in.cronTool(t, in.me, map[string]interface{}{"action": "remove", "job_id": float64(job.ID)}); err != nil {
		t.Fatalf("the person cannot remove their legacy job: %v", err)
	}
}

// The receipt names the creator even when other people use the installation.
func TestLegacyCommandLineJobWithItsReceiptRunsAsItsCreator(t *testing.T) {
	in := newInstallation(t)
	stranger := in.stranger(t)
	job := in.legacyRow(t, "standup notes", "", "", "")
	in.receipt(t, in.me, job)
	before := in.runs(t, in.me)
	in.fire(t, job.ID)
	if in.runs(t, in.me) != before+1 {
		t.Fatalf("the creator's legacy job did not run: runs %d -> %d", before, in.runs(t, in.me))
	}
	if line := in.listed(t, stranger, job); line != "" {
		t.Fatalf("another person sees the job: %q", line)
	}
}

// The invariant-changing variant: a chat's Run could store the same row.
// With a stranger's receipt it is the stranger's, never the command line's;
// with no receipt and a stranger present nobody can be shown to own it. Either
// way it does not run, is turned off, and the command line does not run it.
func TestLegacyCommandLineJobIsNeverRunAsSomeoneWhoDidNotCreateIt(t *testing.T) {
	t.Run("stranger's receipt", func(t *testing.T) {
		in := newInstallation(t)
		stranger := in.stranger(t)
		job := in.legacyRow(t, "exfil", "", "", "")
		in.receipt(t, stranger, job)
		mine, theirs := in.runs(t, in.me), in.runs(t, stranger)
		resp := in.fire(t, job.ID)
		if in.runs(t, in.me) != mine || in.runs(t, stranger) != theirs {
			t.Fatalf("a stranger's command-line job ran: %+v", resp)
		}
		if in.job(t, job.ID).Enabled {
			t.Fatal("a refused job stays on and is refused again at every fire")
		}
		if line := in.listed(t, stranger, job); !strings.Contains(line, "does not run: it was stored for the command line") {
			t.Fatalf("the creator's list shows %q", line)
		}
		if line := in.listed(t, in.me, job); line != "" {
			t.Fatalf("the command-line person sees a stranger's job: %q", line)
		}
	})
	t.Run("no receipt", func(t *testing.T) {
		in := newInstallation(t)
		stranger := in.stranger(t)
		job := in.legacyRow(t, "exfil", "", "", "")
		mine := in.runs(t, in.me)
		in.fire(t, job.ID)
		if in.runs(t, in.me) != mine {
			t.Fatal("a job no one can be shown to own ran as the command-line person")
		}
		if in.job(t, job.ID).Enabled {
			t.Fatal("a refused job stays on")
		}
		// The installation's command line cleans it up; the chat person sees
		// nothing of it.
		if line := in.listed(t, in.me, job); !strings.Contains(line, "does not run: it names no account") {
			t.Fatalf("the command-line person's list shows %q", line)
		}
		if line := in.listed(t, stranger, job); line != "" {
			t.Fatalf("a chat person sees an unclaimed job: %q", line)
		}
		if _, err := in.cronTool(t, in.me, map[string]interface{}{"action": "remove", "job_id": float64(job.ID)}); err != nil {
			t.Fatalf("the command line cannot remove an unclaimed job: %v", err)
		}
	})
}

// P2: the older executor created a cli:local person on the first fire. Its
// jobs now run as the command-line person, not as that ghost nobody watches.
func TestLegacyCommandLineJobLeavesTheLocalGhost(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	ghost, err := in.store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "cli:local")
	if err != nil || ghost.PersonID == in.me.PersonID {
		t.Fatalf("ghost: %+v %v", ghost, err)
	}
	// The ghost ran earlier fires, and its Run recorded a job it created.
	job := in.legacyRow(t, "weekly review", "", "", "cli")
	in.receipt(t, ghost, job)
	ghostRuns, mine := in.runs(t, ghost), in.runs(t, in.me)
	in.fire(t, job.ID)
	if in.runs(t, ghost) != ghostRuns || in.runs(t, in.me) != mine+1 {
		t.Fatalf("the legacy job ran as the ghost: ghost %d -> %d, me %d -> %d", ghostRuns, in.runs(t, ghost), mine, in.runs(t, in.me))
	}
	if line := in.listed(t, in.me, job); line == "" {
		t.Fatal("the person does not see the job the ghost used to run")
	}
}

// P3: an older job addressed to a group chat names no account. It does not
// run, is turned off, and is its creator's to see and remove.
func TestLegacyGroupJobIsTurnedOffAndItsCreatorsToRemove(t *testing.T) {
	in := newInstallation(t)
	job := in.legacyRow(t, "team digest", "weixin", "team@chatroom", "")
	in.receipt(t, in.me, job)
	mine := in.runs(t, in.me)
	in.fire(t, job.ID)
	if in.runs(t, in.me) != mine {
		t.Fatal("a job addressed to a group ran")
	}
	for _, msg := range in.sender.snapshot() {
		if msg.Channel == "team@chatroom" {
			t.Fatalf("a job reached the group: %+v", msg)
		}
	}
	if in.job(t, job.ID).Enabled {
		t.Fatal("a refused job stays on and is refused again at every fire")
	}
	line := in.listed(t, in.me, job)
	if !strings.Contains(line, "disabled") || !strings.Contains(line, "does not run: it names weixin:team@chatroom, which is not bound to anyone here") {
		t.Fatalf("the creator's list shows %q", line)
	}
	if _, err := in.cronTool(t, in.me, map[string]interface{}{"action": "remove", "job_id": float64(job.ID)}); err != nil {
		t.Fatalf("the creator cannot remove the job: %v", err)
	}
}

// A job on the person's own account that names a shared chat is turned off
// and the person is told so in their own chat.
func TestRefusedJobIsTurnedOffAndThePersonIsTold(t *testing.T) {
	in := newInstallation(t)
	job := in.legacyRow(t, "team digest", "weixin", "wx-me", "team@chatroom")
	resp := in.fire(t, job.ID)
	if in.job(t, job.ID).Enabled {
		t.Fatal("a refused job stays on")
	}
	var notices []string
	for _, msg := range in.sender.snapshot() {
		if msg.Channel == "wx-me" {
			notices = append(notices, msg.Content)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "is now turned off") || !strings.Contains(notices[0], "team@chatroom") ||
		!strings.Contains(resp.Content, "is now turned off") {
		t.Fatalf("notices %q, response %q", notices, resp.Content)
	}
}

// M19: a learned label that belongs to someone else is never used; it is
// cleared and the job learns its own.
func TestScheduledJobNeverUsesAnotherPersonsLearnedThread(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	stranger := in.stranger(t)
	theirs, err := in.store.CreateTask(ctx, control.TaskCreate{TenantID: stranger.TenantID, PersonID: stranger.PersonID,
		Title: "their thread", Channel: "wx-stranger"})
	if err != nil {
		t.Fatal(err)
	}
	job := cron.CronJob{Name: "daily", CronExpr: "0 9 * * *", Prompt: "summarize my day", TenantID: "default",
		CreatorPersonID: in.me.PersonID, Platform: "weixin", DeliverTo: "wx-me", Channel: "wx-me", Enabled: true}
	id, err := in.sched.AddJob(ctx, &job)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.sched.SetTaskID(ctx, id, theirs.ID); err != nil {
		t.Fatal(err)
	}
	resp := in.fire(t, id)
	if resp.Task == nil || resp.Task.ID == theirs.ID || resp.Task.PersonID != in.me.PersonID {
		t.Fatalf("the job ran on another person's thread: %+v", resp.Task)
	}
	if learned := in.job(t, id).TaskID; learned != resp.Task.ID {
		t.Fatalf("binding = %q, want the person's own thread %q", learned, resp.Task.ID)
	}
	// The variant: the person's own learned thread is kept.
	again := in.fire(t, id)
	if again.Task == nil || again.Task.ID != resp.Task.ID {
		t.Fatalf("the person's own learned thread was not used: %+v", again.Task)
	}
}

// M20: an account that is no longer active acts for no one.
func TestScheduledJobOnAnInactiveAccountDoesNotRun(t *testing.T) {
	in := newInstallation(t)
	job := in.legacyRow(t, "daily", "weixin", "wx-me", "")
	db, err := sql.Open("sqlite", filepath.Join(in.dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE accounts SET status = 'inactive' WHERE platform = 'weixin' AND platform_user_id = 'wx-me'`); err != nil {
		t.Fatal(err)
	}
	mine := in.runs(t, in.me)
	resp := in.fire(t, job.ID)
	if in.runs(t, in.me) != mine || !strings.Contains(resp.Content, "weixin:wx-me is no longer active") {
		t.Fatalf("a job on an inactive account ran or said %q", resp.Content)
	}
	if in.job(t, job.ID).Enabled {
		t.Fatal("a refused job stays on")
	}
	// The variant: the same job on the active CLI account runs.
	cli := in.legacyRow(t, "daily cli", "cli", "cwill", "")
	in.fire(t, cli.ID)
	if in.runs(t, in.me) != mine+1 {
		t.Fatal("a job on an active account did not run")
	}
}

// M22: a turn the daemon started acts only through an account that exists;
// it never creates one. The same request typed by the person does.
func TestDaemonTurnNeverCreatesAnAccount(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	for name, send := range map[string]func() int{
		"cron origin": func() int {
			_, status := in.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "telegram", PlatformUserID: "900",
				Channel: "900", Content: "summarize my files", Origin: runOriginCron})
			return status
		},
		"watch origin": func() int {
			_, status := in.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "telegram", PlatformUserID: "900",
				Channel: "900", Content: "summarize my files", WatchID: "watch_1"})
			return status
		},
		"turn source": func() int {
			_, status := in.daemon.ProcessMessage(kernel.WithTurnSource(ctx, "cron"), api.MessageRequest{Platform: "telegram",
				PlatformUserID: "900", Channel: "900", Content: "summarize my files"})
			return status
		},
	} {
		if status := send(); status != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", name, status)
		}
		if account, err := in.store.ResolveAccount(ctx, "default", "telegram", "900"); err != nil || account != nil {
			t.Fatalf("%s created telegram:900: %+v %v", name, account, err)
		}
	}
	if _, status := in.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "telegram", PlatformUserID: "900",
		Channel: "900", Content: "hello"}); status != http.StatusOK {
		t.Fatalf("the person's own first message: status %d", status)
	}
	if account, err := in.store.ResolveAccount(ctx, "default", "telegram", "900"); err != nil || account == nil {
		t.Fatalf("the person's own message did not bind their account: %v", err)
	}
}

// A scheduled turn that waits for a model, or for a model change to finish,
// is queued as a scheduled turn, below the person's own input.
func TestScheduledTurnWaitingForAModelStaysScheduled(t *testing.T) {
	for name, block := range map[string]func(*testing.T, *installation){
		"no ready model": func(t *testing.T, in *installation) {
			unready := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(unready, []byte("models:\n  primary:\n    provider: none\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			in.daemon.ModelChanges = &modelchange.Service{ConfigPath: unready}
		},
		"model change": func(t *testing.T, in *installation) {
			in.daemon.beginDraining("model_change:model_123")
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := newInstallation(t)
			ctx := context.Background()
			block(t, in)
			job := in.legacyRow(t, "daily", "weixin", "wx-me", "")
			in.fire(t, job.ID)
			if _, status := in.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-me",
				Channel: "wx-me", Content: "my own request"}); status != http.StatusOK {
				t.Fatalf("the person's own message: status %d", status)
			}
			queued, err := in.store.ListQueued(ctx, in.me.TenantID, in.me.PersonID, control.QueueStatusQueued)
			if err != nil {
				t.Fatal(err)
			}
			classes := map[string]string{}
			for _, row := range queued {
				classes[row.Content] = row.Class
			}
			if classes["summarize my day"] != control.QueueClassCron || classes["my own request"] == control.QueueClassCron {
				t.Fatalf("queued classes = %v", classes)
			}
		})
	}
}
