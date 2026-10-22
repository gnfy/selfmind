package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

// parkedChoiceFixture parks one unbound IM request as a durable multi-Run
// choice while two of the owner's Runs are live: option 1 is Run A, option 2
// is Run B, and option 3 is new work.
type parkedChoiceFixture struct {
	daemon *Server
	store  *control.Store
	owner  *control.IdentityContext
	runA   *activeRun
	runB   *activeRun
	choice *api.TurnChoice
	parked string
}

func newParkedChoiceFixture(t *testing.T, platform, platformUserID, channel string) *parkedChoiceFixture {
	t.Helper()
	ctx := context.Background()
	daemon, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range [][2]string{{"weixin", "wx-local"}, {"telegram", "tg-local"}} {
		if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, binding[0], binding[1], "Local on "+binding[0]); err != nil {
			t.Fatal(err)
		}
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	f := &parkedChoiceFixture{daemon: daemon, store: store, owner: owner, parked: "drop the users table in prod"}
	for i, session := range []string{"session-a", "session-b"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Work " + session, Channel: session})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, session, "work for "+session)
		if err != nil {
			t.Fatal(err)
		}
		handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
			Channel: session, Platform: "cli", PlatformUserID: "local", Summary: "work for " + session,
			Steer: make(chan kernel.SteeringInput, 4), StartedAt: run.StartedAt.Add(time.Duration(i) * time.Second)}
		if !coord.beginActive(owner.PersonID, handle) {
			t.Fatal("cannot register active run")
		}
		t.Cleanup(func() { coord.endActiveRun(owner.PersonID, handle) })
		if i == 0 {
			f.runA = handle
		} else {
			f.runB = handle
		}
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: platform, PlatformUserID: platformUserID, Channel: channel, Content: f.parked, Async: true,
	})
	if code != 200 || response.Choice == nil || len(response.Choice.Options) != 3 {
		t.Fatalf("unbound IM input was not parked as a choice: code=%d response=%+v", code, response)
	}
	f.choice = response.Choice
	return f
}

func (f *parkedChoiceFixture) send(t *testing.T, req api.MessageRequest) api.MessageResponse {
	t.Helper()
	req.Async = req.Platform != "cli"
	response, code := f.daemon.ProcessMessage(context.Background(), req)
	if code >= 500 {
		t.Fatalf("message %+v failed: code=%d response=%+v", req, code, response)
	}
	return response
}

// steered drains everything a live Run received so a later step starts empty.
func steered(run *activeRun) []string {
	var out []string
	for {
		select {
		case input := <-run.Steer:
			out = append(out, input.Content)
		default:
			return out
		}
	}
}

func (f *parkedChoiceFixture) requireChoicePending(t *testing.T) {
	t.Helper()
	pending, err := f.store.PeekPendingTurnChoice(context.Background(), f.owner.TenantID, f.owner.PersonID, f.choice.ID, time.Now())
	if err != nil || pending == nil || !strings.Contains(pending.RequestJSON, f.parked) {
		t.Fatalf("the parked request lost its choice: %+v, %v", pending, err)
	}
}

func (f *parkedChoiceFixture) requireNothingSteered(t *testing.T, step string) {
	t.Helper()
	if a, b := steered(f.runA), steered(f.runB); len(a)+len(b) != 0 {
		t.Fatalf("%s reached a live Run: A=%q B=%q", step, a, b)
	}
}

