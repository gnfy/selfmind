# TUI Alignment With Claude Code — Design & Implementation Plan

Status: proposed. Owner-facing engineering plan for the CLI/TUI surface.
This is a design note, not a rule set; `AGENTS.md` remains canonical for rules.

## 1. Context (why this work exists)

Three user-reported problems drove this plan:

1. **Black-box operations.** Tool runs and file modifications were opaque: no
   intent narration before tool batches, command output truncated to one line,
   file writes shown as an arbitrary first-12-lines slice.
2. **Performance degrades over long sessions.** `renderAllMessages`
   (`internal/gateway/cli/transcript_renderer.go`) rebuilds the *entire*
   transcript every frame — re-running markdown rendering over all history —
   and is triggered on every cosmetic tick (spinner, cursor blink, status,
   stream flush) plus every `View()` call. Cost is `O(total transcript)` per
   frame, so multi-turn sessions (especially with large diffs in history)
   stutter.
3. **Interaction friction / extensibility.** We want richer, mutable UI
   (collapsible diffs, approval cards, live plans) and room to add custom cell
   types later.

### Decision: keep the app-owned TUI, align to Claude Code (not Codex)

We compared two rendering models:

- **Codex** = inline viewport + native terminal scrollback. Per-frame cost is
  `O(active region)` and it composes with native selection/scroll, but history
  is *append-only and immutable*: no in-place updates, no collapse/expand, no
  resize reflow, weak interactive chrome.
- **Claude Code** = app-owned full-screen rendering (Ink/React) that only
  repaints what changed (reconciliation) over a bounded region. Slightly more
  render work than scrollback, but it enables a **mutable, interactive surface**
  (approval dialogs, expand/collapse, live todo/plan lists, menus, overlays).

SelfMind is interaction-heavy (approvals via `control.approval_requests`,
`update_plan`, skills, expandable tool output) and the TUI is the extensible
local surface in a multi-client (CLI/IM/Web) product. The scrollback model would
fight those features. **Therefore we keep the app-owned model (bubbletea) and
fix its performance, rather than rewrite to scrollback.** The scrollback
"theoretical perf win" is solvable engineering, not a reason to give up the
interaction model.

## 2. Target model — Claude Code parity principles

1. **App-owned, mutable surface** (keep bubbletea).
2. **Only repaint what changed**: cache finalized cells; render only the active
   tail each frame (reconciliation-like).
3. **Bounded live window**: keep the last N turns live in memory; older history
   is retrievable from `control.db` via a pager/search, not held for redraw.
4. **Compact-by-default, expand-on-demand**: every heavy cell (command output,
   diff, large result) shows a bounded summary and expands on a keypress. This
   resolves both "too little (black box)" and "too much (flood)".
5. **Rich file-change rendering**: diff hunks with gutter, context, and color.

## 3. Architecture — structured render cells

Introduce a render-cell abstraction so the transcript is a list of cells, each
of which caches its own rendered output.

- A cell wraps one `ChatMessage` (or a synthetic block) and exposes:
  - `Kind` (user / assistant / tool / system / file-change / …),
  - `Immutable` flag (true once finalized),
  - `Collapsed bool`,
  - a cached `render(width) []string`, keyed by `(contentHash, width, collapsed)`.
- `renderAllMessages` becomes: **join cached cell renders + render only the
  active tail** (live stream, in-progress tool, spinner). The assembled body is
  also cached and invalidated only on append / width change / tail change.
- A small **cell renderer registry** maps `Kind` → renderer, so new cell types
  (approval card, plan widget, chart) plug in without touching the main loop.
  This is the main extensibility hook.

Cache invalidation points: `addMessage`, `appendAssistantResponse`,
`commitLiveStream`/`finalizeLiveStream`, `MsgToolDone` (finalizes a tool cell),
width/resize, and expand/collapse toggles (invalidate just that cell).

## 4. Phases

### Phase 0 — already shipped this session
- Progress narration / preamble guidance (`internal/kernel/prompt_guidance.go`
  `progressNarrationGuidance`, wired in `agent.go buildSystemPrompt`).
- Notification bar restyle with categorized glyph/color
  (`controller.go notificationBar` + `notificationStyleFor`).
- Readable command/file tool summaries + bounded command output head
  (`transcript_renderer.go renderCommandOutputBlock`, `formatCommandResult`,
  `formatFileReadResult`).

