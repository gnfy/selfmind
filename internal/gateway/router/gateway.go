package router

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/identity"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/task"
)

// Gateway is the lightweight routing facade used by CLI/HTTP/IM before a
// message enters the durable control-plane flow.
type Gateway struct {
	identityMapper   *identity.IdentityMapper
	taskManager      *task.Manager
	intentClassifier *IntentClassifier
	agent            *kernel.Agent
	agentEventMu     sync.Mutex
	llmProvider      llm.Provider
	modelProvider    string
	modelName        string
}

func NewGateway(
	identityMapper *identity.IdentityMapper,
	taskManager *task.Manager,
	agent *kernel.Agent,
	llmProvider llm.Provider,
) *Gateway {
	return &Gateway{
		identityMapper:   identityMapper,
		taskManager:      taskManager,
		intentClassifier: NewIntentClassifier(),
		agent:            agent,
		llmProvider:      llmProvider,
	}
}

func (g *Gateway) SetModelDisplay(provider, model string) {
	if g == nil {
		return
	}
	g.modelProvider = strings.TrimSpace(provider)
	g.modelName = strings.TrimSpace(model)
}

func (g *Gateway) SetIntentClassifier(classifier *IntentClassifier) {
	if g == nil || classifier == nil {
		return
	}
	g.intentClassifier = classifier
}

func (g *Gateway) ClassifyIntent(input string) IntentResult {
	if g == nil || g.intentClassifier == nil {
		return NewIntentClassifier().ClassifyDetailed(input)
	}
	return g.intentClassifier.ClassifyDetailed(input)
}

type HandleResponse struct {
	Content      string
	Usage        llm.UsageStats
	Stream       <-chan llm.StreamEvent
	IsStreaming  bool
	Intent       Intent
	IntentReason string
}

func (g *Gateway) Handle(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	result := g.ClassifyIntent(input)
	intent, reason := result.Intent, result.Reason
	switch intent {
	case IntentSkill:
		content, usage, err := g.handleSkill(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentQuery:
		content, usage, err := g.handleQuery(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentRoute:
		content, usage, err := g.handleRoute(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentContinue, IntentTask, IntentCasual:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)
	default:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, IntentTask, "agent-first fallback")
	}
}

func (g *Gateway) handleTaskStreaming(ctx context.Context, unifiedUID, channel, input string, intent Intent, reason string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		return nil, fmt.Errorf("gateway agent is not configured")
	}
	if g.taskManager == nil {
		return g.runAgentStreaming(ctx, unifiedUID, channel, input, intent, reason)
	}

	var taskID int64
	var err error
	if intent == IntentContinue {
		t, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
		if err != nil || t == nil {
			return &HandleResponse{Content: "No active task to continue. Tell me what you want to work on next."}, nil
		}
		taskID = t.ID
	} else {
		taskID, err = g.taskManager.CreateTask(ctx, unifiedUID, extractTitle(input))
		if err != nil {
			return nil, err
		}
	}
	g.taskManager.AppendContext(ctx, unifiedUID, channel, "user", input)

	respChan := make(chan llm.StreamEvent, 256)
	go func() {
		defer close(respChan)
		resp, usage, err := g.agent.RunConversation(ctx, unifiedUID, channel, input)
		if err != nil {
			respChan <- llm.StreamEvent{Err: err}
			if intent == IntentTask {
				g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "failed")
			}
			return
		}
		g.taskManager.AppendContext(ctx, unifiedUID, channel, "assistant", resp)
		if isTaskDone(resp) {
			g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "done")
		}
		if resp != "" {
			respChan <- llm.StreamEvent{Content: resp}
		}
		respChan <- llm.StreamEvent{Usage: &usage}
	}()

	return &HandleResponse{
		IsStreaming:  true,
		Stream:       respChan,
		Intent:       intent,
		IntentReason: reason,
	}, nil
}

func (g *Gateway) handleCasual(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	reply := g.casualReply(input)
	if g.taskManager != nil {
		_ = g.taskManager.SaveCasualSummary(ctx, unifiedUID, channel, "casual: "+input)
	}
	return reply, llm.UsageStats{}, nil
}