func (f *parkedChoiceFixture) requireParkedTextNotQueued(t *testing.T) {
	t.Helper()
	queued, err := f.store.ListQueued(context.Background(), f.owner.TenantID, f.owner.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range queued {
		if item.Content == f.parked {
			t.Fatalf("the parked request became queued work without its own endpoint's answer: %+v", item)
		}
	}
}

// A bare number answers only the choice shown on the same account and
// channel. The same digits typed in another CLI window, in a chat with the
// same channel name on another account, or in another chat of the same account
// are that endpoint's own input; the IM request keeps waiting for its answer.
func TestBareNumberAnswersOnlyItsOwnEndpointChoice(t *testing.T) {
	ctx := context.Background()
	f := newParkedChoiceFixture(t, "weixin", "wx-local", "chat-one")

	window := f.send(t, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli-window-c", Content: "2"})
	f.requireNothingSteered(t, "a number typed in an idle CLI window")
	f.requireChoicePending(t)
	if window.Turn == nil || window.Turn.QueueID == "" || window.Choice != nil {
		t.Fatalf("the CLI window's number was not handled as that window's input: %+v", window)
	}
	own, err := f.store.GetQueued(ctx, f.owner.TenantID, window.Turn.QueueID)
	if err != nil || own == nil || own.Content != "2" || own.Channel != "cli-window-c" {
		t.Fatalf("the CLI window's own input was replaced: %+v, %v", own, err)
	}

	for _, other := range []api.MessageRequest{
		// Same channel string, different platform account.
		{Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one", Content: "1"},
		// Same account, different chat.
		{Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat-two", Content: "3"},
	} {
		response := f.send(t, other)
		f.requireNothingSteered(t, other.Platform+"/"+other.Channel)
		f.requireChoicePending(t)
		f.requireParkedTextNotQueued(t)
		if response.Choice == nil || response.Choice.ID == f.choice.ID {
			t.Fatalf("%s/%s did not get its own question for its own input: %+v", other.Platform, other.Channel, response)
		}
		own, err := f.store.PeekPendingTurnChoice(ctx, f.owner.TenantID, f.owner.PersonID, response.Choice.ID, time.Now())
		if err != nil || !strings.Contains(own.RequestJSON, `"content":"`+other.Content+`"`) {
			t.Fatalf("%s/%s input was not parked as its own request: %+v, %v", other.Platform, other.Channel, own, err)
		}
	}

	// The endpoint that saw the question still answers it with a bare number,
	// even though the other endpoints now have questions of their own.
	answer := f.send(t, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat-one", Content: "2"})
	if got := steered(f.runB); len(got) != 1 || got[0] != f.parked || len(steered(f.runA)) != 0 {
		t.Fatalf("the original endpoint's answer did not route its parked request to Run B: B=%q response=%+v", got, answer)
	}
	if answer.Turn == nil || answer.Turn.RunID != f.runB.RunID {
		t.Fatalf("answer receipt names the wrong Run: %+v", answer)
	}
	if _, err := f.store.PeekPendingTurnChoice(ctx, f.owner.TenantID, f.owner.PersonID, f.choice.ID, time.Now()); !errors.Is(err, control.ErrTurnChoiceNotFound) {
		t.Fatalf("answered choice is still pending: %v", err)
	}
}

// A platform-proven reply, approval, or clarification edge names the work a
// short answer belongs to. The bare-number lookup must not run at all, even on
// the very endpoint where a choice is waiting.
func TestExactEdgeSkipsBareNumberChoiceLookup(t *testing.T) {
	ctx := context.Background()
	f := newParkedChoiceFixture(t, "telegram", "tg-local", "chat-one")
	linked, err := f.store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: f.owner.TenantID, PersonID: f.owner.PersonID, Platform: "telegram", PlatformUserID: "tg-local",
		Channel: "chat-one", TaskID: f.runA.TaskID, RunID: f.runA.RunID, Content: "Work A is running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.store.ClaimDelivery(ctx, linked.ID); err != nil || !claimed {
		t.Fatalf("claim outbound reply source: %v, %v", claimed, err)
	}
	if err := f.store.MarkDeliverySentWithNativeID(ctx, linked.ID, "501"); err != nil {
		t.Fatal(err)
	}

	// A native reply "2" to Run A's message is guidance for Run A, not option 2.
	native := f.send(t, api.MessageRequest{Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one",
		Content: "2", NativeReplyMessageID: "501"})
	if a, b := steered(f.runA), steered(f.runB); len(a) != 1 || a[0] != "2" || len(b) != 0 {
		t.Fatalf("native reply did not reach its edge target alone: A=%q B=%q response=%+v", a, b, native)
	}
	if native.Turn == nil || native.Turn.RunID != f.runA.RunID {
		t.Fatalf("native reply receipt names the wrong Run: %+v", native)
	}
	f.requireChoicePending(t)

	// Structured reply metadata with another number reaches its own target.
	structured := f.send(t, api.MessageRequest{Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one",
		Content: "1", ReplyToRunID: f.runB.RunID})
	if a, b := steered(f.runA), steered(f.runB); len(b) != 1 || b[0] != "1" || len(a) != 0 {
		t.Fatalf("reply metadata did not reach its edge target alone: A=%q B=%q response=%+v", a, b, structured)
	}
	f.requireChoicePending(t)

	// An approval or clarification edge never answers the waiting choice
	// either, whatever its own path does with the stale edge.
	for _, edge := range []api.MessageRequest{
		{Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one", Content: "3", ApprovalID: "apr_missing"},
		{Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one", Content: "2", ClarifyID: "clarify_missing"},
	} {
		f.send(t, edge)
		f.requireNothingSteered(t, "an approval/clarification edge")
		f.requireChoicePending(t)
		f.requireParkedTextNotQueued(t)
	}
}

// The explicit control stays exact and person-scoped: any of the person's
// endpoints may answer, a stranger may not, and a stranger's attempt leaves
// the choice answerable.
func TestExplicitChooseAnswersFromAnyOwnEndpointOnly(t *testing.T) {
	ctx := context.Background()
	f := newParkedChoiceFixture(t, "weixin", "wx-local", "chat-one")
	if _, err := f.store.ResolveOrCreateAccount(ctx, "default", "telegram", "tg-stranger", "Stranger"); err != nil {
		t.Fatal(err)
	}
	foreign := f.send(t, api.MessageRequest{Platform: "telegram", PlatformUserID: "tg-stranger", Channel: "chat-one",
		Content: "/choose " + f.choice.ID + " 1"})
	f.requireNothingSteered(t, "a stranger's /choose")
	f.requireChoicePending(t)
	if foreign.Accepted || !strings.Contains(foreign.Content, "expired or was already used") {
		t.Fatalf("a stranger's /choose was not refused: %+v", foreign)
	}

	selected := f.send(t, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli-window-c",
		Content: "/choose " + f.choice.ID + " 1"})
	if a, b := steered(f.runA), steered(f.runB); len(a) != 1 || a[0] != f.parked || len(b) != 0 {
		t.Fatalf("/choose from another endpoint did not route to Run A: A=%q B=%q response=%+v", a, b, selected)
	}
	if !selected.Accepted || selected.Turn == nil || selected.Turn.RunID != f.runA.RunID {
		t.Fatalf("/choose receipt names the wrong Run: %+v", selected)
	}
}
