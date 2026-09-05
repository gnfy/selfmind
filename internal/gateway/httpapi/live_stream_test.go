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
	a, stopA := hub.subscribe("person-a")
	defer stopA()
	b, stopB := hub.subscribe("person-b")
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
