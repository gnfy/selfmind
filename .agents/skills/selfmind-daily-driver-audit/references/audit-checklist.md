# SelfMind runtime audit checklist

Use only the sections relevant to the evidence available. Prefer product commands and read-only database queries; never assume one surface is authoritative without checking its age and cap.

## Evidence inventory

| Source | Verify before use |
| --- | --- |
| `selfmind usage` | requested window, provider calls/tokens, tool-schema share, fingerprint state |
| `selfmind report daily --since ...` | actual window, pagination/lower-bound marker, current versus historical job states |
| `/diag context` | provider request total, assembled prompt subtotal, latest call age, prefix fingerprint |
| `/diag memory` | report generated time/age, durable last success, next due, deferral/failure reason |
| `/diag delivery` | total current backlog, oldest age, kind mix, automatic-window eligibility |
| `/diag` approval funnel | automatic stages, human-request denominator, decisions and response latency |
| `control.db` read-only | table timestamps, status definitions, joins that may exclude synthetic rows |
| report files | mtime/content timestamp, shadow versus applied mode, whether a producer still runs |

## Cross-window minimums

- Compare 24 hours with at least 7 days.
- For a 24-hour periodic task, require evidence spanning daemon restarts and at least two due opportunities.
- Show daily cache rate and zero-hit call count, not only one aggregate.
- Show new delivery backlog separately from old accumulated backlog.
- Show current actionable maintenance separately from succeeded/skipped history.
- Report the oldest pending age for every durable queue.

## Liveness failure patterns

Check for:

- ticker starts after a full interval instead of performing overdue startup catch-up;
- restart resets an in-memory last-success clock;
- active work causes a skipped tick followed by another full interval;
- report file exists but is older than the requested analysis window;
- wrapper omits an optional capability such as request fingerprinting;
- a fixed event cap truncates a long window without pagination;
- a health query inner-joins a table synthetic jobs do not use;
- error paths turn unavailable telemetry into a normal-looking zero.

## Output table

For each finding record:

| Field | Meaning |
| --- | --- |
| Severity | user trust, correctness, safety, cost, or evidence integrity |
| Classification | confirmed defect, observed symptom, or hypothesis |
| Clock | instantaneous, 24h, 7d, or 14d validation |
| Evidence | command/table/event and exact window |
| Mechanism | code path when confirmed |
| Gate | Phase-1 scenario or enterprise vision gate |
| Fix | smallest bounded change |
| Proof | Go test, evalcase, runtime metric, or external live check |
| Rollback | condition that stops or reverses a grey release |

## Common interpretation traps

- `ADD=0` does not prove memory extraction is broken.
- low recall selection does not prove low recall quality.
- zero approval rejections does not prove approval asks are unnecessary.
- a stable SelfMind prompt prefix does not prove the provider's complete prefix is stable.
- `pending_session` is not equivalent to confirmed delivery failure.
- terminal `skipped` maintenance rows are not current backlog.
- a passing cassette does not validate changed prompts unless the eval explicitly forces the intended prompt revision.
