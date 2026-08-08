package memory

import "testing"

// The two-tier classifier's contract: only "concrete instance described in
// current-state terms" is CONFIRMED (safe for automatic run-state handling);
// explanatory rules and bare status vocabulary are candidates at most.
func TestClassifyTransientContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    TransientVerdict
	}{
		{"chinese instance current state", "RUQX-224 当前状态为 QUEUED", TransientConfirmed},
		{"chinese prepared record", "RUQX-222 已按 PREPARED_NOT_EXECUTED 记录到 gcp-run.md，待执行", TransientConfirmed},
		{"chinese status assignment", "已为 RUQX-213 生成发布命令，状态标记为 PREPARED_NOT_EXECUTED，尚未执行", TransientConfirmed},
		{"english instance currently", "Build RUQX-31 is currently IN_PROGRESS", TransientConfirmed},
		{"prefixed operational status", "RUQX-401 当前状态为 CI_PENDING_APPROVAL", TransientConfirmed},
		{"build id created", "Build ID: cw-prod:0d4a9e81 has been created", TransientConfirmed},
		{"ticket record backfilled", "RUQX-369 已回填到 gcp-run.md", TransientConfirmed},
		{"ticket build triggered", "RUQX-370 的生产构建已触发", TransientConfirmed},
		{"generic build id rule", "Build ID should be recorded in the release file", TransientNone},
		{"durable status rule", "RUQX-224 uses FAILED to indicate a terminal build result", TransientCandidate},
		{"chinese rule with token", "PREPARED_NOT_EXECUTED 表示发布已准备但尚未执行", TransientCandidate},
		{"chinese transition rule", "发布记录从 PREPARED_NOT_EXECUTED 转为 EXECUTED 说明执行完成", TransientCandidate},
		{"english rule with token", "QUEUED means the task is waiting for dispatch", TransientCandidate},
		{"bare status vocabulary", "构建已入队 QUEUED", TransientCandidate},
		{"status token without instance", "some job is IN_PROGRESS somewhere", TransientCandidate},
		{"no status vocabulary", "lid-tm-tracking 使用 _COMMIT 参数，不传 _IMG_TAG", TransientNone},
		{"doc fact about current state wording", "docs/STATUS.md 是当前状态的权威文档", TransientCandidate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTransientContent(tc.content); got != tc.want {
				t.Fatalf("ClassifyTransientContent(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}
