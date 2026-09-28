package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestRunEventBrokerIsPersonScopedAndSequencesEvents(t *testing.T) {
	hub := newRunEventBroker(nil)
	a, stopA := hub.subscribe("person-a", "", "")
	defer stopA()
	b, stopB := hub.subscribe("person-b", "", "")
	defer stopB()

	hub.publish(api.RunEvent{PersonID: "person-a", RunID: "run-1", Type: "assistant.delta", Durability: api.EventEphemeral, CreatedAt: time.Now()})
	select {
	case event := <-a.ch:
		if event.Type != "assistant.delta" || event.LiveSeq != 1 {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("person-a did not receive its delta")
	}
	select {
	case event := <-b.ch:
		t.Fatalf("person-b received cross-person delta: %+v", event)
	default:
	}

	hub.publish(api.RunEvent{PersonID: "person-a", RunID: "run-1", Type: "run.finished", Durability: api.EventDurable, CreatedAt: time.Now()})
	event := <-a.ch
	if event.LiveSeq != 2 {
		t.Fatalf("terminal live sequence=%d, want 2", event.LiveSeq)
	}
	hub.publish(api.RunEvent{PersonID: "person-a", RunID: "run-1", Type: "assistant.delta", Durability: api.EventEphemeral, CreatedAt: time.Now()})
	if event = <-a.ch; event.LiveSeq != 1 {
		t.Fatalf("completed run sequence was not released: %+v", event)
	}
}

func TestEventsStreamReplaysAfterCursorAndScopesPerson(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "stream", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "tool.started"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "tool.completed"})
	if err != nil {
		t.Fatal(err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream?platform=cli&platform_user_id=alice&once=true", nil)
	req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", first.Cursor))
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, fmt.Sprintf("id: %d", second.Cursor)) || !strings.Contains(body, "event: tool.completed") {
		t.Fatalf("missing replayed event after cursor %d:\n%s", first.Cursor, body)
	}
	if strings.Contains(body, "event: tool.started") {
		t.Fatalf("event at the supplied cursor was replayed:\n%s", body)
	}

	stranger := httptest.NewRequest(http.MethodGet, "/v1/events/stream?platform=cli&platform_user_id=bob&cursor=0&once=true", nil)
	strangerRec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(strangerRec, stranger)
	if strings.Contains(strangerRec.Body.String(), "tool.completed") {
		t.Fatalf("cross-person event leak:\n%s", strangerRec.Body.String())
	}
}

// A run's text and tools reach only the session that started it and a client
// attached to that run; its lifecycle and human waits reach every session of
// the person. Every session used to receive every run's transcript, so two
// terminals both showed one run's output.
func TestLiveEventsReachOnlyTheRunsSession(t *testing.T) {
	hub := newRunEventBroker(nil)
	subscribers := map[string]*runEventSubscriber{}
	for name, spec := range map[string][2]string{
		"own": {"session-a", ""}, "other": {"session-b", ""}, "legacy": {"", ""}, "attached": {"session-b", "run-a"},
	} {
		sub, stop := hub.subscribe("person", spec[0], spec[1])
		defer stop()
		subscribers[name] = sub
	}
	for _, tc := range []struct {
		event api.RunEvent
		want  []string
	}{
		{api.RunEvent{RunID: "run-a", Channel: "session-a", Type: "assistant.delta"}, []string{"own", "attached"}},
		{api.RunEvent{RunID: "run-a", Channel: "session-a", Type: "tool.started"}, []string{"own", "attached"}},
		{api.RunEvent{RunID: "run-a", Channel: "session-a", Type: "plan.updated"}, []string{"own", "attached"}},
		{api.RunEvent{RunID: "run-a", Channel: "session-a", Type: "run.started"}, []string{"own", "other", "legacy", "attached"}},
		{api.RunEvent{RunID: "run-a", Channel: "session-a", Type: "approval.requested"}, []string{"own", "other", "legacy", "attached"}},
		{api.RunEvent{Type: "memory.disposition"}, []string{"own", "other", "legacy", "attached"}},
	} {
		tc.event.PersonID, tc.event.CreatedAt = "person", time.Now()
		hub.publish(tc.event)
		want := map[string]bool{}
		for _, name := range tc.want {
			want[name] = true
		}
		for name, sub := range subscribers {
			select {
			case <-sub.ch:
				if !want[name] {
					t.Errorf("%s subscriber received %s of session %q", name, tc.event.Type, tc.event.Channel)
				}
			default:
				if want[name] {
					t.Errorf("%s subscriber missed %s of session %q", name, tc.event.Type, tc.event.Channel)
				}
			}
		}
	}
}

