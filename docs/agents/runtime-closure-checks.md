# Runtime Closure: Portable Checks and Evidence

Read the sections named by the assigned package in the [execution guide](runtime-closure-execution.md). These are synthetic reproduction and acceptance specifications, not claims that the repair or live-provider tests already passed. They require no production database, customer command corpus, private report, or external cloud action.

## Evidence and baseline

The 2026-10-15 inspection used HEAD `a698ee9c58594c5682938a99c7b097f34320266c` plus dirty source. HEAD alone is not that baseline. R0 captures the complete required snapshot; compare behavior when sources differ rather than restoring files by modification time.

A portable handoff may contain `base-head.txt`, `tracked.patch`, `untracked.tar.gz`, and `manifest.json` with SHA-256/mode entries for the complete working source and expected deletions. Reconstruct the base in an isolated checkout, check/apply the tracked patch, unpack only relative in-root untracked paths, restore the recorded executable bits, then verify every manifest entry. Git may ignore local file-mode changes, so the patch alone does not restore those bits. Keep the original checkout intact. Such a snapshot preserves unreviewed work; its existence is not approval to merge, publish or execute all of it. The handoff records its archive hash separately, without introducing a self-referential source hash.

The five files below matched the earlier isolated failure probes on reinspection. Hashes identify evidence, not a complete build or an installed daemon:

| Source | SHA-256 at inspection |
| --- | --- |
| `internal/tools/external_effect_observation.go` | `cd5bdc41354d8640e72aed012c9ed137587e292d69a925298bdcaf73fa15a51b` |
| `internal/tools/middleware.go` | `bb154a8124a65f7c22f580796bcc3eee03bd42daf4a9d834525b50fc42df94d2` |
| `internal/tools/grant_prefix.go` | `6af59280a646f4373774acd0ba5927ede22310913132198860c71c73df62bcec` |
| `internal/tools/approval_observation.go` | `5f864c494ca0776e38bde95a776e1568030ed9b30dfd011a190ae797132ce81d` |
| `internal/gateway/httpapi/run_coordinator_lifecycle.go` | `a8392ab319d93eeb7ac9328ee4e67e4f0a23b40633e2d249077b8325c40e007d` |

Use temporary directories/databases, fake process receipts and fake judges through the existing production interfaces. Never execute publishing or hostname-setting commands for these tests. Test names may follow local conventions; report exactly which names ran. A compiler/setup failure is not the expected red regression.

## R1 — Five reproduced failures

| Check | Fixture and operation | Required result and positive control |
| --- | --- | --- |
| S1 Unrelated observation | Fake `terminal` dispatch of `npm publish` returns nonzero/unknown and leaves one own-chain claim. Follow with successful fake `cat unrelated.txt`; repeat with `ps -ef`, without `effect_claim_id`. | Claim remains unresolved, next conflicting effect is refused. Existing explicit same-claim, later, successful read-only observation can release it; failed, earlier and different-person observations cannot. Main supplies relatedness, runtime verifies provenance and process success. |
| S2 New restriction after grant | Seed the live Run grant for `chmod +x script.sh`; current intent says “Leave script.sh unchanged.” Fake judge denies. Track body entries and judge calls. | Body entry count is zero; old grant does not bypass current restriction. An unchanged authorized action still follows normal policy; full-auto cannot bypass the hard floor. |
| S3 Provider versus caller stop | Live caller context, no accepted final outcome, `runErr=context.DeadlineExceeded`. Separately cancel the caller using the normal stop path. | Provider failure remains unfinished/resumable, not terminal user cancellation. Explicit stop revokes unused authority and stays cancelled. A previously committed valid outcome is preserved under the existing finalization rules. |
| S4 Read versus setter | Classify `hostname`, then `hostname renamed-host`, without executing either. Include a benign read-only option if the catalog supports it. | Setter is not proven read-only. Supported read-only form remains usable; unknown options fail closed. |
| S5 Script identity | In temporary roots create `a/skills/demo/scripts/run.sh` and `b/skills/demo/scripts/run.sh`, with different contents and target arguments. Inspect reusable-grant eligibility. | They never share authority merely as `skill:demo run.sh`. Restoring both to ungrantable interpreter calls is valid; this package need not invent a hash-bound family. Keep existing safe non-interpreter grants working. |

