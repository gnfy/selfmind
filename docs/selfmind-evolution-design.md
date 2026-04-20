# SelfMind 自动进化模块开发文档

**版本**：v2.0  
**日期**：2026-04-19  
**目标**：参考 Hermes 实现生产级自进化机制

---

## 一、现状总览

| 维度 | Hermes | SelfMind | 差距级别 |
|------|--------|----------|----------|
| 架构 | Agentic（后台 Agent + 工具调用） | Prompt-based（单次 LLM 调用） | **P0** |
| 迭代次数 | 8 次后台迭代，可主动查/读/比/写 | 1 次，不行就废 | **P0** |
| 已有 skill 感知 | 先 search 再决定 create/update/patch | 盲写，不知同名存在 | **P0** |
| Skill 操作 | create/edit/patch/delete/search/read/write_file | 仅 create | **P0** |
| 安全扫描 | 写前 validate + 写后 scan_skill + 危险删除 | 无任何扫描 | **P0** |
| 触发机制 | 后台 goroutine，不阻塞主循环 | 同步调用，阻塞用户响应 | **P0** |
| Skill 格式 | 目录结构（SKILL.md + 附件） | 单一 .md 文件 | **P1** |
| Review Prompt | 3 种精细设计（memory/skill/combined） | 1 种简单 prompt | **P1** |
| 触发间隔 | nudge_interval 计数器（默认 10 次工具调用） | 无间隔控制 | **P1** |
| 用户反馈 | 短暂通知 `💾 skill X updated` | 无任何反馈 | **P1** |

---

## 二、架构设计：Agentic 自进化

### 2.1 核心思想

Hermes 的自进化不是"一次 LLM 调用生成 skill"，而是**启动一个完整的后台 Agent**，赋予它 `skill_manage` 工具，让它像正常任务一样自主工作：

```
Main Agent                              Background Review Agent
    │                                          │
    │  任务完成，无工具调用                        │
    │ ─────────────────────────────────────►  │
    │                                          │  8 次迭代：
    │                                          │    1. skill_manage(search) 查已有
    │                                          │    2. skill_manage(read) 读内容
    │                                          │    3. LLM 决定 create/update/patch
    │                                          │    4. skill_manage(create/patch)
    │                                          │    5. skill_manage(write_file) 附件
    │                                          │    6. 内部安全扫描
    │                                          │    7. 如危险则回滚
    │                                          │    8. 返回结果
    │                                          │
    │         callback("💾 skill docker-debug updated")
    │ ◄─────────────────────────────────────── │
    │                                          │
```

### 2.2 与 SelfMind 当前架构的本质区别

```
SelfMind 当前（Prompt-based）：
  LLM(ReflectPrompt) → "SKILL.md 内容" → os.WriteFile() → 完成

SelfMind 目标（Agentic）：
  BackgroundAgent.RunConversation()
      │
      ├─► iteration 1: LLM 思考 → skill_manage(search, pattern="docker-*")
      │                        → 查结果返回 LLM
      │
      ├─► iteration 2: LLM 决定 "create new" 或 "update existing"
      │                → skill_manage(read, name="docker-debug")
      │
      ├─► iteration 3-N: skill_manage(create/patch/write_file)
      │                  → 写目录结构
      │
      └─► 最后：安全扫描通过，写入 ~/.selfmind/skills/
```

---

## 三、模块清单与实现顺序

### Phase 1：基础设施（必须先完成）

#### 3.1 skill_manage 工具
**文件**：`internal/tools/skill_manage.go`（新建）

实现 Hermes 全部操作：

| action | 说明 | 关键逻辑 |
|--------|------|----------|
| `create` | 创建新 skill 目录 | validate_name → validate_frontmatter → mkdir → write SKILL.md → scan → commit/rollback |
| `read` | 读取 skill 内容 | 检查路径安全 → 读 SKILL.md |
| `search` | 搜索已有 skill | 遍历 ~/.selfmind/skills/ → FTS 匹配 |
| `patch` | 定向替换 | validate_name → 读文件 → old/new 替换 → scan → 写回 |
| `edit` | 全量重写 | validate_name → validate_frontmatter → 写文件 → scan |
| `delete` | 删除 skill | 检查是 user skill（非 builtin）→ os.RemoveAll |
| `write_file` | 写附件 | validate 路径 → 写 references/templates/scripts/assets |
| `remove_file` | 删除附件 | validate 路径 → os.Remove |
| `list` | 列出所有 skill | 遍历目录 → 返回名称列表 |

