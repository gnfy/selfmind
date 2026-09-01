# Timeline suite — Work Timeline acceptance scenarios

Regression coverage for the twelve acceptance scenarios in
`docs/work-timeline.md`. Model turns are cassette-pinned (`.vcr/timeline_*`,
committed); re-record with:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/timeline
```

| # | Scenario | Coverage |
|---|----------|----------|
| 1 | New work opens a label | `timeline-iterate.yaml` (turn 1) |
| 2 | Iteration continues via spine; each message owns its root task (P2) | `timeline-iterate.yaml` (turn 2: "刚才那首" revised; `require_task_switch`) |
| 3 | Unrelated ask mid-stream stays clean | `timeline-new-topic.yaml` |
| 4 | Ambiguous reference → agent asks in-turn | `timeline-ambiguity.yaml` (reply names both candidates; a few read-only tool calls allowed — the inspect posture may glance at the workspace first) |
| 5 | Cross-endpoint answer continuity (cli → weixin) via the spine | `timeline-cross-endpoint.yaml` (`require_task_switch`; content continuity asserted) |
| 6 | /tasks aggregated view | `timeline-tasks-view.yaml` (control-only, runs offline without a cassette) |
| 7 | Mislabel harmless / rename | Go: `internal/gateway/httpapi/run_labeler_test.go` (MOVE/TITLE/KEEP/NEW, governed-reference correction, durable-evidence INBOX guard), `task_view_test.go` (rename) — deterministic control-flow, not model behavior |
| 8 | Long-run compaction keeps the goal | Go: `internal/kernel/context_engine_test.go` (default compaction, head/tail protection, Relevant Files) + `light_task_layer_test.go` (spine) — unbounded-length runs are not eval-expressible offline |
| 9 | Control-plane zero regression | Existing suites: approvals/queue/recovery Go tests + `evalcases/continuity/*` cassettes |
| 10 | Label decisions auditable | Go: `run_labeler_test.go` (`label.assigned` event on non-KEEP); run→task mapping implicit in `task_runs` |
| 11 | Task governance is user-controlled | `timeline-task-governance.yaml` (pin visibility + reversible unpin); Go: `task_governance_test.go` (hidden Inbox, safe auto-archive) |
| 12 | Task identity references are user-governed | `timeline-task-references.yaml` (add/list/remove, model-free); Go: `task_references_test.go`, `server_test.go`, and `run_labeler_test.go` cover activation, conflict abstention, mention/continue authority, and post-run proposals. |
| 13 | Ambiguous continuation → deterministic run candidates, no model (P2) | `timeline-run-candidates.yaml` (`model_required: false`, seeded parked runs) |
| 14 | Natural-language IM progress → CLI run card, no new run (v8 continuity) | `timeline-natural-progress-cross-endpoint.yaml` (fast-classifier cassette) |
