# SelfMind

SelfMind is a long-running personal AI work agent for Linux, macOS, and WSL. The npm
package installs the matching native Go binary; Node.js is only used as a small
launcher.

## Install

```sh
npm install --global @selfmind/cli@latest
selfmind setup
selfmind doctor
selfmind
```

Supported release targets:

- Linux x64
- Linux arm64
- WSL on x64 or arm64 Windows hosts

Native Windows and macOS are not currently supported release targets.

## Update

```sh
selfmind update check
npm install --global @selfmind/cli@latest
selfmind gateway restart --drain
```

Use `selfmind@next` for prerelease builds.

## Uninstall

Stop the daemon and preserve configuration, tasks, memory, and credentials:

```sh
selfmind uninstall --prepare
npm uninstall --global @selfmind/cli
```

To remove local data too, explicitly run:

```sh
selfmind uninstall --prepare --purge-data --yes
npm uninstall --global @selfmind/cli
```

## Feedback

Feedback reports are written locally by default and redact common secrets:

```sh
selfmind feedback "Describe what went wrong"
```

No prompt, tool output, crash report, or diagnostic bundle is uploaded unless
you explicitly request it.

Full documentation: <https://github.com/gnfy/selfmind>
