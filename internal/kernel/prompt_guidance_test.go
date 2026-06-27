package kernel

import (
	"strings"
	"testing"
)

func TestIsFrontendTask(t *testing.T) {
	frontend := []string{
		"用 js 写一个高品质的对战小游戏,要有历史名将的背景",
		"帮我做一个登录页面,要好看",
		"build a responsive dashboard in React",
		"写个网页版的待办工具",
		"给这个按钮加个动画",
	}
	for _, in := range frontend {
		if !isFrontendTask(in) {
			t.Errorf("expected frontend=true for %q", in)
		}
	}
	backend := []string{
		"用 Go 写一个并发安全的 LRU 缓存",
		"分析这个 PHP 项目的代码结构",
		"写一个读取 CSV 求平均值的 Python 脚本",
		"帮我看下这个 SQL 为什么慢",
		"重构 internal/app 的依赖注入",
	}
	for _, in := range backend {
		if isFrontendTask(in) {
			t.Errorf("expected frontend=false for %q", in)
		}
	}
}

func TestFrontendGuidanceNotInCoreGuidance(t *testing.T) {
	// The always-on guidance must stay domain-agnostic.
	if strings.Contains(taskExecutionGuidance(), "FRONTEND") {
		t.Fatal("taskExecutionGuidance must not contain frontend-specific content")
	}
	if !strings.Contains(frontendQualityGuidance(), "FRONTEND") {
		t.Fatal("frontendQualityGuidance should carry the UI guidance")
	}
	// Verification guidance must be language-agnostic (manifest-driven), not a
	// fixed language list.
	g := taskExecutionGuidance()
	if !strings.Contains(g, "manifest") || !strings.Contains(g, "go.mod") || !strings.Contains(g, "package.json") {
		t.Fatal("verification guidance should reference manifest-based ecosystem detection")
	}
}
