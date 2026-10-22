package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
)

func TestScheduledResultNeverFollowsAnEndpointToAnotherPerson(t *testing.T) {
	for _, differentPerson := range []bool{true, false} {
		name := "same person"
		if differentPerson {
			name = "another person"
		}
		t.Run(name, func(t *testing.T) {
			in := newInstallation(t)
			ctx := context.Background()
			const answer = "Your private unreleased launch budget is ready."
			provider := newSlowLLMProvider(answer)
			t.Cleanup(provider.releaseNow)
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test agent", 1, 1, nil)
			in.daemon.Gateway = router.NewGateway(agent, nil)
			if _, err := in.cronTool(t, in.me, map[string]interface{}{
				"action": "add", "name": "private launch digest", "cron": "0 9 * * *", "prompt": "summarize my unreleased launch budget",
				"platform": "weixin", "deliver_to": "wx-me",
			}); err != nil {
				t.Fatal(err)
			}
			jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs = %+v, %v", jobs, err)
			}
			newOwner := in.me
			if differentPerson {
				newOwner = in.stranger(t)
			}
			before := in.runs(t, newOwner)
			responses := make(chan api.MessageResponse, 1)
			go func() {
				resp, _ := NewCronExecutor(in.daemon, in.sched).Fire(ctx, jobs[0])
				responses <- resp
			}()
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the scheduled run did not start")
			}
			if _, err := in.store.BindAccount(ctx, in.me.TenantID, newOwner.PersonID, "weixin", "wx-me", "Rebound during execution"); err != nil {
				t.Fatal(err)
			}
			provider.releaseNow()
			select {
			case resp := <-responses:
				if resp.Error != "" || resp.Identity == nil || resp.Identity.PersonID != in.me.PersonID || resp.Content != answer {
					t.Fatalf("the creator's admitted work was rewritten after a binding change: %+v", resp)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the scheduled run did not finish")
			}
			account, err := in.store.ResolveAccount(ctx, in.me.TenantID, "weixin", "wx-me")
			if err != nil || account == nil || account.PersonID != newOwner.PersonID {
				t.Fatalf("the fire rewrote the endpoint's new binding: %+v, %v", account, err)
			}
			if differentPerson {
				if got := in.runs(t, newOwner); got != before {
					t.Fatalf("the run transferred to the new endpoint owner: runs %d -> %d", before, got)
				}
				if sent := in.deliveredOn("wx-me"); len(sent) != 0 {
					t.Fatalf("the private result reached the endpoint's new person: %q", sent)
				}
			} else if sent := in.deliveredOn("wx-me"); len(sent) != 1 || !strings.Contains(sent[0], answer) {
				t.Fatalf("same-person rebinding lost the result: %q", sent)
			}
		})
	}
}

// Pause the real account lookup at daemon admission, after the executor's
// schedule check. This injects the interleaving without a production hook or
// relying on the number of queries the schedule check happens to perform.
type cronAdmissionBarrier struct {
	context.Context
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *cronAdmissionBarrier) Done() <-chan struct{} {
	var pcs [32]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "(*Server).processDaemonTurn") {
			c.once.Do(func() {
				close(c.reached)
				<-c.release
			})
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Done()
}

