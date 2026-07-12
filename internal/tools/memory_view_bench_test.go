package tools

import (
	"fmt"
	"testing"

	"selfmind/internal/kernel/memory"
)

// BenchmarkGroupMemoryFacts guards the /memory read-model latency: grouping is
// O(n²) pairwise similarity, so per-fact signature work must be hoisted out of
// the pair loop or a few hundred facts make every /memory call feel stuck.
func BenchmarkGroupMemoryFacts(b *testing.B) {
	facts := make([]memory.Fact, 0, 800)
	for i := 0; i < 800; i++ {
		var content string
		switch i % 4 {
		case 0:
			content = fmt.Sprintf("The current project workspace root is /mnt/d/wwwroot/ai/project%d; resolve relative paths there first.", i/4)
		case 1:
			content = fmt.Sprintf("User prefers concise Chinese replies with direct edits for module %d.", i/4)
		case 2:
			content = fmt.Sprintf("用户偏好低亮度暖色系 UI，倾向深棕、铜橙、暗金配色，方案编号 %d。", i/4)
		default:
			content = fmt.Sprintf("Repo %d builds with GOWORK=off go test ./... on the WSL toolchain.", i/4)
		}
		facts = append(facts, memory.Fact{
			ID:      fmt.Sprintf("fact-%04d", i),
			Target:  "memory",
			Scope:   "global",
			Content: content,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if groups := groupMemoryFacts(facts); len(groups) == 0 {
			b.Fatal("expected groups")
		}
	}
}
