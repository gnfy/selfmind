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
		if notice := WorkingNotice(channel); !strings.Contains(notice, "AI 正在处理") {
			t.Fatalf("working notice for %s = %q", channel, notice)
		}
	}
	if notice := WorkingNotice("cli"); notice != "" {
		t.Fatalf("cli working notice = %q, want empty", notice)
	}
}

func TestAggregateFinalResponsePrefersStructuredStream(t *testing.T) {
	stream := make(chan llm.StreamEvent, 4)
	stream <- llm.StreamEvent{EventType: "stream", Content: "hello "}
	stream <- llm.StreamEvent{EventType: "stream", Content: "world"}
	stream <- llm.StreamEvent{Content: "hello world"}
	stream <- llm.StreamEvent{Usage: &llm.UsageStats{InputTokens: 2, OutputTokens: 3}}
	close(stream)

	content, usage, err := AggregateFinalResponse(&HandleResponse{IsStreaming: true, Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if content != "hello world" {
		t.Fatalf("content = %q", content)
	}
	if usage.InputTokens != 2 || usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestAggregateFinalResponseSummarizesToolFailureForIM(t *testing.T) {
	stream := make(chan llm.StreamEvent, 6)
	stream <- llm.StreamEvent{EventType: "agent.thinking", Content: "Thinking about the request"}
	stream <- llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolArgs: `{"command":"gh auth status"}`}
	stream <- llm.StreamEvent{EventType: "tool.output", ToolName: "terminal", Content: "error connecting to api.github.com"}
	stream <- llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", Err: fmt.Errorf("command timed out after 30 seconds")}
	stream <- llm.StreamEvent{EventType: "stream", Content: "我暂时无法确认 GitHub 登录状态。"}
	close(stream)

	content, _, err := AggregateFinalResponse(&HandleResponse{IsStreaming: true, Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"我暂时无法确认", "处理过程摘要", "Thinking about the request", "gh auth status", "command timed out", "error connecting"} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q: %q", want, content)
		}
	}
}