func TestScheduleCreatorIsCheckedAtAdmissionAfterEndpointRebinding(t *testing.T) {
	for _, differentPerson := range []bool{true, false} {
		name := "same person"
		if differentPerson {
			name = "another person"
		}
		t.Run(name, func(t *testing.T) {
			in := newInstallation(t)
			ctx := context.Background()
			if _, err := in.cronTool(t, in.me, map[string]interface{}{
				"action": "add", "name": "private concurrent digest", "cron": "0 9 * * *", "prompt": "summarize my private concurrent notes",
				"platform": "weixin", "deliver_to": "wx-me",
			}); err != nil {
				t.Fatal(err)
			}
			jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs = %+v, %v", jobs, err)
			}
			newOwner := in.me
			if differentPerson {
				newOwner = in.stranger(t)
			}
			before := in.runs(t, newOwner)
			// The gateway store owns one SQL connection. Rebind through a
			// second connection while its admission lookup is at the barrier.
			rebinder, err := control.OpenStore(in.dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rebinder.Close() })
			barrier := &cronAdmissionBarrier{Context: ctx, reached: make(chan struct{}), release: make(chan struct{})}
			responses := make(chan api.MessageResponse, 1)
			go func() {
				resp, _ := NewCronExecutor(in.daemon, in.sched).Fire(barrier, jobs[0])
				responses <- resp
			}()
			select {
			case <-barrier.reached:
			case <-time.After(5 * time.Second):
				close(barrier.release)
				t.Fatal("the scheduled fire never reached admission")
			}
			_, err = rebinder.BindAccount(ctx, in.me.TenantID, newOwner.PersonID, "weixin", "wx-me", "Rebound during fire")
			close(barrier.release)
			if err != nil {
				t.Fatal(err)
			}
			var resp api.MessageResponse
			select {
			case resp = <-responses:
			case <-time.After(5 * time.Second):
				t.Fatal("the scheduled fire did not finish")
			}
			if differentPerson {
				if got := in.runs(t, newOwner); got != before {
					t.Fatalf("the checked schedule ran as the endpoint's new person: runs %d -> %d", before, got)
				}
				if resp.Error == "" || resp.Task != nil || resp.Run != nil {
					t.Fatalf("the moved endpoint was not refused before dispatch: %+v", resp)
				}
				for _, message := range in.sender.snapshot() {
					if message.Channel == "wx-me" {
						t.Fatalf("the refused fire delivered to the moved endpoint: %+v", message)
					}
				}
			} else if got := in.runs(t, in.me); got != before+1 || resp.Error != "" {
				t.Fatalf("same-person rebinding refused the schedule: runs %d -> %d, %+v", before, got, resp)
			}
		})
	}
}

