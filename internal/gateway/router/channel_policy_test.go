package router

import (
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestChannelPolicyStreamsOnlyForCLI(t *testing.T) {
	if !ShouldStreamToClient("cli") || !ShouldStreamToClient("terminal") {
		t.Fatal("cli-like channels should stream to the client")
	}
	for _, channel := range []string{"weixin", "wechat", "telegram", "webhook", "dingtalk"} {
		if ShouldStreamToClient(channel) {
			t.Fatalf("%s should not stream token chunks to the client", channel)
		}
		if notice := WorkingNotice(channel); !strings.Contains(notice, "SelfMind is working on this") {
			t.Fatalf("working notice for %s = %q", channel, notice)
		}
	}
	if notice := WorkingNotice("cli"); notice != "" {
		t.Fatalf("cli working notice = %q, want empty", notice)
	}
}

// summarize feeds a turn's events to an EventSummary the way the run
// coordinator does, and returns the answer it delivers for content.
func summarize(content string, events ...llm.StreamEvent) string {
	var summary EventSummary
	for _, event := range events {
		summary.Observe(event)
	}
	return summary.WithContent(content)
}

func TestFinalAnswerDoesNotAppendInternalToolSummary(t *testing.T) {
	content := summarize("我暂时无法确认 GitHub 登录状态。",
		llm.StreamEvent{EventType: "agent.thinking", Content: "Thinking about the request"},
		llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolArgs: `{"command":"gh auth status"}`},
		llm.StreamEvent{EventType: "tool.output", ToolName: "terminal", Content: "SelfMind diagnostic instruction: this tool failed"},
		llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", Err: fmt.Errorf("command timed out after 30 seconds")},
		llm.StreamEvent{EventType: "stream", Content: "我暂时无法确认 GitHub 登录状态。"},
	)
	if content != "我暂时无法确认 GitHub 登录状态。" {
		t.Fatalf("content = %q", content)
	}
	for _, leaked := range []string{"Process summary", "Thinking about the request", "gh auth status", "command timed out", "SelfMind diagnostic instruction"} {
		if strings.Contains(content, leaked) {
			t.Fatalf("content leaked %q: %q", leaked, content)
		}
	}
}

func TestFinalAnswerUsesBriefFallbackWhenNoFinalContent(t *testing.T) {
	content := summarize("",
		llm.StreamEvent{EventType: "agent.thinking", Content: "Thinking about the request"},
		llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolArgs: `{"command":"go test ./..."}`},
		llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", Err: fmt.Errorf("command timed out after 30 seconds")},
	)
	if !strings.Contains(content, "tool error") {
		t.Fatalf("fallback content = %q", content)
	}
	for _, leaked := range []string{"Process summary", "Thinking about the request", "go test", "command timed out"} {
		if strings.Contains(content, leaked) {
			t.Fatalf("fallback leaked %q: %q", leaked, content)
		}
	}
}

func TestFinalAnswerMarksIncompleteTurnResumable(t *testing.T) {
	content := summarize("I updated the first file.",
		llm.StreamEvent{EventType: "stream", Content: "I updated the first file."},
		llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
			"status":            "incomplete",
			"completion_reason": "tool_budget_exhausted",
			"resumable":         true,
		}},
	)
	for _, want := range []string{"I updated the first file.", "tool budget exhausted", "continue"} {
		if !strings.Contains(content, want) {
			t.Fatalf("content = %q, want %q", content, want)
		}
	}
}
