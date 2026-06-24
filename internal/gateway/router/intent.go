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

// IntentClassifier handles only explicit commands and high-confidence resume
// cues. Ordinary natural language defaults to IntentTask so the agent, not a
// keyword router, decides whether to answer directly or use tools.
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
			`\bcontinue\b`, `\bkeep going\b`, `\bgo on\b`, `\bresume\b`, `\btry again\b`,
			`\bcarry on\b`, `\bnext step\b`,
			"\u7ee7\u7eed", "\u63a5\u7740", "\u5f80\u4e0b", "\u4e0a\u6b21",
			"\u521a\u624d", "\u521a\u521a", "\u518d\u8bd5", "\u91cd\u8bd5",
		}),
		taskPatterns: compilePatterns([]string{}),
		skillPatterns: compilePatterns([]string{
			`^/skill\b`, `^/s\b`,
		}),
		queryPatterns: compilePatterns([]string{
			`^/query\b`, `^/search\b`,
		}),
		routePatterns: compilePatterns([]string{
			`^/route\b`,
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
	result := c.ClassifyDetailed(input)
	return result.Intent, result.Reason
}

func IsCasualShortQuestion(input string) bool {
	switch normalizeQuestionText(input) {
	case "\u4f60\u597d", "\u60a8\u597d", "hi", "hello", "\u55e8", "hey",
		"\u8c22\u8c22", "\u591a\u8c22", "\u8c22\u4e86", "thanks", "thankyou",
		"\u518d\u89c1", "\u62dc\u62dc", "bye", "\u665a\u5b89",
		"\u4f60\u662f\u8c01", "\u4f60\u53eb\u4ec0\u4e48", "\u4f60\u662f\u5e72\u561b\u7684", "whoareyou", "whatareyou":
		return true
	default:
		return false
	}
}
