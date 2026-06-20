package router

import (
	"context"
	"strings"
	"testing"
)

func TestIsTaskDoneConservative(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{name: "clear done", response: "Task completed successfully.", want: true},
		{name: "clear chinese done", response: "\u5904\u7406\u5b8c\u6210", want: true},
		{name: "plain success wording", response: "The success criteria are listed below.", want: false},
		{name: "not done", response: "Not done yet; remaining work is listed below.", want: false},
		{name: "chinese not done", response: "\u672a\u5b8c\u6210\uff0c\u9700\u8981\u7ee7\u7eed", want: false},
		{name: "blocked", response: "Blocked: need approval before continuing.", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTaskDone(tt.response); got != tt.want {
				t.Fatalf("isTaskDone() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestModelStatusQuestionUsesConfiguredRuntimeLabel(t *testing.T) {
	gw := NewGateway(nil, nil, nil, nil)
	gw.SetModelDisplay("kimi-coding", "kimi-for-coding")

	tests := []string{
		"你是什么模型？",
		"你用的是什么模型？",
		"你用的是哪个模型？",
		"当前使用的模型是什么？",
		"你现在连接的后端是什么？",
		"which model are you using?",
		"current llm?",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			resp, err := gw.Handle(context.Background(), "user1", "cli", input)
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || !strings.Contains(resp.Content, "kimi-coding/kimi-for-coding") {
				t.Fatalf("response = %+v", resp)
			}
		})
	}
}

func TestModelStatusQuestionDoesNotCatchConfigurationHelp(t *testing.T) {
	tests := []string{
		"怎么配置模型？",
		"帮我切换模型",
		"model set kimi-coding kimi-for-coding",
		"你是谁？",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if isModelStatusQuestion(input) {
				t.Fatalf("isModelStatusQuestion(%q) = true, want false", input)
			}
		})
	}
}

func TestCasualIdentityQuestionWithPunctuation(t *testing.T) {
	gw := NewGateway(nil, nil, nil, nil)
	gw.SetModelDisplay("kimi-coding", "kimi-for-coding")

	resp, err := gw.Handle(context.Background(), "user1", "cli", "你是谁？")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatalf("response is nil")
	}
	if resp.IntentReason == "model status question" {
		t.Fatalf("identity question should not be handled as model status")
	}
	if !strings.Contains(resp.Content, "SelfMind") || !strings.Contains(resp.Content, "kimi-coding/kimi-for-coding") {
		t.Fatalf("response = %+v", resp)
	}
}

func TestCasualShortQuestionDoesNotCatchGeneralQuestions(t *testing.T) {
	tests := []string{
		"这个项目能跑吗？",
		"Go 怎么写二分法？",
		"这个方案怎么样？",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if IsCasualShortQuestion(input) {
				t.Fatalf("IsCasualShortQuestion(%q) = true, want false", input)
			}
		})
	}
}
