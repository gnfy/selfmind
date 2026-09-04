# 编程 Agent 基础契约

SelfMind 是 agent-first 的工作助手。编程能力必须兼容不同语言、仓库、
Provider、CLI 会话和 IM 通道，不能依赖 Go 或运维关键词特判。

## 项目识别

每个 run 开始时，SelfMind 会从 workspace 构建一个有界、只读的
`ProjectProfile`：

- 检查根项目，或最多八个直接子项目；
- 只读取 manifest 和 lockfile，不在组装上下文时执行项目代码；
- 只根据已发现的证据给出候选验证命令；
- 候选命令不是强制命令，agent 执行前仍需读取对应 manifest 和项目约束。

首版支持 Go、Rust、Swift、Node.js、Python、PHP、JVM、Ruby、CMake
和 .NET。新增生态应扩展 `internal/kernel/project_profile.go`，不能在
gateway 路由或主 agent loop 中增加语言特判。

## 编程循环契约

普通编程任务应遵循：

1. 确认当前 workspace，并遵守适用的 `AGENTS.md`；
2. 修改前先检查最小必要的项目范围；
3. 只有真正多步骤或不确定的任务才使用 plan；
4. 遵循仓库现有约定，控制修改范围；
5. 最终修改后运行最小且相关的验证；
6. 把工具错误当作证据，分析环境并调整，不盲目更换命令重复尝试；
7. 汇报修改内容、验证证据和剩余风险。

运行时会在提示词之外执行同一套恢复纪律。工具尝试会关联当前持久 plan step、
目标、策略和环境。诊断证据产生后允许一次输入有变化的修正；完全相同的重试、
同策略的第三个表面变体，或未知 effect 后再次变更状态，都会在分发前被拒绝。
此时 Agent 必须先观察当前状态、选择真正不同的策略，或以可执行的阻塞说明结束。
只有当用户、仓库约束或变更本身确实要求可执行验证时，plan step 才设置
`verification_required`；可选验证仍可汇报，但不会让纯检查任务无法完成。
plan 指引同样由证据驱动：当一个 Run 已完成若干次互不相同且成功的非只读工具动作，
却仍没有持久 plan 时，下一次模型调用会把可选 plan 指引升级为必需措辞。该升级只读取
Run 自身的工具证据，绝不解析请求文本；它仍然只是指引——不会伪造 plan，也不会阻止完成。

新 Run 会启用版本化恢复契约。若 daemon 或 Provider 中断且尚未产生外部 effect，
系统可以幂等地排入一个精确指向 parent Run 的 child Run；若已分发 effect 但结果不确定，
则进入 verification-only 恢复：只暴露可信的只读工具，并在分发边界拒绝 mutation。
审批、澄清和 external watch 仍由各自专用恢复路径负责；历史 Run 不会因为二进制升级而
自动获得执行能力。尚未开始的自动恢复优先级低于用户新提交的前台工作。

长时间外部等待使用冻结的持久 observation 契约，而不是由模型反复轮询。成功预检会
记录命令哈希、环境 generation、类型化 observation adapter、目标、截止时间和能力。
adapter 只返回 `pending`、`succeeded` 或 `failed`，Provider 语法留在 tools 层。
同一 Run 内的 `all`/`any` 等待组只产生一个聚合结论，并且最多触发一个 finalization Run。

完成状态必须由证据决定：

- `completed`：工作完成且相关验证通过；若没有可执行验证，需要明确说明；
- `verification_partial`：实现已完成，但部分验证无法执行或没有结束；
- `waiting_user`：需要审批、凭据、外部依赖或用户决策；
- `blocked`：确认存在外部阻塞，无法继续取得有效进展。

仅写入文件或工具返回了输出，都不能单独作为完成依据。文件变更、命令结果、
artifact 和 run event 才是证据面。

## 平台边界

Linux 是执行能力最完整的平台，terminal 可以使用配置的隔离沙箱，并在需要时
通过审批升级权限。

macOS 支持 CLI、daemon、Provider、workspace 工具和 LaunchAgent 生命周期，
但不具备 Linux 隔离沙箱：

- `sandbox.mode: auto` 降级为受审批控制的宿主机执行；
- `sandbox.mode: isolated` 或 `sandbox.required: true` 会 fail closed；
- `/diag` 和 `selfmind doctor` 必须明确展示这个边界。

原生 Windows 不是正式执行目标，Windows 用户应使用 WSL。路径和 shell 行为
必须由运行平台决定，不能根据用户提示词猜测。

## Operator 提示词工作区

深度用户无需增加配置键，就能调整静态提示词层。当前配置文件同级目录是唯一
来源（通常为 `~/.selfmind/prompts/`）：