### Phase 1 — render-cell cache (performance foundation) ✅ shipped
- Added a fingerprint-keyed `renderCache` + a `cellRenderers` registry
  (`transcript_renderer.go`); `renderAllMessages` now reuses cached renders for
  unchanged messages and only re-renders changed ones. Cache resets on width
  change; bounded by `maxRenderCacheEntries`.
- Registry (`renderCell`) is the extensibility hook for Phase 5 cell types.
- Result at 200 turns (400 messages), warm vs cold redraw:
  - cold (old behavior): ~4.98 ms/op, 1.13 MB, 24,658 allocs/op
  - cached (now):        ~0.93 ms/op, 0.22 MB,    443 allocs/op
  - → ~5.3× faster, ~5× less memory, ~55× fewer allocations.
- Follow-ups deferred: the per-frame fingerprint scan is still O(history)
  (cheap), removed fully by the Phase 4 bounded window; the duplicate render
  between `viewModel` and `syncTranscriptContent` is now cache-cheap and can be
  de-duped later.
- Tests: `TestTranscriptCacheConsistentAndInvalidates`,
  `TestTranscriptCacheResetsOnWidthChange`, `BenchmarkRenderAllMessages{Cached,Cold}`.

### Phase 2 — collapsible cells + keyboard navigation
- Add `Collapsed` to `ChatMessage`; cell focus + up/down to move focus;
  a toggle key (e.g. `ctrl+r` or focus+enter) to expand/collapse the focused cell.
- Heavy cells (command output, diff, large results) default collapsed with a
  "… +N more (expand)" affordance.
- Files: `controller.go` (focus state, key handling), `transcript_renderer.go`
  (collapsed vs expanded forms).

### Phase 3 — Claude-Code-style file-change diff (the "avoid black box" payoff)
Depends on Phase 2's expand mechanism.
- **Header**: `Added/Edited/Deleted <file> (+N -M)`; verb inferred from
  `PatchResult` (`FilesCreated/Modified/Deleted`, `patch.go:76`) or write_file
  new-vs-overwrite. Move `(+N -M)` into the header; drop the trailing duplicate
  line in `renderToolDiff`.
- **patch**: parse the unified diff and render hunks — left-aligned line-number
  gutter, context lines, red `-` / green `+`. Full hunks by default (real edits
  are short); only pathologically large diffs collapse + expand.
- **write_file new file**: header + ~20-line green preview + "… +N more".
- **write_file overwrite**: capture the pre-image in `builtin.go` before writing,
  compute a real diff, render as hunks (so the user sees *what changed*, not the
  whole new file).
- Optional: syntax highlighting inside diffs (defer; red/green first).
- Files: `transcript_renderer.go` (`renderToolDiff` → `renderFileChange`),
  `internal/tools/builtin.go` (write_file pre-image), small diff/hunk util.

### Phase 4 — bounded memory + copy ergonomics
- **Bounded live window**: keep last N turns rendered; evict older from the live
  buffer; expose history via pager/search backed by `control.db`.
- **Command-based copy**: "copy last response" / "copy code block" sourced from
  `m.messages` (independent of rendered buffer). Keep mouse selection as-is
  (app-owned), or add a toggle to relinquish mouse capture for native selection.
- Files: `controller.go`, `controller_mouse.go`, `clipboard.go`,
  history/store access.

### Phase 5 — extensibility (optional, enabled by Phase 1's registry)
- Cell renderer registry already in place → add approval-card cells, plan/todo
  widget cells, and centralized theming. New cell types require no change to
  `renderAllMessages`.

## 5. Non-goals / risks

- **Not** doing the Codex scrollback rewrite (Route A). It would break app-owned
  mouse selection, in-app scroll, in-place tool updates, resize reflow, and
  fight our interactive features.
- Resize reflow keeps working because cells re-render on width change (cache is
  width-keyed) — unlike scrollback, which can't reflow committed history.
- Keep all changes behind the existing event/contract surfaces; do not bypass
  task strategy, identity binding, or context selection.

## 6. Verification

- Micro-benchmark: redraw cost at 10/50/200 turns, before vs after Phase 1.
- Unit tests: cache invalidation correctness, expand/collapse toggle, diff hunk
  rendering (add/edit/overwrite/delete), header verb inference.
- `GOWORK=off go test ./internal/gateway/cli/ ./internal/kernel/`.
- WSL build + smoke (`~/.local/bin/selfmind`) after each phase.
