# npm Distribution And Upgrade Lifecycle

This document defines the supported installation, release, update, uninstall,
and feedback lifecycle for SelfMind's open-source CLI.

## Supported Targets

The official npm packages support:

- Linux x64
- Linux arm64
- WSL running one of those Linux architectures

Native Windows and macOS are intentionally rejected by the launcher. They may
remain development environments, but they are not release targets.

Node.js 18 or newer is required only for the launcher. SelfMind itself remains
a single statically linked Go binary.

## Package Topology

SelfMind uses one launcher package and one optional native package per target:

```text
selfmind
  bin/selfmind.js
  optionalDependencies:
    selfmind-linux-x64
    selfmind-linux-arm64

selfmind-linux-x64
  vendor/x86_64-unknown-linux-gnu/bin/selfmind

selfmind-linux-arm64
  vendor/aarch64-unknown-linux-gnu/bin/selfmind
```

npm installs only the optional dependency matching the current platform. The
launcher resolves that dependency, inherits stdio, forwards signals, and exits
with the native process status.

The gateway must be started through the stable `selfmind` launcher path. Never
register a service against a versioned path inside `node_modules`.

## User Lifecycle

Install and configure:

```sh
npm install --global selfmind@latest
selfmind setup
selfmind doctor
selfmind
```

`selfmind setup` follows these rules:

- create a missing config;
- interactively configure a missing model;
- preserve an existing config byte for byte when no change is needed;
- start or reuse the local daemon;
- never replace credentials or unknown config keys.

Check and install an update:

```sh
selfmind update check
npm install --global selfmind@latest
selfmind gateway restart --drain
```

The update checker is advisory. It caches npm dist-tags and never mutates the
binary. `gateway restart --drain` waits for a safe turn boundary so an upgrade
does not silently interrupt active work. Restart already drains by default;
the flag makes that upgrade intent explicit.

Uninstall while preserving local data:

```sh
selfmind uninstall --prepare
npm uninstall --global selfmind
```

Data deletion is deliberately separate and explicit:

```sh
selfmind uninstall --prepare --purge-data --yes
npm uninstall --global selfmind
```

## Update And Compatibility Rules

The binary version and npm package version come from the same git tag.
Release builds inject:

- `internal/buildinfo.Version`
- `internal/buildinfo.Commit`
- `internal/buildinfo.BuiltAt`

The CLI compares its build fingerprint with the daemon fingerprint. A mismatch
must be visible and actionable instead of being treated as a healthy upgrade.

Database migrations are forward-only. A release must:

1. migrate an older supported database on startup;
2. remain restart-safe if migration was interrupted;
3. fail clearly when an older binary sees a newer unsupported schema;
4. never silently recreate or discard user data.

## Release Workflow

Tags and manual workflow dispatches use `.github/workflows/release.yml`:

1. run the full Go test suite;
2. build static linux/amd64 and linux/arm64 binaries;
3. stage npm packages with `scripts/stage-npm-packages.mjs`;
4. pack all npm packages;
5. install the packed launcher and native package in a clean directory;
6. run `selfmind --version` through the npm launcher;
7. publish native packages first;
8. publish the launcher package last;
9. attach native archives, npm tarballs, and checksums to the GitHub release.

All three npm package names require trusted-publisher/OIDC configuration:

- `selfmind`
- `selfmind-linux-x64`
- `selfmind-linux-arm64`

Trusted publishing requires Node.js 22.14 or newer and npm CLI 11.5.1 or
newer. The release workflow uses Node.js 24 and pins a compatible npm CLI.

An npm package must exist before a trusted publisher can be attached to it.
Bootstrap each new platform package exactly once with an owner-authenticated
manual publish, then configure `release.yml` as its GitHub Actions trusted
publisher and remove any temporary automation token. The existing `selfmind`
package only needs its trusted-publisher setting updated.

Publishing the launcher last prevents users from receiving a version whose
native dependency is not available yet.

Release channels:

- stable tags publish to npm `latest`;
- prerelease tags publish to npm `next`;
- prereleases should soak on `next` before promotion.

If a native package publish fails, do not publish the launcher. If the launcher
has already been published with a defect, move the dist-tag to the last healthy
version and publish a corrected patch; do not overwrite an existing npm
version.

Run the same package-and-install smoke test locally before a release:

```sh
scripts/smoke-npm-packages.sh 0.1.0-beta.1
```

## Update Checks

The update checker reads npm registry dist-tags and caches them under
`~/.selfmind/update.json`. It:

- never blocks TUI startup on the network;
- respects `updates.enabled`, `updates.channel`, and
  `updates.check_interval`;
- skips development versions;
- shows a concise startup notice only when a newer version exists.

## Feedback And Crash Privacy

`selfmind feedback` creates a private local report by default. Submission
requires an explicit `--send` action. The default submission path creates a
GitHub Issue in `gnfy/selfmind` through the authenticated `gh` CLI; SelfMind
does not store a GitHub token. An explicit `feedback.endpoint` remains
available for self-hosted collectors.

Default reports contain:

- SelfMind build metadata;
- OS and architecture;
- a redacted user description;
- bounded, non-content diagnostics.

Prompts, assistant output, tool output, credentials, and crash files are not
uploaded by default. A crash attachment requires `--include-crash`.
Crash content remains local even when the public GitHub Issue path is used.

Top-level panics are saved under `~/.selfmind/crashes/` with private
permissions. The next startup displays one notice and lets the user decide
whether to attach the report.

The feedback-to-quality loop is:

```text
real usage -> explicit feedback -> redacted evidence -> eval case -> fix ->
regression gate -> release
```

Any reproducible message-path defect should become an `evalcases/**/*.yaml`
case in the same change that fixes it.
