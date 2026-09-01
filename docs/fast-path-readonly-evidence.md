# Fast Path for Verified Read-Only Evidence

## Decision

Fast Path is deferred. It is not an active plan and no runtime capability is
claimed.

The useful idea is narrower than the former proposal: repeated, verified,
parameterizable read-only evidence collection may eventually be compiled into
a cheaper deterministic path. That path must preserve provenance and fall back
to ordinary planning in the same turn. It must not become a second agent loop,
authorize mutations, or infer quality from ordinary successful Runs.

The earlier `PROPOSAL-fast-path.md` mixed this valid direction with a large
unfunded architecture and descriptions of behavior that the runtime did not
provide. This decision replaces that proposal.

## Current Boundary

The repository has a safe observation floor, not Fast Path:

- Self-evolution defaults to `observe`.
- Ordinary successful Runs create observations only. They do not advance
  shadow matches, revive degraded candidates, or enable `batch_read` advice.
- Runtime advice requires a separately verified comparison contract. The
  current profiler does not create one.
- The current live store has produced workflow profiles and observations but
  no verified Fast Path candidate.
- `batch_read` is a bounded read-only utility. It is not a learned Recipe,
  does not prove equivalence, and may be slower than native parallel tool
  calls because its internal execution is sequential.

Two measurement defects remain relevant if this work is ever activated:

1. Workflow profiling does not cleanly separate model-visible top-level calls,
   nested physical operations, and provider tool rounds. A nested
   `batch_read` call can therefore contaminate the learned sequence and cost
   comparison.
2. The runtime has no real replay/live shadow producer that executes a
   candidate and compares its evidence with a matched control under the same
   workspace state.

These are reasons not to activate the feature, not reasons to add an immediate
schema migration to an unused subsystem.

## Product Contract Worth Keeping

If evidence later justifies implementation, the learning unit is a verified
read-only evidence segment, not a complete Run and not a list of tool names.
Each segment must define:

- typed inputs and bounded read scope;
- a deterministic dependency graph;
- expected evidence fields;
- provenance from every field to a real tool result;
- workspace, tool-schema, Skill, and environment fingerprints;
- a matched ordinary-path baseline;
- quality comparators appropriate to each field;
- a same-turn fallback to ordinary planning.

The first version must exclude shell, network, credentials, approvals, writes,
deletes, process control, and external effects. A Recipe is evidence collection,
never execution authority.

## Activation Gates

Do not start a new implementation plan until daily-driver evidence satisfies
all of these gates:

1. At least 20 comparable read-heavy work units across at least three distinct
   task or workspace families show repeated evidence segments.
2. Physical operations, top-level calls, model tool rounds, duration, tokens,
   provider/model, failures, and verification outcome are measured separately.
3. The candidate segment accounts for a material cost: either at least 30% of
   model tool rounds in its cohort or a reviewed latency/token hotspot.
4. Representative eval cases define the evidence fields that may not regress.
5. A reviewed owner accepts the shadow/canary I/O cost and a rollback policy.

An arbitrary date, profile count, or ordinary success count is not an
activation signal.

## Smallest Future Plan

Once the gates hold, create one dedicated plan with these stages:

1. **Measurement correction.** Separate nested physical operations,
   top-level calls, and model rounds. Stop any remaining unverified advice.
2. **Provenance substrate.** Record bounded typed observations and immutable
   workspace mutation epochs without placing raw tool output in the hot path.
3. **Recipe proposal and validation.** Build candidates from repeated segments;
   deterministically reject unbound inputs, scope widening, unsupported tools,
   missing provenance, or schema drift.
4. **Real shadow.** Replay stored observations first, then perform bounded live
   shadow only under the same mutation epoch. `Inconclusive` never counts as a
   match.
5. **Canary and fallback.** Let a small matched cohort consume candidate
   evidence, compare it with controls, and fall back in the same turn on any
   mismatch or execution failure.

Quality equivalence is the hard gate. Only after it passes may reduced model
rounds, tokens, or latency justify promotion.

## Explicit Non-Goals

- No automatic whole-task compilation.
- No ingress LLM classifier.
- No model-authored script promoted after one successful Run.
- No mutation, network, credential, approval, or sandbox authority.
- No shadow credit from an ordinary successful Run.
- No promotion based on “no error” or model self-report.
- No new `run_fast_path` tool or Recipe schema before activation gates pass.

## Relationship to Active Work

This decision contributes no priority by itself. `docs/STATUS.md` remains the
only priority list. Safe self-evolution continues to collect bounded
observation evidence, and ordinary planning remains the fallback and source of
truth.
