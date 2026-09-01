package cli

import (
	"strings"
	"testing"
)

func TestWatcherEventIsBoundToRunAndDeduplicated(t *testing.T) {
	c := NewController("", "", nil, "")
	model := c.model
	model.watchingRun = true
	model.watchedRunID = "run-current"
	model.runStatus = "working"

	event := uiEventRef{
		Source:  eventSourceWatch,
		RunID:   "run-current",
		EventID: "event-tool-started",
	}
	msg := MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call-1",
		Args:       `{"path":"README.md"}`,
		Event:      event,
	}
	updated, _ := model.updateInner(msg)
	model = updated.(*uiModel)
	updated, _ = model.updateInner(msg)
	model = updated.(*uiModel)

	if len(model.processState().tools) != 1 {
		t.Fatalf("duplicate durable event rendered %d active tools, want 1", len(model.processState().tools))
	}

	updated, _ = model.updateInner(MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call-old",
		Args:       `{"path":"old.md"}`,
		Event: uiEventRef{
			Source:  eventSourceWatch,
			RunID:   "run-old",
			EventID: "event-old",
		},
	})
	model = updated.(*uiModel)
	if len(model.processState().tools) != 1 {
		t.Fatalf("event from another run was rendered: %+v", model.processState().tools)
	}
}

func TestDetachWatcherFinalizesOldStreamBeforeNewUserMessage(t *testing.T) {
	c := NewController("", "", nil, "")
	model := c.model
	model.watchingRun = true
	model.watchedRunID = "run-old"

	updated, _ := model.updateInner(MsgStream{
		Content: "old run answer",
		Event: uiEventRef{
			Source:  eventSourceWatch,
			RunID:   "run-old",
			EventID: "event-old-delta",
		},
	})
	model = updated.(*uiModel)

	model.detachWatchedRunForNewTurn()
	model.addMessage("user", "new request")

	updated, _ = model.updateInner(MsgStream{
		Content: " late old output",
		Event: uiEventRef{
			Source:  eventSourceWatch,
			RunID:   "run-old",
			EventID: "event-late-delta",
		},
	})
	model = updated.(*uiModel)

	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v, want old assistant followed by new user", model.messages)
	}
	if model.messages[0].Role != "assistant" || model.messages[0].Content != "old run answer" {
		t.Fatalf("old stream was not finalized first: %+v", model.messages)
	}
	if model.messages[1].Role != "user" || model.messages[1].Content != "new request" {
		t.Fatalf("new user message order is wrong: %+v", model.messages)
	}
}

func TestDigestHasDedicatedRenderer(t *testing.T) {
	rendered := renderCell(ChatMessage{
		Role:    "digest",
		Content: "While you were away:\nThe previous task finished.",
	}, 80)

	if strings.Contains(rendered, "Learning") {
		t.Fatalf("digest must not render as a learning event: %q", rendered)
	}
	if !strings.Contains(rendered, "While you were away:") {
		t.Fatalf("digest content missing: %q", rendered)
	}
}

func TestFinishRunLifecycleToolIsHidden(t *testing.T) {
	for _, name := range []string{"finish_run", "update_plan"} {
		if !isHiddenLifecycleTool(name) {
			t.Fatalf("%s should be hidden from the ordinary tool transcript", name)
		}
	}
	if isHiddenLifecycleTool("read_file") {
		t.Fatal("ordinary tools must remain visible")
	}
}
