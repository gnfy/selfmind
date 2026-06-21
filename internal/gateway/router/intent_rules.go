package router

import (
	"regexp"
	"strings"
)

// IntentRuleConfig lets product teams adjust routing without changing code.
// Regexes are appended to the built-in lightweight classifier.
type IntentRuleConfig struct {
	Mode            string
	Rules           map[string][]string
	DirectThreshold float64
	AskThreshold    float64
}

// IntentResult is the stable routing contract shared by CLI, IM, and HTTP.
type IntentResult struct {
	Intent             Intent
	Confidence         float64
	Reason             string
	Signals            []string
	ShouldCreateTask   bool
	ShouldUseTools     bool
	NeedsClarification bool
	ClarifyingQuestion string
	Source             string
}

func NewIntentClassifierWithRules(cfg IntentRuleConfig) *IntentClassifier {
	c := NewIntentClassifier()
	c.mode = normalizeIntentMode(cfg.Mode)
	c.directThreshold = defaultFloat(cfg.DirectThreshold, 0.8)
	c.askThreshold = defaultFloat(cfg.AskThreshold, 0.55)
	c.applyExtraRules(cfg.Rules)
	return c
}

func (c *IntentClassifier) Mode() string {
	if c == nil {
		return "hybrid"
	}
	return normalizeIntentMode(c.mode)
}

func (c *IntentClassifier) DirectThreshold() float64 {
	if c == nil || c.directThreshold <= 0 {
		return 0.8
	}
	return c.directThreshold
}

func (c *IntentClassifier) AskThreshold() float64 {
	if c == nil || c.askThreshold <= 0 {
		return 0.55
	}
	return c.askThreshold
}

func (c *IntentClassifier) applyExtraRules(rules map[string][]string) {
	if c == nil {
		return
	}
	for name, patterns := range rules {
		compiled := compilePatternsSafe(patterns)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "continue", "resume":
			c.continuePatterns = append(c.continuePatterns, compiled...)
		case "task", "action":
			c.taskPatterns = append(c.taskPatterns, compiled...)
		case "skill":
			c.skillPatterns = append(c.skillPatterns, compiled...)
		case "query", "search":
			c.queryPatterns = append(c.queryPatterns, compiled...)
		case "route":
			c.routePatterns = append(c.routePatterns, compiled...)
		}
	}
}