Add at least one changed path/wording/target variant preserving each invariant and its positive control. Assert the actual middleware/store/finalizer result, not only a helper's string. S5 proves an identity collision risk, not that a full persistent-authority exploit was previously demonstrated.

## R2 — Human waits

| Check | Required observation |
| --- | --- |
| H1 Exact session | A asks and disconnects while B remains online; B's presence does not hold A. Timely A answer continues the same pending invocation. |
| H2 Pressure and drain | Queued work needing capacity causes checkpointed park; drain does likewise. No input/approval is discarded and another Run can advance. |
| H3 Crash boundaries | Crash during hold, before answer claim, after claim, and after approved execution. Recovery retains exact ownership; observed effects execute once. |
| H4 Durability failure | Checkpoint/event/answer-binding write fails. Wait remains visible and cancellable; no false safe release, lost human decision or silent delivery suppression. |

Reuse `human_wait_hold_test.go`, `human_wait_hold_crash_test.go`, `human_wait_hold_invariance_test.go` under `internal/gateway/httpapi`. Existing tests are starting points; explicitly add the two-session counterexample where missing.

## R3 — Plans and continuity

| Check | Required observation |
| --- | --- |
| P1 Accepted projection | Storage, selector, kernel open-step list and UI use the same selected Run/accepted version. Refused updates and late parent/other-session events do not advance it. |
| P2 Exact carry | Across three approval/watcher/provider continuations, valid progress and exact evidence sources persist without selecting the current Run again. |
| P3 Changed inputs | Related input/criterion/scope change or current failed check blocks affected reuse. Unrelated changes retain other valid steps. Historical success never erases current failure. |
| P4 Manual adoption | Manual historical selection keeps Main's existing explicit adoption and reason; malformed/stale/foreign references fail. A historical pass alone does not make the child's final verdict passed. |

Start with `run_plan_carry_test.go` and `run_plan_reuse.go` in `internal/control`, plus `exact_continuation_edge_test.go`, `adopted_verification_outcome_test.go`, `approval_resume_plan_test.go` and `context_selector.go` in `internal/gateway/httpapi`. Use renderer tests for the CLI side rather than changing transcript state from prose.

## R4/R5 — Contract, final body and manual working set

| Check | Required observation |
| --- | --- |
| F1 Bounded input correction | Compact inputs keep complete accepted receipts. Unknown `criterion`/`target` are not silently dropped. A not-dispatched finish input error gets bounded correction; unchanged errors and rejection do not loop. |
| F2 One accepted body | Successful final Plan + finish is ordered; accepted body survives storage/restart/replay and is not regenerated. Legal summary-only/native-phase inputs retain compatibility. |
| F3 Goal judgment | Read-only investigation of a negative condition can finish with that negative finding. A request to fix the same condition remains unfinished until satisfied or explicitly handed over by the user. |
| F4 Commit and delivery | Outcome/body persistence failure preserves unfinished status; notification failure after commit retries delivery through the existing path without executing work again. |
| M1 Ambiguous A/B | One person has two unfinished tasks in one workspace, then in different workspaces. Hints are bounded references; Main chooses the exact intended Run, without Thread-based authority. |
| M2 Current evidence | A previously read file changes or an old Skill is replaced/expired. Working-set hints do not assert current content or silently activate instructions. |
| M3 Isolation | Another CLI's output cannot update local focus, and an unbound/other-person endpoint cannot obtain hints, artifacts or pending approvals. |

Reuse `plan_input_flow_test.go`, `finish_correction_flow_test.go`, `condition_persistence_test.go` and `human_wait_hold_invariance_test.go` in `internal/gateway/httpapi`; `finish_recovery_test.go`, `contract_transport_test.go` in `internal/tools`; and `final_answer_assembly_test.go` in `internal/kernel`.

## Pending C1/C2 and experimental checks

