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
	if isModelStatusQuestion(input) {
		return &HandleResponse{Content: g.modelStatusReply(), Intent: IntentCasual, IntentReason: "model status question"}, nil
	}

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
	case IntentCasual:
		if IsCasualShortQuestion(input) {
			content, usage, err := g.handleCasual(ctx, unifiedUID, channel, input)
			return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
		}
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)
	case IntentContinue, IntentTask:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)
	default:
		return &HandleResponse{Content: "I could not understand the message route."}, nil
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
	if !containsAnyNormalized(cleaned, []string{"模型", "大模型", "后端", "model", "llm"}) {
		return false
	}
	return containsAnyNormalized(cleaned, []string{
		"什么", "哪个", "哪一个", "当前", "现在", "目前", "正在用", "用的", "使用的", "连接的", "跑的",
		"what", "which", "current", "using", "running", "active",
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
		"？", "",
		"!", "",
		"！", "",
		"。", "",
		".", "",
		"，", "",
		",", "",
		"：", "",
		":", "",
		"；", "",
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

func modelStatusReplyText(label string) string {
	if strings.TrimSpace(label) == "" {
		return "我是 SelfMind，目前没有解析到可用的 AI 模型配置。请运行 selfmind model check 查看原因。"
	}
	return fmt.Sprintf("我是 SelfMind，当前连接的模型是 %s。", label)
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
	return "嗯，我明白了。需要我处理开发任务、检查代码、运行工具或继续某个任务时，直接告诉我。"
}

func (g *Gateway) directCasualReply(input string) (string, bool) {
	switch normalizeQuestionText(input) {
	case "你好", "您好", "hi", "hello", "嗨", "hey":
		return "你好！我是 SelfMind，可以帮你处理开发任务、检查代码、运行工具和跨端协同。", true
	case "你是谁", "你叫什么", "你是干嘛的", "whoareyou", "whatareyou":
		reply := "我是 SelfMind，一个面向开发任务和多端协同的 AI 工作助手。"
		if label := g.modelDisplayLabel(); label != "" {
			reply += " 当前连接的模型是 " + label + "。"
		}
		return reply, true
	case "谢谢", "多谢", "谢了", "thanks", "thankyou":
		return "不客气，需要继续处理任务时直接叫我。", true
	case "再见", "拜拜", "bye", "晚安":
		return "再见，需要时再回来找我。", true
	}
	return "", false
}

func (g *Gateway) handleSkill(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	skillName := trimKnownPrefix(input, []string{"/skill ", "/s ", "调用技能", "用技能", "执行技能", "运行技能"})
	if skillName == "" {
		return "请指定要调用的技能。", llm.UsageStats{}, nil
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
	query := trimKnownPrefix(input, []string{"/query ", "/search ", "查一下", "搜索 ", "查历史"})
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
	return fmt.Sprintf("路由指令已收到：目前正在 %s 渠道为你服务。", channel), llm.UsageStats{}, nil
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
		"未完成", "还没完成", "没有完成", "待完成", "需要继续", "阻塞",
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
		"已完成", "任务完成", "处理完成", "已处理完", "已经完成", "搞定",
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
	for _, prefix := range []string{"帮我", "请帮我", "我想", "请", "please"} {
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
