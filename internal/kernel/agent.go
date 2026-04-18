package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// AgentBackend is the interface for the agent's execution backend (tool dispatch + event channel).
// Concrete implementation is provided by the tools package via NewAgentBackend.
type AgentBackend interface {
	Dispatch(name string, args map[string]interface{}) (string, error)
	GetToolDefinitions() []map[string]interface{}
}

// Agent 核心推理循环
type Agent struct {
	memory         *memory.MemoryManager
	backend        AgentBackend
	llm            llm.Provider
	soul           string
	maxIterations  int
	maxRetries     int
	Reflector      *ReflectionEngine
	contextEngine  *ContextEngine
	EventChannel   chan string // emits "tool_start:name" and "tool_end:name:result" events
}

func NewAgent(mem *memory.MemoryManager, backend AgentBackend, provider llm.Provider, soul string, maxIter, maxRetries int, refl *ReflectionEngine) *Agent {
	ch := make(chan string, 10)
	ag := &Agent{
		memory:         mem,
		backend:        backend,
		llm:            provider,
		soul:           soul,
		maxIterations:  maxIter,
		maxRetries:     maxRetries,
		Reflector:      refl,
		contextEngine:  NewContextEngine(128000, 512),
		EventChannel:  ch,
	}
	ag.contextEngine.SetProvider(provider)
	return ag
}

const MaxRetries = 3

// chatWithRetry 实现了运行时自动 Fallback 逻辑
func (a *Agent) chatWithRetry(ctx context.Context, messages []llm.Message) (string, llm.UsageStats, error) {
	var lastErr error
	var usage llm.UsageStats
	max := a.maxRetries
	if max <= 0 {
		max = 1
	}
	for attempt := 0; attempt < max; attempt++ {
		req := llm.ChatRequest{Messages: messages}
		resp, err := a.llm.Chat(ctx, req)
		if err == nil {
			return resp.Content, resp.Usage, nil
		}
		lastErr = err

		// 如果是上下文取消或超时，立即退出，不再重试
		if ctx.Err() != nil {
			return "", usage, err
		}

		// 如果是 401/403 等鉴权错误，说明 Key 坏了，重试也无意义
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "invalid_api_key") {
			break
		}

		// 指数退避
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return "", usage, fmt.Errorf("llm chat failed after %d attempts: %w", max, lastErr)
}

// streamChatWithRetry 实现了流式调用的自动 Fallback 逻辑
func (a *Agent) streamChatWithRetry(ctx context.Context, messages []llm.Message) (<-chan llm.StreamEvent, error) {
	var lastErr error
	max := a.maxRetries
	if max <= 0 {
		max = 1
	}
	for attempt := 0; attempt < max; attempt++ {
		req := llm.ChatRequest{Messages: messages}
		ch, err := a.llm.StreamChat(ctx, req)
		if err == nil {
			return ch, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, err
		}

		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "invalid_api_key") {
			break
		}

		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return nil, fmt.Errorf("llm stream chat failed after %d attempts: %w", max, lastErr)
}

// SetBackend updates the agent's execution backend
func (a *Agent) SetBackend(b AgentBackend) {
	a.backend = b
}

// Dispatcher returns the tool dispatch backend for gateway handlers and skill tools.
func (a *Agent) Dispatcher() AgentBackend {
	return a.backend
}

// Memory returns the agent's memory manager.
func (a *Agent) Memory() *memory.MemoryManager {
	return a.memory
}

// Analyze implements tools.VisionLLM
func (a *Agent) Analyze(imageBase64, mimeType, question string) (string, error) {
	msg := llm.Message{
		Role: "user",
		MultiContent: []llm.ContentPart{
			{
				Type:     "image_base64",
				MimeType: mimeType,
				Data:     imageBase64,
			},
			{
				Type: "text",
				Text: question,
			},
		},
	}

	// Call LLM with the multimodal message
	return a.llm.ChatCompletion(context.Background(), []llm.Message{msg})
}

func emitToolEndEventWithDuration(ch chan string, name, result string, duration float64, err error) {
	if err != nil {
		ch <- fmt.Sprintf("tool_end:%s:error:%.3f:%v", name, duration, err)
	} else {
		displayResult := result
		if len(displayResult) > 200 {
			displayResult = displayResult[:200] + "...(truncated)"
		}
		ch <- fmt.Sprintf("tool_end:%s:%.3f:%s", name, duration, displayResult)
	}
}