C1, after approval: explicit single-claim compound decision succeeds atomically; ordinary once leaves the claim intact; multi-claim/in-flight/foreign/expired requests cannot settle through that option; duplicates replay, independent state change refuses stale authority; injected settlement/decision/outbox failures leave no partial commit. Wakeup and redelivery execute the approved effect once. Start with `external_effect_settlement_test.go` in `internal/control` and `external_effect_settle_test.go`/`approval_options.go` in `internal/gateway/httpapi`.

C2, after approval: transient failure then valid decision uses exactly two attempts within the shared deadline; final failure asks the human; deny/escalate/cancel/missing judge do not gain retries; late first reply cannot override a later decision; changed intent invalidates the pending review. Include underlying transport retries in elapsed/call accounting. Start with `approval_triage_test.go` and the configured cheap-role judge adapter; no live model is needed to prove fencing.

X1/X2/X3 begin only with a recorded experiment budget and fixed candidate source. Preserve model, wire parameters, tool results, permissions and output limits as controlled inputs. Report each provider separately. For X3 also assert held-call budget refund, failure stopping the tail, second Plan changes, Skill expiry, and refusal when goals/criteria/scope change; no general terminal parallelism.

## Commands from the candidate checkout

Build a local candidate instead of invoking a globally installed older binary:

```sh
closure_check_dir=$(mktemp -d)
go build -o "$closure_check_dir/selfmind" ./cmd/selfmind
"$closure_check_dir/selfmind" docs check
```

The following commands use currently existing test names. Record the matched test count and retain their output. Missing WIP tests must be restored from the approved baseline snapshot or implemented against the specifications above; a zero-match pass is insufficient.

```sh
go test ./internal/tools -run 'TestFinishInputRefusals|TestFinishRefusalsThatAreNotInputMistakes|TestExplicitDeny|TestHardFloorAboveTriage' -count=1
go test ./internal/control -run 'TestExactContinuation|TestCarriedProgress|TestAChangeInAnIntermediateRun|TestAFailedRecheck|TestOtherContinuations' -count=1
go test ./internal/gateway/httpapi -run 'TestCompactPlanInput|TestFinishWithAnUnavailableEvidenceReference|TestASecondInvalidFinishInput|TestApprovalAnsweredInsideTheHold|TestCrash|TestAnswerInPlaceAndAnswerAfterPark' -count=1
```

Run S1–S5 and newly added tests by their actual names as well. For affected message paths, update the relevant committed replay rather than relying on mocks:

```sh
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" eval run evalcases/reliability/resume-adopts-prior-check.yaml
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" eval run evalcases/reliability/resume-changed-criterion-rechecks.yaml
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" eval run evalcases/reliability/inspection-negative-finding.yaml
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" eval run evalcases/reliability/completion-unfinished-without-handoff.yaml
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" eval run evalcases/reliability/completion-user-takeover.yaml
SELFMIND_EVAL_VCR=replay "$closure_check_dir/selfmind" selfcheck --fast
```

Eval data is isolated by default; use scenario-owned scratch workspaces and never `shared_data: true` here. A changed prompt needs real-provider recording under an approved budget plus committed replay before release. No live fallback, weakened error budget, or mocked “provider success” substitutes for that evidence. Apply the existing CI-deferred policy to genuinely non-local evidence.

Before pushing, run `go test ./...`, applicable `-race`, full `selfcheck` without `--fast`, documentation checks and required Linux/macOS x64/arm64 builds. Carry released upgrade fixtures if storage changes are actually required. This guide adds no schema migration.

## Fault matrix and completion record

For each changed control path inject: failure before dispatch; after effect before saved result; before durable finalization; after commit before notification. Assert input retention, exact identity, no duplicated effects, no erased unfinished work and recoverable delivery. Use real HTTP/dispatcher wiring for message behavior and deterministic fakes for external effects. Real IM testing remains deferred.

The handoff records source/snapshot/binary hashes, package/check IDs, exact commands and outcomes, positive controls, replay provenance, provider gaps, and pending decisions/deployment. Report `implemented`, `verified`, `installed`, and `observed in use` separately. A correct already-existing implementation may close a package with evidence and no new product diff.
