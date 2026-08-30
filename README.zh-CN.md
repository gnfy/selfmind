# SelfMind 中文说明

[English README](README.md) · [文档索引](docs/README.md) ·
[当前状态](docs/STATUS.md)

SelfMind 是一个面向 Linux、macOS 和 WSL 的常驻个人 AI 工作 Agent。同一个人可以
从本地 TUI、CLI、IM、cron 和 HTTP 工作，同时保持任务、运行、审批、记忆、
workspace 和 handoff 连续；原始聊天记录仍留在产生它的渠道中。

SelfMind 目前处于 beta。当前实现和 Phase 1 剩余工作以
[`docs/STATUS.md`](docs/STATUS.md) 为准，README 不再复制一份容易过期的功能清单。

## 安装

要求：

- npm 发布版需要 Node.js 18 或更高版本。
- 支持 Linux x64/arm64、macOS x64/arm64 和 WSL；不支持原生 Windows。
- 只有源码构建才需要 Go 1.26 或更高版本。

```sh
npm install --global @selfmind/cli@latest
selfmind
```

npm 包通过一个很小的 Node.js 启动器安装当前平台对应的 Go 原生二进制。预发布版
使用 `@selfmind/cli@next`。

首次交互式启动时，如果缺少 Model Readiness，会打开唯一的 Model Manager。在
这里配置并验证可见的 **Main** 和 **Background** 路由。建立 Model Readiness 后，
下一次适用的启动会继续 runtime 设置：项目 workspace、信任边界、审批模式和常驻
后台服务。某一步失败后可以继续，不会重复已经完成的模型工作。

常用恢复命令：

```sh
selfmind model             # 打开 Model Manager
selfmind setup             # 修复 runtime/首次使用设置
selfmind doctor            # 当前问题和明确的下一步
selfmind doctor --verbose  # 完整脱敏诊断
```

## 日常使用

运行 `selfmind` 打开本地 TUI，也可以不打开界面直接发送任务：

```sh
selfmind send "检查这个仓库并运行针对性测试"
selfmind status
selfmind tasks
selfmind resume <task-id>
selfmind approvals
```

TUI 是 CLI、IM 和 HTTP 共用 gateway 的客户端，不会启动第二套进程内 Agent
runtime。`/status`、`/tasks`、`/workspace`、`/resume`、`/approve`、`/stop`
等显式控制命令不调用模型。

### 输入框操作

| 输入 | 行为 |
| --- | --- |
| `Enter` | 提交当前消息。 |
| `Ctrl+J` | 插入可靠的换行；传统终端无法稳定区分 `Shift+Enter` 与 `Enter`。 |
| `Ctrl+V` | 从 GUI 剪贴板附加图片。 |
| `/paste-image` | 显式触发图片剪贴板读取。 |
| `Up` / `Down` | 调出当前用户的历史输入。 |

图片附加后，输入框会显示实时图片数量和紧凑的 `[Image #N · name]` token。提交前
删除完整 token 就会同时移除附件。macOS 的 `Cmd+V` 由终端应用处理，仍用于普通
文本粘贴。原生 Linux 的图片读取需要 `wl-paste` 或 `xclip`；SSH 会话没有本机
GUI 剪贴板。

TUI 默认跟随终端能力选择颜色，也可以固定明暗对比或关闭颜色，并且不会绘制
全屏背景：

```yaml
tui:
  theme: "auto" # auto | dark | light | mono
```

完整命令与交互说明见[命令参考](docs/command-reference.zh-CN.md)，渲染契约见
[Terminal-First Hybrid TUI](docs/tui-terminal-first-hybrid.md)。

## 模型与 Provider

正常的 provider、凭据和模型路由变更都使用 `selfmind model`。Model Manager 会
验证完整选择，并作为一笔 daemon 事务应用。Provider API key 会先暂存，再保存到
SelfMind 的私有 auth store；生成的 YAML 只包含非密钥连接和路由数据。

内置 provider 直接使用稳定 ID。用户自定义连接统一放在 map 形态的
`providers.custom` 下；map key 同时就是模型路由中的 provider ID：

```yaml
providers:
  custom:
    company-gateway:
      base_url: "https://ai.example.com/v1"
      protocol: "openai-compatible" # anthropic-compatible | responses-compatible
      auth: "bearer"                # x-api-key | none

models:
  primary:                           # 用户界面的 Main 路由
    provider: "company-gateway"
    model: "company-coder"
  auxiliary:                         # 用户界面的 Background 路由
    enabled: true
    provider: "deepseek"
    model: "deepseek-v4-flash"
```

旧 `provider_profiles` 块和 `custom:<id>` 路由写法只作为兼容读入。使用下面的
命令迁移：

```sh
selfmind config upgrade
```

用户配置见[配置参考](docs/config-reference.zh-CN.md)；修改 provider 代码前先读
[Provider Runtime 规范](docs/provider-runtime.zh-CN.md)。

## 常驻 Gateway

常驻后台使用 macOS 当前用户的 launchd 服务，或 Linux 当前用户的 systemd 服务，
不需要管理员权限。

```sh
selfmind gateway service status
selfmind gateway restart --drain
selfmind gateway service uninstall
```

`restart` 默认等活跃 turn 到达安全边界。TUI、CLI、IM adapter、cron 和 HTTP 都
汇聚到这个 daemon 以及它管理的 `control.db` 状态。

## 微信

主要微信路径使用内置 Weixin/iLink 扫码登录：

```sh
selfmind weixin login --timeout 8m
selfmind gateway restart --drain  # 仅首次启用 adapter 时需要
selfmind weixin status
```

默认情况下，登录会把扫码发送者绑定到当前 CLI 用户、把私聊切换到 allowlist，
并共享任务、记忆、workspace、审批和续聊状态。如果 iLink 会话以后过期，只需再次
运行 `selfmind weixin login`；运行中的 gateway 会加载刷新后的凭据，不需要重启。
详见[命令参考](docs/command-reference.zh-CN.md)和
[微信真机检查清单](docs/weixin-live-test.md)。

## 更新、卸载与反馈

```sh
selfmind update check
selfmind update

selfmind feedback "描述遇到的问题"
gh auth login --hostname github.com
selfmind feedback --send "描述遇到的问题"

selfmind uninstall --prepare
npm uninstall --global @selfmind/cli
```

反馈默认只写入本机并脱敏，只有显式使用 `--send` 才会提交。卸载时如果还要删除
本地配置、任务、记忆和凭据，请先运行
`selfmind uninstall --prepare --purge-data --yes`，再卸载 npm 包。

## 开发

```sh
git clone https://github.com/gnfy/selfmind.git
cd selfmind
GOWORK=off go build -o selfmind ./cmd/selfmind
GOWORK=off go test ./...
./selfmind selfcheck --fast
./selfmind selfcheck
```

修改子系统前先阅读 [`AGENTS.md`](AGENTS.md)、[`docs/STATUS.md`](docs/STATUS.md)
以及仓库指南中对应的 domain 文档。`selfmind selfcheck` 是完整本地发布门禁，并且
始终包含文档契约。

关键资料：

- [身份与跨端连续性](docs/identity-continuity.md)
- [工具安全与审批](docs/tool-safety.md)
- [Provider Runtime](docs/provider-runtime.zh-CN.md)
- [TUI 渲染](docs/tui-terminal-first-hybrid.md)
- [npm 分发](docs/npm-distribution.md)
