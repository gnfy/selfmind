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

## 回归门禁

编程循环的行为变更必须同时具备：

- 所属 package 的聚焦 Go 测试；
- 用户可见 message-path 变化对应的 coding eval；
- Linux 上运行 `go test ./...`；
- CI 中的 macOS build/test；
- 所有发布平台 npm package 的 smoke test。

录制后的 eval cassette 才是确定性的发布证据，单次 live provider 成功不能替代
发布门禁。
