package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestResumeSelectionBelongsToTheSelectingSession(t *testing.T) {
	provider := newSlowLLMProvider("I handled this input.")
	provider.releaseNow()
	d, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	task := parkEmptyTask(t, d, "unfinished report")
	parent := seedUnresolvedRun(t, store, task.TenantID, task, "session-a", "assemble report", "waiting_user")
	selected, code := d.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "/resume " + parent.ID})
	if code != http.StatusOK || !strings.Contains(selected.Content, "Selected run") {
		t.Fatalf("select: %d %+v", code, selected)
	}
	other, code := d.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b", Content: "inspect another project"})
	if code != http.StatusOK || other.Run == nil || other.Run.ResumesRunID != "" {
		t.Fatalf("another session captured selection: %d %+v", code, other)
	}
	continued, code := d.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "continue the selected report"})
	if code != http.StatusOK || continued.Run == nil || continued.Run.ResumesRunID != parent.ID {
		t.Fatalf("selecting session lost exact target: %d %+v", code, continued)
	}
	again, code := d.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "a separate question"})
	if code != http.StatusOK || again.Run == nil || again.Run.ResumesRunID != "" {
		t.Fatalf("selection was not one-shot: %d %+v", code, again)
	}
}

func TestLegacyPersonWideResumePinIsNotInputAuthority(t *testing.T) {
	provider := newSlowLLMProvider("This is new work.")
	provider.releaseNow()
	d, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	task := parkEmptyTask(t, d, "old selected work")
	parent := seedUnresolvedRun(t, store, task.TenantID, task, "old-session", "old input", "waiting_user")
	if err := store.PinResumeSelection(ctx, task.TenantID, task.PersonID, task.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	response, code := d.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "new-session", Content: "new independent work"})
	if code != http.StatusOK || response.Run == nil || response.Run.ResumesRunID != "" || response.Task.ID == task.ID {
		t.Fatalf("historical global pin granted continuation: %d %+v", code, response)
	}
}

func TestSessionSelectionConsumedAtomically(t *testing.T) {
	_, store, owner := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, owner, "work", "waiting_user", 0)
	parent := seedUnresolvedRun(t, store, owner.TenantID, task, "origin", "prepare", "waiting_user")
	if err := store.PinSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, "origin", task.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan *control.ResumeSelection, 2)
	for range 2 {
		go func() {
			result, err := store.TakeSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, "origin")
			if err != nil {
				t.Errorf("take: %v", err)
			}
			results <- result
		}()
	}
	count := 0
	for range 2 {
		if <-results != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("selection consumed %d times", count)
	}
}

func TestResumeSelectionIsCapturedBeforeQueueAdmission(t *testing.T) {
	d, store, owner := newTaskViewServer(t)
	ctx := context.Background()
	task := seedTask(t, store, owner, "selected work", "waiting_user", 0)
	parent := seedUnresolvedRun(t, store, owner.TenantID, task, "origin", "prepare", "waiting_user")
	active := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: "busy", TaskID: "busy-thread", Channel: "busy-session"}
	coord := d.coordinator()
	coord.activeLimit = 1
	if !coord.beginActive(owner.PersonID, active) {
		t.Fatal("register busy slot")
	}
	defer coord.endActiveRun(owner.PersonID, active)
	request := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "selected-session", Content: "older independent input"}
	older, code := d.ProcessMessage(ctx, request)
	if code != 200 || older.Turn == nil || older.Turn.QueueID == "" {
		t.Fatalf("older not queued: %d %+v", code, older)
	}
	if err := store.PinSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, resumeSelectionEndpoint(owner, request.Channel), task.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	request.Content = "additional requirement for the selected work"
	newer, code := d.ProcessMessage(ctx, request)
	if code != 200 || newer.Turn == nil || newer.Turn.QueueID == "" {
		t.Fatalf("selected not queued: %d %+v", code, newer)
	}
	before, err := store.GetQueued(ctx, owner.TenantID, older.Turn.QueueID)
	if err != nil || before == nil || before.ReplyToRunID != "" {
		t.Fatalf("older input captured new selection: %+v %v", before, err)
	}
	after, err := store.GetQueued(ctx, owner.TenantID, newer.Turn.QueueID)
	if err != nil || after == nil || after.ReplyToRunID != parent.ID {
		t.Fatalf("queued input lost exact target: %+v %v", after, err)
	}
	selected, err := store.SessionResumeSelection(ctx, owner.TenantID, owner.PersonID, resumeSelectionEndpoint(owner, request.Channel))
	if err != nil || selected != nil {
		t.Fatalf("mutable selection survived admission: %+v %v", selected, err)
	}
}

func TestResumeSelectionDoesNotCrossBoundAccountsWithSameChannel(t *testing.T) {
	d, store, owner := newTaskViewServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "linked", "Linked endpoint"); err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "telegram", "linked", "Linked endpoint")
	if err != nil {
		t.Fatal(err)
	}
	task := seedTask(t, store, owner, "unfinished work", "waiting_user", 0)
	parent := seedUnresolvedRun(t, store, owner.TenantID, task, "same-channel", "prepare", "waiting_user")
	_, _, err = d.selectExactResumeRun(ctx, owner, "same-channel", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.TakeSessionResumeSelection(ctx, other.TenantID, other.PersonID, resumeSelectionEndpoint(other, "same-channel"))
	if err != nil || got != nil {
		t.Fatalf("other platform took selection: %+v %v", got, err)
	}
	got, err = store.TakeSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, resumeSelectionEndpoint(owner, "same-channel"))
	if err != nil || got == nil || got.RunID != parent.ID {
		t.Fatalf("source lost selection: %+v %v", got, err)
	}
}
