package httpapi

// Every path that takes a client-supplied Thread or Run id acts only on the
// requesting person's own work. Another person's id reads exactly like an id
// that does not exist: nothing attaches to it, nothing changes, and the answer
// does not reveal that it exists.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// otherPersonsWork is person B's work, named by ids person A will send.
type otherPersonsWork struct {
	thread   *control.Task
	parked   *control.Run
	asking   *control.Run
	active   *activeRun
	approval *control.ApprovalRequest
	clarify  *control.ClarifyRequest
	choice   *control.PendingTurnChoice
	watch    *control.ExternalWatch
}

func seedOtherPersonsWork(t *testing.T, daemon *Server, store *control.Store) (*control.IdentityContext, otherPersonsWork) {
	t.Helper()
	ctx := context.Background()
	other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	var work otherPersonsWork
	if work.thread, err = store.CreateTask(ctx, control.TaskCreate{TenantID: other.TenantID, PersonID: other.PersonID,
		Title: "Bob's release", Channel: "cli"}); err != nil {
		t.Fatal(err)
	}
	work.parked = seedUnresolvedRun(t, store, other.TenantID, work.thread, "cli", "prepare Bob's release", "waiting_user")
	if work.asking, err = store.StartRun(ctx, work.thread, "cli", "ask Bob a question"); err != nil {
		t.Fatal(err)
	}
	if work.approval, err = store.CreateApprovalRequest(ctx, control.ApprovalRequest{TenantID: other.TenantID,
		PersonID: other.PersonID, TaskID: work.thread.ID, RunID: work.parked.ID, ActionType: "tool_call",
		Payload: json.RawMessage(`{"tool":"terminal","args":{"command":"rm -rf build"}}`)}); err != nil {
		t.Fatal(err)
	}
	if work.clarify, err = store.CreateClarifyRequest(ctx, control.ClarifyRequest{TenantID: other.TenantID,
		PersonID: other.PersonID, TaskID: work.thread.ID, RunID: work.asking.ID, Question: "Which bucket?", Channel: "cli"}); err != nil {
		t.Fatal(err)
	}
	if work.choice, err = store.CreatePendingTurnChoice(ctx, control.PendingTurnChoiceCreate{TenantID: other.TenantID,
		PersonID: other.PersonID, AccountID: other.AccountID, Channel: "cli", RequestJSON: `{"content":"Bob's request"}`,
		Options: []control.TurnChoiceOption{{Key: "1", Label: "Resume", Action: "resume", TaskID: work.thread.ID, RunID: work.parked.ID},
			{Key: "2", Label: "This is new work", Action: "new"}},
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if work.watch, err = store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: other.TenantID, PersonID: other.PersonID,
		TaskID: work.thread.ID, RunID: work.parked.ID, Channel: "cli", CWD: t.TempDir(), Command: "printf READY",
		SuccessPattern: "READY", IntervalSeconds: 5, CommandTimeoutSeconds: 10, TimeoutAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	work.active = &activeRun{TenantID: other.TenantID, PersonID: other.PersonID, TaskID: work.thread.ID, RunID: work.asking.ID,
		Channel: "cli", Platform: "cli", PlatformUserID: "bob", StartedAt: time.Now(), Cancel: func() {},
		Steer: make(chan kernel.SteeringInput, 4)}
	if !daemon.coordinator().beginActive(other.PersonID, work.active) {
		t.Fatal("cannot register Bob's active run")
	}
	t.Cleanup(func() { daemon.coordinator().endActiveRun(other.PersonID, work.active) })
	return other, work
}

// sameAsMissing sends one request naming another person's id and the same
// request naming an id nobody has, and requires the two answers to match once
// each id reads as <id>.
func sameAsMissing(t *testing.T, send func(id string) (int, string), foreign, missing string) {
	t.Helper()
	foreignStatus, foreignBody := send(foreign)
	missingStatus, missingBody := send(missing)
	foreignBody = strings.ReplaceAll(foreignBody, foreign, "<id>")
	missingBody = strings.ReplaceAll(missingBody, missing, "<id>")
	if foreignStatus != missingStatus || foreignBody != missingBody {
		t.Fatalf("another person's id answered differently from a missing one:\nforeign %d %q\nmissing %d %q",
			foreignStatus, foreignBody, missingStatus, missingBody)
	}
}

func TestExplicitTargetsNeverReachAnotherPersonsWork(t *testing.T) {
	provider := newSlowLLMProvider("Done.")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	other, work := seedOtherPersonsWork(t, daemon, store)
	asAlice := func(req api.MessageRequest) (int, string) {
		req.Platform, req.PlatformUserID = "cli", "alice"
		if req.Channel == "" {
			req.Channel = "cli"
		}
		resp, status := daemon.ProcessMessage(ctx, req)
		return status, resp.Content + "|" + resp.Error
	}
	bobsRuns := func() int {
		runs, err := store.ListTaskRuns(ctx, other.TenantID, work.thread.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}

	t.Run("task_id", func(t *testing.T) {
		before := bobsRuns()
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "alice", Channel: "cli",
			TaskID: work.thread.ID, Content: "add the changelog entry"})
		if status != http.StatusOK || resp.Run == nil {
			t.Fatalf("a task id the person cannot see must read as unknown, not fail: status=%d resp=%+v", status, resp)
		}
		if resp.Run.TaskID == work.thread.ID || resp.Run.PersonID != person.PersonID || bobsRuns() != before {
			t.Fatalf("Alice's turn attached to Bob's thread: run=%+v bob's runs %d -> %d", resp.Run, before, bobsRuns())
		}
		missing, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "alice", Channel: "cli",
			TaskID: "task_00000000-0000-0000-0000-000000000000", Content: "add the changelog entry"})
		if status != http.StatusOK || missing.Run == nil || missing.Run.TaskID == work.thread.ID {
			t.Fatalf("an unknown task id = status %d resp %+v", status, missing)
		}
		// Ownership is the person's, not the account's: Alice's own Thread,
		// named from another endpoint she has bound, still continues there.
		if _, err := store.BindAccount(ctx, person.TenantID, person.PersonID, "weixin", "wx-alice", "Alice"); err != nil {
			t.Fatal(err)
		}
		own, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-alice", Channel: "wx-alice",
			TaskID: resp.Run.TaskID, Content: "and tag the release"})
		if status != http.StatusOK || own.Run == nil || own.Run.TaskID != resp.Run.TaskID || own.Run.PersonID != person.PersonID {
			t.Fatalf("the person's own task id must still attach from any of her endpoints: status=%d resp=%+v", status, own)
		}
	})
	t.Run("reply_to_run_id", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return asAlice(api.MessageRequest{ReplyToRunID: id, Content: "carry on"})
		}, work.parked.ID, "run_00000000-0000-0000-0000-000000000000")
	})
	t.Run("approval_id", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return asAlice(api.MessageRequest{ApprovalID: id, Content: "carry on"})
		}, work.approval.ID, "apr_00000000-0000-0000-0000-000000000000")
	})
	t.Run("clarify_id", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return asAlice(api.MessageRequest{ClarifyID: id, Content: "the staging bucket"})
		}, work.clarify.ID, "clr_00000000-0000-0000-0000-000000000000")
	})
	t.Run("choice_id", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return asAlice(api.MessageRequest{ChoiceID: id, Content: "1"})
		}, work.choice.ID, "choice_00000000000000000000000000000000")
	})
	for _, command := range []string{"/resume %s", "/stop %s", "/status %s", "/views %s", "/apply %s", "/choose %s 1",
		"/approve %s", "/reject %s", "/watchers %s", "/effects resolve claim_1 %s"} {
		t.Run(strings.Fields(command)[0], func(t *testing.T) {
			foreign, missing := work.parked.ID, "run_00000000-0000-0000-0000-000000000000"
			switch strings.Fields(command)[0] {
			case "/choose":
				foreign, missing = work.choice.ID, "choice_00000000000000000000000000000000"
			case "/approve", "/reject":
				foreign, missing = work.approval.ID, "apr_00000000-0000-0000-0000-000000000000"
			case "/watchers", "/effects":
				foreign, missing = work.watch.ID, "watch_00000000-0000-0000-0000-000000000000"
			}
			sameAsMissing(t, func(id string) (int, string) {
				return asAlice(api.MessageRequest{Content: strings.Replace(command, "%s", id, 1)})
			}, foreign, missing)
		})
	}
	t.Run("/resume thread", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return asAlice(api.MessageRequest{Content: "/resume " + id})
		}, work.thread.ID, "task_00000000-0000-0000-0000-000000000000")
	})

	server := httptest.NewServer(daemon.Handler())
	defer server.Close()
	call := func(method, path, body string) (int, string) {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	t.Run("/v1/runs/steer", func(t *testing.T) {
		sameAsMissing(t, func(id string) (int, string) {
			return call(http.MethodPost, "/v1/runs/steer",
				`{"platform":"cli","platform_user_id":"alice","channel":"cli","run_id":"`+id+`","text":"also update the docs"}`)
		}, work.asking.ID, "run_00000000-0000-0000-0000-000000000000")
		if len(work.active.Steer) != 0 {
			t.Fatal("Alice's guidance reached Bob's run")
		}
	})
	for _, path := range []string{"/v1/tasks/events", "/v1/tasks/artifacts", "/v1/tasks/events/stream"} {
		t.Run(path, func(t *testing.T) {
			sameAsMissing(t, func(id string) (int, string) {
				return call(http.MethodGet, path+"?platform=cli&platform_user_id=alice&once=true&task_id="+id, "")
			}, work.thread.ID, "task_00000000-0000-0000-0000-000000000000")
		})
	}
	t.Run("work_select", func(t *testing.T) {
		thread, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "Alice", Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.StartRun(ctx, thread, "cli", "look at earlier work")
		if err != nil {
			t.Fatal(err)
		}
		tool := tools.NewWorkSelectTool(store)
		sameAsMissing(t, func(id string) (int, string) {
			_, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": id, "_context": ctx,
				"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID,
					TaskID: thread.ID, RunID: current.ID}})
			if err == nil {
				return 0, "selected"
			}
			return 1, err.Error()
		}, work.parked.ID, "run_00000000-0000-0000-0000-000000000000")
	})

	// Nothing of Bob's moved.
	if got, err := store.GetApprovalRequest(ctx, other.TenantID, work.approval.ID); err != nil || got.Status != "pending" {
		t.Fatalf("Bob's approval changed: %+v %v", got, err)
	}
	if got, err := store.GetClarifyRequest(ctx, other.TenantID, work.clarify.ID); err != nil || got.Status != "pending" {
		t.Fatalf("Bob's question changed: %+v %v", got, err)
	}
	if _, err := store.PeekPendingTurnChoice(ctx, other.TenantID, other.PersonID, work.choice.ID, time.Now()); err != nil {
		t.Fatalf("Bob's choice changed: %v", err)
	}
}
