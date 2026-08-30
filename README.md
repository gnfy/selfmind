# SelfMind

[中文说明](README.zh-CN.md) · [Documentation index](docs/README.md) ·
[Current status](docs/STATUS.md)

SelfMind is an always-on personal AI work agent for Linux, macOS, and WSL. A
single identity can work through the local TUI, CLI, IM, cron, and HTTP while
tasks, runs, approvals, memory, workspaces, and handoffs remain continuous.
Raw chat transcripts stay local to the channel that created them.

SelfMind is in beta. The implementation snapshot and remaining Phase-1 work
live in [`docs/STATUS.md`](docs/STATUS.md); that file is the source of truth
instead of a duplicated feature inventory here.

## Install

Requirements:

- Node.js 18 or newer for the npm release.
- Linux x64/arm64, macOS x64/arm64, or WSL. Native Windows is not supported.
- Go 1.26 or newer only when building from source.

```sh
npm install --global @selfmind/cli@latest
selfmind
```

The npm package installs the matching native Go binary behind a small Node.js
launcher. For prereleases, install `@selfmind/cli@next`.

On the first interactive launch, missing Model Readiness opens the sole Model
Manager. Configure and validate the visible **Main** and **Background** routes
there. After Model Readiness is established, the next applicable launch resumes
runtime setup: project workspace, trust boundary, approval mode, and managed
background operation. A failed stage resumes without repeating completed model
work.

Useful recovery commands:

```sh
selfmind model             # open the Model Manager
selfmind setup             # repair runtime/first-use setup
selfmind doctor            # current problems and concrete next actions
selfmind doctor --verbose  # full redacted diagnostics
```

## Daily use

Run `selfmind` for the local TUI, or send work without opening it:

```sh
selfmind send "Inspect this repository and run its focused tests"
selfmind status
selfmind tasks
selfmind resume <task-id>
selfmind approvals
```

The TUI is a client of the same gateway used by IM and HTTP; it does not start a
second in-process agent runtime. Explicit controls such as `/status`, `/tasks`,
`/workspace`, `/resume`, `/approve`, and `/stop` are model-free.

### Composer controls

| Input | Action |
| --- | --- |
| `Enter` | Submit the current message. |
| `Ctrl+J` | Insert a portable newline. Terminals cannot reliably distinguish `Shift+Enter` from `Enter`. |
| `Ctrl+V` | Attach an image from the GUI clipboard. |
| `/paste-image` | Explicit image-clipboard fallback. |
| `Up` / `Down` | Recall person-local input history. |

After an image is attached, the composer shows its live image count and a compact
`[Image #N · name]` token. Delete the complete token before submitting to detach
the image. On macOS, `Cmd+V` belongs to the terminal application and remains its
normal text-paste action. Native Linux image capture needs `wl-paste` or `xclip`;
SSH sessions have no local GUI clipboard.

TUI colors follow terminal capabilities by default. Choose a fixed contrast
palette or no color without painting a full-screen background:

```yaml
tui:
  theme: "auto" # auto | dark | light | mono
```

The complete command and interaction reference is
[`docs/command-reference.md`](docs/command-reference.md). The rendering contract
is [`docs/tui-terminal-first-hybrid.md`](docs/tui-terminal-first-hybrid.md).

## Models and providers

Use `selfmind model` for normal provider, credential, and route changes. The
Model Manager validates completed selections and applies them as one daemon-owned
transaction. Provider API keys are staged and stored in SelfMind's private auth
store; generated YAML contains only non-secret connection and route data.

Built-in providers are referenced directly by stable ID. A user-defined
connection lives under the single map-shaped `providers.custom` namespace, and
its map key is also the route's provider ID:

```yaml
providers:
  custom:
    company-gateway:
      base_url: "https://ai.example.com/v1"
      protocol: "openai-compatible" # anthropic-compatible | responses-compatible
      auth: "bearer"                # x-api-key | none

models:
  primary:                           # user-facing Main route
    provider: "company-gateway"
    model: "company-coder"
  auxiliary:                         # user-facing Background route
    enabled: true
    provider: "deepseek"
    model: "deepseek-v4-flash"
```

The old `provider_profiles` block and `custom:<id>` route syntax are compatibility
reads only. Migrate them with:

```sh
selfmind config upgrade
```

See the [configuration reference](docs/config-reference.md) for user settings
and the [provider runtime contract](docs/provider-runtime.md) before changing
provider code.

## Background gateway

Managed background operation uses a per-user launchd service on macOS and a
per-user systemd service on Linux. It does not need administrator access.

```sh
selfmind gateway service status
selfmind gateway restart --drain
selfmind gateway service uninstall
```

`restart` drains to a safe turn boundary by default. The TUI, CLI, IM adapters,
cron, and HTTP all converge on this daemon and its `control.db` state.

## WeChat

The primary WeChat path uses the built-in Weixin/iLink QR login:

```sh
selfmind weixin login --timeout 8m
selfmind gateway restart --drain  # first adapter enable only
selfmind weixin status
```

By default, login binds the scanned sender to the current CLI person, switches
direct messages to an allowlist, and shares tasks, memory, workspaces,
approvals, and continuation state. If the iLink session later expires, run
`selfmind weixin login` again; the running gateway reloads refreshed credentials
without a restart. See the [command reference](docs/command-reference.md) and
[live test checklist](docs/weixin-live-test.md).

## Update, uninstall, and feedback

```sh
selfmind update check
selfmind update

selfmind feedback "Describe what went wrong"
gh auth login --hostname github.com
selfmind feedback --send "Describe what went wrong"

selfmind uninstall --prepare
npm uninstall --global @selfmind/cli
```

Feedback is written locally and redacted by default. Nothing is submitted until
`--send` is explicit. To delete local configuration, tasks, memory, and
credentials during uninstall, use `selfmind uninstall --prepare --purge-data
--yes` before removing the npm package.

## Develop

```sh
git clone https://github.com/gnfy/selfmind.git
cd selfmind
GOWORK=off go build -o selfmind ./cmd/selfmind
GOWORK=off go test ./...
./selfmind selfcheck --fast
./selfmind selfcheck
```

Before changing a subsystem, read [`AGENTS.md`](AGENTS.md),
[`docs/STATUS.md`](docs/STATUS.md), and the domain document linked from the
repository guide. `selfmind selfcheck` is the full local release gate and always
includes the documentation contract.

Key references:

- [Identity and continuity](docs/identity-continuity.md)
- [Tool safety and approvals](docs/tool-safety.md)
- [Provider runtime](docs/provider-runtime.md)
- [TUI rendering](docs/tui-terminal-first-hybrid.md)
- [npm distribution](docs/npm-distribution.md)
