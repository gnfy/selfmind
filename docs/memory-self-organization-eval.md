# Memory Self-Organization Evaluation

This experiment validates memory governance against a real, messy history
before SelfMind is allowed to change any stored memory automatically.

## Product Order

1. Silent self-organization is the primary product path.
2. Human search, correction, pinning, and forgetting are safety controls.
3. User-confirmed and pinned memory is never changed automatically.

The experiment has no apply path. It opens the live SQLite database read-only,
writes candidate and judge reports elsewhere, and leaves all facts untouched.

## Two-Stage Algorithm

1. Deterministic candidate retrieval groups only same-target, same-scope facts.
   Complete-link clustering requires every member pair to meet the retrieval
   threshold, avoiding chain clusters.
2. The configured `memory_extract` model judges each candidate as `KEEP`,
   `MERGE`, `REINFORCE`, `SUPERSEDE`, `CONFLICT`, or `ARCHIVE`.

Similarity only finds candidates. It never authorizes a merge.

## Dry-Run Commands

```bash
SELFMIND_MEMORY_AUDIT_DB=~/.selfmind/data/<person>/memory.db \
SELFMIND_MEMORY_AUDIT_OUT=evalruns/memory-consolidation/candidates.json \
GOWORK=off go test ./internal/kernel/memory \
  -run TestMemoryConsolidationLiveDryRun -v

SELFMIND_MEMORY_CONSOLIDATION_LIVE=1 \
SELFMIND_MEMORY_AUDIT_REPORT=evalruns/memory-consolidation/candidates.json \
SELFMIND_MEMORY_JUDGE_OUT=evalruns/memory-consolidation/judge.json \
SELFMIND_MEMORY_JUDGE_LIMIT=16 \
GOWORK=off go test ./internal/app \
  -run TestMemoryConsolidationJudgeLive -v
```

The second command uses the resolved `memory_extract` auxiliary route. It may
come from `models.auxiliary` or an explicit `models.roles.memory_extract`
override, and never falls back to the main coding model.

## Acceptance Gates

- protected-memory automatic changes: zero;
- cross-scope merge candidates: zero;
- sampled automatic-merge precision: at least 98%;
- uncertain or merely related facts resolve to `KEEP` or `CONFLICT`;
- retrieval quality after consolidation does not regress;
- no live database writes during evaluation.

## Configuration Hypotheses

These values now live under `memory.governance`. `shadow` is the production
default; merge/cap values remain calibration gates until shadow evaluation is
accepted:

```yaml
memory:
  governance:
    enabled: true
    mode: "shadow"
    max_active_global: 120
    max_active_per_workspace: 200
    archive_after: "4320h"
    consolidation_interval: "24h"
    consolidation_batch_size: 8
    auto_merge_confidence: 0.95
    pause_while_run_active: true
```

The judge uses the stable `memory_extract` role. A dedicated model is selected
through `models.roles.memory_extract`; otherwise the role uses
`models.auxiliary`.

An upper bound applies to active canonical memories, not raw evidence. Archival
must eventually use `last_accessed_at` and `last_verified_at`; age from creation
alone is not enough to decide that a memory is unused.

## Future Intake Contract

The model should not save every user message. Eligible completed runs are
evaluated once with nearby canonical memories and produce one of:

- `SKIP`: temporary, speculative, secret, or already represented;
- `ADD`: new durable information;
- `REINFORCE`: evidence for an existing memory;
- `SUPERSEDE`: a newer fact replaces an old one;
- `CONFLICT`: preserve both pending later evidence or user correction.

This decision belongs in the unified post-run maintenance pass, not a second
model call per message.
