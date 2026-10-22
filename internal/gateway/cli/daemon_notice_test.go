package cli

import (
	"bytes"
	"testing"
	"time"

	"selfmind/internal/gateway/api"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDurableBackgroundNoticeReplaysOnceThroughCLI(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.channel = "other-session"
	model.daemonRunID, model.daemonRunActive, model.runStatus = "current-run", true, "working"
	var output bytes.Buffer
	program := tea.NewProgram(model, tea.WithInput(bytes.NewReader(nil)), tea.WithOutput(&output), tea.WithoutSignalHandler(), tea.WithoutRenderer())
	model.program = program
	done := make(chan tea.Model, 1)
	errs := make(chan error, 1)
	go func() {
		final, err := program.Run()
		if err != nil {
			errs <- err
			return
		}
		done <- final
	}()
	event := api.RunEvent{Type: "background.notice", EventID: "durable-notice", Cursor: 12,
		RunID: "older-run", Channel: "origin-session", Durability: api.EventDurable,
		Payload: []byte(`{"message":"A queued continuation did not start. Use /resume older-run to inspect.","kind":"warning"}`)}
	model.forwardDaemonRunEvent(event)
	// A reconnected stream carries the same id with a different live sequence.
	event.LiveSeq = 9
	model.forwardDaemonRunEvent(event)
	program.Quit()
	select {
	case err := <-errs:
		t.Fatal(err)
	case final := <-done:
		got := final.(*uiModel)
		if len(got.messages) != 1 || got.messages[0].Role != "notice" {
			t.Fatalf("durable notice replay did not render exactly once: %+v", got.messages)
		}
		if !got.daemonRunActive || got.daemonRunID != "current-run" || got.runStatus != "working" {
			t.Fatal("a notice started execution or changed the active Run")
		}
	case <-time.After(3 * time.Second):
		program.Kill()
		t.Fatal("CLI notice pipeline did not drain")
	}
}

func TestQueuedStartupFailureUsesOnlyItsSessionNoticeSurface(t *testing.T) {
	testQueuedNoticeSurface(t, "queue.start_failed", []byte(`{"message":"Queued work could not start. Its input was retained; use /queue.","kind":"warning"}`))
}

func TestQueuedContinuationChoiceReplaysOnOnlyItsSession(t *testing.T) {
	testQueuedNoticeSurface(t, "continuation.candidates", []byte(`{"choice_id":"choice_exact","question":"Which work?\n1. First\n2. Second\n3. This is new work"}`))
}

func testQueuedNoticeSurface(t *testing.T, eventType string, payload []byte) {
	t.Helper()
	model := NewController("", "", nil, "").model
	model.channel = "origin-session"
	model.daemonRunID, model.daemonRunActive, model.runStatus = "current-run", true, "working"
	var output bytes.Buffer
	program := tea.NewProgram(model, tea.WithInput(bytes.NewReader(nil)), tea.WithOutput(&output), tea.WithoutSignalHandler(), tea.WithoutRenderer())
	model.program = program
	done := make(chan tea.Model, 1)
	errs := make(chan error, 1)
	go func() {
		final, err := program.Run()
		if err != nil {
			errs <- err
			return
		}
		done <- final
	}()
	event := api.RunEvent{Type: eventType, Channel: model.channel, Durability: api.EventDurable,
		EventID: "queued-notice-exact", Cursor: 20, Payload: payload}
	model.forwardDaemonRunEvent(event)
	event.LiveSeq = 30
	model.forwardDaemonRunEvent(event)
	event.Channel = "other-session"
	model.forwardDaemonRunEvent(event)
	program.Quit()
	select {
	case err := <-errs:
		t.Fatal(err)
	case final := <-done:
		got := final.(*uiModel)
		if len(got.messages) != 1 || got.messages[0].Role != "notice" || !got.daemonRunActive || got.daemonRunID != "current-run" {
			t.Fatalf("startup failure lost its notice, leaked another session or changed execution: %+v", got.messages)
		}
	case <-time.After(3 * time.Second):
		program.Kill()
		t.Fatal("CLI startup notice did not drain")
	}
}
