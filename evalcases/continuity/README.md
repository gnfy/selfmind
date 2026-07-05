# Continuity Suite (cross-endpoint north-star scenarios)

These cases assert the core product promise: one person, many endpoints. A task
started from the CLI must be visible (`/status`), resumable (`继续`), and
identity-isolated (a different platform user sees nothing) from another channel.

The three original cases are marked `require_cassette: true`: `selfmind
selfcheck` (and CI) FAILS — does not skip — while their cassettes are missing.
They need one local recording run against a configured live provider before
the CI gate goes green.

`continuity-task-attach.yaml` (task-attach semantics: new work never lands on
the parked task, asserted via `require_task_switch`) is NOT yet
`require_cassette: true` — it skips offline until its cassette is recorded and
committed; flip the flag on in the same commit as the cassette. Its
deterministic coverage lives in
`internal/gateway/httpapi/task_attach_test.go`.

## Recording the cassettes (one command)

From the repo root, with a working model provider configured:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity
```

(or `SELFMIND_EVAL_VCR=record go run ./cmd/selfmind eval run evalcases/continuity`)

Cassettes land in `.vcr/<case-id>/` (one numbered JSON file per model call):

- `.vcr/continuity_status/`
- `.vcr/continuity_resume/`
- `.vcr/continuity_stranger/`

**Commit the `.vcr/continuity_*` directories.** Cassettes are what make the
gate real: `selfmind selfcheck` replays them strictly offline
(`SELFMIND_EVAL_VCR=replay` + `SELFMIND_EVAL_OFFLINE=1`), so CI never burns
provider quota.

## Verifying the recording

```sh
selfmind selfcheck --skip-go
```

All three `continuity_*` cases must report `ok`. If a case fails right after
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
