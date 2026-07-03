# SelfMind 架构约束

这份文档是给开发者和后续 AI 编程工具看的。它的目标不是描述所有实现细节，而是规定 SelfMind 继续迭代时必须保持的边界，避免代码重新变成大文件、大 switch、全局状态和临时 UI 到处散落。

## 总原则

- 保持 `selfmind` 单 binary；守护进程用 `selfmind gateway run` 启动，不要新增独立的 daemon 入口 binary。
- Gateway、TUI、IM、Webhook 可以共享 task/run/workspace/memory/skill 状态，但聊天 transcript 必须保持渠道隔离。
- 新功能优先复用现有层次：`cliapp` 管命令入口，`gateway/httpapi` 管 HTTP，`gateway/cli` 管 TUI 编排，`ui/components` 管可复用界面组件，`app` 管依赖组装，`kernel` 管 agent loop 和模型调用。
- 任何改动都要先问：它属于产品入口、应用组装、agent 核心、工具、gateway、TUI 组件，还是配置/存储平台层。不要把业务逻辑塞进离它最近的大文件。

## 后续 AI 必须遵守

- 不要继续扩大 `internal/gateway/cli/controller.go`。Controller 只负责状态编排、消息分发和组件连接。
- 不要把新的全屏帮助页、详情页、列表页直接写进 Controller。临时页面必须使用 `internal/ui/components/Pager` 或新增同类可复用 surface 组件。
- 不要把 slash command 的提示、帮助文本、补全和执行逻辑分散在多个文件里。新增命令时应优先沉淀成统一 command registry，再由 help、editor hint 和 dispatch 共同读取。
- 不要让 `internal/ui/components/editor.go` 知道完整业务命令。Editor 只能展示外部传入的提示或轻量输入状态。
- 不要新增跨租户、跨测试共享的全局 mutable 状态。必须全局存在的对象要有清晰生命周期，并优先由 `app` 或 gateway runner 注入。
- 不要让 `kernel` 依赖 `gateway`、`server` 或具体工具实现。Agent 通过抽象 backend 调用工具。
- 文件、终端、patch、进程类工具必须继续经过 workspace scope 检查。
- 不要自动把 CLI 聊天同步到 IM，也不要自动把 IM 聊天同步到 CLI。跨渠道共享的是任务状态和事件，不是聊天原文。

## TUI 约束

- Transcript 渲染、composer、状态栏、临时页面要拆成组件，Controller 不直接管理每个视觉细节。
- `/help` 这类临时信息页不进入聊天历史，不占用主 transcript，退出后恢复原上下文。
- 多行输入框必须是正常文档流的一部分，不能浮动遮挡历史消息。
- 输入框高度必须有上限；超过上限后在输入框内部滚动。
- 所有颜色、间距、选中态优先从 `internal/ui/common` 的 style token 读取。

## Gateway 和渠道约束

- Gateway 控制命令，例如 status、tasks、stop、workspace、resume，应该尽量 model-free，不消耗模型 token。
- IM adapter 只负责验签、解析、格式化发送和平台协议差异，不拥有 identity、workspace、task/run 调度逻辑。
- 审批状态归 `control.approval_requests` 和 gateway 控制/API handler 管理；IM adapter 可以渲染按钮或解析回调，但不能拥有审批生命周期状态。
- `person_id` 表示同一个人，`account_id` 表示某个平台账号绑定。
- 同一个人的 active run guard 在 worker pool 成熟前不能移除。

## 模型和工具约束

- 模型选择走 role-based routing，例如 `coding_agent`、`memory_extract`、`background_review`、`skill_curator`、`semantic_recall`。
- Provider 发现、认证解析、实时模型列表、provider profile 覆盖都属于 `internal/modelruntime`。不要把新的厂商认证探测或模型列表拉取逻辑直接写进 `internal/app` 或 LLM adapter。
- P2 外部认证复用目前只覆盖 Codex CLI、Claude Code、Gemini CLI、Qwen CLI。其它厂商使用 API key、自定义 OpenAI-compatible endpoint 或 `provider_profiles`。
- 新 provider 适配器不要堆到单个大文件里；协议、provider、model listing、streaming 应拆分。
- 工具调用优先使用 provider 原生 tool call；文本 `[TOOL:...]` 只作为兼容 fallback。
- 只读工具可以并行，写文件、patch、终端、memory、skill mutation、进程控制和未知工具默认串行。
- Skill 处理必须保持渐进和分层：`skills_list` 只暴露元数据，`skill_view` 读取内容，`skill_manage` 负责变更，`skill_catalog` 负责安装/审计，`skill_bundle` 负责组合。
- Skill 修改应尽量热加载当前 registry；直接 slash 调用时先解析 bundle，再解析单个 skill。
- Curator 自动治理默认只能处理 agent-created skills；manual、catalog-installed、bundled 或 pinned skills 不应被自动归档。
- Catalog 安装必须保留持久化安装 provenance，记录在 `skills/.catalog/lock.json`。安装时默认拒绝同名目录和旧版 `.md` 冲突；只有显式 `--force` 才能替换，并且替换前必须把旧副本备份到 `skills/.catalog/backups/`。
- Memory 和 Skill 的用户可见 history/undo 必须走共享的 `memory` / `skill_manage` tool action，不要在 TUI 或 IM 里写重复的私有回滚逻辑。

## 文件大小红线

这些不是硬性编译规则，但后续 AI 修改时必须主动避免突破：

- 单文件超过 800 行，要优先考虑拆分。
- 单个函数超过 120 行，要优先拆成小函数或组件方法。
- 单个 switch 超过 12 个分支，要优先沉淀 registry、handler map 或策略对象。
- 新增功能如果需要同时修改三个以上不相关大文件，先补一层应用服务或组件接口。

## 状态放在哪里

本文件只放规则。实现状态和唯一的优先级列表见 `docs/STATUS.md`；产品北极星与
跨端连续性契约见 `docs/identity-continuity.md`。不要在这里添加任何按功能的
状态记录。
