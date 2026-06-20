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

// Gateway 统一消息处理入口，整合 identity + intent + task + agent
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

// NewGateway 创建一个统一网关
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

// HandleResponse 统一响应结构，支持同步和流式
type HandleResponse struct {
	Content      string
	Usage        llm.UsageStats
	Stream       <-chan llm.StreamEvent
	IsStreaming  bool
	Intent       Intent
	IntentReason string
}

// Handle 处理一条用户消息，返回响应内容
func (g *Gateway) Handle(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	if isModelStatusQuestion(input) {
		return &HandleResponse{Content: g.modelStatusReply(), Intent: IntentCasual, IntentReason: "model status question"}, nil
	}

	// 1. 意图分类
	intent, reason := g.intentClassifier.ClassifyWithReason(input)

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
		// 优先检查简单规则回复
		if IsCasualShortQuestion(input) {
			content, usage, err := g.handleCasual(ctx, unifiedUID, channel, input)
			return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
		}
		// 复杂的闲聊由 Agent 处理
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)

	case IntentContinue, IntentTask:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)
	}

	return &HandleResponse{Content: "抱歉，无法理解您的意图"}, nil
}

func (g *Gateway) handleTaskStreaming(ctx context.Context, unifiedUID, channel, input string, intent Intent, reason string) (*HandleResponse, error) {
	// 1. 任务管理
	var taskID int64
	var err error
	if intent == IntentContinue {
		t, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
		if err != nil || t == nil {
			return &HandleResponse{Content: "没有正在进行的任务。请告诉我你想要做什么？"}, nil
		}
		taskID = t.ID
	} else {
		title := extractTitle(input)
		taskID, err = g.taskManager.CreateTask(ctx, unifiedUID, title)
		if err != nil {
			return nil, err
		}
	}

	// 2. 注入上下文
	g.taskManager.AppendContext(ctx, unifiedUID, channel, "user", input)

	// 3. 构建 Agent 专用的 EventChannel
	// 注意：这里的 EventChannel 会由 Agent 写入，我们需要转发或消费它
	// 对于 Gateway.Handle，我们返回一个包装好的 Response

	// 这里我们需要稍微修改 Agent 的 RunConversation 或者提供一个新的流式方法
	// 既然 Agent 已经支持了 EventChannel 里的 "stream:" 事件，我们可以直接利用

	// 我们在协程中运行 Agent
	respChan := make(chan llm.StreamEvent, 20)

	go func() {
		defer close(respChan)

		// 监听 Agent 的事件并转发到流式通道
		// 这里由于 Agent.RunConversation 是阻塞的，我们需要在它运行的同时监听 EventChannel
		// 但 Agent 实例只有一个，其 EventChannel 是共享的吗？
		// 是的，目前的实现中 Agent 结构体里有一个 EventChannel

		resp, usage, err := g.agent.RunConversation(ctx, unifiedUID, channel, input)
		if err != nil {
			respChan <- llm.StreamEvent{Err: err}
			if intent == IntentTask {
				g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "failed")
			}
			return
		}

		// 任务完成处理
		g.taskManager.AppendContext(ctx, unifiedUID, channel, "assistant", resp)
		if isTaskDone(resp) {
			g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "done")
		}

		// 发送最终 Usage
		if resp != "" {
			respChan <- llm.StreamEvent{Content: resp}
		}
		respChan <- llm.StreamEvent{Usage: &usage}
	}()

	return &HandleResponse{
		IsStreaming:  true,
		Stream:       respChan, // 注意：这里的 Stream 目前只透传，EventChannel 里的 stream: 还需要在调用方处理，或者我们在这里统一
		Intent:       intent,
		IntentReason: reason,
	}, nil
}

// handleCasual 闲聊：直接回答，存档闲聊摘要（不写 trajectory）
func (g *Gateway) handleCasual(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	reply := g.casualReply(input)

	// 保存闲聊摘要，供后续任务感知用户状态（不污染 trajectory）
	summary := fmt.Sprintf("闲聊: %s", input)
	if g.taskManager != nil {
		_ = g.taskManager.SaveCasualSummary(ctx, unifiedUID, channel, summary)
	}

	return reply, llm.UsageStats{}, nil
}

func isModelStatusQuestion(input string) bool {
	cleaned := normalizeQuestionText(input)
	if cleaned == "" {
		return false
	}

	hasModelWord := strings.Contains(cleaned, "模型") ||
		strings.Contains(cleaned, "model") ||
		strings.Contains(cleaned, "llm") ||
		strings.Contains(cleaned, "后端")
	if !hasModelWord {
		return false
	}

	statusCues := []string{
		"什么", "哪个", "哪一个", "当前", "现在", "目前", "正在用", "用的", "使用的", "连接的", "跑的",
		"what", "which", "current", "using", "running", "active",
	}
	for _, cue := range statusCues {
		if strings.Contains(cleaned, cue) {
			return true
		}
	}
	return false
}

func normalizeQuestionText(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	replacer := strings.NewReplacer(
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
		",", "",
		"，", "",
		":", "",
		"：", "",
	)
	return replacer.Replace(input)
}

