package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestProviderParkingIsNotAnIMFinalResultAndTheContinuationDeliversOnce(t *testing.T) {
	daemon, store, sender := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	provider := &onceRateLimitedProvider{}
	daemon.Gateway = router.NewGateway(kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, llm.NewRequestGate(1).Wrap(provider, "shared-im-route"), "test", 1, 3, nil), nil)
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-model-wait", "Me")
	if err != nil {
		t.Fatal(err)
	}
	resp, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-model-wait", Channel: "wx-model-wait", Content: "finish these notes", Async: true})
	if code != 200 || !resp.Accepted {
		t.Fatalf("admission=%d %+v", code, resp)
	}
	var parent string
	var q *control.QueuedTask
	waitUntil(t, 3*time.Second, func() bool {
		runs, _ := store.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 5)
		if len(runs) != 1 || runs[0].Status != "waiting_external" {
			return false
		}
		parent = runs[0].RunID
		q, _ = store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "provider-wait:"+parent)
		return q != nil && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "the IM run did not release its worker while parked")
	for _, sent := range sender.snapshot() {
		if sent.Kind == delivery.KindFinalResult {
			t.Fatalf("parking was sent as a final answer: %+v", sent)
		}
	}
	waitUntil(t, 3*time.Second, func() bool { return !time.Now().Before(q.NotBefore) }, "the provider cooldown did not end")
	daemon.coordinator().drainQueue(owner)
	waitUntil(t, 3*time.Second, func() bool {
		stored, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
		return stored != nil && stored.Status == control.QueueStatusDone
	}, "the continuation did not finish")
	finals := 0
	for _, sent := range sender.snapshot() {
		if sent.Kind == delivery.KindFinalResult {
			finals++
			if sent.Content != "Run "+sent.RunID+"\ndone" || sent.RunID == parent {
				t.Fatalf("wrong final result: %+v", sent)
			}
		}
	}
	if finals != 1 {
		t.Fatalf("final results=%d want1: %+v", finals, sender.snapshot())
	}
}

func TestCronProviderParkingDefersDeliveryAndCompletedWorkRemainsVisible(t *testing.T) {
	for _, parked := range []bool{true, false} {
		name := "completed"
		if parked {
			name = "parked"
		}
		t.Run(name, func(t *testing.T) {
			in := newInstallation(t)
			daemon, sender := in.daemon, in.sender
			if parked {
				daemon.Gateway = router.NewGateway(kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, &repeatingProviderWait{}, "test", 1, 3, nil), nil)
			}
			ctx := context.Background()
			if _, err := in.cronTool(t, in.me, map[string]interface{}{"action": "add", "name": "provider wait", "cron": "0 9 * * *",
				"platform": "weixin", "deliver_to": "wx-me", "prompt": "finish the scheduled notes"}); err != nil {
				t.Fatal(err)
			}
			jobs, err := in.sched.ListJobs(ctx, in.me.TenantID)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("created schedules=%+v %v", jobs, err)
			}
			resp, err := NewCronExecutor(daemon, in.sched).Fire(ctx, jobs[0])
			if err != nil || resp.Error != "" || resp.Outcome == nil {
				t.Fatalf("cron result=%+v %v", resp, err)
			}
			wantFinals := 1
			if parked {
				wantFinals = 0
				if resp.Outcome.Status != "waiting_external" || resp.Outcome.CompletionReason != "provider_wait" {
					t.Fatalf("cron did not park: %+v", resp.Outcome)
				}
			} else if resp.Outcome.Status != "done" {
				t.Fatalf("cron did not complete: %+v", resp.Outcome)
			}
			if got := len(sender.snapshot()); got != wantFinals {
				t.Fatalf("%s delivery count=%d want=%d", name, got, wantFinals)
			}
		})
	}
}
