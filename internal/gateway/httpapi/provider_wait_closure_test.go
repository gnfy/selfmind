package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type providerWaitSequence struct {
	daemon *Server
	store  *control.Store
	owner  *control.IdentityContext
	task   *control.Task
	run    *control.Run
	req    api.MessageRequest
}

func newProviderWaitSequence(t *testing.T) *providerWaitSequence {
	t.Helper()
	d, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "keep the original work", Channel: "session-provider"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "session-provider", "finish the original request")
	if err != nil {
		t.Fatal(err)
	}
	return &providerWaitSequence{daemon: d, store: store, owner: owner, task: task, run: run,
		req: api.MessageRequest{TenantID: owner.TenantID, Platform: "cli", PlatformUserID: "local", Channel: run.Channel, Content: "finish the original request"}}
}

func (s *providerWaitSequence) park(t *testing.T, reason string, iteration int) error {
	t.Helper()
	ctx := context.Background()
	if err := s.store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{TenantID: s.owner.TenantID, PersonID: s.owner.PersonID,
		TaskID: s.task.ID, RunID: s.run.ID, Iteration: iteration, Outcome: "continue_model", Detail: "provider_request",
		Snapshot: []byte(`[{"role":"user","content":"finish the original request"}]`)}); err != nil {
		t.Fatal(err)
	}
	_, err := s.daemon.coordinator().parkProviderWait(ctx, s.owner, s.task, s.run, s.req,
		&llm.ProviderWait{Reason: reason, NotBefore: time.Now().Add(-time.Second)})
	return err
}

