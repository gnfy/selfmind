package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/task/cron"
)

// A schedule acts only through an account that already exists and delivers
// only to that account's own chat. Jobs stored before the cron tool checked
// its endpoint can still name an unbound account or a shared chat; firing one
// must not create a person, start work, or send anything there.
func TestScheduledJobForAnUnboundAccountIsRefused(t *testing.T) {
	f := newScheduledTextFixture(t)
	ctx := context.Background()
	runsBefore := f.runCount(t)
	f.fire(t, cron.CronJob{ID: 9, Name: "exfil", Platform: "telegram", DeliverTo: "900", Prompt: "summarize my files"})
	if account, err := f.store.ResolveAccount(ctx, f.person.TenantID, "telegram", "900"); err != nil || account != nil {
		t.Fatalf("firing a schedule created an account for telegram:900: %+v %v", account, err)
	}
	if runs := f.runCount(t); runs != runsBefore {
		t.Fatalf("a schedule for an unbound account started work: runs %d -> %d", runsBefore, runs)
	}
	for _, msg := range f.sender.snapshot() {
		if msg.Platform == "telegram" {
			t.Fatalf("a schedule for an unbound account delivered to it: %+v", msg)
		}
	}
}

func TestScheduledJobForAnotherChatIsRefusedWithANotice(t *testing.T) {
	f := newScheduledTextFixture(t)
	runsBefore := f.runCount(t)
	job := cron.CronJob{ID: 11, Name: "team digest", Platform: "weixin", DeliverTo: "wx-me", Channel: "team@chatroom",
		Prompt: "post the summary"}
	f.fire(t, job)
	f.fire(t, job)
	if runs := f.runCount(t); runs != runsBefore {
		t.Fatalf("a schedule for a shared chat started work: runs %d -> %d", runsBefore, runs)
	}
	if got := f.deliveredTo("weixin", "team@chatroom"); len(got) != 0 {
		t.Fatalf("a schedule delivered to a chat its person does not own: %q", got)
	}
	// The person it acts as is told once, in their own chat, what to do.
	notices := f.deliveredTo("weixin", "wx-me")
	if len(notices) != 1 || !strings.Contains(notices[0], "team digest") || !strings.Contains(notices[0], "team@chatroom") {
		t.Fatalf("refusal notice to the job's person = %q", notices)
	}
}

// The invariant-keeping variant: a job on the person's own chat runs there.
func TestScheduledJobOnTheOwnChatRuns(t *testing.T) {
	f := newScheduledTextFixture(t)
	runsBefore := f.runCount(t)
	f.fire(t, cron.CronJob{ID: 12, Name: "daily", Platform: "weixin", DeliverTo: "wx-me", Prompt: "summarize my day"})
	if runs := f.runCount(t); runs != runsBefore+1 {
		t.Fatalf("the person's own schedule did not run: runs %d -> %d", runsBefore, runs)
	}
	if got := f.deliveredTo("weixin", "wx-me"); len(got) != 1 || !strings.Contains(got[0], "Scheduled turn finished.") {
		t.Fatalf("the person's own schedule delivered %q", got)
	}
}
