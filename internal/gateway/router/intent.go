package router

import "regexp"

type Intent int

const (
	IntentContinue Intent = iota
	IntentTask
	IntentSkill
	IntentQuery
	IntentRoute
	IntentCasual
)

// IntentClassifier is intentionally lightweight. It handles hard commands and
// high-confidence multilingual cues; ambiguous cases can be delegated to the
// LLM classifier by the hybrid policy in intent_llm.go.
type IntentClassifier struct {
	continuePatterns []*regexp.Regexp
	taskPatterns     []*regexp.Regexp
	skillPatterns    []*regexp.Regexp
	queryPatterns    []*regexp.Regexp
	routePatterns    []*regexp.Regexp
	mode             string
	directThreshold  float64
	askThreshold     float64
}

func NewIntentClassifier() *IntentClassifier {
	return &IntentClassifier{
		continuePatterns: compilePatterns([]string{
			`继续`, `接着`, `往下`, `上次`, `刚才`, `刚刚`, `再试`, `重试`,
			`\bcontinue\b`, `\bkeep going\b`, `\bgo on\b`, `\bresume\b`, `\btry again\b`,
		}),
		taskPatterns: compilePatterns([]string{
			`帮我`, `请帮我`, `看一下`, `检查`, `分析`, `实现`, `写一个`, `写一段`, `修改`, `修复`,
			`重构`, `优化`, `运行`, `测试`, `部署`, `发布`, `打包`, `提交`, `推送`, `接入`, `配置`,
			`\bcreate\b`, `\bexecute\b`, `\bdeploy\b`, `\brun\b`, `\bbuild\b`, `\bcheck\b`,
			`\bmodify\b`, `\bwrite\b`, `\banalyze\b`, `\breview\b`, `\bfix\b`, `\bdebug\b`,
		}),
		skillPatterns: compilePatterns([]string{
			`^/skill\b`, `^/s\b`, `调用技能`, `用技能`, `执行技能`, `运行技能`,
		}),
		queryPatterns: compilePatterns([]string{
			`^/query\b`, `^/search\b`, `搜索.*历史`, `查历史`, `找之前`, `之前.*说过`,
		}),
		routePatterns: compilePatterns([]string{
			`^/route\b`, `切换到`, `切到`, `转到`, `路由到`,
		}),
		mode:            "hybrid",
		directThreshold: 0.8,
		askThreshold:    0.55,
	}
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	result := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		result = append(result, regexp.MustCompile(`(?i)`+p))
	}
	return result
}

func (c *IntentClassifier) Classify(input string) Intent {
	intent, _ := c.ClassifyWithReason(input)
	return intent
}

func (c *IntentClassifier) ClassifyWithReason(input string) (Intent, string) {
	if c == nil {
		c = NewIntentClassifier()
	}
	for _, re := range c.skillPatterns {
		if re.MatchString(input) {
			return IntentSkill, "matched skill pattern"
		}
	}
	for _, re := range c.queryPatterns {
		if re.MatchString(input) {
			return IntentQuery, "matched query pattern"
		}
	}
	for _, re := range c.routePatterns {
		if re.MatchString(input) {
			return IntentRoute, "matched route pattern"
		}
	}
	for _, re := range c.continuePatterns {
		if re.MatchString(input) {
			return IntentContinue, "matched continue pattern"
		}
	}
	if IsCasualShortQuestion(input) {
		return IntentCasual, "matched casual short question"
	}
	for _, re := range c.taskPatterns {
		if re.MatchString(input) {
			return IntentTask, "matched task pattern"
		}
	}
	return IntentCasual, "no hard pattern matched"
}

func IsCasualShortQuestion(input string) bool {
	switch normalizeQuestionText(input) {
	case "你好", "您好", "hi", "hello", "嗨", "hey",
		"谢谢", "多谢", "谢了", "thanks", "thankyou",
		"再见", "拜拜", "bye", "晚安",
		"你是谁", "你叫什么", "你是干嘛的", "whoareyou", "whatareyou":
		return true
	default:
		return false
	}
}
