# Timeline suite — Work Timeline acceptance scenarios

Regression coverage for the Thread/Run/Attention acceptance scenarios in
`docs/work-timeline.md`. Model turns are cassette-pinned (`.vcr/timeline_*`,
committed); re-record with:

```sh
SELFMIND_EVAL_VCR=record selfmind eval run evalcases/timeline
```

| # | Scenario | Coverage |
|---|----------|----------|
| 1 | Ordinary interaction is retained without becoming Attention | Go: `work_timeline_test.go` |
| 2 | Iteration continues via spine; each root remains independently accountable | Go: `light_task_layer_test.go`, `work_timeline_test.go` |
| 3 | Unrelated ask mid-stream stays clean | Go: `work_timeline_test.go`, `task_attach_test.go` |
| 4 | Ambiguous reference → agent asks in-turn | `timeline-ambiguity.yaml` (reply names both candidates; a few read-only tool calls allowed — the inspect posture may glance at the workspace first) |
| 5 | Cross-endpoint answer continuity (cli → weixin) via the spine | `continuity/continuity-resume.yaml` and Go: `work_timeline_test.go` |
| 6 | bare `/resume` is exact-Run Attention | Go: `task_view_test.go`, `turn_choices_test.go` |
| 7 | Grouping is reversible display metadata | Go: `work_timeline_test.go`, `task_view_test.go` |
| 8 | Long-run compaction keeps the goal | Go: `internal/kernel/context_engine_test.go` (compaction, Relevant Files), `context_safety_test.go` (tool arguments/catalog budget, current goal and steering retention, summary ownership/cancellation, verification priority), and `light_task_layer_test.go` (spine). |
| 9 | Control-plane zero regression | Existing suites: approvals/queue/recovery Go tests + `evalcases/continuity/*` cassettes |
| 10 | Promotion and selection decisions are auditable | Go: `run_finalization_test.go`, `work_selection_test.go` |
| 11 | Thread presentation is user-controlled | Go: `task_governance_test.go`, `work_timeline_test.go` |
| 12 | Thread references are user-governed search hints | Go: `task_references_test.go` (the `/task references` command was retired with `/task`) |
| 13 | Ambiguous continuation → deterministic exact-Run candidates, no model | Go: `turn_choices_test.go`, `work_selection_test.go` |
| 14 | Natural-language IM progress → CLI run card, no new run (v8 continuity) | Go: `work_selection_test.go`, `task_attach_test.go` |
| 15 | Same-channel bare confirmation ("确认执行") resumes the waiting run that asked for it (Main-turn continuity) | `timeline-confirm-after-waiting-run.yaml` (seeded `waiting_user` run; `work_search` lists it as `unresolved_run` without a literal hit, `work.selection_committed` resume asserted) |
| 16 | A and B remain distinguishable when both are open in one workspace | `timeline-interleaved-work.yaml` (return to A after B; prior spine turns are references) plus Go: `light_task_layer_test.go` (same/cross-workspace, missing final answer, exact Run provenance) |