**核心验证函数**（参考 `skill_manager_tool.py`）：
```go
func validateName(name string) error
func validateFrontmatter(content string) error  // YAML 解析 + 必填字段检查
func validateContentSize(content string, maxBytes int) error
```

**安全扫描**（参考 `skills_guard.py`，见 3.2）。

#### 3.2 安全扫描（skills_guard）
**文件**：`internal/kernel/skills_guard.go`（新建）

静态扫描，在 write_file/create/patch/edit 之前调用。

扫描模式（正则）：

| 类别 | 模式 | 严重度 |
|------|------|--------|
| 敏感路径泄露 | `~/.ssh`, `~/.aws`, `~/.hermes/.env`, `~/.docker/config.json` | critical/high |
| 外泄网络请求 | `curl/wget/fetch` 带 `$(KEY)`, `$(TOKEN)` 等环境变量 | critical |
| 凭证文件读取 | `cat ~/.env`, `credentials`, `.netrc`, `.pgpass` | critical |
| 环境变量 dump | `printenv`, `os.environ`, `process.env` | high |
| DNS 渗透 | `dig/nslookup/host` 带变量插值 | critical |
| 持久化攻击 | crontab, systemd path injection, ssh key append | critical |
| 命令注入 | `; rm -rf`, `&& curl`, `\| sh` | critical |
| 压缩包炸弹 | 超大 tar/zip 创建 | high |

**安装策略**：
```
agent-created: safe → allow, caution → allow, dangerous → 询问
community:     safe → allow, caution → block, dangerous → block
builtin:       全部 allow
```

#### 3.3 Skill 目录格式支持
**文件**：`internal/kernel/skill_store.go`（修改）

从当前单一 `.md` 文件升级为目录结构：

```
~/.selfmind/skills/
  docker-debug/
    SKILL.md              # 必填
    references/           # 可选
      api.md
    templates/            # 可选
      config.yaml
    scripts/              # 可选
      setup.sh
    assets/               # 可选
      diagram.svg
```

SkillStore 需要新增方法：
```go
func (s *SkillStore) EnsureSkillDir(name string) (string, error)
func (s *SkillStore) ReadSkill(name string) (Skill, error)
func (s *SkillStore) ListSkills() ([]string, error)
func (s *SkillStore) DeleteSkill(name string) error
```

`Skill` 结构：
```go
type Skill struct {
    Name        string
    Description string
    Content     string    // SKILL.md 正文
    Category    string
    Tags        []string
    References  []string  // references/ 下的文件
    Templates   []string
    Scripts     []string
    Assets      []string
    Source      string    // "builtin" | "user" | "agent-created"
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

---

### Phase 2：核心自进化引擎

#### 3.4 Background Review Engine
**文件**：`internal/kernel/background_review.go`（新建）

```go
type BackgroundReviewEngine struct {
    provider    llm.Provider
    skillStore  *SkillStore
    config      EvolutionConfig
    callback    func(string)  // 通知主 Agent 结果
}

func (e *BackgroundReviewEngine) SpawnReview(
    ctx context.Context,
    messages []llm.Message,
    reviewMemory bool,
    reviewSkills bool,
)
```

**关键设计**：
1. 使用 `goroutine` 而非 thread（Go 风格）
2. 创建独立 `Agent` 实例，共享 LLM Provider 和 SkillStore
3. 独立运行 `maxIterations=8`
4. 不修改主会话历史
5. 通过 callback 将结果回传
6. devnull 抑制所有输出（不干扰主会话）
7. 完成后自动清理资源

**Review Prompt**（3 种）：
```go
// _SKILL_REVIEW_PROMPT
"Review the conversation above and consider saving or updating a skill if appropriate.\n\n" +
"Focus on: was a non-trivial approach used to complete a task that required trial " +
"and error, or changing course due to experiential findings along the way, or did " +
"the user expect or desire a different method or outcome?\n\n" +
"If a relevant skill already exists, update it with what you learned. " +
"Otherwise, create a new skill if the approach is reusable.\n" +
"If nothing is worth saving, just say 'Nothing to save.' and stop."

// _MEMORY_REVIEW_PROMPT
"Review the conversation above and consider saving to memory if appropriate.\n\n" +
"Focus on:\n" +
"1. Has the user revealed things about themselves — their persona, desires, " +
"preferences, or personal details worth remembering?\n" +
"2. Has the user expressed expectations about how you should behave, their work " +
"style, or ways they want you to operate?\n\n" +
"If something stands out, save it using the memory tool. " +
"If nothing is worth saving, just say 'Nothing to save.' and stop."