// Replay after a reconnect shows a session no more than the live stream did.
func TestEventReplayKeepsTheSessionAudience(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "stream", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []control.Event{
		{TaskID: task.ID, Channel: "session-a", Type: "run.started"},
		{TaskID: task.ID, Channel: "session-a", Type: "tool.started", Payload: []byte(`{"tool":"read_parser"}`)},
		{TaskID: task.ID, Channel: "session-b", Type: "tool.started", Payload: []byte(`{"tool":"read_changelog"}`)},
	} {
		if _, err := store.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	replay := func(query string) string {
		rec := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events/stream?platform=cli&platform_user_id=alice&cursor=0&once=true"+query, nil))
		return rec.Body.String()
	}
	body := replay("&session=session-b")
	if !strings.Contains(body, "event: run.started") || !strings.Contains(body, "read_changelog") || strings.Contains(body, "read_parser") {
		t.Fatalf("session-b replay did not keep the audience:\n%s", body)
	}
	if body := replay(""); strings.Contains(body, "read_parser") || strings.Contains(body, "read_changelog") || !strings.Contains(body, "event: run.started") {
		t.Fatalf("a client naming no session replayed session detail:\n%s", body)
	}
}

// Two terminals of one person, end to end through the gateway. Terminal A
// starts a task; terminal B types while it runs. B is told its message went to
// A's task, and B's stream carries only the person-wide facts of A's run —
// never its text; A's stream carries its run in full. Whatever B's input
// becomes, its detail reaches B alone.
func TestTwoTerminalsSeeOnlyTheirOwnSessionsDetail(t *testing.T) {
	provider := newSlowLLMProvider("the answer for A")
	daemon, _, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := daemon.Control.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	subA, closeA := daemon.events().subscribe(identity.PersonID, "session-a", "")
	defer closeA()
	subB, closeB := daemon.events().subscribe(identity.PersonID, "session-b", "")
	defer closeB()

	if resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "long task in A", Async: true}); !resp.Accepted {
		t.Fatalf("A's task not accepted: %+v", resp)
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("A's run never reached the provider")
	}
	receipt, _ := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b", Content: "also check the logs"})
	if receipt.Turn == nil || receipt.Turn.Status != "accepted" || !strings.Contains(receipt.Content, "running in another session") {
		t.Fatalf("B's receipt = %+v", receipt)
	}
	provider.releaseNow()
	waitUntil(t, 10*time.Second, func() bool { return daemon.coordinator().currentActive(identity.PersonID) == nil }, "runs did not finish")

	collect := func(sub *runEventSubscriber) []api.RunEvent {
		var events []api.RunEvent
		for {
			select {
			case event := <-sub.ch:
				events = append(events, event)
			case <-time.After(200 * time.Millisecond):
				return events
			}
		}
	}
	seenA, seenB := collect(subA), collect(subB)
	textOf := func(events []api.RunEvent, channel string) string {
		var text strings.Builder
		for _, event := range events {
			if event.Type == "assistant.delta" && event.Channel == channel {
				text.Write(event.Payload)
			}
		}
		return text.String()
	}
	if !strings.Contains(textOf(seenA, "session-a"), "the answer for A") {
		t.Fatalf("A did not receive its own run's text: %+v", seenA)
	}
	for _, check := range []struct {
		name    string
		events  []api.RunEvent
		foreign string
	}{{"B", seenB, "session-a"}, {"A", seenA, "session-b"}} {
		sawLifecycle := false
		for _, event := range check.events {
			if event.Channel == check.foreign && !personWideRunEventTypes[event.Type] {
				t.Fatalf("terminal %s received another session's %s", check.name, event.Type)
			}
			sawLifecycle = sawLifecycle || (event.Type == "run.started" && event.Channel == check.foreign)
		}
		if check.name == "B" && !sawLifecycle {
			t.Fatalf("terminal B was not told that A's run started: %+v", check.events)
		}
	}
}
