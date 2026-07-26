# npm Distribution And Upgrade Lifecycle

This document defines the supported installation, release, update, uninstall,
daemon, and feedback lifecycle for SelfMind's open-source CLI.

## Supported Targets

Official npm packages support:

- Linux x64 and arm64;
- macOS x64 and Apple silicon arm64;
- WSL running either supported Linux architecture.

Native Windows is not a release target. Windows users should run SelfMind in
WSL. Node.js 18 or newer is required only for the launcher; SelfMind remains a
single Go binary.

The platform security boundary is intentionally explicit:

- Linux prefers the bubblewrap isolated sandbox. `sandbox:isolated` fails
  closed when isolation is unavailable.
- macOS currently uses approval-controlled host execution. It does not claim
  Linux-equivalent filesystem or network isolation. A strict isolated request
  fails closed instead of silently running on the host.

## Package Topology

SelfMind uses one launcher package and one optional native package per target:

```text
@selfmind/cli
  bin/selfmind.js
  optionalDependencies:
    @selfmind/cli-linux-x64
    @selfmind/cli-linux-arm64
    @selfmind/cli-darwin-x64
    @selfmind/cli-darwin-arm64

@selfmind/cli-linux-x64
  vendor/x86_64-unknown-linux-gnu/bin/selfmind

@selfmind/cli-linux-arm64
  vendor/aarch64-unknown-linux-gnu/bin/selfmind

@selfmind/cli-darwin-x64
  vendor/x86_64-apple-darwin/bin/selfmind

@selfmind/cli-darwin-arm64
  vendor/aarch64-apple-darwin/bin/selfmind
```

npm installs only the optional dependency matching the current platform. The
launcher resolves it, inherits stdio, forwards signals, and exits with the
native process status.

The launcher exports its stable Node executable and script paths to SelfMind.
On macOS, the launchd service uses those paths instead of a versioned native
package directory. This keeps service startup stable across npm upgrades.

## User Lifecycle

Install and configure:

```sh
npm install --global @selfmind/cli@latest
selfmind
```

The first interactive `selfmind` launch opens guided model setup when no
default model is ready. It does not start the daemon until setup succeeds.
`selfmind setup` remains available for explicit reconfiguration, while
`selfmind doctor` reports detailed configuration and runtime diagnostics.

`selfmind setup`:

- creates missing configuration;
- interactively configures a missing model;
- preserves existing credentials, values, and unknown keys;
- starts or reuses the local daemon;
- installs and starts a per-user launchd service on macOS.

Manage the macOS service explicitly:

```sh
selfmind gateway service status
selfmind gateway service install
selfmind gateway service uninstall
```

The LaunchAgent is stored at
`~/Library/LaunchAgents/com.selfmind.gateway.plist`; logs are written under
`~/.selfmind/`. It is a user service and requires no root privileges.
Ordinary `selfmind gateway start` reuses an already running service and does
not replace an active daemon. Use `selfmind gateway restart --drain` when an
upgrade must move the service to a new binary.

Check and install an update:

```sh
selfmind update check
npm install --global @selfmind/cli@latest
selfmind gateway restart --drain
```

The update checker is advisory and never replaces the binary. Restart drains
the active turn before the process exits. On macOS, launchd observes the clean
exit and starts the newly installed version. The CLI verifies the daemon build
fingerprint so an old daemon cannot look healthy after an upgrade.

Before promoting the first stable macOS release, run this smoke test on a real
Apple Silicon Mac:

```sh
npm install --global @selfmind/cli@next
selfmind --version
selfmind setup
selfmind gateway service status
selfmind doctor
selfmind gateway restart --drain
selfmind gateway service status
```

Cross-compilation and package staging are release gates, but they do not
replace this real launchd, Keychain, terminal, filesystem, and upgrade test.

Uninstall while preserving user data:

```sh
selfmind gateway service uninstall  # macOS
selfmind uninstall --prepare
npm uninstall --global @selfmind/cli
```

Data deletion remains separate and explicit:

```sh
selfmind uninstall --prepare --purge-data --yes
npm uninstall --global @selfmind/cli
```

## Update And Compatibility Rules

The Git tag is the single version source. Release builds inject:

- `internal/buildinfo.Version`;
- `internal/buildinfo.Commit`;
- `internal/buildinfo.BuiltAt`.

The npm launcher and every native package use the same version. Database
migrations are forward-only and must be restart-safe. An older unsupported
binary must reject a newer schema clearly; it must never recreate or discard
user data.

Release channels use npm dist-tags:

- stable tags publish to `latest`;
- prerelease tags publish to `next`;
- `next` must soak before promotion.

## Release Workflow

Tags and manual dispatches use `.github/workflows/release.yml`:

1. run Go tests;
2. build Linux and macOS binaries for x64 and arm64;
3. package Linux systemd artifacts where applicable;
4. stage all five npm packages;
5. pack the launcher and native packages;
6. smoke-test the Linux launcher in CI;
7. smoke-test the macOS x64 launcher on a macOS runner;
8. publish all native packages first;
9. publish the launcher last;
10. attach archives, npm tarballs, and checksums to the GitHub release.

The following npm package names require trusted-publisher/OIDC configuration:

- `@selfmind/cli`;
- `@selfmind/cli-linux-x64`;
- `@selfmind/cli-linux-arm64`;
- `@selfmind/cli-darwin-x64`;
- `@selfmind/cli-darwin-arm64`.

Bootstrap each new platform package once with an owner-authenticated publish,
then configure the GitHub Actions workflow as its trusted publisher and remove
temporary automation credentials. Never publish the launcher when any required
native package failed.

Run the package smoke test before a release:

```sh
scripts/smoke-npm-packages.sh 0.1.0-beta.1
```

## Update Checks

The update checker reads npm registry dist-tags and caches the result under
`~/.selfmind/update.json`. It:

- never blocks TUI startup;
- respects `updates.enabled`, `updates.channel`, and `updates.check_interval`;
- skips development versions;
- displays one concise notice when a newer version exists.

## Feedback And Crash Privacy

`selfmind feedback` creates a private local report by default. Submission is
explicit:

```sh
selfmind feedback "describe what happened"
selfmind feedback --send "describe what happened"
```

The default submission creates a GitHub Issue in `gnfy/selfmind` through the
authenticated `gh` CLI. SelfMind does not store a GitHub token. If `gh` is
missing or authentication expired, the report remains local and SelfMind
prints recovery instructions plus a pre-filled manual Issue URL.

Reports contain build metadata, OS/architecture, a redacted user description,
and bounded non-content diagnostics. Prompts, assistant output, tool output,
credentials, and crash files are excluded by default. Crash attachment remains
an explicit opt-in.

The quality loop is:

```text
real usage -> explicit feedback -> redacted evidence -> eval case -> fix ->
regression gate -> release
```

Every reproducible message-path defect should add an `evalcases/**/*.yaml`
regression case in the same change.