func (g *Gateway) RunAgent(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	return g.runAgentStreaming(ctx, unifiedUID, channel, input, IntentTask, "agent-only")
}

func (g *Gateway) runAgentStreaming(ctx context.Context, unifiedUID, channel, input string, intent Intent, reason string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		return nil, fmt.Errorf("gateway agent is not configured")
	}
	respChan := make(chan llm.StreamEvent, 256)
	go func() {
		defer close(respChan)
		resp, usage, err := g.agent.RunConversation(ctx, unifiedUID, channel, input)
		if err != nil {
			respChan <- llm.StreamEvent{Err: err}
			return
		}
		if resp != "" {
			respChan <- llm.StreamEvent{Content: resp}
		}
		respChan <- llm.StreamEvent{Usage: &usage}
	}()
	return &HandleResponse{
		IsStreaming:  true,
		Stream:       respChan,
		Intent:       intent,
		IntentReason: reason,
	}, nil
}

func isModelStatusQuestion(input string) bool {
	cleaned := normalizeQuestionText(input)
	if cleaned == "" {
		return false
	}
	if !containsAnyNormalized(cleaned, []string{"model", "llm", "\u6a21\u578b", "\u5927\u6a21\u578b", "\u540e\u7aef"}) {
		return false
	}
	return containsAnyNormalized(cleaned, []string{
		"what", "which", "current", "using", "running", "active",
		"\u4ec0\u4e48", "\u54ea\u4e2a", "\u54ea\u4e00\u4e2a", "\u5f53\u524d", "\u73b0\u5728", "\u76ee\u524d",
		"\u6b63\u5728\u7528", "\u7528\u7684", "\u4f7f\u7528\u7684", "\u8fde\u63a5\u7684", "\u8dd1\u7684",
	})
}

func normalizeQuestionText(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	replacer := strings.NewReplacer(
		"\u3000", "",
		" ", "",
		"\t", "",
		"\n", "",
		"\r", "",
		"?", "",
		"\uff1f", "",
		"!", "",
		"\uff01", "",
		"\u3002", "",
		".", "",
		"\uff0e", "",
		",", "",
		"\uff0c", "",
		":", "",
		"\uff1a", "",
		";", "",
	)
	return replacer.Replace(input)
}

