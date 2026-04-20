# SelfMind 功能补齐路线图

> 本文档基于与 Hermes Agent 的逐模块代码对比，梳理 SelfMind 需优先补齐的四项核心能力及其实现方案。

---

## 目录

1. [技能进化与淘汰机制](#1-技能进化与淘汰机制)
2. [多智能体协作（Multi-Agent Collaboration）](#2-多智能体协作multi-agent-collaboration)
3. [深度用户画像系统](#3-深度用户画像系统)
4. [浏览器自动化（Browser RPA）](#4-浏览器自动化browser-rpa)

---

## 1. 技能进化与淘汰机制

### 1.1 现状分析

**代码位置：** `internal/kernel/reflection.go`

当前实现只有 `Reflect()` → `ArchiveSkill()` 单向链路，没有任何淘汰逻辑：

```go
// 现有逻辑：只增不删
func (r *ReflectionEngine) Reflect(ctx context.Context, history TaskHistory) (bool, string, error) {
    // 1. 复杂度阈值检查
    if len(history.Steps) < r.Config.MinComplexityThreshold {
        return false, "", nil
    }
    // 2. LLM 判断是否归档
    // 3. 直接写磁盘 —— 没有验证、没有评分、没有淘汰
}
```

**问题：**
- `AutoArchiveConfidence` 字段定义了但从未使用
- 技能文件只增不删，长期导致检索污染（"脑雾"）
- 没有 skill 调用元数据，无法做数据驱动决策

### 1.2 目标

实现有界积累 + 数据驱动淘汰：

```
每次 skill 被调用 → 记录元数据 → 定期评估 → 低价值 skill 归档至 archive/
```

### 1.3 实现方案

#### 1.3.1 新增 SQLite 表：skill_metrics

```sql
CREATE TABLE skill_metrics (
    id              TEXT PRIMARY KEY,
    skill_name      TEXT NOT NULL,
    call_count      INTEGER DEFAULT 0,
    fail_count      INTEGER DEFAULT 0,
    total_score     REAL DEFAULT 0.0,
    last_used_at    INTEGER,   -- unix timestamp
    created_at      INTEGER,
    archived        INTEGER DEFAULT 0,  -- 1 = 已归档
    archived_at     INTEGER
);

CREATE INDEX idx_skill_metrics_name ON skill_metrics(skill_name);
CREATE INDEX idx_skill_metrics_archived ON skill_metrics(archived);
```

#### 1.3.2 新增文件：`internal/kernel/skill_store.go`

```go
package kernel

import (
    "context"
    "fmt"
    "path/filepath"
    "sync"
    "time"

    "selfmind/internal/kernel/llm"
)

// SkillMetrics 存储在 SQLite，通过 storageProvider 接口访问
// 以下为 SkillStore 的完整接口设计

// SkillMetrics 技能调用元数据
type SkillMetrics struct {
    SkillName    string
    CallCount    int
    FailCount    int
    TotalScore   float64  // 累计评分，评分范围 0.0~1.0
    LastUsedAt   int64    // unix timestamp
    CreatedAt    int64
    Archived     bool
    ArchivedAt   int64
}

// SkillStore 管理技能文件的元数据、调用记录和淘汰策略
type SkillStore struct {
    skillsDir    string
    archiveDir   string
    metricsPath string  // SQLite DB path for metrics
    mu          sync.RWMutex
}

// NewSkillStore 初始化技能存储
func NewSkillStore(skillsDir string) (*SkillStore, error) {
    archiveDir := filepath.Join(skillsDir, "..", "skills-archive")
    // 确保 archive 目录存在
    // 初始化 SQLite metrics 数据库
    return &SkillStore{
        skillsDir:  skillsDir,
        archiveDir: archiveDir,
        metricsPath: filepath.Join(archiveDir, "..", ".skill_metrics.db"),
    }, nil
}

// RecordCall 记录一次技能调用
// score: 0.0~1.0，由调用方（Agent）评估传入
func (s *SkillStore) RecordCall(ctx context.Context, skillName string, success bool, score float64) error {
    // 实现：
    // 1. 打开 SQLite 连接
    // 2. INSERT OR REPLACE 到 skill_metrics 表
    // 3. 更新 call_count, fail_count, total_score, last_used_at
    // 4. 如果 success=false，fail_count++
    // 5. 关闭连接（或使用连接池）
}

// GetMetrics 返回指定技能的元数据
func (s *SkillStore) GetMetrics(ctx context.Context, skillName string) (*SkillMetrics, error)

// PruneLowValue 淘汰低价值技能
// 淘汰条件：call_count < minCalls AND last_used_at < (now - 30 days)
// 归档位置：skills-archive/{skillName}.md，并从 skills/ 删除
func (s *SkillStore) PruneLowValue(ctx context.Context, minCalls int, maxAgeDays int) ([]string, error) {
    // 返回被归档的 skill 列表（用于日志和通知）
}

// ListActive 返回所有未归档的技能及其元数据
func (s *SkillStore) ListActive(ctx context.Context) ([]SkillMetrics, error) {
    // SELECT * FROM skill_metrics WHERE archived=0 ORDER BY total_score DESC
}
```

#### 1.3.3 修改 `reflection.go` 中的 `Reflect()` 调用链

```go
// 在 internal/kernel/agent.go 的 RunConversation 中
// 每次 skill 调用完成后：

// 在 tools/dispatcher.go 的 Dispatch 方法中
// skill 调用返回后，记录元数据：
func (d *Dispatcher) Dispatch(name string, args map[string]interface{}) (string, error) {
    result, err := d.registry.Call(name, args)
    
    // skill 调用元数据记录（新增）
    if strings.HasPrefix(name, "skill:") && d.skillStore != nil {
        skillName := strings.TrimPrefix(name, "skill:")
        success := err == nil
        score := evaluateSkillScore(name, args, result, err)  // 见下方评分策略
        go d.skillStore.RecordCall(context.Background(), skillName, success, score)
    }
    
    return result, err
}

// evaluateSkillScore 评分策略
// - 任务成功完成 + 结果质量高 → 1.0
// - 任务成功完成但结果被截断 → 0.6
// - 任务失败（工具错误）→ 0.2
// - 任务失败（LLM 幻觉/逻辑错误）→ 0.0
func evaluateSkillScore(name string, args map[string]interface{}, result string, err error) float64 {
    if err != nil {
        return 0.0
    }
    if len(result) > 5000 {
        return 0.6  // 被截断，结果可能不完整
    }
    return 1.0
}
```

#### 1.3.4 添加定时清理任务（通过 cron 系统）

```go
// 在 internal/kernel/task/cron/scheduler.go 中注册
// 每天凌晨 3:00 执行一次 skill 淘汰

job := cron.Job{
    Name:        "skill-pruner",
    Schedule:    "0 3 * * *",
    Description: "清理低价值技能文件",
    Handler: func(ctx context.Context) error {
        store, err := NewSkillStore(skillsDir)
        if err != nil {
            return err
        }
        // call_count < 3 AND last_used > 60天 → 归档
        // call_count < 10 AND last_used > 30天 → 归档
        archived, err := store.PruneLowValue(ctx, 3, 60)
        if err != nil {
            return err
        }
        if len(archived) > 0 {
            fmt.Printf("[SkillPruner] Archived %d skills: %v\n", len(archived), archived)
        }
        return nil
    },
}
```

#### 1.3.5 可选：技能变体生成 + 沙盒测试

**适用于失败率高的 skill，不是所有 skill 都需要这个流程。**

```go
// 在 internal/kernel/skill_evolution.go（新增文件）

// SkillVariant 代表一个技能变体及其测试结果
type SkillVariant struct {
    Content   string
    TestCases []TestCase
    Score     float64
    Passed    bool
}

// TestCase 单个测试用例
type TestCase struct {
    Input    string
    Expected string  // 关键字或模式，不要求精确匹配
}

// EvolveSkill 对指定 skill 生成变体并测试
// 仅在以下情况触发：
//   - 该 skill 在最近 7 天内失败率 > 30%
//   - 或者用户手动触发 /evolve {skill-name}
func (r *ReflectionEngine) EvolveSkill(ctx context.Context, skill SkillDefinition, failureLog string) error {
    // 1. 生成 3 个变体（通过 LLM 对原 skill 内容做突变）
    prompt := fmt.Sprintf(`
Given this skill:
---
%s
---

And recent failures:
%s

Generate 3 improved variants. For each variant, provide:
1. The improved SKILL.md content
2. A brief explanation of what changed

Return as JSON array.`, skill.Content, failureLog)
    
    resp, err := r.Provider.ChatCompletion(ctx, []llm.Message{{Role: "user", Content: prompt}})
    if err != nil {
        return err
    }
    
    variants := parseVariants(resp)  // 解析 JSON
    
    // 2. 并行测试（Go goroutine）
    var wg sync.WaitGroup
    results := make([]SkillVariant, len(variants))
    for i, v := range variants {
        wg.Add(1)
        go func(idx int, variant string) {
            defer wg.Done()
            results[idx] = r.sandboxTest(ctx, skill, variant)
        }(i, v.Content)
    }
    wg.Wait()
    
    // 3. 选择最优变体（基于测试通过率 + 语义相似度）
    best := selectBest(results, skill.Content)
    if best.Score > currentScore(skill) {
        return r.replaceSkill(skill.Name, best.Content)
    }
    return nil  // 没有改进，不替换
}

// sandboxTest 在沙盒环境中运行 skill 的 test cases
func (r *ReflectionEngine) sandboxTest(ctx context.Context, orig SkillDefinition, variant string) SkillVariant {
    // 注意：沙盒测试目前用 mock 实现
    // 未来可通过 subprocess 执行受限的 shell 命令
    // 避免引入复杂的 chroot/docker 依赖
    
    // 1. 提取 variant 中的代码块作为待测步骤
    steps := extractSkillSteps(variant)
    
    // 2. 用模拟输入运行（非真实文件系统）
    // 3. 返回结果评分
    return SkillVariant{
        Content: variant,
        Passed:  true,
        Score:   0.85,  // TODO: 实现真实评分
    }
}

// selectBest 选择最优变体
// 评分 = test_pass_rate * 0.7 + semantic_similarity * 0.3
func selectBest(variants []SkillVariant, original string) SkillVariant {
    best := variants[0]
    for _, v := range variants[1:] {
        if v.Score > best.Score {
            best = v
        }
    }
    return best
}
```

#### 1.3.6 验收标准

- [ ] `skill_metrics` 表正常写入（每次 skill 调用后 metrics 更新）
- [ ] `/cron run` 手动触发 `skill-pruner` 任务，验证低价值 skill 被正确归档
- [ ] 归档后的 skill 在 `Search()` 结果中不出现
- [ ] 可通过 `ListActive()` 查看当前所有活跃 skill 的评分排名
- [ ] 新 skill 的 `call_count=0`，历史 skill 正常累计

---

## 2. 多智能体协作（Multi-Agent Collaboration）

### 2.1 现状分析

**代码位置：** `internal/tools/delegate.go` + `internal/app/delegation.go`

当前实现的本质是**同步串行调用**：

```go
// internal/app/delegation.go 第 99 行
return subAgent.RunConversation(context.Background(), "system", "delegation", fullPrompt)
```

`delegateFn` 是阻塞的，主 agent 等待 subagent 完成才返回，无法并行执行多任务。

此外还存在两个问题：
1. **上下文污染**：所有 subagent 共享同一个 `MemoryManager` 实例
2. **缺少保活机制**：Gateway 的 idle timeout 在 subagent 长时间运行时可能杀死主 agent

### 2.2 目标

```
主 Agent                         主 Agent
  │                                │
  ├─► [subagent-1] ──并行──► 汇总   │  ← 目标形态
  ├─► [subagent-2] ──并行──► 汇总   │
  └─► [subagent-3] ──并行──► 汇总   │


当前形态                          │
  │                              │
  ├─► [subagent-1] ──► 汇总 ──► [subagent-2] ──► 汇总 ──► [subagent-3] ──► 汇总
```

### 2.3 实现方案

#### 2.3.1 新增文件：`internal/kernel/multi_agent.go`

```go
package kernel

import (
    "context"
    "fmt"
    "sync"
    "sync/atomic"
    "time"

    "selfmind/internal/kernel/llm"
    "selfmind/internal/kernel/memory"
)

// SubAgentResult 子 Agent 执行结果
type SubAgentResult struct {
    Index   int
    Goal    string
    Output  string
    Err     error
    StartedAt time.Time
    FinishedAt time.Time
}

// SubAgentConfig 子 Agent 配置
type SubAgentConfig struct {
    Goal       string
    Context    string
    Toolsets   []string  // 空 = 使用父级所有工具
    MaxIter    int       // 0 = 使用默认值 50
    Depth      int       // 传递的递归深度
}

// MultiAgentHost 主 Agent 协调器
type MultiAgentHost struct {
    mem       *memory.MemoryManager
    provider  llm.Provider
    maxDepth  int       // 最大递归深度，默认 2
    maxConcurrent int   // 最大并发子 Agent 数，默认 3
}

// NewMultiAgentHost 创建主 Agent 协调器
func NewMultiAgentHost(mem *memory.MemoryManager, provider llm.Provider) *MultiAgentHost {
    return &MultiAgentHost{
        mem:          mem,
        provider:     provider,
        maxDepth:     2,
        maxConcurrent: 3,
    }
}

// RunParallel 并行执行多个子 Agent
// 主 Agent 在所有子 Agent 完成后获得汇总结果
func (h *MultiAgentHost) RunParallel(ctx context.Context, subAgents []SubAgentConfig) ([]SubAgentResult, error) {
    // 深度检查
    for _, sa := range subAgents {
        if sa.Depth >= h.maxDepth {
            return nil, fmt.Errorf("subagent recursion depth limit reached (%d)", h.maxDepth)
        }
    }

    // 并发控制： semaphore 模式
    sem := make(chan struct{}, h.maxConcurrent)
    var wg sync.WaitGroup
    results := make([]SubAgentResult, len(subAgents))
    var hasErr int32 = 0

    for i, sa := range subAgents {
        wg.Add(1)
        go func(idx int, cfg SubAgentConfig) {
            defer wg.Done()

            // 获取信号量
            select {
            case sem <- struct{}{}:
                defer func() { <-sem }()
            case <-ctx.Done():
                results[idx] = SubAgentResult{
                    Index: idx,
                    Goal:  cfg.Goal,
                    Err:   ctx.Err(),
                }
                atomic.AddInt32(&hasErr, 1)
                return
            }

            started := time.Now()
            subCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)  // 子 Agent 5 分钟超时
            defer cancel()

            output, err := h.runSingleSubAgent(subCtx, cfg)
            finished := time.Now()

            results[idx] = SubAgentResult{
                Index:      idx,
                Goal:       cfg.Goal,
                Output:     output,
                Err:        err,
                StartedAt:  started,
                FinishedAt: finished,
            }
            if err != nil {
                atomic.AddInt32(&hasErr, 1)
            }
        }(i, sa)
    }

    wg.Wait()

    // 如果任何子 Agent 失败，返回错误但不中断整体流程
    if hasErr > 0 {
        return results, fmt.Errorf("%d/%d subagents failed", hasErr, len(subAgents))
    }
    return results, nil
}

// runSingleSubAgent 运行单个子 Agent
func (h *MultiAgentHost) runSingleSubAgent(ctx context.Context, cfg SubAgentConfig) (string, error) {
    // 1. 创建独立的 tenantID（实现上下文隔离）
    subTenantID := fmt.Sprintf("subagent-%d", time.Now().UnixNano())

    // 2. 构建子 Agent backend（工具过滤）
    backend := h.buildSubBackend(cfg.Toolsets)

    // 3. 构建子 Agent
    maxIter := cfg.MaxIter
    if maxIter == 0 {
        maxIter = 50
    }
    subAgent := NewAgent(
        h.mem,              // 注意：仍然共享 MemoryManager，但 tenantID 隔离
        backend,
        h.provider,
        "You are a focused subagent completing a delegated task.",
        maxIter,
        3,
        nil,                 // 子 Agent 不需要独立 Reflector
    )

    // 4. 构造 prompt
    prompt := h.buildSubAgentPrompt(cfg)

    // 5. 执行
    resp, _, err := subAgent.RunConversation(ctx, subTenantID, "delegation", prompt)
    return resp, err
}

// buildSubBackend 根据 toolsets 过滤工具
func (h *MultiAgentHost) buildSubBackend(toolsets []string) AgentBackend {
    // 复用 internal/app/delegation.go 中的过滤逻辑
    // 返回一个受限的 Dispatcher
    return nil  // TODO: 实现
}

// buildSubAgentPrompt 构建子 Agent 的 system prompt
func (h *MultiAgentHost) buildSubAgentPrompt(cfg SubAgentConfig) string {
    prompt := fmt.Sprintf(`Target Goal: %s
Context: %s
Available Toolsets: %v

Complete this task using the available tools. When finished, provide a clear summary of:
- What you did
- What you found or accomplished
- Any files created or modified
- Any issues encountered

Be thorough but concise -- your response is returned to the parent agent as a summary.`, 
        cfg.Goal, cfg.Context, cfg.Toolsets)
    return prompt
}
```

#### 2.3.2 修改 `internal/tools/delegate.go`

```go
// DelegateTool.Execute 修改为支持批量并行

// DelegateTool 新增批量模式字段
type DelegateTool struct {
    BaseTool
    delegateFn      func(goal string, context string, toolsets []string) (string, llm.UsageStats, error)
    batchDelegateFn func(goals []string, contexts []string, toolsets []string) ([]string, error)  // 新增
}

// NewBatchDelegateFn 注册批量委托函数
func (t *DelegateTool) RegisterBatchDelegateFn(fn func(goals []string, contexts []string, toolsets []string) ([]string, error)) {
    t.batchDelegateFn = fn
}

// Execute 支持两种模式：
// 1. 单目标：args["goal"] = string → 调用 delegateFn
// 2. 批量：args["goals"] = []string → 调用 batchDelegateFn
func (t *DelegateTool) Execute(args map[string]interface{}) (string, error) {
    if goals, ok := args["goals"].([]interface{}); ok && len(goals) > 0 {
        // 批量模式
        if t.batchDelegateFn == nil {
            return "", fmt.Errorf("batch delegation not initialized")
        }
        var goalStrs []string
        for _, g := range goals {
            if s, ok := g.(string); ok {
                goalStrs = append(goalStrs, s)
            }
        }
        var ctxStrs []string
        if contexts, ok := args["contexts"].([]interface{}); ok {
            for _, c := range contexts {
                if s, ok := c.(string); ok {
                    ctxStrs = append(ctxStrs, s)
                }
            }
        } else {
            ctxStrs = make([]string, len(goalStrs))
        }
        toolsets, _ := args["toolsets"].(string)
        
        results, err := t.batchDelegateFn(goalStrs, ctxStrs, 
            strings.Split(toolsets, ","))
        if err != nil {
            return "", err
        }
        // 汇总结果
        var sb strings.Builder
        for i, r := range results {
            sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r))
        }
        return sb.String(), nil
    }

    // 单目标模式（保持原有逻辑）
    goal, _ := args["goal"].(string)
    if goal == "" {
        return "", fmt.Errorf("goal is required")
    }
    context, _ := args["context"].(string)
    toolsets, _ := args["toolsets"].(string)

    if t.delegateFn == nil {
        return "", fmt.Errorf("delegate not initialized")
    }
    resp, _, err := t.delegateFn(goal, context, strings.Split(toolsets, ","))
    return resp, err
}
```

#### 2.3.3 修改 `internal/app/delegation.go`

```go
// MakeDelegateFn 改造为同时注册单目标和批量委托函数

func MakeDelegateFn(...) (singleFn, batchFn func(...) (..., error)) {
    singleFn = func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
        // ... 现有逻辑（保持不变） ...
    }

    batchFn = func(goals, contexts []string, toolsets []string) ([]string, error) {
        host := kernel.NewMultiAgentHost(mem, provider)
        // host.MaxConcurrent = cfg.MaxConcurrent
        
        configs := make([]kernel.SubAgentConfig, len(goals))
        for i, g := range goals {
            ctx := ""
            if i < len(contexts) {
                ctx = contexts[i]
            }
            configs[i] = kernel.SubAgentConfig{
                Goal:     g,
                Context:  ctx,
                Toolsets: toolsets,
                Depth:    0,
            }
        }

        results, err := host.RunParallel(context.Background(), configs)
        if err != nil {
            // 部分失败仍返回结果
        }

        outputs := make([]string, len(results))
        for i, r := range results {
            if r.Err != nil {
                outputs[i] = fmt.Sprintf("[ERROR] %v", r.Err)
            } else {
                outputs[i] = r.Output
            }
        }
        return outputs, err
    }

    return singleFn, batchFn
}
```

#### 2.3.4 Gateway 心跳保活（避免 idle timeout）

```go
// 在 internal/gateway/router/gateway.go 中添加

// SubAgentKeepAlive 子 Agent 执行期间的心跳通知
type SubAgentKeepAlive struct {
    tenantID  string
    taskDesc  string
    stopCh    chan struct{}
}

// StartKeepAlive 启动心跳 Goroutine
// 每 25 秒向 gateway 发送一次活动信号
func StartKeepAlive(tenantID, taskDesc string, onHeartbeat func(string)) *SubAgentKeepAlive {
    s := &SubAgentKeepAlive{
        tenantID: tenantID,
        taskDesc:  taskDesc,
        stopCh:   make(chan struct{}),
    }
    go func() {
        ticker := time.NewTicker(25 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                onHeartbeat(fmt.Sprintf("subagent working: %s", s.taskDesc))
            case <-s.stopCh:
                return
            }
        }
    }()
    return s
}

// Stop 停止心跳
func (s *SubAgentKeepAlive) Stop() {
    close(s.stopCh)
}
```

在 `runSingleSubAgent` 中调用：

```go
keepAlive := StartKeepAlive(subTenantID, cfg.Goal, func(desc string) {
    // 回调：更新主 agent 的最后活跃时间
    // 通过 event channel 或共享的 agent reference 实现
})
defer keepAlive.Stop()
```

#### 2.3.5 验收标准

- [ ] `delegate_task(goals=["写第一章", "写第二章", "写第三章"])` 并行执行，总耗时 ≈ max(各子任务耗时)，而非 sum
- [ ] `delegate_task(goal="单一任务")` 保持原有单目标行为不变（向后兼容）
- [ ] subagent 的 tenantID 独立，查询记忆时不返回父 agent 会话内容
- [ ] 子 Agent 数量超过 3 时，超过的进入等待队列（信号量控制）
- [ ] depth >= 2 的 delegation 请求返回明确错误

---

## 3. 深度用户画像系统

### 3.1 现状分析

**代码位置：** `internal/kernel/memory/storage.go`

现有 `MemoryManager` 只有简单的 `AddFact / GetFacts` 接口，没有 profile synthesis 能力。

```go
// StorageProvider 接口（现有）
AddFact(ctx context.Context, tenantID string, target, content string) error
GetFacts(ctx context.Context, tenantID string, target string) ([]Fact, error)
```

Honcho 的 `dialectic_query` / `get_peer_card` 核心价值是：
1. **语义归一化**：从零散 facts 推断用户兴趣、职业、偏好
2. **辩证推理**：基于历史行为模式判断用户认知变化
3. **主动注入**：不需要 LLM 每次从原始 facts 重新推理

### 3.2 目标

在 `MemoryManager` 上构建一层 profile synthesis，使 agent 在每次对话开始时能获取综合后的用户画像，而非原始 facts 列表。

```
原始 facts                    用户画像
────────────────────          ──────────────────────
- 用户在 5 月 10 日配置了 nginx   →  用户熟悉 Linux 服务器管理
- 用户经常使用 docker 命令       →  了解容器化部署
- 用户最近问了很多 Vue 相关问题  →  正在学习前端开发
- 用户对隐私问题表示关注         →  重视数据安全
```

### 3.3 实现方案

#### 3.3.1 新增文件：`internal/kernel/memory/profile.go`

```go
package memory

import (
    "context"
    "fmt"
    "strings"
    "time"
)

// UserProfile 用户画像（综合分析结果）
type UserProfile struct {
    TenantID    string
    Summary     string           // 一段式综合描述（用于 system prompt）
    Preferences []ProfileFact    // 结构化偏好列表
    Patterns    []Pattern        // 行为模式
    LastBuiltAt int64            // unix timestamp
}

// ProfileFact 单条结构化偏好
type ProfileFact struct {
    Category    string  // "tech_stack", "work_style", "communication", "values"
    Fact        string
    Confidence  float64 // 0.0~1.0
    SourceCount int     // 来源 fact 数量（用于置信度计算）
}

// Pattern 行为模式
type Pattern struct {
    Description string
    Examples    []string  // 支持这个模式的具体事实
    Confidence  float64
}

// ProfileBuilder 从 facts 构建用户画像
type ProfileBuilder struct {
    llm LLMProvider  // 调用 LLM 进行综合推理
}

// BuildProfile 综合所有 facts 生成用户画像
// 触发条件：
//   - 距上次构建 > 24 小时
//   - 新增 facts 数量 > 5
//   - 用户手动触发 /profile refresh
func (pb *ProfileBuilder) BuildProfile(ctx context.Context, mem *MemoryManager, tenantID string) (*UserProfile, error) {
    // 1. 获取所有未归档的 facts
    facts, err := mem.GetFacts(ctx, tenantID, "user")
    if err != nil {
        return nil, err
    }
    if len(facts) < 3 {
        return nil, fmt.Errorf("not enough facts to build profile (need >= 3)")
    }

    // 2. 构建 LLM 综合 prompt
    factStrings := make([]string, len(facts))
    for i, f := range facts {
        factStrings[i] = fmt.Sprintf("- [%s] %s", f.Category, f.Content)
    }

    prompt := fmt.Sprintf(`
You are analyzing a user's facts to build a persistent user profile.

Facts collected:
%s

Your task:
1. Identify the user's main interests, skills, and work style
2. Note any patterns (how they communicate, how they approach problems)
3. Note any recent changes in behavior or interests (indicating evolution)
4. Rate your confidence for each insight (high/medium/low)

Return a JSON object with this exact structure:
{
  "summary": "2-3 sentence overview of who this user is",
  "preferences": [
    {"category": "tech_stack", "fact": "...", "confidence": 0.8, "source_count": 3},
    ...
  ],
  "patterns": [
    {"description": "...", "examples": ["...", "..."], "confidence": 0.7},
    ...
  ]
}

Only output valid JSON.`, strings.Join(factStrings, "\n"))

    resp, err := pb.llm.ChatCompletion(ctx, []Message{{Role: "user", Content: prompt}})
    if err != nil {
        return nil, err
    }

    // 3. 解析 JSON 响应
    profile, err := parseProfileResponse(resp)
    if err != nil {
        return nil, err
    }
    profile.TenantID = tenantID
    profile.LastBuiltAt = time.Now().Unix()

    // 4. 将 profile 存储为特殊 fact（带 category="profile"）
    profileJSON, _ := json.Marshal(profile)
    mem.AddFact(ctx, tenantID, "profile", string(profileJSON))

    return profile, nil
}

// GetProfileSummary 返回用于注入 system prompt 的简短摘要
func GetProfileSummary(profile *UserProfile) string {
    if profile == nil || profile.Summary == "" {
        return ""
    }
    var sb strings.Builder
    sb.WriteString("\n\n## USER PROFILE (learned over time)\n")
    sb.WriteString(profile.Summary)
    if len(profile.Preferences) > 0 {
        sb.WriteString("\n\nKey preferences:")
        for _, p := range profile.Preferences {
            if p.Confidence >= 0.6 {
                sb.WriteString(fmt.Sprintf("\n- [%s] %s", p.Category, p.Fact))
            }
        }
    }
    return sb.String()
}
```

#### 3.3.2 修改 `MemoryManager`

```go
// internal/kernel/memory/manager.go 新增方法

// MemoryManager 新增字段
type MemoryManager struct {
    provider     StorageProvider
    profile      *UserProfile        // 缓存的当前用户画像
    profileBuiltAt int64              // 上次构建时间
    profileMu    sync.RWMutex
}

// ShouldRebuildProfile 判断是否需要重新构建画像
func (m *MemoryManager) ShouldRebuildProfile() bool {
    m.profileMu.RLock()
    defer m.profileMu.RUnlock()
    if m.profile == nil {
        return true
    }
    ageHours := (time.Now().Unix() - m.profileBuiltAt) / 3600
    return ageHours > 24
}

// GetProfile 获取当前用户画像（优先返回缓存）
func (m *MemoryManager) GetProfile(ctx context.Context, tenantID string) (*UserProfile, error) {
    m.profileMu.RLock()
    if m.profile != nil && !m.ShouldRebuildProfile() {
        defer m.profileMu.RUnlock()
        return m.profile, nil
    }
    m.profileMu.RUnlock()

    // 重新构建
    builder := NewProfileBuilder(m.llmProvider)
    profile, err := builder.BuildProfile(ctx, m, tenantID)
    if err != nil {
        return nil, err
    }

    m.profileMu.Lock()
    m.profile = profile
    m.profileBuiltAt = profile.LastBuiltAt
    m.profileMu.Unlock()

    return profile, nil
}
```

#### 3.3.3 修改 `Agent.BuildSystemPrompt()`

```go
// internal/kernel/agent.go BuildSystemPrompt 方法

// 在 facts 注入之后添加 profile summary
userFacts, _ := m.memory.GetFacts(ctx, tenantID, "user")
memFacts, _ := m.memory.GetFacts(ctx, tenantID, "memory")

// 新增：profile 注入
var profileBlock string
if profile, err := m.memory.GetProfile(ctx, tenantID); err == nil && profile != nil {
    profileBlock = memory.GetProfileSummary(profile)
}

// 组装 system prompt
factBlock := buildFactBlock(userFacts, memFacts)
parts = append(parts, factBlock)
if profileBlock != "" {
    parts = append(parts, profileBlock)
}
```

#### 3.3.4 新增工具：`/profile` 命令

```go
// internal/tools/profile_tool.go

type ProfileTool struct {
    BaseTool
    getProfileFn func(ctx context.Context, tenantID string) (*UserProfile, error)
}

func (t *ProfileTool) Execute(args map[string]interface{}) (string, error) {
    tenantID, _ := args["_tenant_id"].(string)
    ctx := context.Background()
    
    profile, err := t.getProfileFn(ctx, tenantID)
    if err != nil {
        return "", err
    }

    var sb strings.Builder
    sb.WriteString(fmt.Sprintf("## User Profile (built at %s)\n\n", 
        time.Unix(profile.LastBuiltAt, 0).Format(time.RFC1123)))
    sb.WriteString(profile.Summary + "\n\n")
    
    sb.WriteString("### Preferences\n")
    for _, p := range profile.Preferences {
        sb.WriteString(fmt.Sprintf("- [%s] %s (confidence: %.0f%%)\n", 
            p.Category, p.Fact, p.Confidence*100))
    }
    
    sb.WriteString("\n### Patterns\n")
    for _, pat := range profile.Patterns {
        sb.WriteString(fmt.Sprintf("- %s\n", pat.Description))
        for _, ex := range pat.Examples {
            sb.WriteString(fmt.Sprintf("  - %s\n", ex))
        }
    }
    
    return sb.String(), nil
}
```

#### 3.3.5 验收标准

- [ ] `/profile` 命令返回综合后的用户画像（含 Summary、Preferences、Patterns）
- [ ] 新对话的 system prompt 中包含 `GetProfileSummary()` 输出
- [ ] 同一用户连续 3 次对话后，profile 的 Preferences 能反映至少 2 条稳定偏好
- [ ] Profile 超过 24 小时后自动重新构建

---

## 4. 浏览器自动化（Browser RPA）

### 4.1 现状分析

**代码位置：** `internal/tools/web_search.go`

现有实现仅支持简单的 DuckDuckGo HTML 抓取，没有：
- DOM 操作（点击、滚动、输入）
- 视觉理解（页面截图分析）
- 复杂 RPA 流程（多步交互、验证码处理）

Hermes 的 `browser_tool.py` 底层使用 `agent-browser`（一个 Go 二进制），通过 STDIO 接口调用，支持 CDP（Chrome DevTools Protocol）。

### 4.2 目标

通过 MCP（Model Context Protocol）接入浏览器自动化能力，SelfMind 内核不需要直接集成 Playwright/CDP，但需：
1. 内置 MCP Client，支持连接外部 MCP Server
2. 将 MCP 工具注册为本地工具，供 Agent 调用
3. 预留原生 CDP 驱动接口的扩展点（供未来性能优化）

### 4.3 实现方案

#### 4.3.1 架构概览

```
SelfMind Agent
     │
     ▼
Dispatcher ── MCP Tool Adapter ──────────────────────►  MCP Server (browser)
     │                                                      │
     │                                        ┌────────────┴────────────┐
     │                                        ▼                         ▼
     │                              @playwright/mcp-server     browser-use MCP
     │                                        │                         │
     │                                        └────────────┬────────────┘
     │                                                     ▼
     │                                         Browser (Playwright/CDP)
     │
     ▼
Local Tools (terminal, file, memory, ...)
```

#### 4.3.2 MCP Client 实现

**代码位置：** `internal/tools/mcp_client.go`（已存在但需增强）

```go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "sync"

    "selfmind/internal/kernel/llm"
)

// MCPClient 连接到 MCP Server 并调用其工具
type MCPClient struct {
    command    string
    args       []string
    process    *exec.Process
    stdin      *json.Encoder
    stdout     *json.Decoder
    mu         sync.Mutex
    requestID  int
    capabilities map[string]bool
}

// NewMCPClient 启动一个 MCP Server 进程
func NewMCPClient(command string, args []string) (*MCPClient, error) {
    // 1. 启动子进程
    // 2. 通过 STDIO 进行 JSON-RPC 握手
    // 3. 获取 server capabilities
    return &MCPClient{
        command:     command,
        args:        args,
        requestID:   1,
        capabilities: make(map[string]bool),
    }, nil
}

// ListTools 发现 MCP Server提供的工具
func (c *MCPClient) ListTools(ctx context.Context) ([]ToolDefinition, error) {
    // 发送 JSON-RPC: {"jsonrpc": "2.0", "method": "tools/list", "id": N}
    // 解析响应中的 tools 数组
}

// Call 调用 MCP 工具
func (c *MCPClient) Call(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
    c.mu.Lock()
    defer c.mu.Unlock()

    // 发送 JSON-RPC: {"jsonrpc": "2.0", "method": "tools/call", "params": {...}, "id": N}
    // 读取响应
    // 返回 string 内容（由 MCP Server 定义格式）
}
```

#### 4.3.3 MCP 工具适配器

```go
// MCPToolAdapter 将 MCP 工具适配为 SelfMind 的 Tool 接口
type MCPToolAdapter struct {
    BaseTool
    mcpClient  *MCPClient
    mcpToolName string
    inputSchema  map[string]interface{}  // MCP 的 inputSchema
}

func (a *MCPToolAdapter) Execute(args map[string]interface{}) (string, error) {
    return a.mcpClient.Call(context.Background(), a.mcpToolName, args)
}

// RegisterMCPTools 将 MCP Server 的所有工具注册到本地 Registry
func RegisterMCPTools(registry *Registry, mcpServerName string, command string, args []string) error {
    client, err := NewMCPClient(command, args)
    if err != nil {
        return fmt.Errorf("failed to start MCP server %s: %w", mcpServerName, err)
    }

    tools, err := client.ListTools(context.Background())
    if err != nil {
        return fmt.Errorf("failed to list MCP tools from %s: %w", mcpServerName, err)
    }

    for _, tool := range tools {
        adapter := &MCPToolAdapter{
            BaseTool: BaseTool{
                name:        "mcp:" + mcpServerName + ":" + tool.Name,
                description: tool.Description,
                schema:      convertMCPSchema(tool.InputSchema),
            },
            mcpClient:    client,
            mcpToolName:  tool.Name,
        }
        registry.Register(adapter)
    }
    return nil
}

// convertMCPSchema 将 MCP input schema 转换为 SelfMind ToolSchema
func convertMCPSchema(mcpSchema map[string]interface{}) ToolSchema {
    // MCP 使用 JSON Schema 格式，转换为内部格式
}
```

#### 4.3.4 配置文件支持

```yaml
# ~/.selfmind/config.yaml

mcp:
  servers:
    - name: "browser"
      command: "npx"
      args: ["-y", "@playwright/mcp-server"]
      env:
        PLAYWRIGHT_BROWSERS_PATH: "~/.cache/ms-playwright"
    
    - name: "filesystem"
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/home/user"]
      env: {}

  # MCP 工具调用超时（毫秒）
  timeout: 30000

  # 是否在启动时自动连接所有配置的 MCP Server
  auto_connect: true
```

#### 4.3.5 Browser MCP 工具映射（Playwright）

```go
// 常见 browser tool 的 MCP → SelfMind 映射
var browserToolMap = map[string]string{
    "browser_navigate": "mcp:browser:navigate",
    "browser_snapshot": "mcp:browser:snapshot", 
    "browser_click":    "mcp:browser:click",
    "browser_type":     "mcp:browser:type",
    "browser_scroll":    "mcp:browser:scroll",
    "browser_vision":    "mcp:browser:analyze",  // 截图+视觉分析
}

// Agent 启动时，在 BuildSystemPrompt 中注入 browser tool 的使用说明
// 作为 ToolDefinition 的 description 的一部分
```

#### 4.3.6 未来扩展点：原生 CDP 驱动（可选）

如果未来需要极致性能，可直接集成 CDP：

```go
// internal/tools/cdp_driver.go（扩展点，不在第一阶段实现）
// 使用 chrome-launcher + CDP 协议直接连接 Chrome
// 绕过 MCP STDIO 开销，适合高频调用场景

type CDPSession struct {
    wsURL    string
    conn     *websocket.Conn
    targetID string
}

func NewCDPSession(cdpURL string) (*CDPSession, error)
func (s *CDPSession) Navigate(ctx context.Context, url string) error
func (s *CDPSession) Click(ctx context.Context, selector string) error
func (s *CDPSession) Screenshot(ctx context.Context) ([]byte, error)
```

#### 4.3.7 验收标准

- [ ] 配置 `mcp.servers.browser` 后，Agent 能识别 `mcp:browser:navigate` 等工具
- [ ] Agent 调用 `mcp:browser:navigate` 时，浏览器实际打开目标页面
- [ ] `mcp:browser:snapshot` 返回页面 DOM 结构
- [ ] `mcp:browser:vision` 能返回截图并被视觉分析
- [ ] MCP Server 崩溃后，Agent 的其他工具不受影响
- [ ] 多 MCP Server 同时连接正常

---

## 附录 A：文件清单

```
selfmind/
├── internal/
│   ├── kernel/
│   │   ├── reflection.go          [修改] 集成 skillStore 调用
│   │   ├── agent.go              [修改] BuildSystemPrompt 集成 profile
│   │   ├── skill_store.go        [新增] 技能元数据和淘汰逻辑
│   │   ├── skill_evolution.go    [新增] 技能变体生成和测试
│   │   ├── multi_agent.go        [新增] 多智能体协调器
│   │   ├── memory/
│   │   │   ├── storage.go        [修改] 新增 skill_metrics 表 DDL
│   │   │   ├── manager.go        [修改] 新增 GetProfile / ShouldRebuildProfile
│   │   │   └── profile.go        [新增] 用户画像构建
│   │   └── task/
│   │       └── cron/
│   │           └── scheduler.go   [修改] 注册 skill-pruner cron job
│   └── tools/
│       ├── delegate.go          [修改] 支持批量并行模式
│       ├── mcp_client.go        [修改] 增强连接管理和工具注册
│       ├── profile_tool.go       [新增] /profile 命令
│       └── cdp_driver.go         [预留] 原生 CDP 扩展点（未来）
├── config/
│   └── config.go                 [修改] 新增 mcp.servers 配置结构
└── docs/
    └── selfmind-evolution-roadmap.md  [本文档]
```

## 附录 B：优先级和排期建议

| 优先级 | 功能 | 工作量估计 | 依赖 |
|--------|------|-----------|------|
| P0 | 技能元数据记录（skill_metrics）| 1天 | 无 |
| P0 | 技能淘汰（PruneLowValue）| 0.5天 | P0 |
| P0 | 多 Agent 并行执行 | 1.5天 | 无 |
| P1 | 多 Agent 上下文隔离（独立 tenantID）| 0.5天 | P0 多 Agent |
| P1 | Gateway 心跳保活 | 0.5天 | P0 多 Agent |
| P1 | 用户画像构建（ProfileBuilder）| 1天 | 无 |
| P2 | /profile 工具 | 0.5天 | P1 画像 |
| P2 | MCP Client 增强 | 2天 | 无 |
| P2 | Browser MCP 集成 | 1天 | P2 MCP |
| P3 | 技能变体生成 + 沙盒测试 | 3天 | P0 淘汰 |
| P3 | 原生 CDP 驱动（预留扩展点）| 5天 | P2 Browser |

---

*本文档随实现进度更新。每次功能完成后，在此文档对应章节打勾标记。*
