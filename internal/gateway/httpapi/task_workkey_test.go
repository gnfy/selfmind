package httpapi

import (
	"testing"

	"selfmind/internal/control"
)

// Display-only work-key grouping: cards sharing a ticket key become adjacent
// (anchored at the first occurrence); keyless cards keep relative order; the
// card set itself never changes.
func TestGroupTasksByWorkKey(t *testing.T) {
	tasks := []control.Task{
		{ID: "t1", Title: "RUQX-223 AWS 发布准备"},
		{ID: "t2", Title: "修复输出命名脚本"},
		{ID: "t3", Title: "RUQX-224 GCP 发布准备"},
		{ID: "t4", Title: "RUQX-223 发布执行"},
		{ID: "t5", Title: "RUQX-224 发布执行"},
	}
	got := groupTasksByWorkKey(tasks)
	order := make([]string, 0, len(got))
	for _, task := range got {
		order = append(order, task.ID)
	}
	want := []string{"t1", "t4", "t2", "t3", "t5"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	// Small lists pass through untouched.
	two := []control.Task{{ID: "a", Title: "RUQX-1 x"}, {ID: "b", Title: "RUQX-1 y"}}
	if out := groupTasksByWorkKey(two); out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("short list must pass through: %+v", out)
	}
}
