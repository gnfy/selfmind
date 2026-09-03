# Continuity Suite (cross-endpoint north-star scenarios)

These cases assert the core product promise: one person, many endpoints. Work
started from the CLI must be visible (`/status`), continuable (`继续`), and
identity-isolated (a different platform user sees nothing) from another channel.

What each case proves today:

- `continuity-status.yaml` (deterministic, `model_required: false`): a Run
  seeded as `waiting_user` from the CLI is reported by `/status` on a weixin
  turn of the same person. The assertion is the model-free control-plane view
  of shared structured state; a production-path variant that parks the Run
  through a real turn may be added alongside it.
- `continuity-resume.yaml` and `continuity-start-execution.yaml`
  (cassette-backed): a follow-up cue (`继续`, `开始执行`) after a COMPLETED turn
  is ordinary new work that owns its own root (`require_task_switch`), while
  answer continuity flows through the person work spine (`must_not_contain`
  "continue what?", `contains` `CONTINUED`). Exact resume of a waiting Run is
  covered by `timeline/timeline-confirm-after-waiting-run.yaml` and Go tests.
- `continuity-stranger.yaml` (cassette-backed): a different `platform_user_id`
  asking `/status` sees `No active task.` and never the first user's marker.
- `continuity-task-attach.yaml` (cassette-backed): two unrelated tool-free
  turns complete cleanly; grouping semantics are pinned by Go tests.
- `recall-cross-task.yaml`, `memory-pinned-recall.yaml`, and
  `memory-canonical-query-recall.yaml` (cassette-backed): prior work and saved
  preferences reach a later turn through recall (`context.recall` state
  assertions and the recalled marker).
- `execution-environment-diagnostics.yaml` (deterministic) and
  `execution-state-overlay-first-try.yaml` (cassette-backed): `/diag execution`
  stays credential-free, and a sandboxed tool may write its own state directory
  on first use without an authentication misdiagnosis.

Model-backed cases are marked `require_cassette: true`, so they run even in the
fast local profile. Their cassettes are committed; deterministic control cases
declare `model_required: false` and record nothing.

Ordinary new language owns a fresh root; prior work is related only by
structured edges or by the normal in-Run Main path through `work_search`,
`work_inspect`, and `work_select`. Deterministic coverage lives in
`internal/gateway/httpapi/task_attach_test.go`, `turn_choices_test.go`, and
`work_selection_test.go`.

## Re-recording after intentional behavior changes

From the repo root, with a working model provider configured:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity
```

(or `SELFMIND_EVAL_VCR=record go run ./cmd/selfmind eval run evalcases/continuity`)

Cassettes land in `.vcr/<case-id>/` (one numbered JSON file per model call):

- `.vcr/continuity_resume/`
- `.vcr/continuity_start_execution/`
- `.vcr/continuity_stranger/`
- `.vcr/continuity_task_attach/`
- `.vcr/recall_cross_task/`
- `.vcr/memory_pinned_recall/`
- `.vcr/memory_canonical_query_recall/`
- `.vcr/execution_state_overlay_first_try/`

**Commit the updated cassette directories.** Cassettes are what make the
gate real: `selfmind selfcheck` replays them strictly offline
(`SELFMIND_EVAL_VCR=replay` + `SELFMIND_EVAL_OFFLINE=1`), so CI never burns
provider quota.

## Verifying the recording

```sh
selfmind selfcheck --skip-go
```

All selected cases in this suite must report `ok`. If a case fails right after
recording, the recorded model output violated an assertion (for example, the
`continuity_stranger` reply echoed the `ZWX417` marker even though the prompt
forbids it): delete that case's `.vcr/<case-id>/` directory and re-record.

## Notes

- `/status` turns are pre-agent control commands: they consume no model tokens
  and produce no cassette files — only the model-backed turns are recorded.
- Assertions pin deterministic surfaces (status-card markers such as `Task:` /
  `Status:` / `No active task.`, task-ID continuity via `require_same_task`),
  not free model prose, so replays stay stable.
- `continuity-stranger.yaml` uses the per-turn `platform_user_id` override to
  simulate a second platform user; see `docs/eval-loop.md`.
- **Cross-endpoint active-run steering** (a continuation injected into a run
  that is still executing; 2026-07-09) is deliberately NOT an eval case: the
  eval runner is sequential — turns do not overlap, so there is no live
  active run for a later turn to steer into. Modeling it would require an
  eval-only shortcut (forbidden). Its regression protection is the real-gateway
  Go coverage: `httpapi/steer_active_run_test.go`,
  `httpapi/handlers_steer_test.go`, and `httpapi/queue_test.go`
  (`TestContinuationDoesNotQueue`).
- Fresh natural-language queries near older work are covered by
  `timeline/timeline-natural-new-work-not-captured.yaml`. Natural-language IM
  progress over CLI work is covered by
  `timeline/timeline-natural-progress-cross-endpoint.yaml`, which records the
  normal Main `work_search` → `work_inspect` → `work_select(observe)` path.
  Active overlapping turns remain real-gateway Go integration tests because
  the sequential eval runner cannot create a live steer window without a test
  shortcut.