func (g *Gateway) modelStatusReply() string {
	label := g.modelDisplayLabel()
	if label == "" {
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

// casualReply 根据输入生成闲聊回复
func (g *Gateway) casualReply(input string) string {
	// 简单规则回复
	switch normalizeQuestionText(input) {
	case "你好", "您好", "hi", "hello", "嗨", "hey":
		return "你好！有什么我可以帮你的吗？"
	case "你是谁", "你叫什么", "你干嘛的", "whoareyou", "whatareyou":
		reply := "我是 SelfMind，一个面向开发任务和多端协同的 AI 工作助手。"
		if label := g.modelDisplayLabel(); label != "" {
			reply += " 当前连接的模型是 " + label + "。"
		}
		return reply
	case "谢谢", "多谢", "谢了", "thanks", "thankyou":
		return "不客气！有需要随时找我。"
	case "再见", "拜拜", "bye", "晚安":
		return "再见！有需要随时回来。"
	case "牛逼", "厉害", "真棒":
		return "谢谢认可。我会继续把任务处理得更稳一点。"
	}
	return "嗯，我明白了。如果有需要执行的任务，随时告诉我。"
}

// handleSkill 处理 skill 调用
func (g *Gateway) handleSkill(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	skillName := input
	for _, prefix := range []string{"/skill ", "/s ", "调用技能 ", "用技能 ", "执行技能 ", "运行技能 "} {
		if len(skillName) > len(prefix) && skillName[:len(prefix)] == prefix {
			skillName = skillName[len(prefix):]
			break
		}
	}
	skillName = strings.TrimSpace(skillName)
	if skillName == "" {
		return "请指定要调用的技能", llm.UsageStats{}, nil
	}

	toolName := "skill:" + skillName
	resp, err := g.agent.Dispatcher().Dispatch(toolName, map[string]interface{}{
		"input":      skillName,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

// handleQuery 处理知识库/历史查询
func (g *Gateway) handleQuery(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	query := input
	for _, prefix := range []string{"/query ", "/search ", "查一下 ", "搜索 ", "查历史 "} {
		if len(query) > len(prefix) && query[:len(prefix)] == prefix {
			query = query[len(prefix):]
			break
		}
	}
	query = strings.TrimSpace(query)

	resp, err := g.agent.Dispatcher().Dispatch("session_search", map[string]interface{}{
		"query":      query,
		"limit":      10,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

// handleRoute 处理平台路由指令
func (g *Gateway) handleRoute(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	return fmt.Sprintf("路由指令已收到：目前正在 %s 渠道为您服务", channel), llm.UsageStats{}, nil
}

// ResolveUID 根据 platform + platformID 解析 unified_uid
func (g *Gateway) ResolveUID(ctx context.Context, platform, platformID string) (string, error) {
	return g.identityMapper.EnsureBound(ctx, platform, platformID)
}

// ListTasks 返回用户所有全局任务
func (g *Gateway) ListTasks(ctx context.Context, unifiedUID string) ([]task.Task, error) {
	return g.taskManager.ListTasks(ctx, unifiedUID)
}

// GetCurrentTaskInfo 返回当前进行中任务的信息
func (g *Gateway) GetCurrentTaskInfo(ctx context.Context, unifiedUID string) (*task.Task, error) {
	tt, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
	return tt, err
}

// isTaskDone conservatively detects whether an assistant response closed the task.
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
		"not done",
		"not completed",
		"not finished",
		"remaining work",
		"still need",
		"need to continue",
		"next steps",
		"todo:",
		"blocked",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u672a\u5b8c\u6210",
		"\u8fd8\u6ca1\u5b8c\u6210",
		"\u6ca1\u6709\u5b8c\u6210",
		"\u5f85\u5b8c\u6210",
		"\u9700\u8981\u7ee7\u7eed",
		"\u963b\u585e",
	}) {
		return false
	}
	if lower == "done" || lower == "completed" || lower == "finished" || lower == "all done" {
		return true
	}
	return taskResponseContainsAny(lower, []string{
		"task complete",
		"task completed",
		"completed successfully",
		"finished successfully",
		"all done",
		"implementation complete",
		"tests pass",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u5df2\u5b8c\u6210",
		"\u4efb\u52a1\u5b8c\u6210",
		"\u5904\u7406\u5b8c\u6210",
		"\u5df2\u5904\u7406\u5b8c",
		"\u5df2\u7ecf\u5b8c\u6210",
		"\u641e\u5b9a",
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

// extractTitle 从用户输入中提取任务标题
func extractTitle(input string) string {
	title := input
	prefixes := []string{"帮我", "帮我做", "帮我查", "帮我看看", "请帮我", "我想"}
	for _, p := range prefixes {
		if len(title) > len(p) && title[:len(p)] == p {
			title = title[len(p):]
			break
		}
	}

	if len(title) > 30 {
		title = title[:30] + "..."
	}
	return title
}

// QuickReply 处理快速回复
func (g *Gateway) QuickReply(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	return g.Handle(ctx, unifiedUID, channel, input)
}