func containsAnyNormalized(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func (g *Gateway) modelStatusReply() string {
	return modelStatusReplyText(g.modelDisplayLabel())
}

func (g *Gateway) ModelStatusReply() string {
	if g == nil {
		return modelStatusReplyText("")
	}
	return g.modelStatusReply()
}

func modelStatusReplyText(label string) string {
	if strings.TrimSpace(label) == "" {
		return "I am SelfMind. No usable AI model configuration was resolved. Run `selfmind model check` to inspect the problem."
	}
	return fmt.Sprintf("I am SelfMind. The current model is %s.", label)
}

func (g *Gateway) modelDisplayLabel() string {
	provider := strings.TrimSpace(g.modelProvider)
	model := strings.TrimSpace(g.modelName)
	switch {
	case provider != "" && model != "" && provider != "not configured":
		return provider + "/" + model
	case model != "" && model != "not configured":
		return model
	case provider != "" && provider != "not configured":
		return provider
	default:
		return ""
	}
}

func (g *Gateway) casualReply(input string) string {
	if reply, ok := g.directCasualReply(input); ok {
		return reply
	}
	return "I understand. Tell me what you want me to inspect, change, test, or continue."
}

func (g *Gateway) directCasualReply(input string) (string, bool) {
	switch normalizeQuestionText(input) {
	case "\u4f60\u597d", "\u60a8\u597d", "hi", "hello", "\u55e8", "hey":
		return "Hello, I am SelfMind. I can help with development tasks, code inspection, tool execution, and multi-device collaboration.", true
	case "\u4f60\u662f\u8c01", "\u4f60\u53eb\u4ec0\u4e48", "\u4f60\u662f\u5e72\u561b\u7684", "whoareyou", "whatareyou":
		reply := "I am SelfMind, an AI work assistant for development tasks and multi-device collaboration."
		if label := g.modelDisplayLabel(); label != "" {
			reply += " The current model is " + label + "."
		}
		return reply, true
	case "\u8c22\u8c22", "\u591a\u8c22", "\u8c22\u4e86", "thanks", "thankyou":
		return "You're welcome. Tell me directly when you want to continue a task.", true
	case "\u518d\u89c1", "\u62dc\u62dc", "bye", "\u665a\u5b89":
		return "Goodbye. Come back whenever you need help.", true
	}
	return "", false
}

func (g *Gateway) handleSkill(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	skillName := trimKnownPrefix(input, []string{"/skill ", "/s "})
	if skillName == "" {
		return "Please specify the skill to run.", llm.UsageStats{}, nil
	}
	if g == nil || g.agent == nil || g.agent.Dispatcher() == nil {
		return "", llm.UsageStats{}, fmt.Errorf("skill dispatcher is not configured")
	}
	resp, err := g.agent.Dispatcher().Dispatch("skill:"+skillName, map[string]interface{}{
		"input":      skillName,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

func (g *Gateway) handleQuery(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	query := trimKnownPrefix(input, []string{"/query ", "/search "})
	if query == "" {
		query = strings.TrimSpace(input)
	}
	if g == nil || g.agent == nil || g.agent.Dispatcher() == nil {
		return "", llm.UsageStats{}, fmt.Errorf("session search is not configured")
	}
	resp, err := g.agent.Dispatcher().Dispatch("session_search", map[string]interface{}{
		"query":      query,
		"limit":      10,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

func (g *Gateway) handleRoute(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	return fmt.Sprintf("Route command received. Current channel: %s.", channel), llm.UsageStats{}, nil
}

func trimKnownPrefix(input string, prefixes []string) string {
	value := strings.TrimSpace(input)
	lower := strings.ToLower(value)
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	return value
}

func (g *Gateway) ResolveUID(ctx context.Context, platform, platformID string) (string, error) {
	return g.identityMapper.EnsureBound(ctx, platform, platformID)
}

func (g *Gateway) ListTasks(ctx context.Context, unifiedUID string) ([]task.Task, error) {
	return g.taskManager.ListTasks(ctx, unifiedUID)
}

func (g *Gateway) GetCurrentTaskInfo(ctx context.Context, unifiedUID string) (*task.Task, error) {
	tt, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
	return tt, err
}

func isTaskDone(response string) bool {
	return taskResponseLooksComplete(response)
}

func taskResponseLooksComplete(response string) bool {
	trimmed := strings.TrimSpace(response)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	if taskResponseContainsAny(lower, []string{
		"not done", "not completed", "not finished", "remaining work", "still need",
		"need to continue", "next steps", "todo:", "blocked",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u672a\u5b8c\u6210", "\u8fd8\u6ca1\u5b8c\u6210", "\u6ca1\u6709\u5b8c\u6210",
		"\u5f85\u5b8c\u6210", "\u9700\u8981\u7ee7\u7eed", "\u963b\u585e",
	}) {
		return false
	}
	if lower == "done" || lower == "completed" || lower == "finished" || lower == "all done" {
		return true
	}
	return taskResponseContainsAny(lower, []string{
		"task complete", "task completed", "completed successfully",
		"finished successfully", "all done", "implementation complete", "tests pass",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u5df2\u5b8c\u6210", "\u4efb\u52a1\u5b8c\u6210", "\u5904\u7406\u5b8c\u6210",
		"\u5df2\u5904\u7406\u5b8c", "\u5df2\u7ecf\u5b8c\u6210", "\u641e\u5b9a",
	})
}

func taskResponseContainsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func extractTitle(input string) string {
	title := strings.TrimSpace(input)
	for _, prefix := range []string{"please"} {
		if strings.HasPrefix(strings.ToLower(title), strings.ToLower(prefix)) {
			title = strings.TrimSpace(title[len(prefix):])
			break
		}
	}
	if len([]rune(title)) > 30 {
		runes := []rune(title)
		title = string(runes[:30]) + "..."
	}
	if title == "" {
		return "New task"
	}
	return title
}

func (g *Gateway) QuickReply(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	return g.Handle(ctx, unifiedUID, channel, input)
}
