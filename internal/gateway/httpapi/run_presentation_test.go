package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"strings"
	"testing"
)

func TestPresentationFollowsDurableParentNotTriggerOrWording(t *testing.T) {
	store := controltest.NewStore(t)
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Work", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "session-a", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(ctx, control.Event{RunID: parent.ID, Type: "run.started", Channel: "session-a", Payload: json.RawMessage(`{"input":"user work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := (&Server{Control: store}).coordinator()
	for _, tc := range []struct{ origin, channel, parent, want string }{
		{"provider_wait", "session-a", parent.ID, "foreground"},
		{"resource", "session-a", parent.ID, "foreground"},
		{"approval", "session-a", parent.ID, "foreground"},
		{"provider_wait", "session-b", parent.ID, "background"},
		{"provider_wait", "session-a", "", "background"},
		{"cron", "session-a", parent.ID, "background"},
		{"watch", "session-a", parent.ID, "background"},
	} {
		child := &control.Run{TenantID: parent.TenantID, PersonID: parent.PersonID, ResumesRunID: tc.parent, Channel: tc.channel}
		if got := c.runPresentation(ctx, child, api.MessageRequest{Origin: tc.origin, Content: "Any internal wording"}); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}

func TestCommittedAnswerSurvivesLiveAndReplayWithoutForeignLeak(t *testing.T) {
	store := controltest.NewStore(t)
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Work", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "session-a", "review")
	if err != nil {
		t.Fatal(err)
	}
	d := &Server{Control: store}
	hub := d.events()
	own, stop := hub.subscribe(identity.PersonID, "session-a", "")
	defer stop()
	other, stopOther := hub.subscribe(identity.PersonID, "session-b", "")
	defer stopOther()
	answer := "Conclusion\n" + strings.Repeat("private evidence. ", 300) + "\nNext step: none."
	_, err = store.MaterializeRunFinalization(ctx, control.RunFinalization{Identity: *identity, TaskID: task.ID, RunID: run.ID, RunStatus: "done", TaskStatus: "done", Channel: "session-a", AssistantContent: answer, Summary: "brief", Event: control.Event{Type: "run.finished", Channel: "session-a", Payload: json.RawMessage(`{"outcome":{"status":"done","summary":"brief"}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	assertAnswer := func(event api.RunEvent) {
		t.Helper()
		var p struct {
			Answer string `json:"final_answer"`
		}
		if err := json.Unmarshal(event.Payload, &p); err != nil || p.Answer != answer {
			t.Fatalf("saved reply absent: %v bytes=%d", err, len(p.Answer))
		}
	}
	for event := range own.ch {
		if event.Type == "run.finished" {
			assertAnswer(event)
			break
		}
	}
	if event := <-other.ch; strings.Contains(string(event.Payload), "private evidence") || strings.Contains(string(event.Payload), "final_answer") {
		t.Fatal("foreign terminal received full result")
	}
	for _, sub := range []*runEventSubscriber{own, other} {
		w := httptest.NewRecorder()
		cursor := int64(0)
		if !d.replayPersonEvents(ctx, w, w, identity, sub, &cursor) {
			t.Fatal("replay failed")
		}
		contains := strings.Contains(w.Body.String(), "private evidence")
		if contains != (sub == own) {
			t.Fatalf("replay leaked or lost answer: owner=%v contains=%v", sub == own, contains)
		}
	}
}