func (s *providerWaitSequence) automaticChild(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	q, err := s.store.GetQueuedByIdempotencyKey(ctx, s.owner.TenantID, "provider-wait:"+s.run.ID)
	if err != nil || q == nil {
		t.Fatalf("missing continuation: %+v, %v", q, err)
	}
	token, claimed, err := s.store.ClaimQueued(ctx, s.owner.TenantID, q.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	child, err := s.store.StartRunWithOptions(ctx, s.task, s.run.Channel, "finish the original request", control.StartRunOptions{
		ResumesRunID: s.run.ID, QueueID: q.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	s.run = child
	s.req.QueueID, s.req.QueueClaimToken = q.ID, token
}

func TestProviderWaitRetryLimitCountsOnlyUninterruptedAutomaticWaits(t *testing.T) {
	for _, reset := range []string{"no progress", "progress", "manual resume"} {
		t.Run(reset, func(t *testing.T) {
			s := newProviderWaitSequence(t)
			for range 6 {
				if err := s.park(t, "rate_limit", 0); err != nil {
					t.Fatal(err)
				}
				s.automaticChild(t)
			}
			iteration := 0
			if reset == "progress" {
				iteration = 1
			}
			if reset == "manual resume" {
				outcome := s.daemon.coordinator().finalizeErroredRun(context.Background(), s.owner, s.task, s.run, s.run.Channel, errProviderWaitLimit)
				if outcome.Status != "blocked" {
					t.Fatalf("limit outcome = %+v", outcome)
				}
				child, err := s.store.StartRunWithOptions(context.Background(), s.task, s.run.Channel, "try this exact work again", control.StartRunOptions{ResumesRunID: s.run.ID})
				if err != nil {
					t.Fatal(err)
				}
				s.run = child
				s.req.QueueID, s.req.QueueClaimToken = "", ""
			}
			err := s.park(t, "rate_limit", iteration)
			if reset == "no progress" {
				if !errors.Is(err, errProviderWaitLimit) {
					t.Fatalf("unbounded repeated 429s: %v", err)
				}
			} else if err != nil {
				t.Fatalf("%s did not reset provider retries: %v", reset, err)
			}
		})
	}
}

type repeatingProviderWait struct{ onceRateLimitedProvider }

func (*repeatingProviderWait) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return nil, &llm.ProviderWait{Reason: "rate_limit", NotBefore: time.Now().Add(-time.Second)}
}

func TestEarlierLoopProgressDoesNotResetTheRetryLimitOnEveryChild(t *testing.T) {
	s := newProviderWaitSequence(t)
	if err := s.park(t, "rate_limit", 4); err != nil {
		t.Fatal(err)
	}
	s.daemon.Gateway = router.NewGateway(kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, &repeatingProviderWait{}, "test", 1, 3, nil), nil)
	ctx := context.Background()
	parent := s.run.ID
	for attempt := 1; attempt <= 6; attempt++ {
		q, err := s.store.GetQueuedByIdempotencyKey(ctx, s.owner.TenantID, "provider-wait:"+parent)
		if err != nil || q == nil {
			t.Fatalf("attempt %d has no wakeup: %+v %v", attempt, q, err)
		}
		s.daemon.coordinator().drainQueue(s.owner)
		waitUntil(t, 3*time.Second, func() bool {
			stored, _ := s.store.GetQueued(ctx, s.owner.TenantID, q.ID)
			return stored != nil && stored.Status == control.QueueStatusDone && stored.RunID != "" && s.daemon.coordinator().activeCount(s.owner.PersonID) == 0
		}, "the automatic provider child did not settle")
		q, _ = s.store.GetQueued(ctx, s.owner.TenantID, q.ID)
		child, err := s.store.GetRun(ctx, s.owner.TenantID, q.RunID)
		if err != nil || child == nil {
			t.Fatalf("missing child: %+v %v", child, err)
		}
		checkpoint, err := s.store.IncompleteLoopCheckpointForRun(ctx, s.owner.TenantID, child.ID)
		if err != nil || checkpoint == nil || checkpoint.Iteration != 0 {
			t.Fatalf("child inherited old iteration progress: %+v %v", checkpoint, err)
		}
		if attempt < 6 && child.Status != "waiting_external" {
			t.Fatalf("retry %d ended prematurely: %+v", attempt, child)
		}
		if attempt == 6 && child.Status != "blocked" {
			t.Fatalf("old progress allowed unlimited new retries: %+v", child)
		}
		parent = child.ID
	}
}

func TestStopAndQueueClearEndTheParkedProviderRun(t *testing.T) {
	for _, command := range []string{"/stop exact", "/stop short", "/stop pinned", "/stop", "/queue clear", "/queue drop 1"} {
		t.Run(command, func(t *testing.T) {
			s := newProviderWaitSequence(t)
			if err := s.park(t, "rate_limit", 0); err != nil {
				t.Fatal(err)
			}
			input := command
			if command == "/stop exact" {
				input = "/stop " + s.run.ID
			}
			if command == "/stop short" {
				input = "/stop " + shortRunID(s.run.ID)
			}
			if command == "/stop pinned" {
				input = "/stop"
				if err := s.store.SetPersonSetting(context.Background(), s.owner.TenantID, s.owner.PersonID, resumeRunPinKey, s.run.ID); err != nil {
					t.Fatal(err)
				}
			}
			resp, code := s.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: s.run.Channel, Content: input})
			if code != 200 || resp.Error != "" {
				t.Fatalf("%s: %d %+v", input, code, resp)
			}
			run, _ := s.store.GetRun(context.Background(), s.owner.TenantID, s.run.ID)
			q, _ := s.store.GetQueuedByIdempotencyKey(context.Background(), s.owner.TenantID, "provider-wait:"+s.run.ID)
			if run.Status != "cancelled" || q.Status != control.QueueStatusCancelled {
				t.Fatalf("%s left parked work able to return: run=%+v queue=%+v reply=%q", input, run, q, resp.Content)
			}
			if strings.Contains(resp.Content, "still continue") {
				t.Fatalf("stop offered continuation of cancelled work: %q", resp.Content)
			}
			if requested, err := s.store.RunCancelRequested(context.Background(), s.owner.TenantID, s.run.ID); err != nil || !requested {
				t.Fatalf("the parked run has no cancellation fact: %v, %v", requested, err)
			}
			if command == "/stop pinned" {
				if pin, err := s.store.GetPersonSetting(context.Background(), s.owner.TenantID, s.owner.PersonID, resumeRunPinKey); err != nil || pin != "" {
					t.Fatalf("cancelled work remained pinned: %q %v", pin, err)
				}
			}
		})
	}
}
