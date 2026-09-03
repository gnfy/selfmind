# TUI: Terminal-First Hybrid — Revised Plan

Status: implemented reference. Supersedes the target architecture in
`docs/tui-claude-code-alignment.md` (Phase 1 of that doc still shipped and is
partly reused — see "Carryover"). Design note, not a rule set; `AGENTS.md`
stays canonical.

## 0. Migration status (current)

- H0 spike: ✅ verified (WSL + PowerShell), spike removed.
- H1–H5: ✅ shipped (see §5).
- **hybrid is the ONLY renderer (2026-07-10).** The legacy path and `SELFMIND_TUI_LEGACY` were DELETED. (Historical note: it used to be the
  escape hatch to the old renderer (kept one cycle).
- Overlays (`/help`, `/search current`, `/model`, …): ✅ user-verified in hybrid.
- `/clear` + `ctrl+l`: clear the screen and re-show the startup card in hybrid.
- **Persistent input history (2026-07-20):** up/down-arrow composer history
  survives across sessions via `~/.selfmind/input_history.jsonl` (codex-style
  append-only JSONL). Config: top-level `history:` (`persistence: save-all|none`,
  `max_bytes`, `load_entries`). Mechanics: single async writer goroutine with a
  bounded drop-on-full queue (key path never blocks); every append/trim/read
  holds an advisory flock on a `.lock` sidecar (O_APPEND alone cannot serialize
  against trim rewrites); startup preloads the persistent prefix and in-session
  entries append after it. The file is person-local CLI history rather than a
  transcript channel: new records contain only timestamp and safe pure text,
  while the reader remains compatible with older records that also contain a
  random `channel`. Secure input, unresolved tokens, image paths, rich-paste
  payloads, and text over 4 KiB are never persisted. Current-process history
  retains complete paste/image snapshots for lossless recall, bounded together
  with the persistent prefix by `max_bytes` and 200 entries; Ctrl+C-cleared
  drafts are session-only. See `internal/gateway/cli/input_history_store.go`
  and `internal/ui/components/composer_history.go`.
- **Soft-wrap composer + codex-style key arbitration (2026-08-29):** the composer
  renders soft-wrapped long lines as multiple display rows. Its visible input
  cap is `min(6, max(2, terminalHeight/3))` and scrolls to the cursor row instead
  of showing only the cursor's wrapped segment. The
  display wrap clones the embedded bubbles textarea wrap so rows align with
  `LineInfo` cursor offsets (`internal/ui/components/editor.go`). History starts
  only from an empty composer. While the text exactly matches a recalled entry
  and the cursor is at the whole-buffer start/end, Up/Down continue browsing;
  Down past the newest entry clears the composer. Recalled history suppresses
  slash/Skill completion, while a manually typed completion owns arrows. Editing
  the recalled text or moving into its interior returns ownership to completion
  or the textarea. Esc dismisses completion only for the unchanged token.
  `Editor` owns this arbitration and returns typed outcomes to the controller.
  Completed known commands enter history canonically, unknown commands remain
  verbatim, and `/clear` is excluded.
- **Open input/history boundaries and compact attachments (2026-08-30):** a
  finalized user request and the active Composer inherit the terminal
  background and use only full-width top/bottom rules, with no side rails. The
  Composer labels ordinary, recalled, and overflowing drafts as `Message`,
  `History N/M`, and `Lines A–B/T` (combined when both apply). Large multi-line
  pastes display `[Paste #1 · 80 lines]`, long single lines display a character
  count, and images display `[Image #1 · screenshot.png]`; payload previews are
  never embedded. On terminals wide enough to keep the label readable, the top
  boundary advertises `Ctrl+J newline · Ctrl+V image`; after attachment it
  reports the live image count and changes the final action to `Ctrl+V more`.
  Hints shorten and then disappear before they can widen a narrow Composer.
  Plain Enter submits; Ctrl+J is the portable newline key because traditional
  terminal input cannot reliably distinguish Shift+Enter from Enter. The
  former `[[...]]` spelling is ordinary text. Tokens remain
  real editor text so cursor movement, wide-character wrapping, history recall,
  exact expansion, and delete-to-detach keep one contract. On a local macOS,
  Linux, or WSL session, `Ctrl+V` asks SelfMind to attach an image from the GUI
  clipboard; macOS `Cmd+V` remains the terminal application's text-paste
  shortcut and is not a reliable image signal. `/paste-image` provides the same
  explicit image action. Native Linux requires `wl-paste` or `xclip`; an SSH
  session has no local GUI clipboard, so use an image path or an IM attachment.
  Attaching an image changes only Composer state; it does not commit a success
  notice to transcript history. Deleting the complete image token before submit
  immediately clears its live count and detaches it from the outgoing request.
- **Semantic assistant results, process surface, and terminal theme (2026-08-30):** assistant Markdown is parsed as
  CommonMark/GFM and rendered as width-aware terminal blocks rather than by
  line regexes. Headings, inline emphasis/code, hanging-indent lists,
  blockquotes, fenced code, links, local file references, strikethrough, task
  lists, and tables share one semantic renderer. Tables use a grid when it fits
  and key/value records on narrow terminals. Typed
  `commentary` and `final_answer` metadata travels through the provider,
  kernel, daemon event, client, and TUI seams when available; deterministic
  tool/final boundaries cover providers that omit it. The immutable semantic
  theme in `internal/ui/theme` is resolved once at TUI startup and injected into
  renderers; components consume roles such as Primary, Secondary, Accent,
  Success, Warning, and Error rather than owning ANSI/RGB values. `tui.theme`
  accepts only `auto`, `dark`, `light`, or `mono`. `auto` follows terminal
  capability and background detection, `dark`/`light` select contrast without
  painting the terminal background, and `mono` disables color while preserving
  glyphs and font weight. `NO_COLOR` and a no-color terminal profile are a hard
  floor. Mainline prose, including action narration in every writing system,
  uses the terminal's default foreground; the `› ` marker and semantic action
  verbs share Accent. Tool evidence is nested one level below its action and
  uses a readable Secondary color rather than ANSI `Faint`. Approval uses no
  background fill. ANSI-16 terminals receive a bounded basic-color mapping;
  richer terminals receive adaptive dark/light colors. Until a
  provider phase resolves, streaming text is a
  neutral preview; completed Markdown blocks render semantically while the
  mutable tail remains literal, preventing lists and fences from changing
  shape on every token. The private `processSurface` owns this mutable stream,
  active tool correlation, grouping, and immutable commit effects. It caps the
  process frame at ten rows (or the smaller measured terminal budget), keeps
  the composer/status visible, and shares one Dot spinner at 10 FPS. Exactly
  one tick chain starts with the structured `thinking` phase and remains active
  through `model_wait` heartbeats; Plan updates refresh in place without
  creating an idle-looking gap. Tool execution, streamed output, Approval,
  completion, and idle state stop it, while elapsed text refreshes once per
  second. A waiting provider retains one measured activity row even when a Plan
  and tall Composer consume the normal process budget. A final answer
  uses a dim `• ` gutter. Raw Markdown remains the `/copy` source. Full syntax
  highlighting and resize reflow of immutable scrollback remain deferred.
- **Remaining (deferred follow-ups, not blocking default):**
  - Legacy rendering path + `SELFMIND_TUI_LEGACY`: ✅ deleted 2026-07-10
    (viewport, `controller_mouse.go`, app scroll, `renderCache`).
  - write_file overwrite real diff (needs a pre-image; tool-contract change).
  - `/search current` in-overlay search + `control.db`-backed history beyond the window.

## 1. Decision

Migrate the CLI/TUI from "app-owned full-viewport re-render" to a
**terminal-first hybrid**:

- **History → native terminal scrollback** (committed once, immutable). The
  terminal owns scroll, selection, copy, and long-history performance — the
  things most prone to bugs and slowdown in an app-owned model.
- **Active region → app-controlled** (bubbletea inline view): input, one
  spinner, the bounded process surface (streaming narration plus running tool
  evidence), transient menus, and a blocking Approval panel. Approval stays next
  to the Composer, owns keyboard input, and temporarily preempts a pager without
  hiding the active process, Plan, draft, or status context.

After guided setup, the startup identity band shows Main, Background, every
explicit role-model override, and the logical workspace without exposing
launchd/systemd details. Each displayed route includes one normal-contrast
sentence describing its responsibility; inherited roles are represented by the
Background description instead of six duplicate rows. It uses full-width open
horizontal rules with no side rails or background fill. `MAIN` combines model,
provider, and explicit reasoning, `/model` stays right-aligned when it fits,
and all values and descriptions wrap losslessly instead of being truncated on
narrow terminals. Until
the first successful non-command local task, it also shows one read-only starter
task. That successful final outcome records the private first-use receipt once;
slash commands, empty answers, and failed runs do not complete onboarding.

Key realization driving this: **both reference tools are already this hybrid.**
Codex commits history via `insert_history_lines`/`scroll_region_up`; Claude
Code (Ink) commits history via the `<Static>` component and re-renders only the
dynamic region. SelfMind's "hold the whole transcript in a viewport and
re-render it every frame" is the outlier. We are moving SelfMind onto the same
mainline, not inventing a third model.

**Accepted tradeoff: history is immutable once committed.** No post-hoc
fold/expand or in-place edit of scrolled-back content. "Avoid black box" is
therefore solved at *commit time* (render the right thing once) plus a separate
full-screen history browser for rich review — exactly how Codex/Claude Code do
it.

Migration anchor: `internal/gateway/cli/controller.go:295` uses
`tea.WithAltScreen()` + `tea.WithMouseCellMotion()`. Both must go.

## 2. Target architecture

- Run bubbletea **inline (no alt-screen)**. `View()` renders **only the active
  region** — a small, bounded surface.
- Finalized cells are emitted to scrollback with `tea.Printf`/`tea.Println`
  (returned as `tea.Cmd`s), printed above the inline view and never re-rendered.
- **Commit-on-finalize rule:** the moment a process cell finalizes (an action
  reaches its tool boundary, a tool completes, or the assistant answer ends),
  it is committed to scrollback and removed from the process surface. Only the
  mutable projection + input stay in `View()`, so the active region stays
  bounded even during a 20-tool turn.
- Native terminal handles scroll / selection / copy. App relinquishes mouse
  capture.
- Rich history review (scroll far back, search, expand a big diff) is a
  **full-screen overlay** (temporary alt-screen) that owns its own buffer and is
  backed by recent in-memory messages + `control.db`.

## 3. Active region contract (what `View()` draws)

Included: input editor, status bar, one spinner/activity line, the bounded
process surface (neutral/typed assistant preview and correlated tool cells),
transient selection menus, the notification line, and an unanswered Approval.
The Approval action target uses the available terminal width and wraps without
abbreviation; descriptive context remains compact. Its keyboard ownership lasts
until the daemon accepts a decision.

Excluded (now in scrollback): all finalized user/assistant/tool/system cells.

## 4. Components: cut / rework / keep

| Current | Disposition | Notes |
|---|---|---|
| `tea.WithAltScreen()` | **cut** | inline mode is required for scrollback |
| `tea.WithMouseCellMotion()` | **cut** | relinquish mouse → native selection/scroll |
| transcript `viewport` + `renderAllMessages` over all history | **cut** | history goes to scrollback; View renders active region only |
| `controller_mouse.go` (drag-select, edge-scroll) | **cut** | terminal-native selection |
| `scrollTranscriptPage/Lines`, wheel-over-transcript, PageUp/Down | **cut** | terminal scrollback |
| `restoreOrFollowTranscript`, stick-to-bottom | **cut** | active region is always at bottom |
| Phase-1 `renderCache` | **retire** | history no longer re-rendered |
| `cellRenderers` registry + `render*Message` | **keep & reuse** | now produce the lines we Println to scrollback |
| in-place tool update (`MsgToolDone` mutating `messages[idx]`) | **rework** | render in-progress in active region; commit final form once |
| streaming assistant commit | **rework** | stream in active region; Println final on end |
| pager overlays (`/help`, detail, status) | **rework** | become full-screen overlay (temporary alt-screen) |
| input editor, status bar, spinner | **keep** | active region |
| image paste / attachments | **keep** | input side; pasted images register as `[Image #N · name]` composer tokens (mirroring `[Paste #N · size]`), never raw paths — `Editor.AttachImage`/`ExpandValue` substitute the path back at submit, the transcript echoes the compact token, and the gateway imports attachment files into the person-partitioned scope store (`httpapi/attachments.go`) |
| preamble narration, notification bar restyle, command-output formatting | **keep** | already shipped; renderers reused at commit time |

## 5. Phases

### H0 — Spike / feasibility gate (do first, time-boxed) ⭐
bubbletea's inline + `Println` is coarser than ratatui/Ink. Prove it before
committing to the migration. Build a throwaway prototype: a persistent inline
input + spinner, plus a stream of `tea.Println` history lines.
**Acceptance criteria, verified across WSL + Windows Terminal, tmux, and the
VS Code integrated terminal:**
- history lines land in scrollback in order, no interleave/tearing with the
  inline view;
- native mouse selection + copy works on scrolled-back content;
- terminal scrollback (wheel / shift-pageup) works;
- resize doesn't corrupt the inline view (committed history may stay wrapped at
  old width — acceptable);
