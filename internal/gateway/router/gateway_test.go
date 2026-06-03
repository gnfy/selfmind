package router

import "testing"

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
