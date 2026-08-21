---
name: selfmind-daily-driver-audit
description: Audit SelfMind daily-driver and multi-day runtime evidence to find stale or starved maintenance, broken telemetry, truncated reports, delivery backlog, tool and approval friction, prompt-cache regressions, and memory-governance gaps. Use when analyzing control.db, selfmind report daily, selfmind usage, /diag output, flight-recorder data, or several days of real SelfMind runs; when comparing runtime behavior with docs/vision-saas-enterprise.zh-CN.md or Phase-1 gates; or when a one-day health review may miss liveness and trend defects.
---

# SelfMind Daily Driver Audit

Treat runtime analysis as an evidence-liveness audit before treating it as a product-quality audit. This is a repository development Skill for coding agents; it is not a SelfMind runtime Skill and does not authorize runtime mutations.

## Establish the evidence boundary

1. Read the repository `AGENTS.md`, `docs/STATUS.md`, the active plan, and the relevant domain documents.
2. Record the analysis time, daemon uptime/restarts, requested window, and actual oldest/newest evidence returned.
3. Use at least two windows when data permits: a recent 24-hour slice and a 7-day or longer trend. A single day cannot prove a periodic subsystem is live.
4. Open `control.db` read-only. Do not migrate, repair, replay, dismiss, or otherwise mutate runtime state during an analysis-only task.
5. Read [references/audit-checklist.md](references/audit-checklist.md) for the evidence matrix and reporting template.

## Audit evidence health first

For every report or metric, establish:

- generated_at and age;
- last successful producer run and next due time;
- whether a daemon restart resets its clock;
- whether active foreground work suppresses or merely delays it;
- row/event caps, pagination, and lower-bound markers;
- whether wrapper layers forward optional telemetry interfaces;
- whether zero means a valid outcome, no eligible input, a stale snapshot, or a producer that never ran.

Do not interpret a stale report as current state. Do not call missing fingerprints a cache regression; call them a telemetry break until request fingerprints exist.

## Keep denominators honest

Separate stocks from flows and stages from outcomes:

- current backlog is a stock; jobs created or completed in a window are flows;
- triage escalations, human asks, approval requests, and human decisions are different funnel stages;
- recall candidates, selected slices, prompt injection, and output use are different denominators;
- `sent`, `sent_unconfirmed`, and `pending_session` express different delivery certainty;
- memory `ADD=0` is not a defect unless eligible durable evidence was present and the intake path ran.

State the denominator beside every rate. Mark incomplete windows as lower bounds.

## Check the four product loops

1. **Execution and recovery:** run completion, tool failures and classification, stale identifiers, raw backend errors, waiting-user share, verification evidence.
2. **Delivery and continuity:** new pending rows, oldest age, confirmation state, automatic recovery eligibility, deduplication, and an explicit non-flooding reconciliation path.
3. **Learning and memory:** post-run intake liveness, governance schedule liveness, report freshness, protected memory usage, consolidation backlog, and downstream recall usefulness.
4. **Economics and evidence:** provider calls per turn, native tool-schema share, cache reads and misses, stable/provider prefix fingerprints, truncation, and diagnostic agreement with actual provider usage.

Map findings to the Phase-1 acceptance scenarios and the enterprise vision gates, but do not add SaaS or Runner scope unless an active plan authorizes it.

## Classify conclusions

Use three labels:

- **Confirmed defect:** code path and runtime evidence establish the mechanism.
- **Observed symptom:** data establishes the outcome but not the cause.
- **Hypothesis:** plausible explanation requiring a named probe or longer clock.

Prefer the smallest probe that starts a useful evidence clock. Fix missing telemetry before optimizing the behavior it was meant to measure.

## Recommend work in evidence-clock order

Prioritize deterministic correctness and user-trust failures, but land cheap observability fixes early when their validation needs days. For each proposal state:

- affected product loop and vision gate;
- expected user or cost impact;
- safety and migration boundary;
- Go test versus model-backed eval evidence;
- measurement window and rollback threshold.

Do not recommend blind replay of uncertain deliveries, forced daily memory additions, relaxed approval safety, or bulk deferred-tool rollout without a working activation protocol and measured grey cohort.

## Produce the report

Lead with the conclusion. Include:

1. evidence coverage and freshness;
2. confirmed defects, symptoms, and hypotheses;
3. cross-window trends with denominators;
4. prioritized closure plan and dependencies;
5. metrics whose clocks must run for 7 to 14 days;
6. explicit non-goals and safety rails.

When the same liveness or evidence failure recurs across three similar audits, update this repository Skill's checklist rather than relying on another one-off prompt.