- no excessive flicker on rapid `Println` during streaming.
**Gate:** if it passes, proceed to H1. If it fails on a must-support terminal,
stop and reconsider (stay on app-owned + Phase-1 caching, or reconsider the TUI
substrate). Document results in this file.

**H0 progress:**
- API confirmed: bubbletea v1.3.10 ships `tea.Println` ("prints above the
  Program … persists across renders") and `tea.Sequence` (ordered commits) —
  exactly the scrollback-commit + ordering primitives the hybrid needs.
- Spike built: `cmd/tuispike/main.go` (`//go:build ignore`, throwaway). Inline
  mode, no alt-screen, no mouse capture; persistent bottom active region
  (spinner + input) with auto/manual/burst `Println` of single-line and
  multi-line colored "diff" cells. Compiles; excluded from `go build ./...`.
- Verified in WSL + Windows PowerShell (scrollback order, native selection,
  scroll, resize, no objectionable flicker). Gate PASSED. Spike `cmd/tuispike/`
  removed.

### H1 — Inline + scrollback commit pipeline ✅ shipped (flagged)
- Behind `SELFMIND_TUI_HYBRID` (default off → legacy untouched, safe fallback
  while verification is deferred).
- `Start()` runs inline (no alt-screen) + no mouse capture when hybrid.
- `history_commit.go`: `commit(*ChatMessage)` prints a finalized cell to
  scrollback via `Program.Println` and marks it `Committed` (immutable);
  `renderActiveBlock` renders only `processSurface` plus the single spinner;
  `viewActiveRegion` is the hybrid View.
- Commit-on-finalize wired: user/system/assistant via `addMessage` +
  `addErrorMessage`; tools via `MsgToolDone` (running tools show in the active
  region, commit on completion — folds in the core of H2). Merge-into-last
  guarded against committed cells.
- Tests: `TestHybridCommitMarksMessageImmutable`,
  `TestHybridActiveBlockShowsOnlyUncommitted`,
  `TestHybridViewDoesNotReRenderCommittedHistory`. Legacy tests unchanged.
- A subsequent `WindowSizeMsg` clears the visible inline screen before the
  bounded active region repaints. This is required because terminal reflow can
  change physical rows before Bubble Tea updates its cached logical row count;
  without the clean redraw, Composer and status rows are duplicated after a
  container resize. Immutable history remains in native scrollback.
- Pending interactive verification (H0 gate) before flipping the default.

**H1 fix log:**
- Deadlock on first submit (reported: "froze on input"): `commit` called
  `Program.Println` synchronously inside `Update`, which blocks the loop
  goroutine forever. Fixed: `commit` now queues rendered cells; an `Update`
  wrapper flushes them as ordered `tea.Println` Cmds after the handler returns.
  All `m.program.Send` calls elsewhere are in the agent goroutine (safe).
- Empty initial screen: the startup card now renders in the hybrid active region
  until the first message arrives.
- Verified headlessly: `TestHybridSubmitDoesNotDeadlock` drives the real
  bubbletea loop with piped I/O + timeout and submits a prompt — the loop runs
  and exits cleanly (would time out if still deadlocked). Pure terminal visuals
  (flicker, scrollback look, pager overlays) still need a human terminal.

### H2 — Tool cells: in-progress active, commit-on-complete ✅ shipped
- Folded into H1: a running tool renders in the active region; on
  `MsgToolDone` the final cell is committed to scrollback. Legacy viewport and
  hybrid scrollback use the same identity, orphan-routing, and terminal-cleanup
  reducer invariants; only their presentation/commit transport differs.
- Tool output is correlated end to end by `tool_call_id` (builtin event →
  daemon event log → SSE replay → TUI). Nested `batch_read` children publish
  their derived id and tool name in the canonical event fields, not only in a
  compatibility payload. The reducer refuses an anonymous mutable start row;
  unmatched output is ignored, while an identified completion with no tracked
  start gets its own finalized history cell instead of being guessed onto a
  different running call. Committed cells are never mutated by later events.
- Tool failures have separate model and human surfaces. Main receives typed
  category, recovery hint, and diagnostic instructions; the transcript gets a
  bounded cause only. A `not_dispatched` refusal is a dim `Skipped` row rather
  than a red execution failure, while a command that actually ran and failed
  remains visibly failed.
- Active command output is a bounded three-line tail. A terminal run state
  (`done`, `error`, or `cancelled`) finalizes every unfinished tool cell as an
  interrupted error before committing it. Only an intentional spectator detach
  discards its transient projection because the daemon run remains active. Thus
  no terminal run leaves a `Running` row in the redraw region.
- Verification notices remain concise English control text. Successful
  verification is silent; incomplete, failed, or blocked verification adds one
  actionable line only.
- Production-path coverage, not renderer-only tests, guards canonical child
  identity, orphan completion routing, and terminal cleanup.
- Action/tool grouping is shipped through `processSurface`: narration owns a
  stable process-group id, active and completed tools retain that id, member
  errors use the same terminal cleanup, and tools render one indentation level
  below the action. Existing `Exploring` aggregation remains unchanged.

### H2e - Bounded reasoning-process projection

- `processSurface.Update` is the only mutable reducer for foreground assistant
  deltas and tool lifecycle events. Its effects contain only finalized cells
  ready for immutable transcript commit; `m.messages` is no longer an active
  tool or stream store.
- Commentary is the readable action mainline in English, Chinese, and mixed
  text. Tool rows are subordinate evidence. An unspecified provider phase stays
  neutral until a tool or final boundary resolves it, so the UI never guesses a
  final answer from a partial stream.
- Live Markdown uses immutable-block rendering: blank-line-closed blocks are
  parsed, while the incomplete tail is sanitized, wrapped, and shown literally.
  This removes token-by-token structural churn without delaying visible text.
- The process viewport preserves its first action anchor and newest evidence,
  inserts one bounded elision marker when necessary, and never consumes more
  than ten physical rows or the remaining measured height above the composer
  and status line.

### H2a - Live plan pinned above the composer

- A plan is active run state, not an append-only transcript cell. The daemon's
  latest `plan.updated` snapshot replaces the previous snapshot in memory.
- The active plan renders after normal transcript content and notifications,
  immediately above the composer. A blocking Approval retains the Plan state
  and renders below it rather than hiding it behind a separate surface. This
  matches the compact bottom placement used by coding-agent CLIs while
  preserving native terminal scrollback for completed conversation and tool
  cells. One measured blank row above and below the plan keeps it visually
  separate without causing layout overlap or pushing the composer beyond the
  terminal height.
- Terminal run states, cancellation, `/clear`, and a new user turn clear stale
  active plan state. Plan height is included in transcript layout calculations
  so it cannot cover history or move the composer off screen.
- `update_plan` snapshots must describe every current step. Before a run may
  complete successfully, all steps must be resolved; an unresolved plan is
  repaired through the agent loop or leaves the run resumable rather than
  falsely complete.
- An exact-parent continuation copies the parent's latest durable plan into the
  child before Main starts and emits that child snapshot immediately. Plan
  state therefore survives multi-turn transfer instead of depending on a
  display-only replay event.

### H2b - Physical-row-safe structured tool cells

- Every cell is sanitized and pre-wrapped by terminal display columns before
  it enters native scrollback. CSI/OSC/DCS escapes, carriage-return progress
  frames, backspaces, invalid UTF-8, tabs, and unbroken long tokens cannot make
  Bubble Tea's logical row count diverge from the terminal's physical rows.
  Every committed physical row ends with an explicit style reset. This removes
  stale composer-background strips after long commands and here-docs.
- Command cells expose intent, not shell syntax. Known commands render compact
  semantic titles such as `Ran tests`, `Searched files`, or
  `Ran Google Cloud command`; here-doc bodies never become titles. Unknown
  commands retain only a bounded first command, with a maximum two-row header.
- Tool action verbs carry a stable semantic color without overriding outcome:
  run/command verbs are magenta, read/list/search verbs are cyan, file/memory
  mutation verbs are yellow, and plan/lifecycle verbs are blue. The independent
  status bullet remains dim while running, green for successful commands, and
  red for failures, so a failed `Search` is still visibly a failure.
- Command output is limited to five physical rows using a head/tail preview and
  a hidden-row count. The durable tool event remains unchanged; the transcript
  is a readable operational summary rather than a raw terminal dump.
- This presentation policy is channel-specific. The CLI receives structured
  live tool cells and assistant deltas. IM channels keep concise English
  working/approval/final messages and do not receive raw shell output.
- Regression coverage: `terminal_text_test.go` verifies control sanitization,
  hard wrapping, per-row resets, hidden here-doc bodies, bounded head/tail
  output, and the complete commit-to-scrollback boundary.

### H2c - Run-scoped event ordering

- The active-run digest carries the daemon run id. A reconnecting TUI watches
  that exact run instead of every event associated with the person.
- Startup digest headings distinguish event time from current state: terminal
  runs completed after the last presence appear under `While you were away`,
  while older Runs that still derive Attention appear under
  `Still needs attention`.
  Maintenance-only task-card updates never re-date a terminal run.
- Every forwarded event retains its durable event id/cursor, live sequence,
  task id, and run id. The reducer deduplicates replayed events and accepts
  watcher events only while their run is still the run being watched.
- Queue acknowledgements and `run.started` share an opaque `queue_id`. The TUI
  uses that exact correlation to own a drained or transferred Run's spinner;
  equal or truncated input text is never treated as ownership evidence.
- Submitting a new prompt is an ordering barrier: the old assistant fragment
  is committed first, the old watcher is detached, and only then is the new
  user cell committed. Late events from the detached run are discarded.
- Digest text, learning events, and completion notices use distinct cell
  roles. Lifecycle-only tools update control state without creating noisy
  transcript rows.
- External watcher lifecycle uses the durable watcher id as its only display
  identity. Completion renders one compact, transport-neutral status line
  (`Watcher <id> | status: succeeded | task: waiting_finalization`), and the
  system finalization run opens with
  `Watcher <id> | status: finalizing | task: running` instead of exposing its
  internal prompt. The
  current user run is never interrupted; finalization still obeys the
  per-person durable queue.
- Regression coverage: `event_identity_test.go`, `attach_digest_test.go`, and
  the targeted watcher tests in `gateway/client/client_test.go` and
  `gateway/cli/daemon_queue_test.go`.

### H2d - Approval resolution and notices

- An armed prompt is the highest-priority TUI surface. It owns every terminal
  row and every key, centers the existing 76-column-readable decision panel,
  and hides process narration, tools, Plan, Composer, status, pager, and Model
  Manager until resolution. Resize recomputes the surface; resolving restores
  the underlying transient page or active region without committing either to
  transcript history.
- Approval prompts remain FIFO and may wait behind active typing. A successful
  answer clears local state only after the daemon accepts it; transport failure
  keeps the same panel available for retry and never records a false approval.
- The active prompt follows Codex's interaction hierarchy: an action-specific
  question, inspectable command/path and execution context, a selectable list
  of daemon-issued decisions, then `enter to confirm / esc to cancel`. Reusable
  choices display the daemon's exact `rule_label`; the client neither derives a
  rule from prose nor invents an authorization option. `No`, Esc, and Ctrl+C
  all send an explicit rejection before the panel closes. A cancellation
  advances to the next queued durable request rather than silently dropping it.
- The person event stream forwards durable `approval.approved`,
  `approval.rejected`, `approval.parked`, `approval.expired`, and
  `approval.archived` events. Human resolution from another endpoint closes the
  matching active/queued item by approval id and advances the queue. Parked is
  non-terminal: the same panel remains answerable after the original run ends
  and explains that a decision starts a continuation. The local answer's
  stream echo is deduplicated. Expiration/archival never render as rejection or
  claim that a dead run resumed.
- Durable notice cells and the transient notification bar consume one typed
  notice kind and one visual mapping. They do not infer success, guidance,
  warning, or failure from display prose. Approval success/denial feedback
  clears after 1.5 seconds, and generation-tagged timers cannot erase a newer
  notice.
- Explicit cancellation emits expiration only when the daemon wins the
  conditional `pending -> expired` transition. A resource deadline or daemon
  recovery instead emits idempotent `approval.parked`; if a human decision wins
  the race, the waiter honors the stored decision. Re-attach hydration restores
  complete server-issued options rather than a reduced yes/no summary.

### H3 — Commit-time file-change rendering ✅ shipped (patch); ⏳ write_file overwrite deferred
- `renderPatchCell` (`transcript_renderer.go`) parses the V4A patch input
  (`args["patch"]`) into per-file changes and renders:
  - header `Added/Edited/Deleted/Moved <file> (+N -M)` (verb from the V4A
    markers), bounded colored diff body (context dim, `+` green, `-` red),
  - line-number gutter for **new files** (1..N — V4A has no absolute numbers,
    so updates show color-only, no faked numbers — honest),
  - bounded by `maxPatchPreviewLines` with a "… +N more — open history" note.
- Applies in both legacy and hybrid (shared `renderToolMessage`). Falls back to
  the generic path when no V4A input is present.
- Deferred: **write_file overwrite real diff** needs a pre-image captured in
  `builtin.go` (changes the tool's result contract) — left as a follow-up to
  avoid an unverifiable tool-side change. New-file writes still get a header +
  preview via the existing path.
- Tests: `TestPatchCellRenders{AddWithGutterAndStat,EditHunks}`,
  `TestPatchCellBoundsLargeAdd`.

### H4 — Full-screen history browser overlay ✅ shipped (basic)
- `/search current` opens the existing `Pager` over the in-memory log
  (`renderHistoryContent`), rendering **unbounded** diffs (the "expand the full
  diff" escape hatch the bounded scrollback hint points to).
- Deferred: in-overlay text search and `control.db`-backed history beyond the
  in-memory window.
- Tests: `TestHistoryContentRendersUnboundedDiff`.

### H5 — Bounded in-memory window + copy ✅ shipped
- `trimHistoryWindow` caps the in-memory log at `maxHistoryWindow` (2000),
  evicting only the oldest **committed** prefix (never an in-flight cell).
- `/copy` (`handleCopyLast`) copies the last assistant response from
  `m.messages`, independent of the rendered buffer.
- Cell-kind extensibility via `cellRenderers` already in place (Phase 1).
- Tests: `TestTrimHistoryWindowEvictsCommittedPrefix`,
  `TestHandleCopyLastSelectsAssistant`.

## 6. Carryover from the previous plan / Phase 1

- **Reused:** `cellRenderers` registry and the `render*Message` functions —
  they now produce scrollback lines instead of viewport content.
- **Retired:** the `renderCache` (history is no longer re-rendered every frame,
  so memoization is moot). Keep it until H1 lands, then remove.

## 7. Risks & mitigations

- **bubbletea inline/Println is coarser than ratatui/Ink.** → H0 spike gates the
  whole effort; explicit cross-terminal acceptance criteria.
- **Immutable history** (accepted): no fold/edit in scrollback; resize won't
  reflow committed lines. → commit-time rendering + H4 browser for rich review.
- **Loss of app-owned selection/scroll** (accepted): native terminal replaces
  them; add command-based copy for convenience.
- **Large rework surface.** → phased, each phase independently shippable; H1 is
  the irreversible-ish core, gated by H0.

## 8. Verification

- H0: the explicit cross-terminal acceptance checklist above.
- Per phase: unit tests for renderers/commit logic; `GOWORK=off go test
  ./internal/gateway/cli/ ./internal/kernel/`; WSL build + smoke; manual
  cross-terminal check of scroll/select/copy.