- `agent.md` 管理前台人格和表现偏好。仓库中的 `AGENTS.md`/`.selfmind.md`
  仍是较低信任级别的项目上下文，不能替代 operator 层。替换或关闭 Progress
  Updates 会改变 transcript 形状，因为每条工具调用前的 preamble 都会持久化为
  独立 assistant 消息。每个主前台 turn（包括不暴露工具的纯直答）都会保留锁定的
  响应与交互底线，用来约束语言跟随、结论优先、有限澄清、面向终端的标题、平铺列表、
  代码与路径格式、不使用固定报告模板，以及默认不倾倒原始协议，但不对使用者职业作
  任何断言。锁定的工作质量底线同样不依赖具体能力，即使没有
  模型可见工具也会用于前台和委派工作；只有绑定 workspace 时才加入工作区实现指引，
  而且只有存在命令能力时才要求命令验证。生命周期指令由当前 turn 实际下发的工具
  定义生成，因此只读 finalizer 或受限角色不会被要求调用它没有的计划、终端、Skill
  或结束工具；
- 委派 Agent 只从同一冻结快照继承适用的工作风格、验证和语义条件式用户界面指引。
  系统不通过关键词分类器判断是否适用；提示词自身声明适用边界，并要求模型在其他
  任务中忽略它。委派 Agent 保留面向父 Agent 的专用身份，不继承 Persona、
  Progress Updates 或 Persistent Learning。
  委派上下文会保留父任务的取消、workspace/run 权限、artifact 和事件证据，但使用
  全新的策略与延迟工具激活状态。父任务拥有的计划、结束、watch、memory 与 Skill
  变更工具不会下放；结果以 evidence/files/tests/blockers 的结构化交接返回；
- `background/memory_extract.md`、`background/background_review.md`、
  `background/skill_curator.md`、`background/summarizer.md` 与
  `background/semantic_recall.md` 只影响对应角色/任务。后台复盘不会继承前台
  `agent.md`、通用任务策略或仓库指令文件。模型只看到 memory 与 session 证据工具；
  仅供验证器使用的 Skill 读取工具仍留在内部。对话快照按不可信数据围栏；
- 新生成模板以独立成行且格式精确的 `selfmind:section`/`selfmind:end` 标记作为兼容
  边界，因此标记章节内的 Markdown 标题只是普通自定义内容，Markdown 代码围栏
  内的标记示例也仍是正文。首版生成的无标记文件继续兼容，但其中固定二级标题仍
  保留为章节边界；使用 `selfmind prompt edit` 打开时会迁移到新语法，并在旁边保留
  带时间戳的原文件备份。`default` 继承代码内置指引；只有明确可替换的
  表现层章节接受 `off`。自定义质量指引追加在锁定的 schema、治理、工具和安全
  契约之后，不能删除这些底线。

daemon 在启动时加载并校验一份不可变快照，turn 执行期间不读取提示词文件。有效的
活跃快照会成为该 prompt root 明确的最近一次有效 revision。活跃工作区无效时，
daemon 会继续开放 CLI、IM、cron 和 HTTP 端点：优先使用同一 root 的最近有效快照，
若不存在匹配的激活记录则使用内置默认。它会写入持久、脱敏的降级状态事件和日志
告警，`selfmind doctor` 会报告无效文件与实际选择的来源。校验本身不会放宽：prompt
root、嵌套目录或文件上的不安全权限与符号链接，以及错误标记和未知章节仍会被拒绝。
`prompt validate` 与 `prompt apply`
仍然严格；活跃工作区无效时，显式执行 `gateway restart` 也不会先停掉健康运行中的
daemon。

快照 identity 只覆盖解析后的章节值，不受注释或文件排版影响。持久维护 payload
会固定该哈希和本地内容寻址 revision，因此重启后的重试不会改变模型契约。daemon
会用已验证的当前快照修复其损坏缓存，并尽力持久化内置回退 revision，但不会把它
提升为最近一次有效快照。历史 revision 缺失时，相关工作进入
`blocked_prompt_revision`，不会误报 provider 故障或静默换用新版本。恢复历史
revision 后，`selfmind maintenance replay` 会显式把相关最新 generation 作业送回
队列。revision 只包含静态资产，不包含任务数据、memory、项目上下文或凭据。

上下文压缩使用锁定的 summarizer system 契约，并把待压缩状态放在独立的数据围栏
消息中。交接必须保留活跃目标和 plan/work unit、验证状态、不能重复的失败尝试、
审批或外部等待状态、精确标识符，以及相关文件路径。一个有界的结构化路径后备会
补齐模型遗漏，并在超过安全上限时显式报告。operator 的摘要指引可以调整侧重点和
语言，但不能替换这些恢复执行所必需的章节。

Eval 无视 operator 工作区，强制使用嵌入默认值和空 identity。提示词加载、长度
限制、插入位置、revision 固定和默认字节稳定性由机制型 Go 测试证明，不依赖
provider replay cassette。

## 回归门禁

编程循环的行为变更必须同时具备：

- 所属 package 的聚焦 Go 测试；
- 用户可见 message-path 变化对应的 coding eval；
- Linux 上运行 `go test ./...`；
- CI 中的 macOS build/test；
- 所有发布平台 npm package 的 smoke test。

录制后的 eval cassette 才是确定性的发布证据，单次 live provider 成功不能替代
发布门禁。
