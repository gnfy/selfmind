# Timeline suite — Work Timeline acceptance scenarios

Regression coverage for the Thread/Run/Attention acceptance scenarios in
`docs/work-timeline.md`. Model turns are cassette-pinned (`.vcr/timeline_*`,
committed); re-record with:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/timeline
```

| # | Scenario | Coverage |
|---|----------|----------|
| 1 | Ordinary interaction is retained without becoming Attention | `timeline-iterate.yaml` plus Go: `work_timeline_test.go` |
| 2 | Iteration continues via spine; each root remains independently accountable | `timeline-iterate.yaml` (turn 2: "刚才那首" revised; `require_task_switch`) |
| 3 | Unrelated ask mid-stream stays clean | `timeline-new-topic.yaml` |
| 4 | Ambiguous reference → agent asks in-turn | `timeline-ambiguity.yaml` (reply names both candidates; a few read-only tool calls allowed — the inspect posture may glance at the workspace first) |
| 5 | Cross-endpoint answer continuity (cli → weixin) via the spine | `timeline-cross-endpoint.yaml` (`require_task_switch`; content continuity asserted) |
| 6 | bare `/resume` is exact-Run Attention | `timeline-tasks-view.yaml`, `timeline-ordinal-refs.yaml` (control-only) |
| 7 | Grouping is reversible display metadata | Go: `work_timeline_test.go`, `task_view_test.go` |
| 8 | Long-run compaction keeps the goal | Go: `internal/kernel/context_engine_test.go` (compaction, Relevant Files), `context_safety_test.go` (tool arguments/catalog budget, current goal and steering retention, summary ownership/cancellation, verification priority), and `light_task_layer_test.go` (spine). `timeline-iterate.yaml` checks the production request stays in budget; unbounded-length runs are not eval-expressible offline. |
| 9 | Control-plane zero regression | Existing suites: approvals/queue/recovery Go tests + `evalcases/continuity/*` cassettes |
| 10 | Promotion and selection decisions are auditable | Go: `run_finalization_test.go`, `work_selection_test.go` |
| 11 | Thread presentation is user-controlled | `timeline-task-governance.yaml`; Go: `task_governance_test.go`, `work_timeline_test.go` |
| 12 | Thread references are user-governed search hints | Go: `task_references_test.go` (the `/task references` command was retired with `/task`) |
| 13 | Ambiguous continuation → deterministic exact-Run candidates, no model | `timeline-run-candidates.yaml` |
| 14 | Natural-language IM progress → CLI run card, no new run (v8 continuity) | `timeline-natural-progress-cross-endpoint.yaml` (fast-classifier cassette) |
| 15 | Same-channel bare confirmation ("确认执行") resumes the waiting run that asked for it (Main-turn continuity) | `timeline-confirm-after-waiting-run.yaml` (seeded `waiting_user` run; `work_search` lists it as `unresolved_run` without a literal hit, `work.selection_committed` resume asserted) |
