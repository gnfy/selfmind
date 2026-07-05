package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

func (g *Gateway) ClassifyIntentWithContext(ctx context.Context, input, channel string) IntentResult {
	rules := g.ClassifyIntent(input)
	if g == nil || g.llmProvider == nil || !g.shouldConsultIntentLLM(rules, input) {
		return rules
	}
	llmResult, err := g.classifyIntentWithLLM(ctx, input, channel, rules)
	if err != nil {
		return rules
	}
	return llmResult
}

func (g *Gateway) shouldConsultIntentLLM(rule IntentResult, input string) bool {
	if g == nil || g.intentClassifier == nil {
		return false
	}
	mode := g.intentClassifier.Mode()
	if mode == "rules" {
		return false
	}
	if hardRuleIntent(rule) {
		return false
	}
	if rule.Intent == IntentTask {
		return false
	}
	if mode == "llm" {
		return true
	}
	clean := strings.TrimSpace(input)
	return rule.Confidence <= g.intentClassifier.AskThreshold() ||
		(rule.Intent == IntentCasual && len([]rune(clean)) > 20 && rule.Confidence < g.intentClassifier.DirectThreshold())
}

func hardRuleIntent(rule IntentResult) bool {
	if rule.Source == "llm" {
		return true
	}
	switch rule.Intent {
	case IntentSkill, IntentQuery, IntentRoute, IntentContinue:
		return rule.Confidence >= 0.8
	case IntentCasual:
		return rule.Confidence >= 0.9
	default:
		return false
	}
}

type llmIntentPayload struct {
	Intent             string   `json:"intent"`
	Confidence         float64  `json:"confidence"`
	Reason             string   `json:"reason"`
	Signals            []string `json:"signals"`
	ShouldCreateTask   *bool    `json:"should_create_task"`
	ShouldUseTools     *bool    `json:"should_use_tools"`
	NeedsClarification bool     `json:"needs_clarification"`
	ClarifyingQuestion string   `json:"clarifying_question"`
}

func (g *Gateway) classifyIntentWithLLM(ctx context.Context, input, channel string, rule IntentResult) (IntentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	resp, err := g.llmProvider.Chat(callCtx, llm.ChatRequest{
		MaxTokens: 300,
		Messages: []llm.Message{
			{Role: "system", Content: intentClassifierSystemPrompt()},
			{Role: "user", Content: fmt.Sprintf("channel: %s\nrule_intent: %s\nrule_confidence: %.2f\nmessage:\n%s", channel, intentName(rule.Intent), rule.Confidence, input)},
		},
	})
	if err != nil {
		return IntentResult{}, err
	}
	var payload llmIntentPayload
	if err := json.Unmarshal([]byte(stripJSONFence(resp.Content)), &payload); err != nil {
		return IntentResult{}, err
	}
	intent, ok := parseIntentName(payload.Intent)
	if !ok {
		return IntentResult{}, fmt.Errorf("unknown intent: %s", payload.Intent)
	}
	confidence := payload.Confidence
	if confidence <= 0 {
		confidence = 0.5
	}
	result := IntentResult{
		Intent:             intent,
		Confidence:         clampConfidence(confidence),
		Reason:             firstNonEmptyIntent(payload.Reason, "classified by llm"),
		Signals:            payload.Signals,
		ShouldCreateTask:   intent == IntentTask || intent == IntentContinue,
		ShouldUseTools:     intent != IntentCasual,
		NeedsClarification: payload.NeedsClarification,
		ClarifyingQuestion: strings.TrimSpace(payload.ClarifyingQuestion),
		Source:             "llm",
	}
	if payload.ShouldCreateTask != nil {
		result.ShouldCreateTask = *payload.ShouldCreateTask
	}
	if payload.ShouldUseTools != nil {
		result.ShouldUseTools = *payload.ShouldUseTools
	}
	if result.Confidence <= g.intentClassifier.AskThreshold() && !hardRuleIntent(result) {
		result.NeedsClarification = true
	}
	if result.NeedsClarification && result.ClarifyingQuestion == "" {
		result.ClarifyingQuestion = defaultClarifyingQuestion()
	}
	return result, nil
}

// RecentTaskContext is the minimal recent-task summary the implicit-continuation
// judgment needs. Kept tiny on purpose (title + one-line summary + status +
// age): it is background context for a cheap LLM call, not a prompt payload.
type RecentTaskContext struct {
	Title   string
	Summary string
	Status  string
	Age     time.Duration
}