func emitToolEndEvent(ch chan string, name, result string, err error) {
	emitToolEndEventWithDuration(ch, name, result, 0, err)
}
// RunConversation 执行 Agent 推理循环
// channel 用于渠道隔离的历史记录（如 'cli'、'wechat'、'dingtalk'）
func (a *Agent) RunConversation(ctx context.Context, tenantID, channel string, initialPrompt string) (string, llm.UsageStats, error) {
	var totalUsage llm.UsageStats

	// 0. Build dynamic system prompt (including facts)
	systemPrompt, _ := a.BuildSystemPrompt(ctx, tenantID)

	// 0.1 Auto-recall relevant context from history (Gap 1 Improvement)
	recallContext := a.autoRecall(ctx, tenantID, initialPrompt)
	if recallContext != "" {
		systemPrompt += "\n\n# RELEVANT CONTEXT FROM PREVIOUS SESSIONS\n" + recallContext
	}

	// Build messages using ContextEngine
	messages, err := a.contextEngine.BuildMessages(
		ctx, a.memory, tenantID,
		channel,
		systemPrompt,
		initialPrompt,
	)
	if err != nil {
		return "", totalUsage, fmt.Errorf("build messages: %w", err)
	}

	history := TaskHistory{
		Goal:  initialPrompt,
		Steps: []string{},
	}

	for i := 0; i < a.maxIterations; i++ {
		streamCh, err := a.streamChatWithRetry(ctx, messages)
		if err != nil {
			return "", totalUsage, fmt.Errorf("llm chat: %w", err)
		}

		var fullResp strings.Builder
		for event := range streamCh {
			if event.Err != nil {
				return "", totalUsage, fmt.Errorf("stream error: %w", event.Err)
			}
			if event.Content != "" {
				fullResp.WriteString(event.Content)
				if a.EventChannel != nil {
					a.EventChannel <- "stream:" + event.Content
				}
			}
			if event.Usage != nil {
				totalUsage.InputTokens += event.Usage.InputTokens
				totalUsage.OutputTokens += event.Usage.OutputTokens
			}
		}
		resp := fullResp.String()

		messages = append(messages, llm.Message{Role: "assistant", Content: resp})
		history.Steps = append(history.Steps, resp)

		// Extract and dispatch tool calls using regex
		calls := ExtractToolCalls(resp)
		if len(calls) > 0 {
			var wg sync.WaitGroup
			results := make([]struct {
				index int
				step  string
				msg   llm.Message
			}, len(calls))

			for idx, call := range calls {
				wg.Add(1)
				go func(i int, c ToolCall) {
					defer wg.Done()
					
					args := make(map[string]interface{})
					if c.Args != "" {
						json.Unmarshal([]byte(c.Args), &args)
					}
					if args == nil {
						args = make(map[string]interface{})
					}
					args["_tenant_id"] = tenantID

					// Emit tool start event
					if a.EventChannel != nil {
						a.EventChannel <- fmt.Sprintf("tool_start:%s:%s", c.Name, c.Args)
					}

					startTime := time.Now()
					result, err := a.backend.Dispatch(c.Name, args)
					duration := time.Since(startTime).Seconds()

					// Emit tool end event
					if a.EventChannel != nil {
						emitToolEndEventWithDuration(a.EventChannel, c.Name, result, duration, err)
					}

					var step string
					var msg llm.Message
					if err != nil {
						errorMsg := fmt.Sprintf("Error executing %s: %v", c.Name, err)
						if len(errorMsg) > 2000 {
							errorMsg = errorMsg[:2000] + "...(error message truncated)"
						}
						step = errorMsg
						msg = llm.Message{Role: "tool", Content: step}
					} else {
						llmResult := result
						if len(llmResult) > 10000 {
							llmResult = fmt.Sprintf("%s\n\n... (Content truncated: total %d chars) ...\n\n%s",
								llmResult[:5000], len(llmResult), llmResult[len(llmResult)-5000:])
						}
						step = fmt.Sprintf("Executed tool: %s, result: %s", c.Name, llmResult)
						msg = llm.Message{Role: "tool", Content: llmResult}
					}
					
					results[i].index = i
					results[i].step = step
					results[i].msg = msg
				}(idx, call)
			}
			wg.Wait()

			// Append results in order
			for _, res := range results {
				history.Steps = append(history.Steps, res.step)
				messages = append(messages, res.msg)
			}
			continue
		}

		// No tool calls — task complete, attempt reflection
		history.Outcome = resp
		if a.Reflector != nil {
			should, content, _ := a.Reflector.Reflect(ctx, history)
			if should {
				a.Reflector.ArchiveSkill(content)
			}
		}

		// Save trajectory to memory
		a.saveHistory(ctx, tenantID, channel, messages)

		return resp, totalUsage, nil
	}

	return "max iterations reached", totalUsage, nil
}

