package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

type replayRecallExpander struct{ calls atomic.Int32 }

func (e *replayRecallExpander) Expand(_ context.Context, query string) string {
	e.calls.Add(1)
	return query + " alternate"
}

func TestApprovalRecoveryInstallsExactReplayBeforeRecall(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	expander := &replayRecallExpander{}
	daemon.Recall = NewRecallEngine(nil, nil, expander)
	daemon.Recall.sources = []RecallSource{replayRecallSource{}}
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "recall")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "recall", WorkspaceID: ws.ID, Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	var approval *control.ApprovalRequest
	waitUntil(t, 2*time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 && pending[0].WaiterState == "parked" && daemon.coordinator().activeCount(owner.PersonID) == 0 {
			approval = &pending[0]
			return true
		}
		return false
	}, "approval did not park")
	if expander.calls.Load() != 1 {
		t.Fatalf("ordinary work expansion calls=%d", expander.calls.Load())
	}
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "recall", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
		return err == nil && string(data) == "approved effect" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "exact action did not complete")
	if expander.calls.Load() != 1 {
		t.Fatalf("recovery made extra semantic expansion: %d", expander.calls.Load())
	}
	events, _ := store.ListTaskEvents(ctx, approval.TaskID, 100)
	found := false
	for _, event := range events {
		if event.Type == "context.recall" && strings.Contains(string(event.Payload), "exact_checkpoint_replay") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing recorded expansion skip reason")
	}
}

type replayRecallSource struct{}

func (replayRecallSource) Name() string { return "fixture" }
func (replayRecallSource) Search(context.Context, RecallQuery) ([]RecallHit, error) {
	return []RecallHit{{Slice: kernel.RecallSlice{Source: "fixture", Title: "Related reference", Excerpt: "Original lexical search remains available", Ref: "reference"}, Score: 1, WorkKey: "independent-reference"}}, nil
}

func TestExactReplaySkipsOnlySemanticExpansion(t *testing.T) {
	for _, kind := range []string{"exact", "ordinary", "marker-without-ledger"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "recall-replay", "User")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Check saved work", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "check the saved work")
			if err != nil {
				t.Fatal(err)
			}
			if kind != "marker-without-ledger" {
				ctx = kernel.WithLoopResumeMessages(ctx, []llm.Message{{Role: "user", Content: "Original goal"}})
			}
			if kind != "ordinary" {
				ctx = kernel.WithExactLoopReplay(ctx)
			}
			expander := &replayRecallExpander{}
			engine := NewRecallEngine(nil, nil, expander)
			engine.sources = []RecallSource{replayRecallSource{}}
			server := &Server{Control: store, Recall: engine}
			selected := server.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, run, nil, "cli", "cli", "approved pending action", attachContextNone, nil)
			want := int32(1)
			if kind == "exact" {
				want = 0
			}
			if expander.calls.Load() != want {
				t.Fatalf("semantic calls=%d want=%d; exact recovery must not buy another expansion", expander.calls.Load(), want)
			}
			if len(selected.RecallSlices) != 1 {
				t.Fatalf("lexical recall was lost: %+v", selected.RecallSlices)
			}
		})
	}
}