func compilePatternsSafe(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)` + p)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

func (c *IntentClassifier) ClassifyDetailed(input string) IntentResult {
	text := strings.TrimSpace(input)
	if c == nil {
		c = NewIntentClassifier()
	}
	if text == "" {
		return IntentResult{Intent: IntentCasual, Confidence: 0.2, Reason: "empty input"}
	}
	if isModelStatusQuestion(text) {
		return IntentResult{
			Intent:           IntentCasual,
			Confidence:       0.96,
			Reason:           "matched model status question",
			Signals:          []string{"casual.model_status"},
			ShouldCreateTask: false,
			ShouldUseTools:   false,
			Source:           "rules",
		}
	}
	if signals := matchPatterns(c.skillPatterns, text, "skill"); len(signals) > 0 {
		return intentResult(IntentSkill, 0.95, "matched skill rule", signals)
	}
	if signals := matchPatterns(c.queryPatterns, text, "query"); len(signals) > 0 {
		return intentResult(IntentQuery, 0.9, "matched query rule", signals)
	}
	if signals := matchPatterns(c.routePatterns, text, "route"); len(signals) > 0 {
		return intentResult(IntentRoute, 0.9, "matched route rule", signals)
	}
	if signals := matchPatterns(c.continuePatterns, text, "continue"); len(signals) > 0 {
		return intentResult(IntentContinue, 0.88, "matched continue rule", signals)
	}

	lower := strings.ToLower(text)
	compact := normalizeQuestionText(text)
	if containsAnyIntent(compact, []string{
		"\u7ee7\u7eed", "\u63a5\u7740", "\u5f80\u4e0b", "\u4e0a\u6b21", "\u521a\u624d", "\u521a\u521a", "\u518d\u8bd5",
	}) || containsAnyIntent(lower, []string{"continue", "keep going", "go on", "resume", "try again"}) {
		return intentResult(IntentContinue, 0.86, "matched multilingual continue cue", []string{"continue.cue"})
	}

	if isCasualIdentityQuestion(text) || IsCasualShortQuestion(text) {
		return IntentResult{
			Intent:           IntentCasual,
			Confidence:       0.92,
			Reason:           "matched direct casual question",
			Signals:          []string{"casual.direct"},
			ShouldCreateTask: false,
			ShouldUseTools:   false,
		}
	}

	if signals := taskSignals(text); len(signals) > 0 {
		return intentResult(IntentTask, 0.78, "matched task/action cue", signals)
	}
	if signals := matchPatterns(c.taskPatterns, text, "task"); len(signals) > 0 {
		return intentResult(IntentTask, 0.74, "matched task rule", signals)
	}

	return IntentResult{
		Intent:           IntentCasual,
		Confidence:       0.55,
		Reason:           "no task signal matched",
		ShouldCreateTask: false,
		ShouldUseTools:   false,
		Source:           "rules",
	}
}

func intentResult(intent Intent, confidence float64, reason string, signals []string) IntentResult {
	return IntentResult{
		Intent:           intent,
		Confidence:       confidence,
		Reason:           reason,
		Signals:          signals,
		ShouldCreateTask: intent == IntentTask || intent == IntentContinue,
		ShouldUseTools:   intent != IntentCasual,
		Source:           "rules",
	}
}

func normalizeIntentMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "rules", "rule", "off":
		return "rules"
	case "llm", "model":
		return "llm"
	default:
		return "hybrid"
	}
}

func defaultFloat(value, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func matchPatterns(patterns []*regexp.Regexp, input, prefix string) []string {
	var signals []string
	for _, re := range patterns {
		if re != nil && re.MatchString(input) {
			signals = append(signals, prefix+":"+re.String())
			if len(signals) >= 4 {
				break
			}
		}
	}
	return signals
}

func taskSignals(input string) []string {
	lower := strings.ToLower(input)
	compact := normalizeQuestionText(input)
	var signals []string
	if containsAnyIntent(compact, []string{
		"\u5b9e\u73b0", "\u5199", "\u751f\u6210", "\u521b\u5efa", "\u4fee\u6539", "\u4fee\u590d", "\u4f18\u5316", "\u91cd\u6784",
		"\u68c0\u67e5", "\u5206\u6790", "\u5bf9\u6bd4", "\u8ba1\u5212", "\u65b9\u6848", "\u8fd0\u884c", "\u6d4b\u8bd5",
		"\u90e8\u7f72", "\u6253\u5305", "\u63d0\u4ea4", "\u63a8\u9001", "\u63a5\u5165", "\u914d\u7f6e", "\u8c03\u8bd5",
	}) {
		signals = append(signals, "task.zh.action")
	}
	if containsAnyIntent(lower, []string{
		"implement", "write", "create", "fix", "debug", "analyze", "compare", "plan", "run ", "test", "deploy",
		"build", "refactor", "optimize", "configure", "integrate", "check", "review",
	}) {
		signals = append(signals, "task.en.action")
	}
	if containsAnyIntent(lower, []string{"go ", "golang", "rust", "php", "python", "javascript", "typescript", "cicd", "ci/cd"}) ||
		containsAnyIntent(compact, []string{"go\u8bed\u8a00", "rust", "php", "\u4ee3\u7801", "\u9879\u76ee", "\u4ed3\u5e93"}) {
		signals = append(signals, "task.dev.context")
	}
	return signals
}

func containsAnyIntent(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func isCasualIdentityQuestion(input string) bool {
	switch normalizeQuestionText(input) {
	case "\u4f60\u662f\u8c01", "\u4f60\u53eb\u4ec0\u4e48", "\u4f60\u662f\u5e72\u561b\u7684", "whoareyou", "whatareyou":
		return true
	default:
		return false
	}
}