func (a *Agent) autoRecall(ctx context.Context, tenantID, query string) string {
	if a.memory == nil {
		return ""
	}
	// Search sessions with a limit of 2 for conciseness
	sessions, err := a.memory.SearchSessions(tenantID, query, 2)
	if err != nil || len(sessions) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, s := range sessions {
		snippet := s.Summary
		if snippet == "" {
			if len(s.Content) > 200 {
				snippet = s.Content[:200] + "..."
			} else {
				snippet = s.Content
			}
		}
		sb.WriteString(fmt.Sprintf("- Session %s: %s\n", s.SessionID, snippet))
		if i >= 1 {
			break
		}
	}
	return sb.String()
}

func (a *Agent) saveHistory(ctx context.Context, tenantID, channel string, messages []llm.Message) {
	if a.memory == nil {
		return
	}
	record := struct {
		Messages []llm.Message `json:"messages"`
	}{Messages: messages}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	a.memory.SaveTrajectory(ctx, tenantID, channel, data)

	sessionID := generateSessionID(messages)
	a.memory.IndexSession(ctx, tenantID, channel, sessionID, data)
}

func generateSessionID(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role == "user" {
			content := m.Content
			if len(content) > 64 {
				content = content[:64]
			}
			sum := sha256Hash(content)
			return sum[:16]
		}
	}
	return fmt.Sprintf("sess-%d", len(messages))
}

func sha256Hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (a *Agent) BuildSystemPrompt(ctx context.Context, tenantID string) (string, error) {
	var parts []string

	// 1. Core Persona (Soul)
	if a.soul != "" {
		parts = append(parts, a.soul)
	}

			// 2. Tool Instructions - 增强指令强度
	if a.backend != nil {
		defs := a.backend.GetToolDefinitions()
		if len(defs) > 0 {
			var sb strings.Builder
			sb.WriteString("\n# CRITICAL: TOOL USE INSTRUCTIONS\n")
			sb.WriteString("You MUST use local tools whenever the user asks about local files, directories, or system status.\n")
			sb.WriteString("To call a tool, you MUST use the exact format: [TOOL:tool_name:{\"arg\": \"val\"}]\n")
			sb.WriteString("The ONLY valid tool names are: ")
			for i, d := range defs {
				sb.WriteString(fmt.Sprintf("'%s'", d["name"]))
				if i < len(defs)-1 {
					sb.WriteString(", ")
				}
			}
			sb.WriteString(".\n")
			sb.WriteString("DO NOT use tools like 'ls', 'cat', 'read', 'run_command', or 'sh' which do not exist. Use the specific tools listed above.\n")
			sb.WriteString("DO NOT explain that you are using a tool, just output the [TOOL:...] tag.\n\n")
			sb.WriteString("## Available Tools\n")
			for _, d := range defs {
				sb.WriteString(fmt.Sprintf("### %s\n%s\n", d["name"], d["description"]))
				if params, ok := d["parameters"].(map[string]interface{}); ok {
					if props, ok := params["properties"].(map[string]interface{}); ok {
						sb.WriteString("Parameters:\n")
						for pName, pDef := range props {
							if def, ok := pDef.(map[string]interface{}); ok {
								sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", pName, def["type"], def["description"]))
							}
						}
					}
				}
				sb.WriteString("\n")
			}
			parts = append(parts, sb.String())
		}
	}

	if a.memory == nil {
		return strings.Join(parts, "\n\n"), nil
	}

	userFacts, _ := a.memory.GetFacts(ctx, tenantID, "user")
	memFacts, _ := a.memory.GetFacts(ctx, tenantID, "memory")

	if len(userFacts) > 0 || len(memFacts) > 0 {
		var factBlock strings.Builder
		factBlock.WriteString("<MEMORY>\n")
		for _, f := range userFacts {
			factBlock.WriteString(fmt.Sprintf("- [User Preference]: %s\n", f.Content))
		}
		for _, f := range memFacts {
			factBlock.WriteString(fmt.Sprintf("- [Environment]: %s\n", f.Content))
		}
		factBlock.WriteString("</MEMORY>")
		parts = append(parts, factBlock.String())
	}

	return strings.Join(parts, "\n\n"), nil
}
