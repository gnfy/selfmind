# TUI: Terminal-First Hybrid — Revised Plan

Status: proposed. Supersedes the target architecture in
`docs/tui-claude-code-alignment.md` (Phase 1 of that doc still shipped and is
partly reused — see "Carryover"). Design note, not a rule set; `AGENTS.md`
stays canonical.

## 0. Migration status (current)

- H0 spike: ✅ verified (WSL + PowerShell), spike removed.
- H1–H5: ✅ shipped (see §5).
- **hybrid is the ONLY renderer (2026-07-10).** The legacy path and `SELFMIND_TUI_LEGACY` were DELETED. (Historical note: it used to be the
  escape hatch to the old renderer (kept one cycle).
- Overlays (`/help`, `/history`, `/model`, …): ✅ user-verified in hybrid.
- `/clear` + `ctrl+l`: clear the screen and re-show the startup card in hybrid.
- **Remaining (deferred follow-ups, not blocking default):**
  - Legacy rendering path + `SELFMIND_TUI_LEGACY`: ✅ deleted 2026-07-10
    (viewport, `controller_mouse.go`, app scroll, `renderCache`).
  - write_file overwrite real diff (needs a pre-image; tool-contract change).
  - `/history` in-overlay search + `control.db`-backed history beyond the window.

## 1. Decision

Migrate the CLI/TUI from "app-owned full-viewport re-render" to a
**terminal-first hybrid**:

- **History → native terminal scrollback** (committed once, immutable). The
  terminal owns scroll, selection, copy, and long-history performance — the
  things most prone to bugs and slowdown in an app-owned model.
- **Active region → app-controlled** (bubbletea inline view): input, spinner,
  the currently-streaming reply, the currently-running tool cell, and transient
  dialogs (approval, menus).

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
- **Commit-on-finalize rule:** the moment a cell finalizes (tool completes,
  assistant message ends, system note done) it is committed to scrollback and
  removed from the active region. Only actively-changing cells + input stay in
  `View()`, so the active region stays bounded even during a 20-tool turn.
- Native terminal handles scroll / selection / copy. App relinquishes mouse
  capture.
- Rich history review (scroll far back, search, expand a big diff) is a
  **full-screen overlay** (temporary alt-screen) that owns its own buffer and is
  backed by recent in-memory messages + `control.db`.

## 3. Active region contract (what `View()` draws)

Included: input editor, status bar, spinner/activity line, the in-progress
assistant stream, the in-progress tool cell(s), transient dialogs (approval
prompt, selection menu), the notification line.

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
| image paste / attachments | **keep** | input side, unaffected |
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
  `renderActiveBlock` renders only the uncommitted tail (in-progress tools, live
  stream, spinner); `viewActiveRegion` is the hybrid View.
- Commit-on-finalize wired: user/system/assistant via `addMessage` +
  `addErrorMessage`; tools via `MsgToolDone` (running tools show in the active
  region, commit on completion — folds in the core of H2). Merge-into-last
  guarded against committed cells.
- Tests: `TestHybridCommitMarksMessageImmutable`,
  `TestHybridActiveBlockShowsOnlyUncommitted`,
  `TestHybridViewDoesNotReRenderCommittedHistory`. Legacy tests unchanged.
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
  `MsgToolDone` the final cell is committed to scrollback. The legacy in-place
  mutation path is retained for legacy mode (no behavior change there).

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
- `/history` opens the existing `Pager` over the in-memory log
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