// _COMBINED_REVIEW_PROMPT
"Review the conversation above and consider two things:\n\n" +
"**Memory**: Has the user revealed things about themselves...\n\n" +
"**Skills**: Was a non-trivial approach used...\n\n" +
"Only act if there's something genuinely worth saving. " +
"If nothing stands out, just say 'Nothing to save.' and stop."
```

#### 3.5 触发逻辑改造
**文件**：`internal/kernel/agent.go`（修改）

**改造点 1**：nudge counter
```go
type Agent struct {
    // ... 现有字段 ...
    toolCallCount    int
    nudgeInterval    int  // 默认 10
}

func (a *Agent) Dispatch(name string, args map[string]interface{}) (string, error) {
    result, err := a.backend.Dispatch(name, args)
    if err == nil {
        a.toolCallCount++
        if a.toolCallCount >= a.nudgeInterval {
            a.toolCallCount = 0
            a.triggerBackgroundReview()
        }
    }
    return result, err
}
```

**改造点 2**：移除同步 Reflect() 调用
```go
// 旧（同步，阻塞）：
if a.Reflector != nil {
    should, content, _ := a.Reflector.Reflect(ctx, history)
    if should {
        a.Reflector.ArchiveSkill(content)
    }
}

// 新（不调用，background review 接管）：
// history.Outcome = resp
// 触发由 nudge counter 在工具调用时决定，而非在此处同步调用
```

**改造点 3**：Background Review 触发
```go
func (a *Agent) triggerBackgroundReview() {
    if a.ReviewEngine == nil {
        return
    }
    // 截取最近 N 条消息作为 snapshot
    messages := a.contextEngine.GetRecentMessages(50)
    
    go a.ReviewEngine.SpawnReview(
        context.Background(),
        messages,
        reviewSkills=true,
        reviewMemory=a.memory != nil,
    )
}
```

---

### Phase 3：配套改造

#### 3.6 SkillLoader 目录支持
**文件**：`internal/tools/skill_loader.go`（修改）

当前：扫描 `~/.selfmind/skills/*.md`  
目标：扫描 `~/.selfmind/skills/*/SKILL.md`

```go
func loadSkills() ([]Skill, error) {
    // 遍历所有子目录，读取 SKILL.md
    // 解析 frontmatter 提取 name/description/tags/category
    // 注册到 dispatcher
}
```

#### 3.7 tools.go 注册
**文件**：`internal/app/tools.go`（修改）

新增 `skill_manage` 工具注册：
```go
skillManage := tools.NewSkillManageTool(skillStore)
dispatcher.Register("skill_manage", skillManage.Handle)
```

---

## 四、配置项

```yaml
evolution:
  enabled: true
  mode: "silent"              # silent | interactive
  nudge_interval: 10          # 每 N 次工具调用触发一次 background review
  min_complexity_threshold: 2  # 最小复杂度（step 数量）
  auto_archive_confidence: 0.8
  max_review_iterations: 8    # background agent 最大迭代次数
  skill_dir: "~/.selfmind/skills"
```

---

## 五、文件变更总表

| 文件 | 操作 | 优先级 |
|------|------|--------|
| `internal/kernel/skills_guard.go` | 新建 | P0 |
| `internal/tools/skill_manage.go` | 新建 | P0 |
| `internal/kernel/skill_store.go` | 修改 | P0 |
| `internal/kernel/background_review.go` | 新建 | P0 |
| `internal/kernel/agent.go` | 修改 | P0 |
| `internal/tools/skill_loader.go` | 修改 | P1 |
| `internal/app/tools.go` | 修改 | P1 |
| `internal/kernel/reflection.go` | 删除/废弃 | - |

---

## 六、Phase 划分建议

```
Phase 1（基础）：skill_manage + skills_guard
  → 先把 skill 的 CRUD + 安全扫描跑通，不影响现有流程

Phase 2（核心）：background_review_engine
  → 后台 Agent + 3 种 prompt + callback 通知

Phase 3（集成）：agent.go 触发逻辑 + skill_loader 目录支持
  → 把 nudge counter + 异步触发接上，让整个流程跑起来

Phase 4（完善）：callback TUI 通知 + 配置项整理
  → 用户体验优化
```