// continuationUpgradePayload is the JSON contract for the implicit-continuation
// judgment. "decision" is deliberately continue|new only — this call may never
// route ordinary work into a casual/direct path.
type continuationUpgradePayload struct {
	Decision   string  `json:"decision"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// UpgradeTaskToContinueWithLLM is the ONE narrow exception to the "never consult
// the LLM for a rules IntentTask" gate (shouldConsultIntentLLM). The httpapi
// layer calls it only when the rules classifier landed on IntentTask AND the
// person has a recently active resumable task within the configured window. It
// asks a cheap LLM whether this message continues that task or is new work, and
// may ONLY upgrade task -> continue: any other verdict (new work, low
// confidence, parse/transport error, rules mode) returns ok=false so the caller
// keeps the rules IntentTask. It never downgrades to casual/direct — the
// agent-first red line stays. At most one LLM call per inbound message.
func (g *Gateway) UpgradeTaskToContinueWithLLM(ctx context.Context, input, channel string, recent RecentTaskContext) (IntentResult, bool) {
	if g == nil || g.llmProvider == nil || g.intentClassifier == nil {
		return IntentResult{}, false
	}
	if g.intentClassifier.Mode() == "rules" {
		return IntentResult{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	resp, err := g.llmProvider.Chat(callCtx, llm.ChatRequest{
		MaxTokens: 200,
		Messages: []llm.Message{
			{Role: "system", Content: continuationUpgradeSystemPrompt()},
			{Role: "user", Content: continuationUpgradeUserPrompt(channel, recent, input)},
		},
	})
	if err != nil {
		return IntentResult{}, false
	}
	var payload continuationUpgradePayload
	if err := json.Unmarshal([]byte(stripJSONFence(resp.Content)), &payload); err != nil {
		return IntentResult{}, false
	}
	if strings.ToLower(strings.TrimSpace(payload.Decision)) != "continue" {
		return IntentResult{}, false
	}
	confidence := clampConfidence(payload.Confidence)
	if confidence <= 0 {
		confidence = 0.7
	}
	return IntentResult{
		Intent:           IntentContinue,
		Confidence:       confidence,
		Reason:           firstNonEmptyIntent(payload.Reason, "llm upgraded implicit continuation"),
		Signals:          []string{"continue.llm_upgrade"},
		ShouldCreateTask: true,
		ShouldUseTools:   true,
		Source:           "llm",
	}, true
}

func continuationUpgradeSystemPrompt() string {
	return `You decide whether a new SelfMind message continues the person's most recent unfinished task or starts new work.
Return JSON only. No markdown.

You are given the recent task (title, summary, status) and the new message.
- Answer "continue" ONLY when the new message is clearly follow-up on that same task: refining it, reacting to its result, correcting it, or asking to keep going.
- Answer "new" when the message is about something else, a fresh request, or you are unsure.
- Never invent a task. When in doubt, answer "new".

Schema:
{"decision":"continue|new","confidence":0.0,"reason":"..."}`
}

func continuationUpgradeUserPrompt(channel string, recent RecentTaskContext, input string) string {
	age := ""
	if recent.Age > 0 {
		age = recent.Age.Round(time.Second).String()
	}
	return fmt.Sprintf(
		"channel: %s\nrecent_task_title: %s\nrecent_task_summary: %s\nrecent_task_status: %s\nrecent_task_age: %s\nnew_message:\n%s",
		channel, recent.Title, recent.Summary, recent.Status, age, input)
}

func intentClassifierSystemPrompt() string {
	return `You classify a SelfMind user message before task creation.
Return JSON only. No markdown.

Valid intent values:
- casual: greeting, identity/model question, simple chat, direct answer that should not create a task.
- task: user asks SelfMind to do work, write/change/check/debug/build/deploy/analyze something, or use tools.
- continue: user asks to continue/resume previous work.
- query: user explicitly asks to search prior memory/history.
- skill: user explicitly invokes a skill command.
- route: user asks to route/switch platform.

Rules:
- Prefer casual for "who are you", "what model are you", greetings, thanks.
- Prefer task for development work, repo work, debugging, CI/CD, implementation plans, or requests that need tools.
- If ambiguous and a wrong route would be harmful, set needs_clarification=true and ask one short question.

Schema:
{"intent":"casual|task|continue|query|skill|route","confidence":0.0,"reason":"...","signals":["..."],"should_create_task":false,"should_use_tools":false,"needs_clarification":false,"clarifying_question":""}`
}

func stripJSONFence(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") {
		value = strings.TrimPrefix(value, "```json")
		value = strings.TrimPrefix(value, "```")
		value = strings.TrimSuffix(value, "```")
	}
	return strings.TrimSpace(value)
}

func parseIntentName(name string) (Intent, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "continue", "resume":
		return IntentContinue, true
	case "task", "action", "work":
		return IntentTask, true
	case "skill":
		return IntentSkill, true
	case "query", "search":
		return IntentQuery, true
	case "route":
		return IntentRoute, true
	case "casual", "chat", "direct":
		return IntentCasual, true
	default:
		return IntentCasual, false
	}
}

func intentName(intent Intent) string {
	switch intent {
	case IntentContinue:
		return "continue"
	case IntentTask:
		return "task"
	case IntentSkill:
		return "skill"
	case IntentQuery:
		return "query"
	case IntentRoute:
		return "route"
	default:
		return "casual"
	}
}

func clampConfidence(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func firstNonEmptyIntent(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func defaultClarifyingQuestion() string {
	return "\u6211\u4e0d\u592a\u786e\u5b9a\u8fd9\u662f\u95f2\u804a\u95ee\u9898\uff0c\u8fd8\u662f\u8981\u6211\u5efa\u7acb\u4efb\u52a1\u5e76\u8c03\u7528\u5de5\u5177\u5904\u7406\u3002\u4f60\u5e0c\u671b\u6211\u76f4\u63a5\u56de\u7b54\uff0c\u8fd8\u662f\u5f00\u59cb\u5904\u7406\u4e00\u4e2a\u4efb\u52a1\uff1f"
}
