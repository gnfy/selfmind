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

The first interactive `selfmind` launch opens the sole Model Manager when Model
Readiness is missing. After its transaction is applied, the next launch resumes
runtime/first-use setup without reconfirming models. Linux and macOS use the
same flow, and completed stages are not repeated. Platform service names stay
out of the primary flow. `selfmind setup` re-enters runtime reconciliation,
while `selfmind doctor` reports detailed configuration and runtime diagnostics.

`selfmind setup`:

- creates missing configuration;
- directs the person to `selfmind model` if Model Readiness is missing instead
  of maintaining a second picker or probe path;
- relies on the Model Manager transaction and the daemon's actual environment
  to validate the primary, background, and optional role routes;
- stores accepted API credentials in `~/.selfmind/auth.json` with mode `0600`,
  never in a service definition;
- confirms one canonical project workspace, grants trust only through the
  authenticated local-control path, and refuses `/` or the home directory as
  implicit workspace defaults;
- confirms the approval mode and managed-background/on-demand choice in one
  second fast-path confirmation;
- preserves existing credentials, values, and unknown keys;
- starts or reuses the local daemon;
- installs and starts a per-user launchd service on macOS or per-user systemd
  service on Linux when managed background operation is selected;
- records versioned private runtime and first-use state beside `config.yaml`;
  model routes and their verified-running evidence remain solely in
  `model-state.json`.

Manage the platform user service explicitly:

```sh
selfmind gateway service status
selfmind gateway service install
selfmind gateway service uninstall
```

The macOS LaunchAgent is stored at
`~/Library/LaunchAgents/com.selfmind.gateway.plist`; the Linux user unit is
stored at `~/.config/systemd/user/selfmind-gateway.service`. Logs and runtime
state stay under the normal per-user locations. Both are user services and
require no root privileges. If a system-wide Linux `selfmind.service` is
already active, personal setup stops and reports the conflict rather than
shutting down or replacing that service.
Each managed installation carries a non-secret service generation. Setup is
ready only when the running platform job and Gateway agree on that generation,
the executable version, the resolved configuration, and a non-secret effective
model-route fingerprint. Replacement drains active work without force,
waits for the old job and runtime lock to disappear, and permits one bootstrap
retry only after absence is proven again. A bounded drain timeout defers repair
and restores foreground availability instead of interrupting the active run.
Once launchd/systemd receives startup ownership, installation only waits for
that managed process; it never races it with an on-demand detached fallback.
Ordinary `selfmind gateway start` reuses an already running service and does
not replace an active daemon. Use `selfmind gateway restart --drain` when an
upgrade must move the service to a new binary.

Check and install an update:

```sh
selfmind update check   # advisory: report only
selfmind update         # check + package-manager install + verify + drained daemon restart
```

`selfmind update` is the supported one-command path; the manual equivalent is
`npm install --global @selfmind/cli@<tag>` followed by
`selfmind gateway restart --drain`. The startup notice is advisory and never
replaces the binary on its own. Restart drains the active turn before the
process exits. The platform user service observes the clean exit and starts
the newly installed version. The CLI verifies the daemon build
fingerprint and control-schema health; an unreachable, stale, or
schema-incompatible daemon makes the update command fail instead of degrading
to a warning.

Every daemon start reads the durable schema version and rejects a database
newer than the binary. When the versions match, startup performs no schema DDL
or full-database integrity scan. When the database is older, the newly installed
daemon verifies it, creates a recoverable backup, applies the ordered migration,
verifies the result, and only then begins serving traffic. Keeping this fallback
in daemon startup also covers direct package installs, restored databases, and
manual binary replacement without charging normal cold starts for migration
work.

Running `selfmind update` also refreshes an equal-version npm release. This
restores package contents after a developer temporarily replaces the staged
binary without requiring the person to know which package manager owns the
launcher. A running build newer than the selected dist-tag is not downgraded
unless `--force` is explicit.

Before cutting a stable release, run this smoke test on real Apple Silicon
and representative systemd-user Linux hosts:

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
replace real launchd/systemd-user, terminal secret-input, filesystem, and
upgrade tests.

Uninstall while preserving user data:

```sh
selfmind gateway service uninstall
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

The npm launcher and every native package use the same version. `control.db`
migrations are forward-only, explicitly versioned, and restart-safe. A legacy
database is integrity-checked and copied with SQLite's consistent snapshot
mechanism under `<data-dir>/backups/` before its first schema transition. The
migration verifies that historical approval/run/queue/task states did not gain
new executable meaning before recording the new version. An older unsupported
binary rejects a newer schema before any write; it never recreates or discards
user data.

If a migrated installation must be recovered, stop the gateway and use the
explicit backup path reported by the failed migration:

```sh
selfmind maintenance restore-control --backup <data-dir>/backups/control-vOLD-to-vNEW-TIME.db --yes
selfmind gateway start
selfmind gateway status
```

Restore verifies the snapshot, preserves the failed database beside the active
file, and never accepts an arbitrary path outside that data directory's backup
folder. Database rollback and binary rollback are paired: an older binary must
only be started after restoring the schema snapshot it understands.

Release channels use npm dist-tags:

- stable tags publish to `latest`;
- prerelease tags publish to `next`;
- prereleases support fast feedback, but no calendar-duration soak is required;
  every stable version must independently pass the full release gate and core
  installed-package smoke before it reaches `latest`.

## Release Workflow

Tags and manual dispatches use `.github/workflows/release.yml`:

1. require an existing tag that points at the workflow SHA and is reachable
   from `main`;
2. run docs, vet, build, tests, focused race tests, and the complete offline
   release corpus;
3. build Linux and macOS binaries for x64 and arm64;
4. stage and pack all five npm packages without publishing them;
5. install the packed Linux x64 launcher and native package, then start,
   health-check, exercise authenticated `status` and `tasks` against its
   isolated `control.db`, drain-restart, recheck durable access, and stop its
   daemon in an isolated home/data directory;
6. run the same packed-package lifecycle on a native macOS arm64 runner;
7. publish all native packages first, then publish the launcher, using the
   already-smoked tarballs;
8. fail closed when only part of a version already exists; a fully existing
   version may resume only when every registry integrity matches the verified
   tarball;
9. attach archives, npm tarballs, and checksums to the GitHub release.

Release tarballs contain the binary and documentation. They do not bundle the
obsolete root/system-wide Linux installer; supported service setup is the
per-user `selfmind setup` / `selfmind gateway service install` path.

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
- respects `updates.enabled`, `updates.channel`, and `updates.check_interval`.
  The default channel `auto` follows the installed version line (a prerelease
  build checks `next`, a stable build checks `latest`), so switching lines via
  plain `npm install -g @selfmind/cli@<tag>` needs no config edit; an explicit
  `latest`/`next` pins one line;
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
