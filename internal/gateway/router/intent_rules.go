package router

import (
	"regexp"
	"strings"
)

// IntentRuleConfig lets product teams adjust explicit routing without changing
// code. These rules should not turn ordinary natural language into a pre-agent
// task/chat classifier.
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
		case "skill":
			c.skillPatterns = append(c.skillPatterns, compiled...)
		case "query", "search":
			c.queryPatterns = append(c.queryPatterns, compiled...)
		case "route":
			c.routePatterns = append(c.routePatterns, compiled...)
		case "task", "action":
			c.taskPatterns = append(c.taskPatterns, compiled...)
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
		return IntentResult{Intent: IntentCasual, Confidence: 0.2, Reason: "empty input", Source: "rules"}
	}
	if signals := matchPatterns(c.skillPatterns, text, "skill"); len(signals) > 0 {
		return intentResult(IntentSkill, 0.95, "matched explicit skill command", signals)
	}
	if signals := matchPatterns(c.queryPatterns, text, "query"); len(signals) > 0 {
		return intentResult(IntentQuery, 0.9, "matched explicit query command", signals)
	}
	if signals := matchPatterns(c.routePatterns, text, "route"); len(signals) > 0 {
		return intentResult(IntentRoute, 0.9, "matched explicit route command", signals)
	}
	if signals := matchPatterns(c.continuePatterns, text, "continue"); len(signals) > 0 {
		return intentResult(IntentContinue, 0.88, "matched explicit continue cue", signals)
	}
	if signals := matchPatterns(c.taskPatterns, text, "task"); len(signals) > 0 {
		return intentResult(IntentTask, 0.82, "matched configured task rule", signals)
	}
	return intentResult(IntentTask, 0.82, "agent-first default", []string{"agent_first.default"})
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