func TestCreatedScheduleDoesNotMoveWithItsEndpoint(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	const name = "private release notes"
	if _, err := in.cronTool(t, in.me, map[string]interface{}{
		"action": "add", "name": name, "cron": "0 9 * * *", "prompt": "send my private release notes",
		"platform": "weixin", "deliver_to": "wx-me",
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("created schedules = %+v, %v", jobs, err)
	}
	job := jobs[0]
	other := in.stranger(t)
	if _, err := in.store.BindAccount(ctx, in.me.TenantID, other.PersonID, "weixin", "wx-me", "Other phone"); err != nil {
		t.Fatal(err)
	}
	before := in.runs(t, other)
	in.fire(t, job.ID)
	if got := in.runs(t, other); got != before {
		t.Fatalf("the creator's schedule executed as the endpoint's new person: runs %d -> %d", before, got)
	}
	if line := in.listed(t, other, job); line != "" {
		t.Fatalf("the endpoint's new person sees another person's schedule: %q", line)
	}
	for _, action := range []string{"remove", "enable"} {
		if result, err := in.cronTool(t, other, map[string]interface{}{"action": action, "job_id": float64(job.ID), "enabled": true}); err == nil || strings.Contains(result, job.Prompt) {
			t.Fatalf("another person could %s the creator's schedule or see its prompt: %q, %v", action, result, err)
		}
	}
	if line := in.listed(t, in.me, job); !strings.Contains(line, name) || !strings.Contains(line, "does not run") {
		t.Fatalf("the creator cannot manage their refused schedule: %q", line)
	}
}

func TestScheduleFromAnotherOwnEndpointDoesNotTransferItsPrompt(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	phone, err := in.store.BindAccount(ctx, in.me.TenantID, in.me.PersonID, "telegram", "777", "My other phone")
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "summarize my unreleased financial notes"
	if _, err := in.cronTool(t, phone, map[string]interface{}{
		"action": "add", "name": "private financial summary", "cron": "0 8 * * *", "prompt": prompt,
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
	if err != nil || len(jobs) != 1 || jobs[0].CreatorPersonID != in.me.PersonID {
		t.Fatalf("creating IM endpoint lost the person's authority: %+v, %v", jobs, err)
	}
	other := in.stranger(t)
	if _, err := in.store.BindAccount(ctx, in.me.TenantID, other.PersonID, "telegram", "777", "Other phone"); err != nil {
		t.Fatal(err)
	}
	before := in.runs(t, other)
	in.fire(t, jobs[0].ID)
	if got := in.runs(t, other); got != before {
		t.Fatalf("the private schedule ran as another person: runs %d -> %d", before, got)
	}
	for _, message := range in.sender.snapshot() {
		if strings.Contains(message.Content, prompt) || message.Channel == "777" {
			t.Fatalf("private schedule reached the endpoint's new person: %+v", message)
		}
	}
}

func TestCreatedScheduleKeepsItsCreatorAcrossRestartAndSamePersonRebinding(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	if _, err := in.cronTool(t, in.me, map[string]interface{}{
		"action": "add", "name": "nightly export", "cron": "0 23 * * *", "prompt": "summarize the nightly export",
		"platform": "weixin", "deliver_to": "wx-me",
	}); err != nil {
		t.Fatal(err)
	}
	if err := in.cronDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(in.dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	in.cronDB = db
	in.sched = cron.NewScheduler(db, nil)
	if err := in.sched.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
	if err != nil || len(jobs) != 1 || jobs[0].CreatorPersonID != in.me.PersonID {
		t.Fatalf("creator was not persisted across restart: %+v, %v", jobs, err)
	}
	if _, err := in.store.BindAccount(ctx, in.me.TenantID, in.me.PersonID, "weixin", "wx-me", "Renamed phone"); err != nil {
		t.Fatal(err)
	}
	before := in.runs(t, in.me)
	in.fire(t, jobs[0].ID)
	if got := in.runs(t, in.me); got != before+1 {
		t.Fatalf("same-person endpoint update refused the creator's job: runs %d -> %d", before, got)
	}
}

func TestHistoricalEndpointBindingDoesNotProveScheduleOwnership(t *testing.T) {
	in := newInstallation(t)
	other := in.stranger(t)
	job := in.legacyRow(t, "old private digest", "weixin", other.PlatformUserID, "")
	before := in.runs(t, other)
	in.fire(t, job.ID)
	if got := in.runs(t, other); got != before {
		t.Fatalf("an endpoint alone authorized an old job: runs %d -> %d", before, got)
	}
	if line := in.listed(t, other, job); line != "" {
		t.Fatalf("unproven creator sees a historical schedule: %q", line)
	}
	for _, message := range in.sender.snapshot() {
		if strings.Contains(message.Content, job.Prompt) {
			t.Fatalf("an unclaimed historical prompt was disclosed: %q", message.Content)
		}
	}
}

func TestHistoricalCreatorEvidenceCannotTransferWithAnEndpoint(t *testing.T) {
	in := newInstallation(t)
	other := in.stranger(t)
	job := in.legacyRow(t, "my verified digest", "weixin", "wx-me", "")
	in.receipt(t, in.me, job)
	if line := in.listed(t, in.me, job); line == "" {
		t.Fatal("verified historical creator cannot manage their job")
	}
	if _, err := in.store.BindAccount(context.Background(), in.me.TenantID, other.PersonID, "weixin", "wx-me", "Other phone"); err != nil {
		t.Fatal(err)
	}
	before := in.runs(t, other)
	in.fire(t, job.ID)
	if got := in.runs(t, other); got != before {
		t.Fatalf("historical creator evidence was reused as another person's authority: runs %d -> %d", before, got)
	}
	if line := in.listed(t, other, job); line != "" {
		t.Fatalf("another person sees the historical creator's schedule: %q", line)
	}
	if line := in.listed(t, in.me, job); !strings.Contains(line, "does not run") {
		t.Fatalf("historical creator lost the refused job: %q", line)
	}
}

func TestHistoricalAdoptionDoesNotTransferWithTheCommandLineAccount(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	other := in.stranger(t)
	in.enableAdoption()
	job := in.legacyRow(t, "adopted private digest", "", "", "")
	if status, body, adopted := in.adoptAs(t, "cwill", job.ID, true); status != http.StatusOK || !adopted.Adopted {
		t.Fatalf("the historical row was not adopted: %d %s", status, body)
	}
	before := in.runs(t, in.me)
	in.fire(t, job.ID)
	if got := in.runs(t, in.me); got != before+1 {
		t.Fatalf("the verified adoption was lost: runs %d -> %d", before, got)
	}
	if _, err := in.store.BindAccount(ctx, in.me.TenantID, other.PersonID, "cli", "cwill", "Other command line"); err != nil {
		t.Fatal(err)
	}
	before = in.runs(t, other)
	in.fire(t, job.ID)
	if got := in.runs(t, other); got != before {
		t.Fatalf("the persisted adoption transferred to another person: runs %d -> %d", before, got)
	}
	if status, body, _ := in.adoptAs(t, "cwill", job.ID, false); status != http.StatusConflict || strings.Contains(body, job.Prompt) {
		t.Fatalf("the moved command line can see the original person's adopted prompt: %d %s", status, body)
	}
}
