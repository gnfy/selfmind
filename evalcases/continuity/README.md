# Continuity Suite (cross-endpoint north-star scenarios)

These cases assert the core product promise: one person, many endpoints. A task
started from the CLI must be visible (`/status`), resumable (`继续`), and
identity-isolated (a different platform user sees nothing) from another channel.

The north-star cases are marked `require_cassette: true`, so they run even in
the fast local profile. Their cassettes are committed; `selfmind selfcheck`
fails if any recording is missing or invalid.

`continuity-task-attach.yaml` covers explicit continuation semantics. Ordinary
new language owns a fresh root task; prior work is related only by structured
edges or by the normal in-Run Main path through `work_search`, `work_inspect`,
and `work_select`. Deterministic coverage lives in
`internal/gateway/httpapi/task_attach_test.go`, `turn_choices_test.go`, and
`work_selection_test.go`.

## Re-recording after intentional behavior changes

From the repo root, with a working model provider configured:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity
```

(or `SELFMIND_EVAL_VCR=record go run ./cmd/selfmind eval run evalcases/continuity`)

Cassettes land in `.vcr/<case-id>/` (one numbered JSON file per model call):

- `.vcr/continuity_status/`
- `.vcr/continuity_resume/`
- `.vcr/continuity_stranger/`

**Commit the updated `.vcr/continuity_*` directories.** Cassettes are what make the
gate real: `selfmind selfcheck` replays them strictly offline
(`SELFMIND_EVAL_VCR=replay` + `SELFMIND_EVAL_OFFLINE=1`), so CI never burns
provider quota.

## Verifying the recording

```sh
selfmind selfcheck --skip-go
```

All selected `continuity_*` cases must report `ok`. If a case fails right after
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
