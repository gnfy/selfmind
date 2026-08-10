# SelfMind Implementation Status

> **Read this first.** This is the current-state snapshot for any AI/coding agent
> picking up work, and the **only live priority list** in this repo. The code is
> the ground truth; this page summarizes it so you do not re-implement something
> that already exists. The north star (Phase 1 = cross-endpoint continuity) and
> the acceptance scenarios live in `docs/identity-continuity.md`. Historical
> planning docs were removed from the tree (2026-07-03; retrieve via git
> history) — never resurrect their backlog items or code samples.
>
> **Snapshot date:** 2026-08-10. When you finish a change that moves a row,
> update this table in the same PR. See `docs/phase1-modules.md` for the
> Phase-1 feature-module index.

## Health

- `GOWORK=off go build ./...` — passing.
- `GOWORK=off go test ./...` — passing.
- ~336 Go files, ~74.5k LOC, 109 test files (2026-07-05).

## Status Legend

- ✅ **Done** — implemented and exercised; safe to build on.
- 🟡 **Partial** — works but has a known gap or acknowledged limitation.
- ❌ **Missing** — planned in a roadmap doc but not started.

## Capabilities

| Area | Status | Notes |
|------|--------|-------|
| Provider runtime | ✅ | 13+ built-in profiles; credential precedence; live model catalog + 1h cache. **Primary model configuration convergence (2026-07-28):** new writes use one foreground selection at `models.primary`; `models.roles` contains exceptions only and absent roles inherit primary. Legacy `agent.provider/model` and `model.provider/default/context_length` remain readable and `selfmind config upgrade` migrates them with a backup. Reasoning/service-tier values are optional (`auto` = provider/model default, no forced wire value) and dynamically validated when capability metadata exists. Codex local `models_cache.json` supplies context window, default/supported reasoning levels, service tiers, and vision capability for `codex-cli`; diagnostics report the winning context source. `/model` is display-only in the shared-daemon TUI; `selfmind model set ...` updates config and `gateway restart --drain` activates it. `internal/modelruntime`, `internal/platform/config`, `internal/cliapp/model_commands.go`. |
| OAuth / token refresh | ✅ | Codex CLI and `minimax-oauth` auto-refresh expired tokens. Claude Code / Gemini CLI / Qwen CLI are reuse-only (re-login on expiry). |
| LLM protocol adapters | ✅ | `openai_chat`, `openai_compatible`, `anthropic_messages`, `codex_responses` + transport registry + provider quirks. `internal/kernel/llm`. **Transport resilience (Package Zero, 2026-07-05):** the agent retry loop (`agent.go` `streamChatWithRetry`/`chatResponseWithRetry`) now uses exponential backoff `base*2^(attempt-1)` with `[0.9,1.1)` jitter (defaults base 300ms, cap 30s, attempts 5), a ctx-cancellable sleep so `/stop` interrupts a pending backoff, and retryable-vs-fatal classification (`llm.IsRetryableError`): EOF/reset/refused/`net.Error` timeout/5xx/429/stream-idle retry, while context-window/quota/401/400/invalid-request fail fast (no wasted attempts). 429 `Retry-After` (header folded into the error + codex/OpenAI "try again in N" body phrasing) is honored via `llm.RetryAfterFromError`, capped 600s. The codex SSE idle watchdog (`responses_adapter.go`) is config-driven (default 180s) and aborts a stalled stream with a retryable error so the loop reconnects. Shared provider HTTP client sets TCP keepalive (30s) so dead sockets surface fast. Store=false/stream-only contract and no cursor-resume unchanged. Config knobs: `agent.llm_max_retries`/`llm_retry_base`/`llm_retry_cap`/`llm_stream_idle_timeout`. `internal/kernel/llm/retryable.go`, `httpclient.go`. Tests: `llm/retryable_test.go`, `llm/responses_idle_test.go`, `kernel/agent_retry_test.go`. |
| Native tool calling | ✅ | Native `tool_calls` first, `[TOOL:...]` fallback, repeated-failure guardrails, secret redaction. **Cross-protocol schema normalization (2026-08-10):** source tool definitions omit empty `required`; every native-tool adapter normalizes detached schema copies recursively, Chat/Anthropic omit empty or invalid `required`, and Responses applies its stricter empty-array rule. This fixes optional-only tools such as `delegate_task` being serialized as `required:null`, which strict OpenAI-compatible endpoints reject before inference. **Registration-time schema governance (2026-08-10):** `tools.Registry` now compiles and hashes one detached provider-neutral schema per tool. Deterministic shape defects are repaired; ambiguous defects quarantine only an external MCP/plugin tool, while any non-active built-in schema fails startup. MCP retains its full raw nested `inputSchema`; `/diag tools`, gateway status, and doctor expose redacted active/repaired/quarantined health. `selfmind model check --live [--role <name>]` and `doctor --probe-models` exercise a real optional-only native-tool request. Tests: `tools/schema_compile_test.go`, `tools/dispatcher_test.go`, `httpapi/diag_tools_test.go`, `llm/tool_schema_test.go`, `llm/adapters_test.go`, `llm/responses_adapter_test.go`, `app/model_probe_test.go`. |
| Task strategy / intent routing | ✅ | Agent-first; coarse `TaskStrategy`; rules/hybrid/llm intent. `internal/kernel/task_strategy.go`, `internal/gateway/router`. **Implicit-continuation upgrade REMOVED (P3, 2026-07-06):** the 2026-07-05 pre-agent "continue vs new" LLM call (`router.UpgradeTaskToContinueWithLLM`, `intent.continue_window`) is gone — working context is the person-level spine, so task attachment no longer affects what the model sees and the call bought nothing. Rules-based `IntentContinue` (explicit cues, short acceptance) remains and still drives the busy/steer path. `intent.continue_window` in existing configs is a deprecated no-op (`config.IntentConfig`). Do not reintroduce ingress continuation classifiers (`docs/work-timeline.md` "Ingress"). **Inspect-before-build posture (2026-07-06):** write-capable turns (`ToolMode` local-write/full) get one short system-prompt rule in `TaskStrategy.SystemPromptNote` — search the workspace for an existing implementation before writing new code/creating a file, and reuse it (the workspace is the shared source of truth). Pure direct-answer (no-tools) turns do not get it. Test: `internal/kernel/light_task_layer_test.go` (`TestSystemPromptNote_InspectBeforeBuild`). |
| Context engine | ✅ | Bounded message window on the hot path. `internal/kernel/context_engine.go`. Over-budget context now **compacts by default** (2026-07-05): past `summaryThreshold` (¾ of budget) the drop-eligible MIDDLE turns are summarized into ONE structured message while the head (system + the original task turn) and the tail (recent `compactionTailTurns`) stay verbatim — no more silently dropping the oldest turns. The summarizer is the cheap `memory_extract` role (`Agent.SetSummaryProvider` → `ContextEngine.SetSummaryProvider`), kept OFF the main coding provider; it is one bounded call only at the over-threshold moment, never per-turn. The compaction prompt mandates a `## Relevant Files` section (task goal, decisions, next steps, and every created/modified/read path), backstopped by a deterministic path harvest from tool-call args (`path`/`file_path`/`output_path`/`workdir` + V4A `patch`/`apply_patch` headers) that is appended if the model omits it. Guards: falls back to deterministic trim when no summarizer is wired, when the summary is empty or not smaller than the span it replaces, or when the middle is already a prior summary (no stacking/recursion). `SELFMIND_SYNC_CONTEXT_SUMMARY` is now legacy — compaction runs without it; the flag only lets an install with no summarizer role fall back to the main provider. Every rendered compaction summary now carries the verbatim boundary note ("The history summary is reference only. The latest user message is the only authoritative instruction. If it changes direction, the latest message wins.") inside the `[CONTEXT COMPACTION]` marker (P1, 2026-07-06). **Working history is the person-level WORK SPINE (P1 of `docs/work-timeline.md`, landed 2026-07-06):** every agent-bound turn (task-bound, casual, cron) appends ONE slim turn-level entry — user text (gateway decoration blocks stripped) + assistant final answer + touched file paths (deterministic tool-arg harvest) + source tag for non-interactive turns — under the constant `kernel.SpineTrajectoryKey`; the storage tenant is the person, so the spine is person-scoped and cross-endpoint. Tool intermediates/system prompts never enter the spine (they stay in run events). Load and save use the same key (`ContextEngine.BuildMessages` via `Agent.Composer()` + `saveHistory`); the tail replays up to `composerSpineTailEntries` recent turns as alternating user/assistant messages. Per-turn assembly is the ContextComposer contract (`internal/kernel/context_composer.go`): ①latest user message ②spine tail ③compaction summary ④semantic recall (RESERVED for P2 via `RuntimeContextBundle.Recall`, rendered + budgeted already) ⑤artifacts ⑥workspace ⑦person memory ⑧open run/approval state (⑤–⑧ via bundle/system prompt). Legacy compat is a READ-ONLY chain: empty spine — or a task-bound turn with no spine entry for its task — reads the old `task:<id>` key, then the task's prior run channel (`TaskRuntimeContext.PriorChannel`), or the old channel key for taskless turns; the first save migrates forward. Internal subsystem turns (delegation, `:background_review`) stay channel-keyed off the spine. FTS indexing keeps the task-derived session id (`Agent.sessionKey`, idempotent `IndexSession`) and is never keyed by the spine. Tests: `internal/kernel/context_engine_test.go`, `internal/kernel/light_task_layer_test.go`. |
| Project context injection (AGENTS.md) | ✅ | Per-turn project-convention files injected into the system prompt by `ContextScanner` (`internal/kernel/context_scanner.go`), assembled on an **independent budget** from the person-memory layer (facts+profile, `buildSystemPrompt`) so raising one never starves the other. **Route-3 fusion (2026-07-09)** — Codex discovery depth + Hermes budget elasticity + selfmind's own memory kept separate: (1) **root→leaf discovery** — walk up from the workspace root to the git root, collect the highest-priority context file at EACH level (`.selfmind.md`→`AGENTS.md`→`.cursorrules`→`.claude.md`→`CLAUDE.md`; README removed — human-facing, low signal), emit **root→leaf** so a deeper/more-local `AGENTS.md` sits last and overrides (the model is told deeper wins); (2) **dynamic budget** scaled to the model context window (`window_tokens × 4 chars/token × 6%`, floor 24KB, ceiling 256KB) instead of the old fixed 8KB/16KB — a big-window daemon rarely truncates, a small-window model stays bounded; (3) **never silently drop** — the old ">8KB ⇒ skip the whole file" (which dropped every real AGENTS.md: this repo's is 36KB, hermes's 71KB) is gone; over-budget files are head/tail truncated (70%/20%) with a pointer telling the model to `read_file` the full path for the omitted middle; each level gets a fair per-file share so deep/local files always survive; (4) **untrusted-data fence** — the block is wrapped as workspace-provided conventions that operator/user instructions and safety policy OUTRANK (defense against a malicious `AGENTS.md` in a cloned repo injecting instructions via the IM path). Follow-ups (not in this pass): a promptware DETECTOR (vs the current fence), and moving the block into a stable prompt-cache prefix. `internal/kernel/context_scanner.go`; tests `internal/kernel/context_scanner_test.go`; eval `evalcases/…` (a >8KB AGENTS.md convention is honored). |
| Control store | ✅ | tenants/persons/accounts/workspaces/tasks/runs/events/handoffs/approvals/grants/notifications/outbound/person_settings/`task_queue`/etc. `internal/control/store.go`. |
| Memory + session search | ✅ | `AddFact`/`GetFacts`, FTS recall, memory fence. **Task-coherent recall (2026-07-06):** a task's turns are FTS-indexed under a stable task-derived session id (`task:<id>`, `Agent.sessionKey`) so `session_search` retrieves the whole task cross-endpoint ("what we did on the order system") as ONE session rather than a fragment per turn; `IndexSession` is now idempotent (delete-then-insert per session id) so re-indexing the growing trajectory each turn does not accumulate duplicate FTS rows. Person/tenant scoping unchanged. Tests: `internal/kernel/memory/sqlite_provider_test.go` (`TestSQLiteProvider_TaskSessionRecall`). **Automatic recall v1 (P2, 2026-07-06):** recall is no longer tool-only — the gateway context selector attaches ≤3 bounded "possibly related prior work" slices per turn (session FTS + task label cards, `semantic_recall`-role query expansion when configured) as ephemeral `TaskRuntimeContext.RecallSlices`; see the Work Timeline P2 row above and `docs/work-timeline.md` "Semantic recall". Tests: `internal/gateway/httpapi/recall_test.go`. **Memory governance P0 (2026-07-11, docs/memory-governance.zh-CN.md):** (1) layered model landed — `memory_observations` (immutable evidence) / `canonical_memories` (revisable read model) / `memory_evidence` / `memory_events` tables in memory.db; opening a tenant DB incrementally imports legacy `facts` rows as observations, folding same-`NormalizedContentHash` duplicates into one canonical with evidence counters (idempotent by observation id; `facts` untouched; profile target skipped); read surface via `MemoryManager.Canonical()` (`internal/kernel/memory/canonical.go`). (2) maintenance idempotency — `maintenance_jobs` (control.db, UNIQUE(run_id, analyzer_version)) born inside the `FinishRun` terminal transaction; the post-run analyzer CLAIMs via CAS before model work, completes with a result hash, fails with a bounded retry horizon; crashed 'running' jobs reset to pending in the stuck-run sweep. One run = one logical maintenance result; one provider request may cover several same-tenant/person/workspace jobs after the debounce window. (3) intake decisions — the daemon-batched maintenance analyzer returns one run-keyed result that judges candidates AGAINST offered neighbors (top-8 similarity ∪ recent-5 per target; recent slice is the cross-language dedup net) and returns `memory_decisions` (SKIP/ADD/REINFORCE/SUPERSEDE/CONFLICT); the deterministic policy layer (`internal/app/memory_intake.go`) enforces: refs resolve only against OFFERED facts (matchOpenLabel pattern), REINFORCE bumps confidence + last_verified (RepetitionBoost) without rewriting, SUPERSEDE needs conf≥0.98 and never touches SourceUser facts (degrades to CONFLICT = both kept), invalid refs degrade to ADD through the dedup net (exact/containment duplicates now REINFORCE instead of being silently dropped). All analyzer writes now land in the learning audit (visible in `/memory history`, undoable). Agent memory tool add/replace/pin write full metadata (`SourceAgent`/`SourceUser`), replace keeps the fact id. **Human governance view (2026-07-11):** `/memory` is now a compact health/category directory rather than a fact dump; `/memory category <name> [page]` provides ranked, paged short refs, `/memory conflicts` isolates required attention, and `/memory show <ref>` explains canonical status, protection and supporting observations. Tests: `memory/canonical_import_test.go`, `control/maintenance_jobs_test.go`, `app/memory_intake_test.go`, `app/post_run_analyzer_test.go`, `tools/memory_test.go`. **Memory governance P1 — consolidation (2026-07-11/12):** background two-stage consolidation shipped and wired (`app/memory_consolidator.go`; `httpapi/memory_governance.go` loop started by the runner; foreground runs always win; per-person 5-min bound; cluster-level judgement checkpoints). Mode gates apply: `shadow` (default) writes nothing but DRY-RUNS the same deterministic gates and annotates `would_apply` in the report, `merge-only` applies gated MERGE (confidence ≥0.95 + no-novel-token check) / REINFORCE (≥0.90 + canonical must restate one member VERBATIM — the member's original text is written, never model wording) / ARCHIVE (≥0.90, reversible), `full` adds global/per-workspace caps + age archival. SUPERSEDE is report-only for consolidation (owner decision 2026-07-12; intake owns supersede). Judgement checkpoints carry `consolidationJudgeVersion` — bump on any judge-prompt/gate change to invalidate cached decisions. Read path is on the canonical transition read model (`agent.go` injection + memory tool via `MemoryManager.Canonical()`). Tests: `app/memory_consolidator_test.go`. Remaining: record the `memory-pinned-recall` cassette; recalibrate caps from post-merge steady state before enabling `full` (a 120 cap against today's ~780 actives would mass-archive in one pass). |
| Skills system | ✅ | list/view/manage/catalog/bundle/curator; history + undo; provenance; governance archive/restore. `internal/tools/skill_*.go`. Auto-create via `SpawnReview` (scripted-provider end-to-end: `background_review_integration_test.go`) and curator governance (pin/manual protection, archive audit + restore: `skill_curator_test.go`) now have deterministic integration coverage. Background-review change claims ("skill created/updated/patched: <name>") are now verified against the toolchain (`skill_view` through the restricted backend) before notifying — a hallucinated claim with no tool call is detected, logged, and reported as no-change instead of being forwarded. **Durable review jobs (execution-quality W7, 2026-07-12):** in the daemon, `SpawnReview` no longer fires an unsupervised goroutine — it serializes a bounded snapshot (≤8 tail messages, ≤2KB each) and enqueues an idempotent `maintenance_jobs` row (payload-hash key, version namespace `httpapi.SkillReviewJobVersion`); the maintenance worker executes ≤2 per pass via CAS claim → `RunReviewFromPayload` → complete/fail-with-retry (10min horizon, 5 attempts), so reviews survive daemon crashes, dedupe, and stop competing with foreground work. No enqueue wired (eval/tests/CLI-only) keeps the immediate in-process path. `kernel/background_review.go`, `httpapi/maintenance_worker.go`, runner wiring. Tests: `httpapi/skill_review_jobs_test.go`. |
| Skill metrics + pruning | ✅ | `internal/kernel/skill_store.go` RecordCall/Prune. (Roadmap lists this as "to do" — it is done.) Deterministic Prune coverage in `internal/kernel/skill_store_test.go`. |
| Learning audit | ✅ | Tenant JSONL log + per-change snapshots + undo. `internal/tools/learning_audit.go`. |
| Multi-agent delegation | ✅ | Parallel, semaphore-bounded batch delegation. `internal/app/multi_agent.go`. (Roadmap lists this as serial-only — it is parallel.) **Delegation depth bounded (2026-07-09):** previously `MakeDelegateFn` handed a sub-agent the SHARED parent backend (delegate_task included) on empty toolsets, and the `maxDepth` field was never read — a sub-agent could call `delegate_task` again forever (runaway token/recursion mine). Now a HARD STRUCTURAL bound: sub-agent backends are always freshly cloned (never the shared dispatcher, so the parent's wiring can't be mutated) with `delegate_task` stripped unless the depth budget allows another hop, in which case a fresh nested delegate wired to depth+1 is added; at `depth == MaxDepth` the sub-agent is a leaf with no delegation tool. Config `delegation.max_depth` (default 1 = flat), `max_concurrent` (5), `max_subtasks` (16, batch fan-out cap → clear error when exceeded). `internal/app/delegation.go`, `internal/app/multi_agent.go` (`SetSubBackendBuilder`), `internal/platform/config/loader.go`. Tests: `internal/app/delegation_test.go`. |
| Extended tools | ✅ | `web_search`, `web_extract`, `execute_code`, `delegate_task`, vision, tts beyond file/terminal. **Web search rebuild (2026-07-12):** the old default (regex-scrape DuckDuckGo HTML) returned "No results found" on the anti-bot 202 challenge that many egresses/GFW hit — the model then burned calls rephrasing and invented negative conclusions ("this car does not exist"). Now a backend registry (`internal/tools/web_search.go`, `WebSearchOptions`): hosted APIs **Tavily / Brave / Serper / Firecrawl** (structured, snippet-bearing results, config-driven keys) + **SearXNG** (self-host) + DuckDuckGo scraping as best-effort fallback ONLY. Backend selection: explicit `web.search_backend` → first credentialed backend → duckduckgo. **Honesty invariant:** a backend failure (non-200, anti-bot page, missing key) returns an ERROR ("backend unavailable … configure a hosted backend"), NEVER "No results found" — the latter is reserved for a backend that ran and got zero hits. Keys live in `config.yaml` `web:` (NOT env — the detached `setsid` daemon never inherited shell exports, which is why the old env-only Tavily/Firecrawl backends never worked); env vars remain a convenience fallback. DDG parser fixed to tolerate `<b>` highlight tags and unwrap redirect URLs; results now carry snippets. `app.InitTools` wires `cfg.Web` in. Tests: `tools/web_search_test.go`. Follow-up: codex-native server-side search passthrough when the provider supports it (quality = codex, zero key); eval case for the honest-error behavior (needs live cassette). **Tool-output artifacts (execution-quality W1, 2026-07-12, docs/execution-quality-plan.zh-CN.md):** capture is capped at 2MiB head/tail (codex-style bound; the dropped middle exists nowhere); a tool output over the 24KB model budget is spooled by the per-run gateway sink (`kernel.ToolArtifactSink` → `httpapi/tool_artifacts.go`, per-run caps 32 artifacts / 64MiB) to `<data>/tool-output/<person>/<art_id>.txt` plus a durable `tool_output` row in `control.task_artifacts`; the truncation note now references the artifact and the new read-only, parallel-safe `tool_output_view` tool (person-scoped filesystem resolution, ≤24KB per read) replaces the old "re-run a narrower call" advice. Artifact ids render in the per-turn Relevant Artifacts block (`TaskArtifactContext.ID`) and the resume context lists `large_tool_outputs`, so continuations — cross-endpoint included — re-read by reference instead of re-running commands. Same-turn aging: artifact-backed tool results older than 3 iterations shrink in place to 4KB around the reference (`shrinkAgedToolResult`; lossless only because the artifact stays addressable). Spool failure degrades to the plain head/tail note. Tests: `kernel/tool_artifacts_test.go`, `tools/tool_output_view_test.go`, `httpapi/tool_artifacts_test.go`. Eval: `evalcases/quality/tool-output-artifact.yaml` (cassette pending — both providers hit quota 2026-07-12; record with `SELFMIND_EVAL_VCR=record … --provider kimi-coding` when quota refreshes). |
| MCP client | 🟡 | Real stdio/HTTP JSON-RPC client, multi-server, on-demand tool registration. `sampling/createMessage` not implemented. `internal/tools/mcp_client.go`. **Reader hardening (2026-07-09):** a non-numeric response id no longer panics the stdio reader (safe assertion, message dropped), and a reply arriving after the 30s waiter timeout no longer re-creates an undrained `respChans` entry (`lookupResponseChan` — only the request sender registers channels), closing the unbounded-map leak. |
| Eval loop | ✅ | Real gateway-path runs; P0 deterministic checks + state-predicate oracle (`assert_state`); VCR record/replay for free offline regression; `selfmind eval run/report/repair/scorecard/capture/clean`; day-in-the-life suite with recorded cassettes. **CI offline gate fixed (2026-07-27):** in a credential-less environment the mock-provider fallback bypassed `MaybeWrapVCR` (the wrapper rides transport construction, which never runs when resolution fails), so cassettes were never consumed and the gate failed the same 8 replay cases on every push while passing on any machine with credentials — the gate only ever worked where it was least needed. The mock fallback is now VCR-wrapped (`app/agent.go`; no-op outside VCR/flight modes), making `selfmind selfcheck` genuinely self-contained: zero credentials required. Test: `app/vcr_mock_fallback_test.go`. The macOS-only `TestSyncRunSurvivesClientDisconnect` flake was a real race in the test: net/http notices a dropped client asynchronously, and the run-completion detached check could beat the ctx cancellation on slow runners — the test now lets propagation settle before releasing the run (the same few-ms production window is an accepted residual: only the push is skipped, the outcome/handoff persists). **Data-isolated by default**: every run (record and replay) uses a throwaway temp data dir (`shared_data: true` opts out); post-case run-finalization sweep forces leftover `running` rows terminal; `selfmind eval clean [--yes]` removes historic eval residue from a real control.db. `internal/eval`, `evalcases/`. |
| Flight recorder + capture | ✅ | `SELFMIND_FLIGHT_RECORDER=1` records each real turn; `/capture` / `eval capture` promotes the last turn into a replayable eval case — everyday friction becomes a permanent regression test. `internal/kernel/llm/flight.go`, `internal/kernel/flight_recorder.go`, `internal/eval/capture.go`. |
| Telegram adapter | ✅ | Webhook + long poll, signature verify, send. |
| Personal/Enterprise WeChat (Weixin) adapter | ✅ | iLink protocol (`ilinkai.weixin.qq.com`): poll loop, AES, per-peer context_token, typing, media, group/DM policy, dedup. Built-in QR login (`selfmind weixin login`) — no external bridge needed. This is the primary multi-device WeChat path. `GetUpdates` checks the in-band `ret`/`errcode` of the HTTP-200 body, so session expiry surfaces as `weixin.ErrSessionExpired` instead of an empty success. The adapter then suspends polling, logs one actionable error, and watches the account credential file every 15 seconds. Running `selfmind weixin login` refreshes the token or `saved_at`; the live daemon hot-replaces the client, reloads the sync buffer, and resumes polling without a restart. Inbound dedup is durable: `isDuplicate` backs the in-memory 24h map with `control.Store.MarkInboundSeen` (48h retention, `inbound_dedup` table), so sync-buffer replay after a daemon restart does not re-run already processed messages. Tests: `weixin/client_test.go` (`TestGetUpdatesDetectsSessionExpiry`), `weixin/adapter_test.go` (durable dedup and credential refresh). |
| WeChat Official Account adapter | 🟡 | Inbound passive-reply + signature verify (`internal/gateway/wechat`); outbound now supported via the customer-service `custom/send` sender (`internal/gateway/delivery/wechat.go`, registered as platform `wechat`). Still no message encryption/decryption. |
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` + staged approval modes (`/mode`) done. Approval UX shipped (2026-07-04): all surfaces (control commands, `POST /v1/approvals/respond`, CLI, Telegram buttons) resolve references through one shared resolver (`httpapi/approval_resolver.go`) — list ordinal (`/approve 1`), unique `apr_` prefix, bare `/approve` with a single pending, `task_` ids rejected with a hint; `/approvals` shows tool + bounded args preview + reason + task title; CLI-originated approvals fan out to the person's other bound IM accounts (`notifyApprovalRequested` + `ListAccountsByPerson`); Telegram gets native inline approve/reject buttons (typed `delivery.Message.Kind`, persisted on the outbound row so retries keep buttons) with `callback_query` handled in both the telegram adapter and the generic `/v1/im/*` webhook; `selfmind approve/reject` returns one-line errors, never raw JSON; `selfmind send --mode` threads `approval_mode`. Remaining: the long-poll `internal/gateway/telegram` adapter is still not mounted by the daemon (generic webhook path is), and Weixin stays text-fallback by design. Outbound dispatch is claim-based (`ClaimDelivery`): the immediate attempt and the retry poller are mutually exclusive, fixing the live duplicate approval push. IM approvals are conversational and task-free (owner request 2026-07-04): the push is `Approval needed — reply y or n:` + the command/reason only (no task label, no apr_ id, no ordinal); a bare `y`/`n` (or 好/可以/不行) answers the single pending approval, degrading to a numbered `/approve <n>` list only when multiple runs have approvals pending in parallel. The task concept stays in the control plane, out of the IM UX. CLI-originated async results now fan out to bound IM endpoints (`deliverAsyncResult` → `fanOutToBoundIM`) so a fire-and-forget terminal run's final answer — including a rejection acknowledgment — is visible on WeChat/Telegram instead of vanishing. Watch items: (a) one live WeChat `/reject 1` got no reply, likely a message lost in a gateway-restart window (iLink getupdates canceled mid-poll); (b) two result pushes were `sent` (correct target, iLink API accepted) but never arrived on the phone ~4.7h after the user's last inbound message — suspect iLink proactive-push context_token staleness; verify the weixin sender checks the response errcode and consider marking undeliverable pushes failed for retry. **Pending-notification escrow (2026-07-05):** the initial push is one-shot at creation, so a CLI-attached approval whose CLI then quit used to sit pending invisibly. A `notified_at` column (`approval_requests` + `clarify_requests`, `ensureColumn` migration) is stamped only when a push actually SENDS (never when suppressed); the in-daemon 60s sweep (`run_recovery.go`) now re-pushes pending approvals/clarifies older than `gateway.pending_notify_after` (default 2m; `0` disables) whose person has since detached from the CLI and were never notified, to the single preferred IM — idempotent (marks after `EnqueueAndTry` succeeds, so a crash retries next sweep; boot sweep covers restarts). `Store.MarkApprovalNotified`/`MarkClarifyNotified`/`ListPendingApprovalsForEscrow`/`ListPendingClarifiesForEscrow`; `RunCoordinator.escrowApprovalNotification`/`escrowClarifyNotification`. Tests: `control/escrow_test.go`, `httpapi/escrow_test.go`. |
| Clarify lifecycle (G3) | ✅ | A mid-run agent question is a first-class DB-backed pending question modeled exactly on the approval waiter (2026-07-04). `gatewayClarify` (formerly a stub) creates a `clarify_requests` row (`internal/control/clarifies.go`: `Create`/`Get`/`List`/`Answer`/`Expire`/`ExpireOrphanedClarifies`), appends the `clarify.requested` event, pushes a presence-aware, single-preferred-endpoint notification through the shared `RunCoordinator.routePendingNotification` (same routing as approvals; `notifyClarifyRequested` + `delivery.KindClarify`, body `Question — reply with your answer:`), then blocks polling the row for up to 30 min. An answer recorded from ANY endpoint (`Store.AnswerClarifyRequest`) returns verbatim as the tool result; timeout/expiry returns a best-judgment fallback sentinel so the run never hangs. Inbound: a plain non-slash reply while a question is pending IS the answer (`tryHandleClarifyAnswer`, in `tryHandleControlCommand` above new-task/queue logic and below the bare y/n approval leg). Orphan hygiene rides `MarkInterruptedRuns` next to the approval sweep. Surfaced in `/status`, `/diag`, and the attach digest (`api.DigestClarify`). A question survives the CLI closing exactly like an approval (docs/identity-continuity.md "Runtime attachment model"). Tests: `control/clarifies_test.go`, `httpapi/clarify_inbound_test.go`. |
| Command output lost when reaping raced the readers (2026-08-10) | ✅ | `runCommandStreaming` called `cmd.Wait()` while both scanner goroutines were still reading the pipes from `StdoutPipe`/`StderrPipe`. os/exec tears those pipes down as soon as the process exits, so reaping first races the in-flight read — documented as incorrect usage — and the caller sees a successful command with a nil error and EMPTY output. A one-shot writer like `printf READY` has the widest window, so the loss showed up under CI contention as two unrelated-looking intermittent failures: `TestExecuteAcceptsEveryRequestShape` ("durable request produced nothing") and `TestExternalWatchCompletesOutsideAgentRun` (watch stuck `running` because its success pattern never matched). Both go through `Execute` → `runCommandStreaming`, the single output path. Fixed by draining both scanners before reaping; this does not change how long the goroutine blocks, since it already waited on the process AND the scanners before signalling. Never reproduced locally (single package, whole package, GOMAXPROCS=1, CGO_ENABLED=0, stripped env, sandbox unavailable, and 3× full-suite runs at 2 cores all passed) — the regression test instead pins the ordering contract with a fake runner whose `Wait` tears the pipes down, and it reproduces the exact symptom (`output = ""`) when the old ordering is restored. `internal/tools/builtin.go`. Tests: `command_streaming_test.go`. |
| Offline eval gate green again (2026-08-10) | ✅ | `selfcheck` had been red since 2026-07-26 (16 passed / 3 failed, 12m44s). Now **16 passed / 0 failed in 1m32s**. Two failures were the scoped-deny product bug above; the third was test-data debt: five cassettes recorded 2026-06-28 carried the recording machine's absolute paths, because `WithVCRWorkspace` portability only landed 2026-07-14. The mechanism was never broken — those cassettes simply predate it — so the fix is a migration, not a code change: the literal workspace prefix became `{{SELFMIND_VCR_WORKSPACE}}` in the five files (verified equivalent by parsing before and after, and by replaying the case from a different checkout path). Corpus hygiene is now enforced by `internal/kernel/llm/vcr_corpus_test.go`: no machine absolute paths, no NEW recorded provider failures (ratcheted against the known nine), no empty cassette directories. Recording a provider failure still writes the cassette — replay is ordinal and a hole would desynchronize the case, per `TestRecordModePreservesImmediateFailuresInSequence` — but now prints a loud warning. Two empty cassette directories removed. **Cleared 2026-08-10:** all six affected cases were re-recorded live against `codex-cli/gpt-5.6-sol`; the corpus now holds **zero** recorded provider failures and 10 clean `completion` cassettes, so that call path is verified for the first time. The record-time warning fired on 5 of 11 attempts, which is exactly how the original nine were created — each case was retried until it recorded clean. `reliability_external_watch_handoff` also needed a case fix: it polled `printf 'SUCCEEDED'`, which the registration preflight (2026-07-30) correctly refuses to watch because the work is already finished, so the handoff it asserts could never happen; the polled command is now `printf 'PENDING'` and no assertion changed. The corpus ratchet is tightened from "must not grow" to "must be zero". Remaining debt: coverage is still 16 of 86 cases. |
| Scoped deny: a prohibition constrains what it names (2026-08-10) | ✅ | `ExplicitDeny` was a bare substring scan for 不要/禁止/do not/never over the WHOLE message, and any hit made every write, exec, and dangerous call in the run demand a human decision. "不要修改文件" blocked a read-only `go test` probe; "Do not execute the polling command directly" — an instruction to USE `watch_external` — blocked the registration it was asking for. Both then hung until their run deadline, which is how two CI eval cases had been red since 2026-07-26. Prohibitions are now extracted deterministically (no model call), bound to their own clause, and resolved to operation classes (`write`, `delete`, `exec.in_turn`, `exec.delegated`, `network`) plus any literal path or command fragment named; the pending call is compared in the same vocabulary. Manner qualifiers (直接/自己/手动/directly/yourself/manually) narrow `exec` to `exec.in_turn`, so delegation survives while an unqualified "不要跑任何命令" still covers both. Fail-safe preserved: an unclassifiable prohibition keeps the blanket effect, a pre-scoping snapshot keeps the old behavior, and the dangerous-op heuristic alone never activates an unrelated deny. Unmatched prohibitions still reach the judge as stated limits. Verification: both eval cases pass with NO assertion, case-text, or cassette change (623ms and 1m16s, previously 120s and 540s timeouts); the two existing pinning tests pass unmodified. `internal/tools/run_intent.go`, `middleware.go`, `approval_triage.go`, `internal/gateway/httpapi/approval_options.go`. Tests: `deny_scope_test.go`. |
| Approval wait bounded by the caller (2026-08-10) | ✅ | The human ask waited a hardcoded 30 minutes regardless of the caller's own deadline, so any run with a shorter budget (eval cases today; cron, watcher finalization, and future delegated sub-runs next) died on `context deadline exceeded` with NO assistant output instead of taking the designed "park the work as waiting_user" path. The wait is now `min(configured budget, caller remaining - 3s reserve)`; `agent.approval_wait` (30m) and `agent.approval_wait_unattended` (30s) are configurable, and the short bound applies only when no endpoint is live AND no account is bound (presence expires after 90s, a bound account does not). With no time left the ask becomes an `approval.skipped_no_budget` run event and creates no durable row. Also fixed a pre-existing race the new tests exposed: on a tick boundary `select` could take the poll branch and read the store with an already-expired context, reporting an unanswered approval as a transport failure roughly half the time — the poll now uses its own bounded context and loop exit belongs to the Done branch. Outcome contract unchanged: a timeout is never a rejection and never an approval. `internal/gateway/httpapi/run_coordinator_lifecycle.go`, `presence.go` (`AnyAttached`), `internal/platform/config/loader.go`, `internal/runtime/gateway/runner.go`. Tests: `approval_wait_test.go`. Follow-up: `gatewayClarify` still uses a fixed `clarifyWaitTimeout` and should adopt the same derivation. |
| Background model roles in setup (2026-08-09) | ✅ | `models.roles` is NOT inherited from `models.primary` for the cheap roles: `fast_classifier`, `background_review`, and `memory_extract` must be set explicitly, and the daemon disables smart approval triage, the post-run analyzer, and memory governance when they are absent (correct per AGENTS.md "missing cheap-role fallbacks must pause learning", but invisible on a fresh install — observed live: a new instance ran with 0% auto-approval and no memory accumulation while `selfmind doctor` reported a healthy model). `selfmind setup` and the first-run TUI bootstrap now name what each missing role disables and offer reuse-foreground / different-model / skip; reusing the coding model stays an explicit labelled choice, existing hand-tuned roles are never overwritten, and the prompt runs BEFORE the gateway starts (the daemon reads roles once at startup). Declining never fails setup. `internal/cliapp/setup_roles.go`, `setup_commands.go`. Tests: `setup_roles_test.go` (reuse / skip / gap-fill-only / non-interactive guidance / interactive-without-stdin). |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| TUI rendering (terminal-first hybrid) | ✅ | **Only renderer (2026-07-10):** history committed to native terminal scrollback (`tea.Println`), only the active region redrawn (`history_commit.go`); terminal owns scroll/select/copy. The legacy alt-screen viewport (`SELFMIND_TUI_LEGACY`, `viewport`, `controller_mouse.go`, the per-message render cache) was DELETED with the in-process path — there is no `hybridMode()` switch anymore. Colored patch diffs (`renderPatchCell`), `/search current` (full diffs), `/copy`. Codex-style interactive approval panel (2026-07-05): `approval.requested` arms a bordered selector in the ACTIVE region (`ui/components.ApprovalPrompt`, wired by `gateway/cli/approval_flow.go`) — ↑/↓/j/k + Enter or shortcuts y/t/a/n mapping to grant scope ""/task/person on the existing `/v1/approvals/respond` path; Esc does nothing (explicit decision required); "No" opens a deny follow-up composer (Enter = bare deny, text = deny + mid-turn guidance); queued approvals re-arm FIFO; duplicate text notice + "Preparing to run" spinner suppressed while the panel is up; transcript keeps ONE compact `notice` line per request/decision; status bar shows `⏸ waiting approval`. IM/text approval surfaces unchanged. **Persistent input history (2026-07-20):** up/down-arrow composer history survives across sessions via `~/.selfmind/input_history.jsonl` (append-only JSONL under an advisory flock; async single-writer queue, key path never blocks; codex-style persistent-prefix + in-session-suffix merge). Top-level `history:` config (`persistence: save-all|none`, `max_bytes`, `load_entries`). Recorded entries are the EXPANDED submitted text — paste placeholders were unrecoverable after `editor.Reset()` even in-session (pre-existing bug, fixed); secure input and entries >4KiB are never recorded in memory or on disk. `internal/gateway/cli/input_history_store.go` + tests. Regression fixed 2026-07-27: pre-seeding `inputHistory` from disk made `HasConversationHistory()` always true, so a zero-input open-and-close printed a useless "Resume this session" hint on every exit — the check now looks for a user-role transcript message from THIS session (assistant/system startup content and persisted history don't count; `resume_hint_test.go`). **Exit render + look-back consolidation (2026-07-28):** exiting left an empty composer stranded between the transcript and the resume hint — bubbletea repaints the model once on graceful shutdown, and every quit path returned `tea.Quit` with the composer still in `View`. All four paths (`/exit`, ctrl+c idle, ctrl+c-with-run, exit-prompt `b`) now go through `uiModel.quitNow`, and `View` returns a single blank line while `quitting` (NOT `""` — `standardRenderer.flush` skips an empty buffer and keeps the old frame; one line lets `stop()`'s `EraseEntireLine` remove it). Commands consolidated per owner decision: `/history` deleted and folded into **`/search current`** (same full-diff pager) so "look back at work" has one entry point, and `/search` joined the shared `internal/gateway/command` registry (it was TUI-only drift, invisible to other endpoints). Bare **`/resume`** is now a codex-style picker instead of a usage error: it relays `/tasks` and presents the daemon's reply as the menu (the daemon owns ordering — a locally numbered list would drift from the resolver `/resume <n>` uses), arming a one-shot numeric shortcut that expands a bare number to `/resume <n>`; digit-leading prose still reaches the agent. Handlers that bound to `slashCommandMetas` by position now bind by name. Tests: `quit_render_test.go`, `resume_picker_test.go`, `command/registry_test.go`. Remaining: write_file overwrite real diff; `/search current` in-overlay search + `control.db` backing; `--resume` transcript rehydration (session_messages by channel) so `/search current` shows prior-session turns; the interactive `SessionBrowser` component is still unwired (client mode has no structured session-search API, only the text `session_search` dispatch), so selecting a past session from a list is not possible yet. Live plan checklist now renders in **client mode** (2026-07-05): the daemon's `plan.updated` event carries the full structured plan, forwarded by `client.eventToStream` and rendered as an `update_plan` cell so `renderPlanCell` shows the `[x]/[>]/[ ]` steps instead of a stray "plan updated" line (`agent_events.go` `forwardGatewayEvent` + `planJSONFromEvent`); `maxPlanSteps` raised 20→50 so a normal plan is never truncated. Status bar always shows the effective approval mode (`statusLine` `mode:<effective>`), learned from `GET /v1/digest` `approval_mode` at startup and updated by `/mode`. See `docs/tui-terminal-first-hybrid.md`. |
| Image paste + attachment scope (2026-07-27) | ✅ | Fixes the observed-live paste chain break: (1) **command-shape routing** — the shared predicate `command.LooksLikeCommand` (only `^/[A-Za-z][A-Za-z0-9_-]*$` first tokens are command territory) replaces every bare leading-"/" check (TUI submit, gateway reject gate, `tryHandleControlCommand`, queue-drain poison flush, clarify-answer gate), so "/mnt/c/…png <question>" is agent-first message text, never "Unknown command"; a "/"-leading path is now also a valid clarify answer. (2) **image placeholder tokens** — a clipboard paste registers as `Editor.AttachImage` → compact `[[ image:N name ]]` token (mirroring `[[ paste:N ]]`), never a raw path in the composer; `ExpandValue` substitutes the path back at submit (existing path→attachment scan unchanged), the transcript echoes the compact display form, and the gateway 400-rejects unexpanded `paste`/`image` tokens (`input_validation.go`). WSL clipboard saves now honor the unique timestamped file name (second paste no longer overwrites the first). (3) **attachment scope channel** — `RunCoordinator.importAttachments` copies attachment files into `<data>/attachments/<person>/<run>/` (8 files / 20MiB caps, sanitize, degrade-to-original-path) and the person's partition joins the run's `AllowedRoots`, so `read_file`/vision can read the managed copy while OS temp dirs stay out of scope. (4) **vision_analyze scoped** — its local-path branch obeys `WorkspaceScopeMiddleware` like `read_file` (was: raw `os.ReadFile` any path, an AGENTS.md scope violation); http(s) URLs stay on the SSRF path. Tests: `command/registry_test.go`, `ui/components/editor_image_test.go`, `httpapi/attachments_test.go`, `httpapi/input_validation_test.go`, `tools/workspace_scope_test.go`. Eval: `evalcases/reliability/path-leading-message-not-command.yaml` (cassette pending — record on next live run). Remaining: queued-message attachments import lazily at drain (temp file may be gone by then); IM inbound media → attachment channel not wired yet. |
| Update UX loop (2026-07-27) | ✅ | Designed against codex/hermes survey (both: never upgrade under a live session; codex: restart-then-verify; hermes: check must never break startup). (1) **Refresh cadence** — `updatecheck.defaultInterval` 24h→15m (= background re-check on effectively every TUI startup, floored against restart storms; sized for daily beta cadence — raise via `updates.check_interval` when the pace slows); `ParseInterval` sub-minute values now clamp UP to 1m instead of silently falling back to 24h. (2) **In-session announcement** — the startup background check's result feeds a one-shot buffered channel into the TUI (`cliapp.printUpdateNotice` → `Controller.SetUpdateNotices` → `maybeAnnounceUpdate` in `update_notice.go`): ONE compact transcript notice line, consumed only when idle (streaming run / armed approval panel = structural deferral, the result just stays buffered), deduped by version against the startup cache notice, dev builds excluded. (3) **Exit hint** — TUI exit prints a cache-only reminder (after the resume hint) that between-sessions is the safe time to run `selfmind update`. (4) **Self-update restart fixed** — `selfmind update` no longer restarts in-process (npm swaps the global package dir to a deleted staging path, so the old process's `os.Executable()` fork/exec'd a nonexistent file — observed live on beta.6→beta.8); it now runs `gateway restart` through the freshly installed launcher on PATH, then **verifies** the restarted daemon reports the installed version via `/v1/gateway/status` (PATH-shadowing installs produce an actionable warning, not silent success). `StartDetached` independently defends: `StartOptions.Executable` override + existence check on the current executable with PATH fallback (`pickDaemonExecutable`). Tests: `updatecheck_test.go` (ParseInterval), `runtime/gateway/client_test.go` (pickDaemonExecutable), `gateway/cli/update_notice_test.go` (idle/defer/dedup/nil), `cliapp/update_hint_test.go` (exit-hint gating). Ships in the next npm release; the beta.8→next upgrade still needs one manual `selfmind gateway restart` (fix rides the new binary). Declined by design: dismissed-version state, status-bar segment, daemon auto-update loop. **Prerelease ordering fixed (2026-08-09):** `Compare` compared prerelease segments with `strings.Compare`, so `beta.10` sorted BELOW the running `beta.9` and every release from beta.10 onward read as "no update available" — the notice, the exit hint, and `selfmind update`'s staleness check all went silent at the 9->10 rollover. `comparePrerelease` now follows SemVer 11.4 (dot-separated identifiers, numeric fields compared numerically, numeric below alphanumeric, longer identifier set wins). Tests: `updatecheck_test.go` (`TestComparePrereleaseNumericRollover`, `TestUpdateAvailableAcrossBetaRollover`). |
| Run execution coordinator | 🟡 | `RunCoordinator` (`httpapi/run_coordinator.go`) owns lifecycle, active-run registration, workspace/task resolution, execution scope, context assembly, streaming, outcome persistence, queue drain, and async delivery. Every sync/async run keeps task visibility current; stuck-run recovery repairs heartbeat-stale runs and orphaned `running` labels, while parked work remains resumable. Run finalization now performs durable bookkeeping only: it stores one replayable maintenance job per run and never calls a governance model on the response path. The daemon maintenance worker debounces and batches only same-tenant/person/workspace jobs, freezes one proposal per run, and applies KEEP/MOVE/TITLE/INBOX plus memory intake independently. Explicit task evidence remains authoritative. Async panic recovery converts panics into interrupted resumable runs and frees the person's slot. `/status` and `/tasks` distinguish a live run from a parked turn. Tests: `control/runtime_test.go`, `httpapi/run_recovery_test.go`, `httpapi/run_labeler_test.go`, `httpapi/maintenance_batch_test.go`, `httpapi/run_panic_test.go`, `httpapi/parked_status_test.go`. |
| Multi-terminal concurrency (daemon-client) | 🟡 | Decision: converge every terminal on ONE gateway daemon instead of cross-process locks. Foundation shipped: `gateway.EnsureRunning` (discover-or-autostart + health wait, race-safe via the `gateway.lock` flock); CLI client paths (`selfmind send/status/...`) auto-start a local daemon; `internal/gateway/client` daemon-backed `MessageProcessor` (sync `/v1/message` final answer + unified `/v1/events/stream` observer with durable cursor replay). Client mode is the ONLY TUI path (2026-07-10): the in-process gateway build and the `SELFMIND_TUI_INPROC` opt-out were deleted — a daemon that can't start fails with actionable guidance, never a local agent. Chat + agent-backed slash commands (`/skills`, `/memory` incl. `list`, `/bundles`, `/checkpoint`) run on the daemon via a safelisted `/v1/dispatch` (workspace-mutating/code-exec tools refused 403); `/status`/`/tasks` route via the message processor; `/skills stats`,`/model` switch show a client-mode notice. Worker pool (`internal/runpool` + `SELFMIND_WORKERS`, default 1) runs inside that daemon. `workspaceSerialKey` serializes **write** turns only (read turns concurrent, Exclusive/SharedRead semantics). Interactive tool approval works in client mode (Codex-style TUI approval panel driven by the `approval.requested` event → `/v1/approvals/respond`, incl. grant scope; see TUI rendering row). The message-based-channel working notice (`router.WorkingNotice`) is English-only (2026-07-05: "Got it — SelfMind is working on this…"; the stray bilingual TUI composer hint in `history_commit.go` was also de-duplicated to English). **Remaining**: soak at N>1; per-provider cap (adapter layer, deferred). (Session search over the daemon + in-process deletion shipped 2026-07-10, see ACTIVE PLAN P0-3.) See `docs/worker-pool-design.md` §8. |
| Process sandbox | ✅ | **Cross-platform execution contract (2026-07-24):** `terminal`, `verify`, and `execute_code` accept `sandbox:auto|isolated|host`. Linux `auto` prefers bubblewrap, `isolated` fails closed without it, and `host` remains approval-gated; `exec_sandbox.required=true` disables host fallback. The default Linux profile exposes a read-only host root and a writable workspace. macOS `auto` uses approval-controlled host execution because Linux isolation primitives are unavailable; strict `isolated` and `required=true` requests fail closed instead of pretending to isolate. Doctor/startup report the capability state. This remains a single-user boundary, not a multi-tenant container/seccomp/cgroup boundary. **Network default flipped to SHARED (2026-07-27, `allow_network: true`):** the network-less default made every IP-allowlisted/internal service unreachable from sandboxed commands, and retrying clients (gRPC, kubectl) disguised the instant `--unshare-net` failures as timeouts — observed live against ArgoCD, verified by A/B (sandbox+unshare-net: instant fail; sandbox+shared net: HTTP 200). Filesystem isolation unchanged; egress stays approval-governed as a named dangerous class; `allow_network: false` restores the strict posture. A restart launched from an IDE/updater environment used to strip either the daemon's proxy or the login shell's user-tool `PATH`, making outbound requests fail first and later hiding `argocd`/`kubectl`/`helm`. `gateway restart` now keeps a narrow restart-safe environment: current `PATH` entries first, old-only tool directories appended, old proxy variables carried forward when the caller lacks them, and arbitrary credentials never copied. Two awareness fixes landed with it (codex/hermes survey informed): (1) **model knows the environment** — `TaskStrategy.ExecSandboxNote` (populated from `tools.ExecSandboxPromptNote()` at the gateway, kernel stays tools-free) injects the sandbox + network posture into tool-capable turns' system prompt, incl. "retry once with sandbox=host, don't repeat the isolated attempt" for the network-less posture; (2) **timeout carries sandbox context** — command-timeout failures in isolated no-network mode append an `error_class: sandbox_no_network` hint (`isolatedSandboxTimeoutHint`, terminal + execute_code); previously the timeout branch bypassed `enrichIsolatedSandboxFailure` entirely, so the most common disguise of a network-less sandbox carried zero diagnostic clue. DNS-failure strings were already in the `network` classifier. Tests: `tools/exec_sandbox_note_test.go`, `kernel/light_task_layer_test.go` (`TestSystemPromptNote_ExecSandboxNote`). Follow-up (triggered, not scheduled): per-domain egress allowlist via managed proxy, codex `NetworkRule` shape. |
| Execution engine (0a–0c 2026-07-28; 1–10 2026-07-29) | ✅ | Design baseline: `docs/execution-engine.zh-CN.md` (evidence, three-layer split, five state primitives, run scratch, profile catalog, Runner seams). Measured on 2026-07-28 across 293 exec calls: 26 calls hit `read-only file system`, all on tool state directories under `$HOME` (`~/.config/gcloud/credentials.db` 64 lines, `logs` 42, `~/.cache/helm`); host-mode usage climbed 3→39 calls/hour as the model gave up on isolation; a `watch_external` gcloud check failed with `CHECK_ERROR ×6` while the aws checks in the same workspace succeeded (aws reads `~/.aws` without writing) — the durable watch path has no host escape, so it could not self-recover. **Landed:** (0a) sandbox-gated failure classification — `classifySandboxDenial` decides FIRST for isolated commands and emits `sandbox_fs_denied` / `credential_state_readonly`; the `auth` rule no longer matches a bare `credential` substring (14 calls had been told "fix auth state first" plus "host execution is not an authentication or setup fix" for a denied write, contradicting the only correct remedy), and new `credential_missing` / `credential_expired` classes split "no credential" from "credential rejected". Event `error_category` now reads the structured `error_class:` marker instead of re-parsing the prose hint — the old prose match on the word "permission" (present in the tools layer's own hint text) reported genuine permission/sandbox failures as `workspace_scope`. (0b) reusable-grant floor (`tools/grant_floor.go`, L1 source data): interpreters/shells, general-purpose exec facilities (`git`, `find`, `xargs`, `make`), irreversible operations, shell control flow, and any redirection/substitution/expansion/wildcard are approvable once and never remembered; `execute_code` is never remembered; ordinary dangerous classes (`chmod`, `kill`) stay grantable. `execCommandFamily` delegates to the same floor so the fingerprint cannot disagree with eligibility. (0c) approval ledger became user-owned state: `approval_grants` gained `expires_at`/`revoked_at` with `List`/`Revoke` (it previously supported only INSERT + an existence check, so a remembered class was permanent, invisible and irrevocable — ten person-scope host grants accumulated on 2026-07-28, two of them keyed `command:set`/`command:for`); host-escape and person-scope grants are bounded at 8h to match the execution-capability policy; a boot-time review (`app.ReviewApprovalGrants`, called from the gateway boot sweep) re-applies the current floor to stored keys, is idempotent, and only removes authority; the decision reply now names the class that was remembered and says "not remembered" when the floor refused to persist one. Tests: `tools/{tool_errors,grant_floor,grant_review}_test.go`, `kernel/tool_failure_class_test.go`, `control/approval_grants_test.go`, `app/approval_grant_review_test.go`, `httpapi/approval_grant_note_test.go`. **Steps 1–10 landed 2026-07-29.** (1) `EnvironmentRegistry` (`internal/executionenv/registry.go`): a run binds to ONE immutable snapshot through its lease; snapshots are never mutated in place (an idle re-sample keeps generation AND values, or the binding it exists to provide would be defeated); three fingerprints (principal / environment / credential-source) over a NORMALIZED environment — hashing PATH verbatim would have changed the fingerprint on every restart because the daemon PATH contained `/run/user/1000/fnm_multishells/<pid>_<ts>/bin` (9 such directories existed), parking every recovered run as `environment_changed`; lease gained `environment_snapshot_id/generation/fingerprint/credential_source_hash`; restart with a missing snapshot rebuilds only when all three match (event `environment.snapshot_rebuilt`), otherwise `waiting_user: environment_changed`; `os.Environ()` now appears at exactly one construction site. (2) Run scratch (`internal/executionenv/scratch.go`): `<runtime>/leases/<lease>/{tmp,state}` at 0700, BESIDE the data dir so it never enters backups or indexes; `SetRuntimeRoot` refuses a root under `/tmp` because the `/tmp` bind shadows every real path beneath it (reproduced: `Directory nonexistent`); isolated execution binds scratch TWICE (real path + `/tmp`) so `$SELFMIND_RUN_TMP` and `TMPDIR` are literally the same path in host and isolated mode; 24h TTL sweep at boot; 2 GiB soft quota blocks the NEXT command instead of killing the running one. (3) Tool environment profiles (`internal/tools/envprofiles/`, Go source data — `config.yaml` gained ZERO keys, locked by `config_surface_test.go`): five primitives (`copy_in`/`map_ro`/`map_rw`/`env_redirect`, `write_back` reserved and nil); bounded `copy_in` (include/exclude/max_bytes/max_files/max_depth, non-regular files refused, staging + atomic rename, permission bits preserved); catalog covers gcloud, kubectl-gke (requires gcloud), aws, gh, docker, go-toolchain, node-toolchain, validated at build time (unique ids/executables, acyclic requires, no traversal). Trust decides operator credential access: trusted uses it, untrusted needs `credential:read` and otherwise gets redirects + empty state so the tool reports "not logged in" instead of a read-only error, WITH a recorded reason. Verified end-to-end in real bubblewrap: a fake gcloud that opens its credential store read-write for a read-only question now succeeds on FIRST use, the operator directory stays byte-identical, `logs/` is not copied, and the overlay survives across commands of one run. (4) Execution engine types (`execution_engine.go`, `sandbox_backend.go`): serializable versioned `SandboxPlan` (carries a scratch HANDLE, never a node-local path) + non-serializable `ProcessMaterial` (test asserts it marshals to `{}`); `SandboxBackend` interface with bubblewrap and host implementations. (5) Every exec path migrated, including `RunSandboxedShellScoped` for durable watches — the gcloud watch that failed `CHECK_ERROR ×6` had no host escape hatch and could not self-recover; its overlay is now keyed by watch id and cleaned on completion. (6) Program-set extraction from the shell AST fixed a real gap: `for ...; do kubectl ...; done` yields a `do kubectl get ns` segment, so a segment-level skip missed kubectl entirely; control keywords (a command follows) and builtins (arguments follow) are now distinguished. (7) Recovery is stage-based and bounded to one retry, gated on "produced no output" as the only decidable proxy for "no side effects yet"; capability expiry is now ruled by the control plane (`approvalDecisionExpiry`) instead of a constant inside the tool middleware. (8) Sandbox policy is resolvable per request (`ExecutionScope.SandboxPolicy`) instead of only process-wide, and scopes are additionally keyed by run (`WithExecutionScopeKey`) so a caller resolves exactly its own execution. (9) `selfmind env show|refresh` (login-shell re-sample, local-only, adoption via restart so no in-flight run changes environment mid-run) and `/diag execution` now reports snapshot generation/age, volatile entries dropped, scratch size + quota state, profiles applied, recoveries, and host escapes BY REASON. (10) `/approvals grants|revoke <n>` completes the L2 ledger surface with durable `approval.grant_revoked` events. Tests: `executionenv/{registry,scratch}_test.go`, `tools/{execution_engine,exec_scratch,exec_profile,grant_floor,grant_review}_test.go`, `tools/envprofiles/{catalog,apply}_test.go`, `platform/config/config_surface_test.go`. **Review round 2 (2026-07-29), all confirmed and fixed:** (a) durable watches persisted no environment identity, so a watch registered under account A could silently keep checking under account B after a restart — the failure mode is not an error but a check that succeeds against the wrong project; `external_watches` now carries snapshot id, generation and the three fingerprints (non-secret), verified BEFORE every check, with `environment_changed` termination and an event, and pre-identity watches grandfathered. (b) `env refresh` sampled a login shell but discarded it, and told the user a plain `gateway restart` would adopt it — a restart inherits the CLI's own environment, the stale one; `StartOptions.Environment` plus `env refresh --restart` now hand the sample straight to the new daemon (never written to disk, since an env file would persist credential values), and the macOS launchd plist passes through non-credential proxy/config-location variables (0644, so credential-shaped names and values with inline credentials are excluded). (c) `background:true` bypassed the engine entirely (no lease binding, scratch, profiles, or evidence), so a run's foreground gcloud could succeed while its background gcloud failed; it now uses the same material and records host-escape reason. (d) the gh profile set `GH_STATE_DIR`, which gh does not read (verified: absent from `gh help environment`, while `GH_CONFIG_DIR=$T gh config set` writes to `$T`) — replaced with a bounded `copy_in` and `GH_CONFIG_DIR` pointed at the writable copy. (e) every kubectl/helm was treated as GKE and forced to depend on gcloud, breaking EKS/AKS/local clusters and needlessly widening credential exposure — split into a generic `kubernetes` profile plus a new generic `ConditionalRequire` primitive that pulls a credential helper in only when the kubeconfig's exec plugin names it (heuristic, failing safe: a miss means that helper's state is not prepared, never that wrong credentials are used). (f) `SandboxPlan` identity fields were empty in production and `ExecutionRequest`/`ExecutionResult` were type-only — the plan now carries the real snapshot/generation/scratch handle (asserted by test) and terminal/verify/execute_code/watch/background share one material construction and one backend interface. **Review round 3 (2026-07-29):** (g) `env refresh` compared the CLI's own environment against the login-shell sample, which reported "unchanged" precisely when the daemon was the stale one — the CLI is normally the first process to see a new toolchain; the baseline is now the RUNNING DAEMON via new non-secret fingerprint fields on `/v1/gateway/status`, `--restart` no longer early-exits, and launchd mode refuses explicitly (a plist pins the environment, so a restart cannot adopt a sample) instead of reporting success. (h) helm shared the kubectl profile and only had `KUBECACHEDIR` redirected, so `HELM_CACHE_HOME` and `HELM_CONFIG_HOME` still pointed at read-only host directories — this was the original live failure (`open ~/.cache/helm/repository/argo-cd-9.2.1.tgz: read-only file system`); a dedicated `helm` profile now requires the generic `kubernetes` one, sends the cache to the person-level persistent root and copies the config directory in, while `HELM_DATA_HOME` is deliberately left alone so installed plugins stay visible. (i) `ExecutionRequest`/`ExecutionResult` were type-only: a real `Execute(ctx, req, args)` is now the single entry for terminal, verify, execute_code and watch_external, asserted mechanically by `TestExecuteIsTheOnlySandboxConstructionSite`; `background:true` shares the same material but is not routed through `Execute` (fire-and-forget has no streamed output or exit code to return), and the docs say so rather than claiming full unification. **Self-audit round (2026-07-30), 11 items closed:** the three that mattered were promises the code could not keep or real failure classes — (1) `credential:read` was only ever READ, with no approval path at all, so an untrusted workspace's only route to a credential CLI was trusting the whole workspace (the all-or-nothing escalation the capability exists to prevent); it now has the same pre-execution approval path as `network:shared`, asked only when the program set matches an operator profile, and declining leaves the command runnable with an empty overlay. (2) The AWS SSO token cache is derived from `HOME` (`~/.aws/sso/cache`), not from `AWS_CONFIG_FILE`, so no env redirect could move it and SSO accounts still failed on refresh — a new generic `map_rw_at` primitive binds a writable state dir OVER a host path (host untouched, seeded from the host so existing tokens work). (3) `KUBECONFIG` is list-valued; treating `a:b` as one path silently mapped a nonexistent file and skipped exec-plugin detection entirely. Also: per-request `SandboxPolicy` is now actually populated (it was a decorative seam); scratch is swept periodically, not only at boot, with live runs and unfinalized watches exempt; the quota check caches its measurement instead of walking the tree before every command; writable roots are deduped in the plan; background processes get a ceiling so a wedged one cannot hold its scratch and copied credentials until restart, and record their plan as evidence; the two owed eval cases exist (and a second look caught `disallowed_error_category` sitting under `checks:` where it was silently ignored — moved to `expect:` and verified by parse); `docs/tool-safety.md` caught up two rounds and no longer describes `credential:read` as planned. **Not implemented:** native macOS Seatbelt backend (needs real-device verification; macOS uses the host backend with the same plan/snapshot/scratch/profiles) and `write_back` (SQLite credential stores need locking/WAL/conflict handling, so `gcloud auth login` from inside the sandbox does not persist by design). |
| Project profile | ✅ | **Language-agnostic coding profile (2026-07-24):** each run receives a bounded, deterministic, read-only profile derived from manifests, lockfiles, and declared scripts. It recognizes Go, Rust, Swift, Node.js, Python, PHP, JVM, Ruby, CMake, and .NET projects, proposes evidence-backed verification candidates, and never executes repository code during prompt assembly. Ecosystem support belongs in `internal/kernel/project_profile.go`, not gateway routing or prompt keyword rules. Cross-language coding evals live under `evalcases/coding-foundations/`. |
| Feishu / Lark adapter | 🟡 | Inbound via the generic `/v1/im/feishu` webhook (verification-token / encrypt-key signature, challenge); outbound via `delivery.FeishuSender` (tenant_access_token + `im/v1/messages`, chat_id/open_id routing). Config drives both. Encrypt-envelope AES decryption still TODO (use plaintext mode). **Inbound redelivery dedup (2026-07-09):** the `/v1/im/*` webhook now acknowledges a duplicate delivery 200 without re-running the agent — keyed by the platform's own id (`imMessageID`: generic `message_id`/`event_id`, Feishu `header.event_id`/`event.message.message_id`, QQ `d.id`, Telegram `update_id`), persisted in `control.inbound_dedup` (48h retention) so it survives restarts; no-id payloads and dedup-store errors fail open. `handlers_channels.go`, `control/inbound_dedup.go`; tests `httpapi/handlers_channels_test.go`, `control/inbound_dedup_test.go`. |
| QQ official bot adapter | 🟡 | Inbound via `/v1/im/qq` webhook (group/C2C/guild events parsed into a `group:`/`c2c:`/`channel:` target); outbound via `delivery.QQSender` (app access token + per-target message API). Active push only — webhook ed25519 signature verify and passive `msg_id` threading are follow-ups. Inbound redelivery dedup by `d.id` shipped 2026-07-09 (see the Feishu row — shared `/v1/im/*` mechanism). |
| Production hardening batch (2026-07-09) | ✅ | Pre-production fixes from the 2026-07-08 full audit: (1) **config fail-fast** — `gateway run` now aborts with `load config: …` on a broken config instead of half-starting with an empty one (`runtime/gateway/runner.go`; LoadConfig still auto-creates the default template on first run); (2) **cron.db SQLite hygiene** — WAL + `busy_timeout=5000` + `MaxOpenConns(1)`, same as control.db (`app/gateway.go`); (3) **data privacy on shared hosts** — data dir 0700 and `control.db`/-wal/-shm chmod 0600 best-effort at open (`control.OpenStore` `tightenStorePerms`). |
| Pinned memory injection (profile synthesis removed) | ✅ | **2026-07-11:** the legacy `ProfileSynthesizer` was DELETED as dead code — it had no callers, so pinned facts never reached the model and a stale profile row could be injected forever. `buildSystemPrompt` now reads the `pinned` target directly and injects user-confirmed facts FIRST, unconditionally, outside the bounded `SelectFacts` slots (they never compete with extracted facts and never decay out). Do not reintroduce a profile-synthesis model call; the "knows you" signal is pinned + high-score facts. Tests: `internal/kernel/pinned_injection_test.go`. |
| Scheduled tasks (cron) | ✅ | SQLite-backed scheduler with timezone; jobs run a real agent turn and deliver the result to their channel (e.g. daily summary → WeChat); `web` opt-in per job; built-in liveness canary; idempotent built-in jobs. `internal/kernel/task/cron`, `internal/gateway/httpapi/cron_executor.go`. **Stable task binding (execution-quality W6, 2026-07-12):** `cron_jobs.task_id` (idempotent column migration) pins a recurring job to ONE work label — learned from the first successful execution's resolved task, passed as explicit attach evidence on later fires, and cleared automatically when the label is archived (the next run re-learns). Display/organization only; labels never gate context (P3). `Scheduler.SetTaskID`, executor learn/verify in `cron_executor.go`. Tests: `cron/scheduler_test.go` (`TestTaskIDBindingRoundTrip`). |
| Self-check & CI gate | ✅ | `selfmind selfcheck` has explicit `local-full` (default), `local-fast`/`--fast`, and complementary `ci` profiles. Provider responses replay strictly offline, while replayed tools use a preflighted host toolchain; missing Go/repo/eval dirs/required commands fail closed as environment-unavailable (exit 2), never green. Eval YAML separates local cassette duty (`require_cassette`), measured local cost (`slow`), host commands (`requires.commands`), and CI ownership (`ci.required/reason/platforms`). CI runs only named clean-checkout/credentialless/cross-platform/concurrency/timing evidence on Linux and macOS, with per-job timeouts and failure artifacts; selected missing cassettes fail by identity rather than a numeric floor. Corpus guards cover machine paths, provider failures, valid JSON, empty dirs, and contiguous ordinals. `scripts/bisect-selfcheck.sh` maps unavailable commits to bisect 125. `internal/cliapp/selfcheck_commands.go`, `internal/eval/case.go`, `.github/workflows/ci.yml`. |
| Continuity eval coverage | ✅ | Cases + gate + cassettes recorded and committed; selfcheck replays 7 cases offline. `evalcases/continuity/` (cross-endpoint `/status`, `继续` resume, stranger identity isolation via per-turn `platform_user_id`), all `require_cassette: true`; record with `SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity` and commit `.vcr/continuity_*`. `continuity-task-attach.yaml` rewritten for P3 pre-label semantics (2026-07-06): an ordinary follow-up now runs under the open current label by design, so the case no longer asserts `require_task_switch`; its old cassette predates the rewrite — re-record before it gates. |
| CLI image input | ✅ | Image-path detection in input + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect; WSL/macOS/Linux); routed to the `vision_analyze` tool via the attachment pipeline. Clipboard requires a local GUI (not over SSH). `internal/gateway/cli/attachments.go`, `clipboard.go`. |
| Approval modes | ✅ | Staged `on-request` / `read-only` / `auto-edit` / `full-auto` / `smart` via `/mode`, enforced in `SmartApprovalMiddleware`; on-demand y/N reuses the clarify bridge. `internal/tools/middleware.go`. **Layered approval funnel shipped (H1, 2026-07-04):** the middleware now runs (1) an unbypassable **hard floor** (`hardlineToolCall`) that fires before any mode bypass — full-auto included — for irreversible ops (recursive delete of `/`,`/home`,`$HOME`,`/etc`,`/usr`,`/var`,`/boot`; `mkfs*`; `dd of=/dev/sd*|nvme*`; redirect over a raw disk device; fork bomb; `shutdown`/`reboot`/`halt`/`poweroff`/`init 0|6`), returning `operation blocked by safety policy: …` (distinct from the user-rejection contract; kernel `isUserRejectionErr` does NOT match it); (2) mode bypass; (3) **class-level approval memory** — approving a coarse *class* (`approvalPatternKey`, e.g. any `chmod` → `exec:invokes dangerous command: chmod`) for the task (session) or person (persistent) via the durable `approval_grants` table skips later same-class asks; (4, smart mode) **LLM triage**; (5) human ask. Reply grammar: `/approve [n] task` / `/approve [n] always|person` (or bare `yt`/`ya`) records the grant scope; `/mode` is now an IM control command persisting per-person `approval_mode`. **Smart-mode LLM triage shipped (H2, 2026-07-04):** in `smart` mode a dangerous (non-hardline) op with no matching grant is triaged by a cheap role model (`background_review`, off the main run provider) before the human ask — APPROVE auto-runs AND records a task-scope class grant (judge consulted at most once per class per task), DENY blocks with the user-rejection contract (`operation rejected: blocked by safety triage`, do-not-retry), ESCALATE / no judge / any error / 15s timeout falls through to the human ask (fails SAFE, never open). The judge (`ApprovalJudge` interface in `internal/tools/approval_triage.go`, injected via `ExecutionScope.Judge`) strips shell comments and wraps the command in `<command>` delimiters with a prompt-injection defense. The hard floor stays ABOVE triage (hardline ops never reach the judge). `internal/tools/middleware.go`, `internal/tools/approval_triage.go`, `internal/app/approval_judge.go`, `internal/control/approval_grants.go`, `internal/gateway/httpapi/approval_resolver.go`. **Live mode lookup (2026-07-05):** the mode is resolved at EACH ask, not frozen at run start — `ExecutionScope.ModeGetter` (installed by `installExecutionScope`) re-resolves with run-start precedence (explicit request mode wins, else the CURRENT persisted `person_settings.approval_mode`, else on-request), so `/mode smart` sent from IM mid-run governs the in-flight run's later approval decisions (triage included). The TUI no longer force-sends its local default mode: requests omit `approval_mode` unless the user ran TUI `/mode` this session, so the persisted preference governs (`selfmind send --mode` stays explicit). Tests: `tools/approval_mode_live_test.go`, `httpapi/mode_command_test.go` (`TestApprovalModeLiveLookupMidRun`), `cli/approval_mode_omission_test.go`. **`/mode` retro-resolves ALREADY-pending approvals (2026-07-06):** live-mode lookup only governed the NEXT ask, so an approval asked BEFORE the switch stayed blocked forever (observed live: a `read_file` approval sat pending for minutes after `/mode smart`). `approvalModeReply` now re-checks the person's currently-pending approvals under the new mode via the shared classifier `tools.EvaluateModeDecision` (hard floor authoritative — a hardline pending op is NEVER auto-approved by any mode; smart consults the same `ApprovalJudge`, fails safe to still-pending on ESCALATE/no-judge/error; full-auto/auto-edit auto-approve their non-dangerous classes; on-request/read-only leave everything pending). Settled rows flip status so the blocked waiter (`server.go` ~812, 1s poll) wakes without a new wakeup channel; the reply reports the count (`Re-checked 1 pending approval: 1 auto-approved.`). `retroResolvePendingApprovals`/`approvalReasonIsDangerous` in `httpapi/server.go`, `tools.EvaluateModeDecision`/`ModeDecision` in `tools/middleware.go`. Tests: `httpapi/mode_retro_test.go`. **Security hardening (2026-07-07):** (1) arbitrary-code exec (`terminal`/`shell`/`execute_command`/`execute_code`) now ALWAYS requires approval in on-request AND smart modes (`approvalNeeded` returns true for `isExecTool`), not only when the dangerous heuristic fires — closing the hole where `execute_code` (payload in `args["code"]`, not `args["command"]`) ran with NO approval by default; (2) both the hard floor (`hardlineToolCall`) and the dangerous heuristic (`dangerousToolCall`) now read the exec payload via `execCommandPayload` (command/code/script) and inspect **wrapper-unwrapped** segments (`expandCommandSegments`): a shell `-c` script, `sudo`/`doas`/`env`/`xargs`/`nohup`/`timeout`/`nice`/`ionice`/`setsid`/`stdbuf`/`command` prefixes (bounded recursion depth 3) are unwrapped to their real inner program, so `bash -c "rm -rf /"`, `sudo rm -rf /`, `env rm -rf $HOME` are caught; an unparseable wrapped payload degrades to dangerous (approval) but NOT hardline (the floor only denies what it can positively identify). Tests: `tools/middleware_wrapper_test.go`, `tools/approval_mode_test.go`. **Network egress classifier (2026-07-09):** `dangerousToolCall` now flags network-egress commands (`egressCommand`: curl/wget/nc/ncat/netcat/socat/telnet/ftp/tftp/scp/sftp/rsync/ssh by wrapper-unwrapped basename, plus `/dev/tcp/`·`/dev/udp/` redirects) as a first-class safety class — the exfiltration half of the IM-injection threat (untrusted message → terminal data-out). It is its own named function (testable/tightenable independently), reuses the already-unwrapped segments so `sudo curl` / `bash -c "curl…"` can't hide, and excludes `git` (push/pull to configured remotes = pure fatigue). Egress is a **dangerous** class, so on-request (default) and smart modes ask/triage it; **full-auto still bypasses it by the existing documented contract** (owner decision 2026-07-09 — keep egress in on-request/smart only, not a new unbypassable tier). Tests: `tools/middleware_egress_test.go`. **Approval-UX batch A (2026-07-30).** Investigating "why is it approving so much" found two independent causes and one missing surface. (1) **Judge output budget was the real breaker:** `judgeMaxTokens` was 8, sized for the one-word verdict alone, but every configured cheap role model is a REASONING model — the thinking consumed the whole cap, so anthropic-protocol judges returned a response with no text block (`stop_reason=max_tokens`) and OpenAI-protocol judges returned truncated prose that the strict parser rejects. Both escalate, so smart mode was silently equivalent to on-request whenever it ran. Now 512, measured: the smallest budget at which nvidia/nemotron (openai_chat), kimi-for-coding and MiniMax-M2.7 (anthropic_messages) all return a bare `APPROVE`/`DENY`, in 0.4–5.4s, inside the 15s `triageWaitTimeout`. Thinking blocks are dropped by the adapter, so the strict verdict parse stays strict. `internal/app/approval_judge.go`; tests `app/approval_judge_test.go`. (2) **Decision context now travels with the ask:** `ToolApprovalRequest` carries `Environment`, `Cwd` (the scope's own root — never the daemon cwd and never a path from the model's args), `ChangeSummary` (content-free counts: `2 files +48/-12`, `120 lines, 3.4 KB`), and `TriageState`; `toolApprovalHandler` persists them on the row and publishes them on `approval.requested`, and the TUI panel renders `change:` / `where:` / `reason:` / `remembering allows:` lines with the command WRAPPED rather than middle-truncated (a command whose middle is `…` cannot be judged). `tools/approval_view.go`, `ui/components/approval_prompt.go` (`ApprovalDetails`, `WrapDisplay`). (3) **Triage is now observable:** person-partitioned 24h counts (`tools.RecordTriageOutcome`/`TriageDiagnostics`, in-memory read model, never an audit log) distinguish a deliberate escalation from a judge that never ruled; `/diag` prints `Smart triage (24h) [mode: …]` with the actionable advisory and the last judge error, and the panel says `automatic triage unavailable — asking you` only for the fail-safe case. (4) **Typing-idle guard:** an approval arriving within 900ms of a keystroke is held (`delayedApprovals` + `MsgApprovalDelayElapsed`) so the panel cannot swallow an in-flight key as a decision — `y` is both a letter and "yes, run it"; the daemon-side request stays durable meanwhile. Tests: `tools/approval_context_test.go`, `ui/components/approval_context_test.go`, `cli/approval_typing_guard_test.go`, `httpapi/approval_context_test.go`. **Approval-UX batches B and C (2026-07-30).** (B1) **The daemon issues the answer set.** `buildApprovalDecisions` (`httpapi/approval_options.go`) computes one ordered option list per ask from the grant class the floor minted plus the rules this call could create, persists it on the row and the `approval.requested` event, and every surface renders THAT: the TUI panel, the `/approvals` text, and the IM push (which now lists the same lettered menu, so `yp` on WeChat means what `p` meant in the terminal — `parseBareApprovalReply` returns the letter, `approvalOptionByShortcut` resolves it against the row's own list). Clients no longer invent options; a client that answers with an option the ask did not offer is refused at the execution layer. (B2) **Rules, and typed outcomes.** `tools/approval_rules.go` derives three narrow authorizations — command prefix (`rule:exec_prefix:git status`), network host, writable root — each recomputed from the current call so redemption is a lookup, never a text match. Prefix rules exist because the grant floor rightly refuses `git` as a family; they are refused for shells/interpreters/irreversible tools and, after a test caught it, for anything reached through a privilege wrapper (`sudo systemctl restart api` must not mint `systemctl restart`). `approval_requests.decision_grant_key`/`decision_note` (ensureColumn) carry the chosen rule and the person's refusal words; `ApprovalDecisionInput` replaces the positional scope argument and refuses to store a rule alongside a rejection. A timeout is now `ApprovalOutcomeTimedOut`, distinct from a rejection, and the model is told "nobody is at the keyboard" instead of "do not retry". (B3) **All-keys semantics:** `approvalTargetRuleKeys` emits one path-root key per out-of-scope target of a write, and a stored grant releases a multi-file patch only when EVERY target is covered. (C1) **Containment replaces command shape in smart mode:** `execSandboxContained` (`tools/approval_containment.go`) claims containment only for an exec call whose EXPLICIT effective mode is isolated, with the sandbox enabled, network off, and enforceable on this host (Linux + bubblewrap); `approvalNeeded` then skips the ask, because that blast radius equals the in-workspace writes smart mode already allows. Dangerous ops (egress, out-of-workspace, destructive), host mode, and the other modes keep their gate; the /mode retro path passes containment=false so a pending host-mode ask can never be released by a policy inference. Counted as `TriageOutcomeContained` in `/diag`. (C2) **Guardian-shaped triage:** the judge now returns `{risk_level, user_authorization, outcome, rationale}` (legacy one-word replies still parse, everything unparseable escalates), the gateway supplies the person's own words via `ExecutionScope.TriageIntent` so authorization is judged separately from risk, the rationale reaches the panel and the IM push and is stored for audit, and a per-run denial breaker (2 consecutive) makes triage step aside to the human rather than let the model circle. (C3) **`request_permissions`** (`tools/request_permissions.go`, registered in `RegisterExtendedTools`) is the reverse channel: one ask for a task's roots and hosts, expressed as the same rule keys, refusing `/` and a bare home directory, reporting already-held permissions without re-asking, and returning the user-decision contract on refusal. Tests: `tools/approval_rules_test.go`, `tools/approval_containment_test.go`, `tools/request_permissions_test.go`, `httpapi/approval_options_test.go`. **Still owed:** Telegram native buttons remain approve/reject (the option contract is shared, only button rendering is not — `delivery.Message` would need to carry the list), and `/watchers` from batch 2 of the watcher plan. |
| Mid-turn steering | ✅ | Works in-process (`internal/kernel/steering.go` + controller `steerCh`) **and** in client mode: input typed during a daemon run is forwarded via `POST /v1/runs/steer` (`httpapi/handlers_steer.go`) into the active run's buffered steering channel (`kernel.WithSteering`, sync + async paths), leaving an auditable `run.steered` event. Daemon refusals surface honestly in the TUI (409 no active run / 429 buffer full → transcript notice, never silently dropped). **Cross-endpoint steering closed (2026-07-09):** the thin-client path only covered the TUI; a continuation arriving on the ordinary `/v1/message` entry (IM/web) while a run was active used to bounce off a bare "busy" reply, so WeChat could not steer a running task. Both entries now share one core (`Server.steerActiveRun` in `handlers_steer.go`): an `IntentContinue` message on `/v1/message` injects into the SAME active-run steering channel (non-blocking; a full buffer or absent channel falls back to the honest busy reply, never a silent drop) and returns an `accepted` turn (`formatSteeredIntoRun`) plus a `run.steered` event — the cross-endpoint takeover the north star requires. Continuations still never queue. Covered by unit tests (`httpapi/steer_active_run_test.go`, `httpapi/handlers_steer_test.go`, `httpapi/queue_test.go`), not VCR. |
| Task queueing (G1+G2) | ✅ | New work while a run is active is **queued, not rejected** (2026-07-04). Durable `control.task_queue` (`internal/control/queue.go`: `EnqueueQueued`/`ListQueued`/`NextQueued`/`CountQueued`/`MarkQueued`/`ClearQueued`/`ListAllQueued`/`RequeueStartedQueued`). `ProcessMessage` enqueues a genuinely-new message behind the active run with an honest acceptance (`Queued behind the running task (N ahead)…`); a continuation (`IntentContinue`) is never queued — it **steers the active run** on every entry (2026-07-09; see Mid-turn steering row), falling back to the busy reply only when the steering channel is full/absent. On every run finalization (sync + async paths) `RunCoordinator.drainQueue` auto-starts the next queued item as a normal async run (per-person `draining` re-entrancy guard; reverts the row to `queued` if a fresh run races the slot). Boot drain (`Server.DrainQueuedAtBoot`, wired in the gateway runner) requeues `started` rows and resumes pending work after a restart. Visibility: `/queue` (list) / `/queue clear`; `/stop` cancels the active run and then drains. `httpapi/queue.go`, `httpapi/run_coordinator.go`. Tests: `control/queue_test.go`, `httpapi/queue_test.go`. **Queue done-state (2026-07-07):** a drained row's async run finalization now marks the row `QueueStatusDone` (the coordinator threads the queue id onto the drained `MessageRequest.QueueID`; `runMessage` marks done in a deferred terminal-path write). Previously the row stayed `started` with no back-reference, so boot recovery (`RequeueStartedQueued`) re-ran already-completed work at every restart (only masked by `maxQueueRestarts=1`). `RequeueStartedQueued` still touches only `started` rows, so `done` rows are never resurrected. Test: `httpapi/queue_test.go` (`TestDrainedItemMarkedDoneNotRequeued`). |
| Task-attach semantics | ✅ | **Pre-label guess (Work Timeline P3, 2026-07-06 — supersedes the 2026-07-05 explicit-evidence-only rule).** Explicit evidence stays deterministic: caller `task_id`, `IntentContinue` (router cue / short acceptance), or the one-shot `/resume` pin (`person_settings.resume_pin_task`, consumed by the next agent-bound message; the pin alone may reopen an ARCHIVED label). Every other agent-bound message — sync, async, queued-drain, cron — pre-labels onto the person's current OPEN (non-terminal, non-archived) label, else a fresh placeholder. Harmless by construction: labels never gate context (spine P1 + recall P2), the EXECUTION workspace follows the REQUEST for pre-label turns (`workspaceForTask` — a guessed label's binding is never stamped or inherited), and the post-run labeler re-points wrong guesses (see the Run execution coordinator row). `resolveTask` returns a `taskAttach{created,preLabel}` provenance flag (`httpapi/server.go`, `httpapi/continue_resolver.go`). Resume carries real work state (2026-07-05): a resumed run keeps the **task's own workspace** even when the request carries a different client-cwd workspace (`workspaceForTask` prefers `task.WorkspaceID`, fixing a cross-endpoint `继续` that ran in the terminal's dir and tripped out-of-root approvals), and `withResumeContext` now injects a bounded (≤10) `files_this_task_created_or_changed` list merged from the handoff and the task's file-mutating tool events (`resumeChangedFiles`) — so an interrupted run (no handoff) still tells the continuation which file to edit instead of rediscovering and overwriting the wrong one. Harvest hardened 2026-07-05 to cover `write_file`/`edit`/`edit_file` (path via `path`/`file_path`/`output_path`) and `patch`/`apply_patch` (V4A `Update`/`Add`/`Delete`/`Move File` headers), never `read_file`. Tests: `httpapi/task_attach_test.go`, `httpapi/server_test.go` (`TestResumeContextIncludesCreatedFilesFromEvents`, `TestResumeChangedFilesHarvestsPatchAndEditPaths`, `TestWorkspacePreservedOnResume`); eval: `evalcases/continuity/continuity-task-attach.yaml` (cassette pending). |
| Observability / diagnostics | ✅ | Self-serve diagnostics so the owner never re-describes bugs by hand (2026-07-04). `selfmind doctor [--out FILE]` (`internal/cliapp/doctor_commands.go`): a redacted bundle — gateway status (live HTTP status, else on-disk PID record), last 10 runs (status/title/elapsed/last_error), pending approvals, queued tasks, `sent_unconfirmed`/`failed` pushes, presence snapshot (durable `accounts.last_seen_at`), per-channel activity, and the last 50 gateway.log lines. Read-only; works whether or not the daemon is up (reads control.db + log directly). `/diag` control command returns a compact phone-friendly snapshot (active run + elapsed, queued count, pending approvals, last error, recent events). Content redacted via `tools.RedactSensitive`. Store queries: `control.ListRecentRunsForPerson`, `control.CountChannelMessagesByChannel` (`internal/control/doctor_queries.go`). Tests: `cliapp/doctor_test.go`, `httpapi/queue_test.go` (`/diag`). **Diagnostics expansion (execution-quality W2, 2026-07-12):** two new structured run events — `context.compacted` (before/after tokens, messages folded, duration; emitted by `ContextEngine.TruncateMessagesCtx` only when compaction fires) and `context.recall` now emitted on EVERY recall-wired turn including zero-hit and skipped ones (`sources/refs/expanded/terms/slices/elapsed_ms/skipped`; "found nothing" and "never ran" are different diagnoses). New pre-agent subcommands, zero model tokens: `/diag context` (expanded per-section breakdown of the newest `context.breakdown`, last compaction delta, last recall line), `/diag tasks` (label hygiene stats, queued/pending-approval/question counters, and a bounded possibly-stuck list — interrupted/blocked >48h, in_progress >7d idle, read-model only, never a state change); `/diag memory` additionally reports consolidation judgement progress via the optional `PassSummary` capability. `httpapi/diag.go`, `httpapi/context_selector.go`, `kernel/context_engine.go`, registry usage updated. Tests: `httpapi/diag_w2_test.go`, `kernel/context_compacted_event_test.go`, `httpapi/recall_test.go` (zero-hit event). **Assembly-time accounting (execution-quality W5, 2026-07-12):** `buildSystemPrompt` records every appended section (`kernel.PromptSection`: category, token estimate, P1-3 stable/volatile flag) as it joins them; the `context.breakdown` event now uses `BreakdownFromSections` (exact accounting, not the marker-scan heuristic, which remains as `ComputeContextBreakdown` for legacy callers) plus new `stable`/`volatile` payload fields — the authoritative cacheable-prefix boundary, rendered by `/diag context` as a "Prompt cache" line. Tests: `kernel/prompt_accounting_test.go`. |
| Task governance | ✅ | Reversible label hygiene uses additive task metadata (`kind`, `visibility`, `pinned`, `archived_at`, `last_activity_at`). The daemon-batched `PostRunAnalyzer` produces one logical task decision and memory decision set per run, while one provider request may cover several completed runs. Casual/identity/diagnostic work may move to one hidden Inbox per person/workspace; runs and events remain durable, and Inbox is excluded from normal task/recall/continuation views. `/task <id> pin|unpin` is explicit user authority. The 6h deterministic sweep archives only stale terminal work and suggests same-workspace duplicates without model calls; only explicit `/task <src> merge <dst>` folds labels. `/tasks` stays SQLite-filtered and paged, and `/diag tasks` surfaces possibly stuck work. Tests: `control/task_governance_test.go`, `app/post_run_analyzer_test.go`, `httpapi/run_labeler_test.go`, `httpapi/maintenance_batch_test.go`, `httpapi/task_view_test.go`, `control/task_merge_test.go`, `httpapi/task_dupes_test.go`, `httpapi/diag_w2_test.go`; eval: `evalcases/timeline/timeline-task-governance.yaml`. |
| Skill variant evolution / sandbox test | ❌ | Old roadmap P3 (doc removed; see git history); not started, and out of scope for the north star. |

### Composer Paste Integrity (2026-08-04)

- Large-paste placeholders expand again. The composer's own label embedded
  `[80 lines]` while its expansion pattern forbade `]` inside a token, so every
  paste over the threshold reached the daemon as literal placeholder text and was
  rejected ("the pasted content was not expanded by the client"). Observed live
  on 2026-08-04 10:49 with zero runs created; the same shape sits in
  `input_history.jsonl` from 2026-07-20, when the failure was still silent.
- `internal/platform/pastetoken` now owns the token contract for both sides: the
  composer builds every token through `Format` (label sanitized, so a token can
  never carry a bracket or a line break) and the daemon guard delegates to
  `ContainsUnresolved`, which stays permissive enough to keep rejecting tokens
  minted by older clients.
- Expansion is exact string replacement against the registered token, never a
  pattern match, so a label change can no longer strand a payload. A token that
  survives expansion (edited by hand, or recalled from an older client) is
  refused in the composer with the placeholder named, and the composer is kept
  intact — the previous flow reset the snippet buffer before the daemon replied,
  which made "paste it again" impossible. Unexpanded text is also no longer
  written to input history.
- Paste line counting accepts CR, LF, and CRLF, and the stored payload is
  normalized to LF. A terminal bracketed paste separates lines with bare CR, so
  the old `\n`-only count reported every pasted document as "1 lines" and left
  `editor.large_paste_lines` permanently unreachable.
- Image attachments were never affected (a normal file name carries no `]`), but
  now survive brackets in the name (`Screenshot [1].png`) through the same
  builder.

Tests: `internal/platform/pastetoken/pastetoken_test.go`,
`internal/ui/components/editor_paste_test.go`,
`internal/gateway/httpapi/input_validation_test.go`; eval:
`evalcases/reliability/unresolved-paste-rejected.yaml`.

### Execution Stall Hardening (2026-08-03)

- The `patch` tool no longer uses the unbounded arbitrary-subsequence LCS
  fallback. Misses use one cancellable, same-line-count whitespace-normalized
  scan with a hard comparison ceiling; ambiguous matches fail without writing.
- Context-aware tools receive the authenticated run context through the
  dispatcher. `patch` now honors cancellation, making the existing run idle
  watchdog effective for the incident path; the default idle ceiling is 10m
  and `SELFMIND_RUN_IDLE_TIMEOUT=off` disables it.
- `/stop` records `run.cancel_requested`; only the run goroutine materializes a
  terminal outcome after execution has actually exited. Database state can no
  longer claim cancellation while a tool is still mutating the workspace.
- The idle watchdog is now phase-aware: approval and clarify waits pause the
  stall timer and keep their own durable expiry, while model/tool execution
  remains bounded. A real idle expiry finalizes with completion reason
  `stalled`; cancellation still reaches clarify waiters through the run ctx.
- Tool-ledger completion uses a context detached from run cancellation after
  the executor returns, so `/stop` cannot strand a cooperative call in
  `started`. Crash-left `started` calls remain explicit uncertain evidence on
  recovery rather than being falsely declared failed.
- Approval answers persist the exact server-issued decision id. A new `run`
  option remembers an eligible action class only in memory for the current run;
  task/person grants remain durable. `execute_code` approval rows retain only a
  redacted bounded preview, language/size metadata, and a source digest.
- Smart approval now evaluates enforced containment on separate filesystem,
  network, and credential axes. Shared-network or credentialed calls bypass a
  human only when a conservative declarative catalog proves they are read-only
  observations; arbitrary scripts and unknown Agent CLIs remain gated. Opaque
  code may be approved for a byte-identical repeat in the same run only, never
  as durable task/person authority. `/diag` exposes containment, grant hits,
  exact-run hits, judge outcomes, and human asks; `/status` shows how long the
  oldest pending approval or clarification has waited.
- Task reduction now preserves an older resumable run until a deliberate
  continuation links it through `resumed_by_run_id`. A single explicit issue
  key (for example `RUQX-369`) is resolved at ingress before the current-label
  guess and is recorded as an auditable `label.assigned` decision; it remains
  display-only and never selects workspace or context.
- Worker-pool admission emits `run.scheduler` only when a run really waits for
  a workspace write lock or worker, followed by a resumed state after admission.
- Queue rows now carry a class and priority. Interactive work remains first,
  watcher finalization is explicitly classified, and ordering stays FIFO within
  a priority. Worker count remains 1 by default pending a multi-run ownership
  soak; this is a local scheduling seam, not a remote Runner implementation.
- Line-level `tool.output` remains live for the TUI but is not stored in
  `task_events`; `tool.completed` keeps the bounded durable result.

Tests: `internal/tools/patch_safety_test.go`, `internal/runpool/pool_test.go`,
`internal/control/queue_test.go`, `internal/gateway/httpapi/run_events_test.go`.

### Execution Quality Closure (2026-08-05)

- Smart approval no longer treats every trusted-workspace command as a
  credentialed call. Credential capability is derived only for a recognized
  credential-bearing tool profile, so contained local observations keep their
  automatic path. The conservative read-only catalog now covers common GCP
  build/trigger/project-policy observations and AWS IAM/KMS metadata reads;
  scripts, unknown Agent CLIs, secret reads, and mutations remain gated.
- Approval-triage outcomes are persisted as a command-free diagnostic
  projection. `/diag` can now distinguish judge approvals, deliberate
  escalation, containment/grant hits, and provider failure across daemon
  restarts. Only a bounded redacted error is retained for 14 days; diagnostic
  writes have a short deadline and never become a foreground dependency.
- Run continuation ownership is intent- and work-key scoped.
  `task_runs.work_key` is written atomically with run creation, but selecting a
  label through an explicit task id, work-key pre-label, or one-shot `/resume`
  pin remains display-only. Only an explicit continuation may create
  `resumed_by_run_id`; it claims exactly one matching unfinished work line, or
  one unambiguous keyless legacy predecessor. Reused keys and other ambiguity
  remain visible instead of being guessed away.
- Smart approval observation proof now uses a separate quote-aware Bash AST.
  Static read-only pipelines, provider global flags, and quoted format filters
  can take the deterministic path without weakening the security tokenizer;
  substitutions, heredocs, assignments, opaque scripts, privilege wrappers,
  unknown flags, and writes remain gated. Model triage uses the explicit
  `fast_classifier` route with a configurable bounded wait and a structured JSON contract;
  `background_review` is legacy fallback only and the primary model is never
  borrowed silently.
- Watch registration now rejects a first check that consumes its complete
  per-check timeout. Repeating an already over-budget aggregate check cannot
  produce a useful durable watcher, so the agent is told to split targets or use
  one bounded status query while the foreground turn can still repair it.
- Background-process status no longer reads `exec.Cmd.ProcessState` concurrently
  with `Wait`, and its output buffer is synchronized. `/process` polling and the
  process ceiling now share an explicit registry-owned status instead of racing
  on `os/exec` internals.
- Retryable post-run and skill-review jobs now park as provider-blocked after
  their bounded retry budget, preserve the concrete provider error, remain
  visible in `/diag`, and receive the existing one-shot restart probe instead
  of being permanently discarded as `skipped`.

Tests: `internal/tools/approval_containment_test.go`,
`internal/tools/approval_context_test.go`,
`internal/tools/execution_capability_test.go`,
`internal/tools/external_watch_preflight_test.go`,
`internal/tools/exec_scratch_test.go` (including `-race`),
`internal/control/approval_triage_test.go`,
`internal/control/maintenance_jobs_test.go`,
`internal/control/run_finalization_test.go`,
`internal/gateway/httpapi/server_test.go`; eval:
`evalcases/reliability/external-watch-preflight-refuses-timeout.yaml`.

### Runtime Truth, Blockers, Watch V2, And Diagnostic Closure (2026-08-05)

- Gateway liveness is derived from an instance id, PID identity, and a 15-second
  heartbeat instead of a stale `running` file. Startup reconciles an unclean
  predecessor once into `gateway_runtime_events`; `gateway status` and doctor
  expose the last exit reason without guessing from a recycled PID.
- Unfinished work is represented by durable `task_blockers`. A later run may
  resolve only blocker ids it was explicitly shown, and task status is reduced
  from the latest outcome plus currently open blockers instead of scanning all
  historical run statuses. Approval, clarification, and deterministic resume
  paths settle only their owned blocker.
- WatchSpec v2 separates a desired intermediate target from terminal success
  and terminal failure. Failure wins, then terminal success, then target;
  unchanged output backs off from 10 to 60 seconds and resets when evidence
  changes. V1 watches keep their original success/failure semantics.
- Smart triage has a configurable foreground bound (`agent.approval_triage_timeout`,
  30 seconds by default), a 1024-token low-reasoning
  verdict budget, and a durable command-free audit row containing run/tool-call
  identity, risk, authorization, grant key, provider route, latency, policy
  version, rationale, and a bounded redacted error. Resolved approval pushes
  become `superseded` immediately before delivery, including catch-up replay.
- Execution-quality closeout (2026-08-07): smart triage no longer mistakes a
  healthy reasoning-capable cheap model for an outage under the regressed
  5-second bound; `watch_external` registration now requires one clean,
  non-empty, check-definition-safe first execution; `selfmind doctor` resolves
  the real CLI account without creating a phantom person; and explicitly
  attached short continuation cues no longer skip bounded recall solely due to
  message length. Focused tests cover each boundary.
- Invalid memory references now fail closed instead of degrading into ADD;
  referenced target/scope/category are inherited. The agent memory tool and
  post-run intake share the same conservative transient-state classifier, so
  per-build creation/backfill/status facts remain in task handoffs/artifacts.
- `provider.call.context_breakdown` records a per-call token estimate for stable
  system text, native tool schemas, history, tool results, workspace, task
  runtime, recall, memory, and artifacts. Recall events separately report
  source candidates and budget-surviving selected slices; `/diag context`
  renders both, avoiding the old misleading `memory_count=0` shorthand.

Tests: `internal/runtime/gateway/state_test.go`,
`internal/control/gateway_runtime_events_test.go`,
`internal/control/run_finalization_test.go`,
`internal/control/external_watches_test.go`,
`internal/control/approval_triage_test.go`,
`internal/gateway/delivery/delivery_test.go`,
`internal/gateway/delivery/catchup_test.go`,
`internal/kernel/prompt_accounting_test.go`,
`internal/kernel/memory/transient_classifier_test.go`; eval cases:
`evalcases/reliability/task-done-with-resolved-blocker.yaml`,
`evalcases/reliability/task-stays-waiting-on-open-blocker.yaml`, and
`evalcases/reliability/external-watch-target-state-skipped.yaml`.

### Smart Approval Default (2026-07-28)

- A person with no persisted `/mode` preference now starts in `smart`.
  Explicit request modes and existing person preferences remain authoritative.
  If the cheap triage judge is unavailable, `smart` degrades to a human ask;
  the hard safety floor remains unchanged.

### Deterministic Governance And Cache Diagnostics (2026-07-21)

- `/diag context` includes a short stable-prefix fingerprint. It distinguishes
  provider cache misses from prompt-prefix churn without exposing prompt
  content.
- Responses requests now carry a stable `prompt_cache_key` derived from the
  tenant, work label/workspace, and model role, but never the run id. Repeated
  turns in the same work line can therefore reuse the provider cache without
  sharing a cache domain across unrelated work.
- A unique issue key such as `RUQX-224` is deterministic post-run display
  evidence. When exactly one offered open label has that key, an ordinary
  pre-label placeholder moves there even if the maintenance model returns
  `KEEP`. Ambiguous duplicate labels do nothing. Explicit task attachment stays
  authoritative, and execution workspace, permissions, and completed-run
  context never change.
- Cross-endpoint Delivery Continuity covers `pending_session`,
  `sent_unconfirmed`, and `failed` final results. It lets an explicit CLI
  continuation restate a possibly missed IM result without blind resend.
- Weixin `prepare failed` responses are treated as stale-session failures, not
  rate limits. When a context token exists, the sender drops it and retries
  once without the token so an otherwise healthy iLink session can recover.
- Direct answers now materialize a terminal `run.outcome` before the terminal
  run event. The run is `done`, while the reusable task label remains open
  unless a structured `finish_run` outcome explicitly changes its lifecycle.
- Linux terminal commands consistently execute through `/bin/bash -c`, so the
  documented shell contract supports `pipefail`, arrays, and Bash syntax in
  both host and sandbox execution paths.

### Live Plan Lifecycle (2026-07-16)

- This section supersedes the 2026-07-05 live-plan note in the TUI status row.
- `plan.updated` is mutable run state, not terminal history. The CLI keeps one
  live plan block immediately above the approval/composer area, replaces it in
  place on every complete snapshot, gives it one measured blank row above and
  below, and removes it when the run reaches a terminal state. Older plan
  snapshots are never committed to scrollback.
- `update_plan` accepts complete snapshots and has a bounded lifecycle cap of
  16 calls. A successful `finish_run(done)` cannot retain unresolved steps.
  When a model skips `finish_run` and returns a final answer while plan steps
  remain unresolved, the agent requests reconciliation before completion; if
  reconciliation still fails, the run ends incomplete and resumable with
  completion reason `plan_unresolved` instead of reporting a false success.
- Coverage: `internal/gateway/cli/plan_live_test.go`,
  `internal/tools/task_protocol_test.go`,
  `internal/kernel/task_strategy_test.go`, and
  `internal/kernel/agent_kernel_test.go`.

### Terminal Tool Presentation Closeout (2026-07-16)

- Finalized and active terminal cells now pass through one physical-row-safe
  boundary before rendering: terminal controls are sanitized, long tokens are
  hard-wrapped by display width, and every physical row ends with a style
  reset. This closes the stale grey composer-strip failure caused by terminal
  auto-wrap disagreeing with Bubble Tea's logical row accounting.
- Command cells use bounded semantic titles instead of raw shell bodies.
  Output is a five-row head/tail preview with hidden-row count, preserving the
  actionable tail while keeping completed history calm and scannable.
- The policy is CLI-only. IM delivery remains concise milestones, approvals,
  and final results; it does not inherit detailed command transcripts.
- Coverage: `internal/gateway/cli/terminal_text_test.go` and the command-tool
  transcript tests in `internal/gateway/cli/controller_test.go`.

### Run-Scoped TUI Timeline Ordering (2026-07-23)

- Digest replay, live watcher events, and a newly submitted turn no longer
  share an unowned mutable stream. The digest carries the active `run_id`, the
  thin client watches that exact run, and each forwarded event preserves its
  durable event id/cursor plus run identity.
- Before a new user message is committed, the CLI commits the watched run's
  final assistant fragment and detaches its live view. Events that arrive late
  from that old watcher are ignored, so they cannot appear below the new user
  message or mutate the new run's active cells. Durable event ids and live
  sequence ids are also deduplicated across reconnect/replay.
- Attach summaries render as a distinct digest cell, not as `Learning`.
  Watcher completion renders with the durable watcher id and both states
  (`Watcher <id> | status: succeeded | task: waiting_finalization`); its queued
  system run opens with `Watcher <id> | status: finalizing | task: running`
  rather than leaking the internal finalization prompt. Lifecycle-only tools
  (`update_plan`, `finish_run`) remain control
  state instead of transcript tool rows.
- **Background runs render as a result, not a transcript (2026-07-31).** The
  person-level SSE stream carries every run, so a watcher finalization run —
  work deliberately moved off the agent turn — was replayed into the terminal
  as a full turn: thinking lines, tool cells, tool output, plan snapshots and
  streamed text, arriving unannounced under whatever the person was doing.
  Cron fires had the same shape.
  - The rule is **run origin**, not the watcher. `api.MessageRequest.Origin`
    (internal, `json:"-"`) names the initiator of a run the daemon started on
    the person's behalf; `runOrigin` resolves it as explicit request state →
    watcher inferred from `WatchID` → `kernel.TurnSourceFromContext` last,
    because an async run executes under a fresh `context.Background()` and the
    ctx tag does not survive it. The cron executor and the external-watch queue
    drain set it; `run.started` publishes it as `origin`.
  - A turn the person typed at another endpoint has NO origin and is left
    alone: they are working right now, just elsewhere. (Whether an IM turn's
    tool progress should mirror into an attached CLI at all is a separate
    identity-continuity decision, not settled here.)
  - The CLI marks that run id (`markBackgroundRun`) and drops its content
    events at reduction time (`backgroundRunEvent`): stream deltas, thinking,
    tool start/output/heartbeat/done, plan snapshots, learning notices. It
    renders one closing line — `Watcher <id> | status: finalized | task: <run
    status>` or `Background run (<origin>) | task: <run status>` — with the
    bounded outcome summary. The raw run status is used, so a blocked watcher
    check's `waiting_user` stays distinguishable from `done`. A watcher also
    keeps its opening notice (it continues a boundary the person already saw);
    a bare background run stays silent until it has something to report.
  - Approvals, clarifications, watcher notices and the run lifecycle are NOT
    filtered: a background run that needs a human must still reach one, and
    `runStatus`/`daemonRunActive` stay accurate so the person still sees why
    their next message queues. The run id is retained after the run ends, so
    trailing events cannot land under the next turn. Detail stays in
    `/diag execution`.
  - Not covered: a TUI that starts while such a run is already in flight still
    attaches to it through the digest re-attach path (an explicit, separately
    labelled "Watching" mode). Follow-up: `kernel.WithTurnSource` is now the
    third of three origin signals and only survives synchronous paths — the
    spine's `[cron]` tag is lost on an async cron run, and could be derived
    from `req.Origin` instead.
- Coverage: `internal/gateway/cli/event_identity_test.go`,
  `internal/gateway/cli/attach_digest_test.go`,
  `internal/gateway/cli/daemon_queue_test.go`
  (`TestWatcherFinalizationRunRendersResultNotProcess`,
  `TestCronRunRendersResultWithoutStartNoticeOrProcess`,
  `TestForegroundRunStillStreamsAfterBackgroundRun`),
  `internal/gateway/httpapi/run_origin_test.go` (classification plus the
  `run.started` wire contract), and `internal/gateway/client/client_test.go`.

### Memory Governance Closeout (2026-07-12)

- Query-aware canonical recall shipped (2026-07-26): governed canonical memory
  now registers as a bounded `RecallSource` beside task cards and indexed
  sessions, reusing optional `semantic_recall` query expansion without adding
  another foreground model call. Selection is person-partitioned, limited to
  global/current logical-workspace scope, validity-aware, and excludes pinned
  rows (already injected unconditionally). Only canonical rows surviving the
  shared three-slice budget are access-touched; the static memory fallback
  suppresses those ids in the same turn. `context.recall.sources.canonical`
  exposes actual adoption. Embedding recall remains a later implementation of
  the same source seam.
- Existing canonical references support `/memory pin <ref>` / `unpin <ref>`
  without changing evidence or scope. `/diag memory` reports status,
  protection, scope, visible-topic, consolidation-candidate counts, and the
  effective governance mode.
- Prompt access accounting touches only canonical rows actually selected for
  injection; scanning a candidate no longer refreshes archival freshness.
  Native tool dispatch carries `_workspace_id`, keeping project/environment
  facts workspace-scoped while user preferences remain global.
- Shadow consolidation now emits private JSON and human-readable Markdown
  calibration reports (`memory/reports/shadow-<person>.{json,md}`) with active
  counts, candidate groups, judged groups, rejected groups, projected active
  count, and each proposed action. `/diag memory` summarizes the latest pass.
- Next: review real-history report precision, deliberately enable
  high-confidence `merge-only`, then tune caps/archival and FTS+CJK memory
  search. Automatic promotion remains forbidden.

## Highest-Value Next Work (by priority)

These are the live gaps, ordered by their distance from the north star
(`docs/identity-continuity.md` — the three continuity scenarios). This section
is the only priority list in the repo; other docs must point here.

### Execution-quality closeout — shipped 2026-08-08

The 2026-08-07 live review exposed five related correctness gaps: a recovered
system queue row could be reclaimed while its worker was still alive; watcher
checker failures could overwrite a successful external operation; retry runs
could repeat one logical finalization effect; approval triage blurred task
context with the user's current authorization; and weak task attachment could
rewrite an established task card. The following boundaries are now enforced:

- **Leased queue claims.** `task_queue` carries an opaque `claim_token`,
  `lease_until`, and `attempt_generation`. Claim, bind, and heartbeat renewal
  are token-checked; recovery only requeues a started system row after its
  lease expires. A stale worker cannot bind or renew a newer attempt.
- **Effect-level finalization idempotency.** `effect_receipts` owns one stable
  logical effect key (watch id + verdict revision) across retry run ids. A
  duplicate retry still receives its own terminal run events, but task status,
  handoff, assistant message, maintenance job, and final delivery are emitted
  only by the effect owner.
- **Watcher phase separation.** Durable watches persist independent
  `checker_status`, `operation_status`, and `verification_status`. Once the
  external operation is terminal-success, a later checker or verification
  failure parks verification without rewriting the business verdict as a
  timeout or failure.
- **Typed approval intent.** Smart triage receives a `RunIntentSnapshot` with
  raw user text, advisory task summary, deterministic allow/deny facts,
  workspace id, source, and work key. The task summary is explicitly context,
  never authorization. A current explicit deny forces a human decision and
  outranks full-auto, deterministic containment, durable grants, and the
  judge; hard safety floors remain unconditional.
- **Task-card source protection.** A weak pre-label attachment to an existing
  task may add the run, events, handoff, and maintenance proposal, but cannot
  overwrite the task's stable lifecycle, summary, or next steps before its
  display label is resolved. A deterministic sole label or a successful KEEP
  decision reconciles lifecycle afterward; a new placeholder can still receive
  its first card and be relabeled after the run.
- **Delivery truth.** Delivery distinguishes durable acceptance from confirmed
  transport delivery. `pending_session` and `sent_unconfirmed` remain eligible
  for the existing catch-up path and no longer mark the source notification as
  delivered. `/diag` reports sent/unconfirmed/pending/failed health per
  platform without exposing peer ids.
- **Memory boundary.** Concrete ticket/build/run current state is discarded at
  intake even if a model labels it durable; prefixed operational states such as
  `CI_PENDING_APPROVAL` are classified too. Canonical recall access touches use
  a cancellation-independent context, so a selected memory's usage evidence is
  not lost when the foreground request closes. Maintenance remains one logical
  batched analyzer result per run; this closeout adds no foreground model call.

Regression coverage: `control/queue_test.go`,
`control/run_finalization_test.go`, `control/catchup_test.go`,
`gateway/httpapi/external_watch_match_test.go`,
`gateway/httpapi/approval_options_test.go`, `gateway/delivery/delivery_test.go`,
`tools/approval_triage_test.go`, `app/memory_intake_test.go`, and
`kernel/memory/transient_classifier_test.go`.

Next evidence gates: exercise queue recovery during a real long-running run,
observe one watcher operation-success/verification-blocked case, and calibrate
per-platform delivery health from live IM traffic. These are runtime
validation gates, not unfinished alternate code paths.

### ACTIVE PLAN — watcher four-layer boundary (2026-07-30)

Two live failures on 2026-07-30 had the same shape: a lower execution layer
failed and the watcher rendered it as a business verdict. Evidence:
`watch_ad92afb7` (AWS) repeated one identical `credential_state_readonly`
failure 65 times until its deadline, and `watch_bf0bff20` (GCP) turned "cannot
query" into `BUILD_FAILED` while both Cloud Build reruns had in fact
SUCCEEDED. Layers now enforced in `httpapi/external_watch_policy.go`:
L0 environment → L1 execution → L2 observation → L3 business.

- **Batch 1 shipped (2026-07-30).**
  - `SynthesizeDir` primitive (`envprofiles`, `tools/sandbox`): a writable
    tmpfs shell at a declared state root makes nested overlay targets
    mountable. The aws profile declared writable `~/.aws/sso/cache` and
    `~/.aws/cli/cache`, but the host had neither directory and bwrap cannot
    create a mount point under a read-only root — so the sandbox aborted
    before the command ran and EVERY `aws` command failed in isolated mode,
    including `aws --version`. Non-mountable overlays are now dropped with a
    recorded note instead of producing a plan that cannot be enforced.
    `SandboxPlanVersion` = 2.
  - Durable checks return the typed `ExecutionResult`
    (`tools.RunDurableCheck`). The watcher's private marker table is deleted:
    one classifier (`ClassifyToolError`) feeds one policy table. The two
    classifiers had disagreed on exactly the case that mattered
    (`read-only file system` was in one and not the other).
  - Fixed layer order: an L0/L1 failure never reaches pattern matching. Success
    additionally requires a clean exit; a terminal failure does not (status
    CLIs legitimately exit non-zero).
  - Circuit breaker: `external_watches.failure_class`/`check_signature`/
    `consecutive_failures`; three identical failures park the watch
    (~90s at the default interval, versus the previous two hours).
  - A parked watch records a structured reason (`blocked_environment`,
    `invalid_check`, `environment_changed`, `repeated_failure`). The notice
    reads `Watcher <id> blocked: <reason>`, the finalization prompt states the
    external state is UNKNOWN and routes to `waiting_user`, and the recovery
    path refuses to re-match the stored output of a failed check.
  - Tests: `tools/envprofiles/synthesize_test.go`, `tools/sandbox/sandbox_test.go`,
    `httpapi/external_watch_policy_test.go`, `httpapi/recovery_notification_test.go`
    (park-on-first-check and defective-check-never-matches-patterns),
    `control/external_watches_test.go` (streak).
- **Batch 2 shipped (2026-07-30).**
  - **Decidability-gated host inventory.** `envprofiles.AvailableOnHost` returns
    the operator profiles whose state actually exists on this machine, and
    `execCommandProgramSet` reports whether a payload's program set is
    decidable. A decidable command still gets exactly what it names (a non-GKE
    kubectl must not receive Google credentials — that least-privilege decision
    from review round 2 is preserved and tested); an UNDECIDABLE payload
    (interpreter + inline script/heredoc, `./script.sh`, `make`, `xargs`,
    `find`, npm scripts, `execute_code`) gets the host inventory, because
    parsing cannot know what it will invoke. Verified live: the exact GCP
    failure shape (`python3 - <<PY … subprocess.run(['gcloud'…])`) now resolves
    `CLOUDSDK_CONFIG` into the lease overlay and returns `SUCCESS`, while
    `printf` in the same workspace still gets no gcloud state.
  - Preparation is memoized per lease (`exec_prepared.go`), keyed by state dir +
    resolved profile set + trust + credential access. Conditional requires are
    resolved BEFORE the key is computed (`envprofiles.Resolve`), or a kubeconfig
    that changed to GKE mid-lease would reuse a preparation without gcloud.
  - **Registration preflight** (`external_watch_preflight.go`): the frozen check
    runs once, in the agent's turn, with the run's own material. Observed
    success → nothing registered; observed FAILURE on the first check → an
    error, never an unattended verdict (this is the case that produced a false
    `BUILD_FAILED`); blocking class (environment/syntax/not_found) → refused
    with the typed class so the model fixes it now; pending → registered. No new
    approval surface: `watch_external` is already an exec tool and its command
    passes the safety floor at registration.
  - Dedicated terminal status `blocked_environment` (distinct from `failed`)
    with finalization/notification scans, notice, task summary and
    `/diag execution` updated; a blocked watch lists reason + failure class +
    remedy. Eval case
    `evalcases/reliability/external-watch-preflight-refuses-broken-check.yaml`
    covers the refusal message path.
  - Tests: `tools/exec_profile_test.go` (inventory for opaque payloads,
    least privilege for decidable ones, decidability table),
    `tools/envprofiles/synthesize_test.go` (inventory scope, toolchain-root
    guard, catalog redirect uniqueness — applying the whole inventory makes a
    duplicate redirect a global failure, so it is now a build-time assertion),
    `tools/external_watch_preflight_test.go` (five outcomes).
- **Batch 2.5 shipped (2026-08-01): frozen durable execution binding.**
  - New watches persist a versioned, secret-free `executionenv.Binding`
    derived from the creating run's lease. Registration no longer samples
    `Registry.Current()` after preflight, and polling no longer reconstructs
    trust/capabilities from whatever the daemon currently exposes.
  - The binding freezes environment profile, credential references, trust,
    effective capability names and capability provenance. Later grants cannot
    widen a running watch; trust withdrawal and persisted-grant revocation or
    expiry stop it before the next command. A one-shot registration approval is
    bounded by the watch deadline.
  - Snapshot ids are process-local indices rather than authorization material:
    after restart all three fingerprints are checked even when an id collides.
    Compatible restarts rebind; account/toolchain/credential-source changes
    park with `environment_changed`. Exact in-process snapshots keep the
    environment and authentication material that passed preflight.
  - `DurableExecutionScope` now carries the same binding future runners can
    consume. Tool credential overlays stay stable across polls, and a missing
    person-level toolchain cache falls back to private watcher scratch instead
    of failing valid work.
  - Coverage: `executionenv/binding_test.go`,
    `control/external_watch_binding_test.go`, `tools/durable_binding_test.go`,
    and `httpapi/external_watch_environment_test.go` (frozen grant, no later
    expansion, immediate revocation).
- **Deterministic reliability closeout shipped (2026-08-06).** A v2 watch that
  declares `target_pattern` must also declare both terminal success and failure
  patterns, so an external operation that skips an intermediate handoff state
  reaches a real verdict instead of timing out. Gateway single-owner admission
  now treats `gateway.lock` as authoritative and PID/heartbeat files as health
  metadata, avoiding false already-running results after PID reuse. Legacy
  lifecycle records receive a stable derived instance id before their unclean
  exit event is written. Historical parked-task cleanup is deliberately
  offline and evidence-bound: `selfmind maintenance task-audit` is dry-run by
  default; `--apply` only backfills a missing blocker when the inactive task and
  newest finished run have exactly the same blocker status, never rewriting
  task/run state. Smart-approval routing remains unchanged pending live evidence.
- **Still owed.** A dedicated `/watchers` command (batch 2 uses
  `/diag execution`), and lease-level preparation for `start_process`
  background children beyond the material they already share.
- **Batch 3 (Watch Protocol v2).** Structured `checks[] + reducer` so a
  multi-target watch never needs a model-authored aggregator script, plus a
  `watch_attempts` ledger carrying layer/owner per attempt. This is also the
  Runner job envelope; see `docs/execution-engine.zh-CN.md` §12.

### ACTIVE PLAN - execution environment foundation (2026-07-28)

- **Foundation shipped:** every terminal/code/verification shell and stdio MCP
  child now receives an explicitly constructed environment through
  `BuildProcessEnv`. Operator toolchain and Agent CLI login variables remain
  compatible, while SelfMind gateway/control-plane addresses, tokens, and
  tenant/person/task/channel identity do not reach child processes.
- **Foundation shipped:** daemon-configured opaque credentials and stripped
  control-plane values register with the shared runtime `SecretRegistry`.
  Existing `RedactSensitive` callsites now mask exact values as well as known
  token shapes. Values remain in memory only.
- **Foundation shipped:** reusable `sandbox=host` grants now include a
  non-secret workspace plus command-family fingerprint. A request without
  durable workspace identity is approvable once but cannot mint a reusable
  host grant. Specific inner classes such as egress, chmod, and destructive
  commands remain visible instead of collapsing into one generic host class.
- **Foundation shipped:** `/diag execution` reports the effective sandbox
  backend/capability, network posture, process-environment builder, and active
  workspace roots without exposing credential names or values.
- **Execution environment boundary shipped:** local CLI is the only authority
  that can run `selfmind ws trust|untrust`; remote discovery creates an
  untrusted workspace. Untrusted exec starts without network and may request
  an expiring workspace-scoped `network:shared` capability after a classified
  network failure. Untrust revokes active capabilities.
- **Run binding shipped:** each run materializes one immutable
  `EnvironmentLease` containing policy, capability names, credential
  references, and non-secret principal fingerprints only. Replay reuses the
  lease; task continuation creates a new run/lease. A missing local workspace
  becomes `waiting_user` with `environment_unavailable`, not a misleading tool
  failure.
- **Skill and long-process boundary shipped:** workspace skill discovery uses
  `ExecutionScope`, excludes untrusted workspace roots, and rejects
  credential-shaped environment passthrough declarations. `terminal`,
  `verify`, and `execute_code` share vendor-neutral `standard`,
  `long-running`, and `interactive` timeout/heartbeat profiles.
- **Diagnostics shipped:** `/diag execution` shows workspace trust, active
  capability names, safe lease/profile metadata, and only a hidden credential
  reference count. Follow-up: credential-aware private HOME views and
  `credential:read`; automatic wake-up when a compatible remote environment
  becomes available. See `docs/tool-safety.md`.

### Maintenance provider cost controls (2026-07-22)

- Kimi/Anthropic-compatible HTTP 200 responses that contain no usable text but
  end with `max_tokens` now preserve usage and finish metadata, open a bounded
  soft circuit, and fail over once instead of repeatedly consuming the same
  route for every queued maintenance batch.
- Provider, quota, network, and timeout failures no longer recursively bisect
  a multi-run maintenance batch. Bisection is reserved for malformed batch
  result shapes, where it can isolate the bad result without multiplying an
  upstream outage.
- Maintenance calls persist per-route success/failure/circuit and token
  accounting. `/diag models` exposes the recent totals without invoking a
  model. Batch output budgets are bounded to 3,072 tokens for one run and
  10,240 tokens for a full batch.

### ACTIVE PLAN — Loop Engineering: typed state + exact recovery (approved 2026-07-19)

Owner-approved 2026-07-19 after a three-round Codex-vs-SelfMind loop analysis
(capability map, structural map, corrective review) plus live incidents
(87-minute approval stall; boot requeue re-running side-effectful turns;
steering accepted into a memory channel before durability). Principles: keep
the product moats (daemon/queue/memory/IM/cron/watchers); unify state and
completion semantics BEFORE adding more rules; hard iteration/budget caps stay
as safety limits but become per-model-tier configuration; assistant deltas
stay ephemeral and task_events stays the single durable source; sandbox and
state-machine work proceed as separate change streams.

- **P0-A Persistent steering mailbox — ✅ shipped 2026-07-19.** `steering_mailbox` table
  (accepted → claimed → consumed / deferred / expired, full text + content
  hash + idempotency key). Persist BEFORE returning Accepted; the in-memory
  channel becomes delivery, not the record. Consumed is marked when the
  kernel's `agent.steering` event commits; run finalization and daemon boot
  defer leftovers into the task-pinned durable queue
  (`steering:<id>` idempotency keys); stale rows expire. Acceptance: crash in
  any accepted-but-unconsumed window loses nothing; replay is at-least-once
  with queue-level dedup.
- **P0-B Internal StepOutcome state machine + tool execution ledger — ✅
  shipped 2026-07-19.**
  Shipped first: the tool execution ledger. `tool_ledger`
  (dispatched → completed/failed) records every dispatch with a retry class
  (`kernel.ClassifyToolRetry`: read_only / idempotent / side_effect, failing
  SAFE — unknown tools are side_effect) via an injected `kernel.ToolLedger`
  seam (mirrors ToolArtifactSink; nil-safe, best-effort). A crash between
  dispatch and outcome leaves a durable `dispatched` (uncertain) row; on
  resume, `withResumeContext` surfaces the task's uncertain SIDE-EFFECT
  entries (`uncertain_tool_calls`) instructing the model to VERIFY real state
  read-only before repeating — never blindly re-fire a deploy/build/POST.
  Read-only uncertain entries are excluded (blind re-run is safe); resolved
  rows prune at boot, dispatched rows never. Tests:
  `kernel/tool_ledger_test.go`, `control/tool_ledger_test.go`.
  **Ledger closure shipped 2026-07-19:** the uncertain-tool warning is now
  injected INDEPENDENT of continuation intent (`withUncertainToolWarning` at
  run setup, not gated on IntentContinue) — a boot-requeued 'started' row
  re-drains with its ORIGINAL content (classifies as a new message), so the
  intent-gated resume path would have missed it and let the run silently
  re-fire a deploy/build/POST. Any run touching a task with uncertain
  side-effect entries is told to verify real state read-only first. Tests:
  `httpapi/tool_ledger_warning_test.go`. P0-B is functionally complete; the
  full StepOutcome control-flow INVERSION (each iteration returning an outcome
  that a match drives) remains an optional readability follow-up now that the
  transitions are typed and observable.
  **StepOutcome completion classification shipped 2026-07-19:** the three
  duplicated turn.completed emission sites collapse into one typed
  `resolveTurnCompletion(completionSignals)` (pure, precedence-preserving:
  output_limit > tool_budget_exhausted[unless finish_run status] >
  plan_unresolved > max_iterations > completed) emitted via a single
  `emitTurnCompleted`. The hard iteration cap is DEMOTED to a safety
  backstop: hitting it no longer returns the "max iterations reached" stub
  that discarded work — it finalizes from the collected answer (continued
  buffer → last assistant content → honest resume note) and saves the spine,
  same as any bounded stop. `StepOutcome` vocabulary
  (continue_model/execute_tools/complete_turn/fail_turn) is defined; the
  mid-loop continue/execute/compact transitions remain inline. Tests:
  `kernel/step_outcome_test.go`. The kernel owns typed model/tool/compact/
  terminal transitions. Approval and external-wait remain tool/gateway
  lifecycle states rather than duplicate kernel outcomes. A future readability
  refactor may make each iteration return one `StepOutcome`, but it is not a
  missing reliability behavior: completion classification, durable checkpoint,
  uncertain-side-effect recovery, and the safety-limit fallback are shipped.
- **P0-C Mid-turn compaction as a loop state — ✅ shipped 2026-07-19.** The
  loop now recomputes the window budget every iteration and runs the existing
  head(goal)+tail(recent input/steering/plan/evidence)+summarize-middle
  compaction WITHIN the run before the next model call, instead of growing
  until a provider context-window rejection forces emergency recovery. A
  `compact_context` StepOutcome + the existing `context.compacted` event fire
  when it triggers; the run continues. Tool_call/result pairs orphaned by the
  summary are dropped safely at EVERY adapter boundary (verified:
  chat/anthropic `sanitizeToolMessageLedger`, responses `pendingToolOutputs`).
  No-op under budget (a cheap per-iteration token estimate), skipped on the
  first iteration (fresh from the Composer). Tests:
  `kernel/agent_kernel_test.go` (`TestMidTurnCompactionFiresWithinRun`).
  Smaller follow-ups (not blocking): an explicit open-fresh-window primitive
  (summarize + deterministic-trim fallback already keep the run under budget),
  per-model-tier action-budget relaxation, and head-trim-on-summary-retry
  cache-prefix nicety.
  ORIGINAL PLAN: Recompute the token budget
  after every model step; over threshold → `CompactContext` → summarize or
  open a fresh window (keep original task + latest summary + plan + changed
  files) → continue the SAME run. Hard rules: tool_call/result pairs live or
  die together; newest user input, unconsumed steering, goals, plan, and
  verification evidence always survive; compaction retries trim from the head
  to preserve the cache prefix. Afterwards relax action budgets per model
  tier.
- **P0-D Sandbox-first execution — ✅ Linux contract shipped 2026-07-19.**
  Terminal, verify, and execute-code tools expose an explicit per-call
  `sandbox: auto|isolated|host` contract. `auto` is the default: it uses
  bubblewrap isolation when available and otherwise records an observable
  fallback to approval-controlled host execution; `isolated` fails closed
  when isolation is unavailable; explicit `host` execution is approval-gated;
  and operator policy `exec_sandbox.required=true` disables every host
  fallback. The default operator policy is enabled/no-network, while doctor
  and startup health report whether the host is ready, degraded, or blocked.
  The implementation is `internal/tools/sandbox` (bwrap capability detection —
  binary + unprivileged user namespaces — and read-only-root /
  writable-workspace / no-network argv construction, unit-tested for argv
  correctness and detection); config `exec_sandbox.{enabled,required,
  allow_network}`; and `sandboxedShellCommand`, shared by terminal, verify,
  and execute-code. The WSL target has `/usr/bin/bwrap` 0.9.0 installed and a
  live isolation soak passed: workspace writes succeed, host configuration is
  read-only, and the default shared-network policy reaches ArgoCD through the
  daemon's inherited proxy. `allow_network: false` still creates an empty
  network namespace for operators choosing the strict posture.
  External side effects such as deploy APIs are not made exactly-once by a
  filesystem sandbox; their safety remains P0-B's ledger. Tests:
  `tools/sandbox/sandbox_test.go`,
  `tools/exec_sandbox_test.go`.
  ORIGINAL PLAN: **P0-D Sandbox-first execution MVP (parallel track, separate changes).**
  bubblewrap + seccomp (Landlock alone cannot gate network egress):
  terminal/verify default to workspace-writable + read-only-elsewhere +
  no-network; sandbox denial escalates through the existing approval funnel
  with the first approval cached (class grants). External side effects
  (gcloud/deploys) are NOT sandboxed — their safety is P0-B's ledger
  verification. Kills the 87-minute-stall class structurally.
- **P1**: **Compatibility normalizer shipped 2026-07-19:** fallback
  `[TOOL:...]` and XML parsing, aliases, incomplete-call detection, and
  balanced JSON extraction now live in `internal/kernel/llm/tool_compat.go`;
  the Agent core consumes normalized calls through a thin wrapper. Remaining:
  a real provider-owned `TurnSession` only when a provider can preserve remote
  response state without violating `store=false`; item lifecycle events with
  commit-before-confirm ordering; and in-stream execution only after the
  transport can prove a tool item complete and recover it from the ledger.
  Per-call parallelism must preserve model order and read/write dependencies,
  so mixed batches intentionally remain serial until that contract exists.
- **P2**: PTY/unified exec; review sub-agent mode (restricted-backend parts
  already exist); rate-limit window surfacing in /status; sub-agent messaging
  and context-projection options.

**Acceptance matrix (every P0 lands against it):** crash/interrupt injected
at ① steering accepted-not-consumed ② tool planned/dispatched ③ side effect
succeeded but result uncommitted ④ waiting on approval ⑤ compacting
⑥ provider truncation/context overflow ⑦ new message at the final-answer
boundary — assert no message loss, no duplicated side effect, no state lie,
and `uncertain` verifies instead of re-running.

### ACTIVE PLAN — Daemon-only + north-star experience (approved 2026-07-10)

Owner-approved consolidation (2026-07-10, multi-agent cross-review calibrated).
Core goal: CLI / IM / cron / HTTP API are ALL just entrances; everything enters
ONE daemon; the daemon is the only agent runtime. Supersedes item 7 below and
absorbs the weixin-push remainder of item 1(0). Order within a phase is the
execution order.

**P0 — north-star closure**

- **P0-1 IM delivery loop completion — ✅ shipped and owner-verified on a real
  WeChat device 2026-07-10.** iLink ret/errcode checking and
  `sent_unconfirmed` (terminal, no blind retry — duplicate risk) were already
  shipped. New: (a) **catch-up re-push** — any IM inbound (the weixin adapter
  saves the fresh context_token before dispatch) fires
  `delivery.Service.CatchUpUnconfirmed` (goroutine off `ProcessMessage`,
  30s-bounded ctx) which re-delivers that person+platform+channel's
  `sent_unconfirmed` rows behind THREE anti-duplicate rails: at-most-once per
  row (`outbound_messages.catchup_at` claim column, claim-before-send via
  `Store.ClaimDeliveryCatchUp`), a freshness window
  (`gateway.delivery_catchup_max_age`, default 4h), and a per-catch-up cap
  (default 3) oldest-first; a re-push that is STILL unconfirmed keeps its
  consumed claim — never a duplicate drip. (b) `/diag` shows `Outbound (24h):
  sent N, unconfirmed N, failed N` + the newest undelivered reason
  (`Store.CountOutboundByStatusSince`). (c) `selfmind doctor` explains the
  stale-context_token failure mode and the reply-to-recover path. Tests:
  `control/catchup_test.go`, `delivery/catchup_test.go`. The owner verified
  text, async completion, file delivery, approval round-trip, reminder, and
  diagnostics on a real phone; one observed iLink `ret=-3` remains visible in
  `/diag` as a failed push rather than a false success.
- **P0-2 cassette gate integrity — ✅ shipped, re-recorded, and replay-verified
  2026-07-14.**
  Root cause fixed: the VCR per-session counter was
  process-global and never reset, so a case re-run in one process continued
  numbering (recording 0001+ holes — the observed `.vcr/continuity_task_attach/`
  corruption — and breaking same-process replays). Now `llm.ResetVCRSession`
  runs at the start of EVERY case execution and record mode wipes the case's
  previous cassette generation first (`llm.WipeVCRSessionRecordings`,
  runner wiring in `internal/eval/runner.go`). `HasCassetteSession` is strict:
  0000.json required AND gap-free 0000..max ("any *.json" would mask exactly
  this corruption). The normal-turn flight recorder also now yields to an
  explicit eval VCR mode instead of replacing the case session with a random
  `flight-*` session. The owner re-recorded a gap-free
  `.vcr/continuity_task_attach/0000..0003.json` generation and
  `continuity-task-attach.yaml` now requires the cassette. Tests:
  `llm/vcr_test.go`, `kernel/flight_recorder_test.go`; acceptance: strict
  offline replay of `continuity-task-attach.yaml` passes without provider
  access, and `selfmind selfcheck` stays strictly offline.
- **P0-3 session-search-over-daemon, THEN delete in-process — parity ✅ shipped
  2026-07-10; deletion still pending.** Landed: (1) **dispatch partition fix**
  — daemon runs store memory/session-FTS/checkpoints under the PERSON id
  (`RunAgentWithEvents(ctx, identity.PersonID, …)`), but `/v1/dispatch` forced
  the CONTROL tenant on every tool, so client `/memory list` read an empty (or
  stale in-process-era) partition. Now person-partitioned tools (`memory`,
  `session_search`, `checkpoint`) dispatch with `identity.PersonID`; skill
  tools keep the control tenant (the daemon skills-dir key)
  (`handlers_dispatch.go` `personPartitionTools`). (2) **structured
  `GET /v1/sessions`** — search (`?q=`), recent, and message window
  (`?session_id=&around=&window=`), always person-partitioned, backed by
  `Server.Sessions` (`SessionsBackend`, wired to the memory manager in the
  runner); nil-safe 503 (`handlers_sessions.go`). (3) **TUI `/search`** —
  works in BOTH modes via the `m.dispatch` seam (client → daemon dispatch with
  the fixed partition; in-process → local dispatcher), closing the named
  parity gap; the dead in-process-only `sessionSearchFn` wiring was removed.
  Tests: `httpapi/handlers_sessions_test.go`. **Deletion step ✅ done
  2026-07-10:** `SELFMIND_TUI_INPROC` and the in-process fallback are GONE —
  `runTUI` always runs the thin client (`cliapp/root.go`); a daemon that can't
  be reached/started FAILS with actionable guidance (gateway status / `gateway
  run` / doctor), never a local agent. `EnsureRunning` timeout error now names
  the address, wait, cause, and where to look (stale gateway.lock / port /
  config). The legacy alt-screen TUI (`SELFMIND_TUI_LEGACY`, viewport,
  `controller_mouse.go`, render cache) was removed with it — hybrid is the only
  renderer. Scope note: "eval covers the daemon path" means eval keeps
  exercising the full gateway `Server` code path (in-process harness,
  data-isolated) — it does NOT spawn daemon OS processes.

- **P0-4 execution continuity + post-run/task governance — ✅ shipped
  2026-07-10.** Thin CLI clients subscribe to daemon-owned
  `GET /v1/events/stream` SSE and render durable run events plus ephemeral
  assistant deltas while the final message response remains the source of truth.
  Every committed event receives a monotonic durable cursor; reconnects replay
  with `Last-Event-ID`, and a stream gap triggers a bounded database catch-up.
  IM/cron deliberately keep low-frequency stage/final delivery only and never
  receive token deltas.
  The agent recovers opening and partial-stream EOF/reset/idle failures through
  bounded non-stream continuation, compacts again on context-window rejection,
  and never executes an incomplete native tool call. Run finalization now
  persists maintenance evidence without a model call; the daemon groups only
  same-tenant/person/workspace jobs behind a configurable debounce/max-wait/run
  cap. One app-layer `PostRunAnalyzer` provider call (explicit
  `tasks.maintenance_model_role`, default `memory_extract`) may return several
  run-keyed results, while every run keeps one independently frozen proposal
  combining KEEP/MOVE/TITLE/INBOX with durable user/workspace facts. The old
  per-turn/final/profile calls are no longer wired. Task listing
  is SQLite-filtered and paged by view/status/workspace/keyword; failed first-
  run placeholders are deleted only when they have no durable history. Tests:
  `client/client_test.go`, `httpapi/live_stream_test.go`,
  `kernel/agent_retry_test.go`, `app/post_run_analyzer_test.go`,
  `control/task_governance_test.go`, `httpapi/run_labeler_test.go`.

P0 acceptance: CLI/IM/cron all execute in the daemon; no `SELFMIND_TUI_INPROC`;
no automatic local-agent fallback; WeChat reliably receives progress/approval/
completion/failure; continuity eval replays offline.

**P1 — long-conversation quality**

- **P1-1 native-tools prompt dedup — ✅ shipped 2026-07-10.** Tool definitions
  were DOUBLE-SENT (native `ChatRequest.Tools` AND the full name list +
  descriptions + param schemas in the system prompt). Now `buildSystemPrompt`
  keeps the behavior contract always, but emits the name-list/descriptions/
  schemas only for providers WITHOUT native tool support, probed via
  `llm.ProviderSupportsNativeTools` (the `NativeToolsCapable` interface,
  forwarded through the role router and VCR wrappers to the provider actually
  routed this turn). Native providers get a one-line note instead.
  `kernel/context…`, `llm/provider.go`, adapters. Tests:
  `kernel/tools_prompt_dedup_test.go`.
- **P1-2 context usage breakdown — ✅ shipped 2026-07-10.** Each turn emits a
  `context.breakdown` run event (`kernel.ComputeContextBreakdown`:
  identity/tools/project_context/memory/runtime/history token shares by section
  marker); `/diag` reads the newest one back and renders a one-line share view
  (the TUI shows it via `/diag`). Tests: `kernel/context_breakdown_test.go`.
- **P1-3 stable/volatile prompt split + prompt-cache prefix — ✅ shipped
  2026-07-10.** `buildSystemPrompt` groups content into a STABLE prefix (soul,
  guidance, tool contract+defs, skills) and a VOLATILE suffix (runtime context,
  memory/profile, per-turn conditionals like frontend guidance), joined
  stable-then-volatile so the cacheable prefix is maximized. The pre-fix bug
  injected volatile runtime context BETWEEN soul and tools. No AGENTS.md
  diffing (rejected — upkeep outweighs the marginal cache win). Tests:
  `kernel/prompt_layering_test.go`.

- **P1-4 evidence-backed execution completion — ✅ shipped 2026-07-14.**
  Agent completion is now a structured contract rather than an optimistic final
  sentence. `turn.completed` and `api.RunOutcome` carry
  `completion_reason`, `resumable`, and deterministic verification state. The
  action-tool budget starts small, extends only after a new successful,
  non-lifecycle tool call (a unique evidence signature), and stops at a hard
  ceiling; budget/output/iteration exhaustion is
  terminal `interrupted`, never a false `done`. File/patch/terminal/verify
  calls emit durable `evidence.recorded` events. The gateway derives changed
  files and verification freshness from those events, rejects unsupported
  completion claims, and persists provider/transport failures as resumable
  interrupted runs; daemon recovery emits the same structured outcome.
  `verify` is a first-class, approval-aware tool for explicit checks.
  Workspace writes use same-directory temp files plus atomic replacement on
  Unix and Windows, including empty-file writes. Client workspace discovery
  now preserves explicitly registered `allowed_roots`; only an explicit
  workspace update can revoke roots. Eval expectations can assert completion
  reason, resumability, and verification state. Tests:
  `kernel/agent_kernel_test.go`, `tools/atomic_write_test.go`,
  `httpapi/evidence_outcome_test.go`, `httpapi/run_completion_test.go`,
  `control/runtime_test.go`, `eval/checks_test.go`. Eval:
  `evalcases/reliability/create-and-verify.yaml` (live cassette pending).
  **Terminal reconciliation (2026-07-15; storage bounded 2026-08-03):**
  `tool.output` preserves `tool_call_id` through builtin execution and the live
  SSE/TUI path, but line-level output is intentionally not persisted. The
  bounded `tool.completed` preview is the durable replay/audit record. Only a
  matching active call can update a tool cell; unmatched and
  late events are ignored, active output is a three-line tail, and terminal run
  states remove unfinished transient cells. Verification control text stays
  concise English and successful verification adds no extra transcript line.
  Tests: `gateway/cli/controller_test.go`, `gateway/client/client_test.go`,
  `gateway/httpapi/run_events_test.go`, `tools/builtin_test.go`.
  **Budget and lifecycle reconciliation (2026-07-17):** action tools now use a
  finite elastic budget of 10 initial calls and at most 34 calls, extending in
  four-call increments only when new successful evidence is produced. Exhausting
  that action budget no longer blocks lifecycle-only `update_plan`/`finish_run`
  calls or overrides a successful structured completion. Output limits,
  transport truncation, and missing lifecycle closure still remain incomplete.
  Identical visible plan snapshots are deduplicated, resolved snapshots remain
  available for deduplication, and `update_plan` is capped at eight calls per
  run. Tests: `kernel/agent_kernel_test.go`,
  `kernel/native_tool_call_test.go`, `kernel/task_strategy_test.go`,
  `tools/task_protocol_test.go`, `httpapi/run_completion_test.go`.

- **P1-5 durable external waits + visible recovery health — ✅ shipped
  2026-07-16.** CI/CD and other slow external systems no longer require an
  active model loop to poll. The native `watch_external` tool registers one
  bounded, persisted command with success/failure patterns, interval and hard
  timeout; the registering run exits as `waiting_external`, while the daemon
  claims due checks from `external_watches`, executes them under the existing
  terminal scope/egress/approval policy, updates the task, records events and
  sends the preferred IM notification. This removes foreground token burn and
  frees the per-person run slot while waiting. Structured `waiting_external`
  remains authoritative through gateway finalization and is exposed as the
  public turn status instead of being overwritten by generic `completed`.
  Post-run maintenance now treats
  non-retryable provider errors (quota/auth/config) as `blocked_provider`
  instead of spending five retries; `/diag` reports background learning as
  paused and daemon restart grants one fresh recovery probe. Boot/stale-run
  recovery also sends one idempotent preferred-channel interruption notice
  (`run.recovery_notified`) after enqueue succeeds. Tests:
  `control/external_watches_test.go`,
  `control/recovery_notifications_test.go`,
  `httpapi/recovery_notification_test.go`,
  `httpapi/run_labeler_test.go`. Eval:
  `evalcases/reliability/external-watch-handoff.yaml`; the live cassette was
  recorded on 2026-07-16 and now provides the offline replay gate in addition
  to deterministic unit coverage.
  **Adoption contract (2026-07-17):** the agent prompt now requires durable
  `watch_external` handoff after at most one short status check for long-running
  CI/CD, deployment, and other remote jobs, rather than foreground polling.
  **Watch correctness + closure (2026-07-17, after two live false timeouts):**
  pattern matching now normalizes CRLF/whitespace and matches per line, so
  anchored patterns like `^SUCCESS$` hit real CLI output (`"SUCCESS\n"`);
  terminal-state classification wins over the deadline (a late SUCCESS is a
  success, not a timeout); a watch whose final output is fresh and
  non-terminal earns exactly ONE bounded 30m extension
  (`external_watches.extensions` CAS); daemon startup re-matches recently
  timed_out watches' recorded output and revises misjudged verdicts with full
  completion side effects; and a SUCCEEDED watch now auto-enqueues one
  idempotent finalization run (task_queue rows carry `task_id`; resolveTask
  honors it) that treats the durable watcher result as authoritative evidence
  and backfills records without polling the external system again — the
  "Reply continue" notice is the fallback, not the closure path. Timeout
  notices state the operation may still be running and include the last
  output. Tests: `httpapi/external_watch_match_test.go`,
  `control/external_watches_test.go`.
  **Review hardening (same day):** (a) finalization is at-least-once, never
  lost — the terminal-status CAS and the side effects are separate steps, so
  `external_watches.finalized` (one-time backfill on upgrade) tracks whether
  side effects ran and boot recovery compensates unfinalized terminal
  watches; verdict revision resets the flag so corrected outcomes re-finalize.
  (b) Cross-endpoint origin: finalization and notices resolve the account
  behind the watch's channel (`AccountForChannel`) — a weixin-registered
  watch routes back to weixin; unmatched channels keep the cli/preferred-IM
  path. (c) `migrate-memory` moves canonical+observations+evidence+events as
  connected components and only when every linked observation resolves to the
  SAME person — split-evidence components stay in legacy intact. (d) The
  durability gate fails closed against permanence: empty/unknown durability
  is stored time-bounded (never permanent), unlabeled transient content is
  dropped, and an explicit-durable fact that mentions status tokens survives
  bounded instead of being dropped (false-drop fix); batch prompt example now
  carries the durability fields.
  **Sixth-review hardening (same day):** (a) Watch finalization products are
  replay-safe by ROW IDENTITY, not just ordering — `task_queue` gained
  `idempotency_key` (partial unique index; only that conflict is ignored, while
  unrelated constraint failures remain visible), the finalization run keys on
  `external-watch:<id>:r<revision>:finalization`, and `task_events` gained a
  database-enforced idempotency key for one completed event per
  watch+revision. Core finalization is marked complete only after the task,
  event, and finalization-run intent are durable. Notification delivery is a
  separate `external_watches.notified` state: failures stay pending and are
  retried by the sweep, while the content-derived outbox key prevents duplicate
  durable notices. `external_watches.verdict_revision` bumps on verdict
  correction, yielding one fresh correction run/event/notice while
  same-verdict replays create nothing; the boot compensation scan pages until
  drained (capped) and cancelled watches are born finalized. IM network
  delivery remains at-least-once by nature — exactly-once holds for durable
  intent materialization. (b) The transient
  gate is two-tier (`memory.ClassifyTransientContent`): CONFIRMED requires
  instance id + current-state semantics + status token, and explanatory rule
  cues (表示/转为/means/…) veto it; only CONFIRMED is ever auto-dropped or
  auto-archived — candidates are stored time-bounded / report-only, and
  pinned/user-confirmed rows are skipped at the audit layer AND refused in
  the archive SQL. `memory-audit --archive-confirmed` replaces blanket
  `--archive`. Live dry-run: 8 confirmed (all genuine pollution), 3
  candidates (incl. two durable doc-facts previously at risk), zero false
  archives. Tests: `control/external_watches_test.go`
  (idempotency-by-row-count), `memory/transient_classifier_test.go`,
  `cliapp/memory_audit_commands_test.go` (pinned/candidate protection and
  UTF-8-safe audit truncation).
  **Historical status closure hardening (2026-07-18):** daemon-originated
  queue recovery reconstructs delivery routes from accounts already bound to
  the durable `person_id`; a blank `platform_user_id` is never resolved as the
  synthetic `cli:local` account and therefore cannot move a finalization run
  onto another person. Successful watches enter the visible
  `waiting_finalization` state until their release-record run finishes. A
  periodic reconciler checks successful watches whose tasks remain
  non-terminal, restarts only their idempotent system queue row with a hard
  three-attempt budget, and exposes exhausted/cancelled closure as `blocked`
  instead of silently leaving stale `in_progress` tasks. Gateway shutdown is
  an infrastructure interruption rather than a user cancellation: active runs
  become resumable `interrupted` work, their started system queue row is
  compare-and-swap reopened, and the unwinding goroutine cannot overwrite that
  row as `done`. The reconciler also heals the legacy `gateway shutdown`
  cancellation written by older binaries. Tests:
  `httpapi/route_identity_test.go`,
  `httpapi/recovery_notification_test.go`,
  `httpapi/gateway_shutdown_test.go`, `control/queue_test.go`.

- **Runtime hygiene batch — ✅ shipped 2026-07-19.** (a) **Cron governance:**
  skill pruning is control-tenant-only — the daemon registers exactly ONE
  `skill-pruner-default` job instead of one per data-directory entry (the old
  `os.ReadDir(dataDir)` tenant discovery had accumulated ~2.5k rows across
  eval residue and person partitions, and their 03:00 fires kept touching
  every stale partition's memory.db). `cron_jobs` gained `system_key` with a
  partial unique index (empty keys — user jobs — never constrained; users may
  freely duplicate ordinary names, while the `skill-pruner-*` namespace is
  reserved for system safety); a boot migration deletes only rows matching
  the historical built-in schedule/prompt/channel shape, preserves
  coincidentally prefixed user rows, and collapses historic built-in
  duplicates BEFORE the index is created; `EnsureJob`
  resolves system jobs by key. `selfmind doctor` gained a "Cron governance"
  section (totals, system jobs, pruner count, duplicate groups, runaway
  warning). Tests: `cron/scheduler_test.go` (`TestSystemJobGovernance…`).
  (b) **Sender-aware progress notices:** `startAsyncProgressNotices` skips
  platforms the delivery service cannot send to (observed live 2026-07-18:
  163 doomed `platform=cli` outbound rows in one day); CLI async progress is
  dropped by design — the final result already routes to the preferred IM,
  and forwarding 30s ticks would violate the IM cadence contract.
  (c) **Responses cache telemetry:** the Responses adapter now parses
  `usage.input_tokens_details.cached_tokens` into
  `UsageStats.CacheReadInputTokens` (non-stream + stream + RequireStream
  aggregation); OpenAI cached tokens are discounted, not free, so
  `billed_input_tokens` is an approximation on this protocol. Tests:
  `llm/responses_adapter_test.go`. (d) **Eval isolation + residue:** the eval
  harness isolates `Evolution.SkillsDir` into the per-case temp dir, and —
  because `tools.SkillRootsForTenant` hard-anchors a second per-tenant skill
  root at `~/.selfmind/<tenant>/skills` via os.UserHomeDir regardless of
  config (the source that actually minted the ~500 `eval-*/skills` dirs) —
  the harness also self-sweeps its own verified eval tenant dir on Close.
  Follow-up: make tool skill roots config-injectable so the sweep becomes
  unnecessary. `selfmind eval clean` additionally removes on-disk eval
  residue directories under strict verification (exact name pattern + direct
  child of a known root + known contents only; symlinks never qualify —
  never a generalized recursive delete). (e) **Legacy import dedup:**
  `importLegacyFacts` skips run-attributed facts only when the live intake
  already recorded a deterministic `obs_` twin with the same
  run+target+scope+hash, and
  `selfmind maintenance memory-dedup [--apply]` repairs existing duplicated
  evidence only when the redundant row is proven by a matching legacy fact
  (keeps the deterministic `obs_` row, prunes imported twins, recomputes
  confidence/counts from surviving evidence, and stores a reversible snapshot
  in `memory_events`). Canonical
  single-write migration remains the planned follow-up (four-phase: import →
  coverage audit → single write → rollback window).

- **Memory partition convergence — ✅ shipped 2026-07-17 (P0).** Background
  post-run intake wrote canonical/legacy facts to the control-tenant partition
  (`data/default/memory.db`) while the foreground agent reads the person
  partition — everything learned since the layered store landed was never
  recallable (219 canonical rows, zero `last_accessed_at`). All intake and
  analyzer write/read sites now use `memoryPartition(req)` (PersonID with a
  legacy tenant fallback). `selfmind maintenance migrate-memory [--apply]`
  moves stranded default-partition rows to their person partition by
  `created_from_run → task_runs.person_id`; unresolved rows stay in the
  legacy partition. Tests: `app/post_run_analyzer_test.go` (person-partition +
  control-tenant-stays-empty assertions), `app/memory_intake_test.go`.

- **Memory durability enforcement — ✅ shipped 2026-07-17 (P1).** The
  post-run analyzer now returns `durability` (durable | time_bounded |
  episodic), `valid_until`, and `category` per memory decision, and the intake
  policy layer ENFORCES it in code: episodic decisions and transient
  run-state content (deterministic marker backstop: IN_PROGRESS / QUEUED /
  PREPARED_NOT_EXECUTED / 当前状态 / 尚未执行 …) never reach the facts or
  canonical stores, and are filtered BEFORE the 3+3 quota so they cannot
  crowd out durable knowledge; time_bounded facts always carry `valid_until`
  (model value or 30d default). Motivation: 10/29 facts stored on 2026-07-17
  were transient run state despite prompt-level SKIP instructions. Existing
  pollution is handled offline: `selfmind maintenance memory-audit
  [--archive]` shadow-classifies active canonicals with the SAME marker rule
  (`memory.TransientContentMarkers`) and archives reversibly — never a
  physical delete. Tests: `app/memory_intake_test.go`
  (`TestIntakeDurabilityEnforcement`), `cliapp/memory_audit_commands_test.go`.

- **Maintenance chain diagnosability — ✅ shipped 2026-07-17 (P0).** New
  append-only `maintenance_attempts` history (outcome, REAL error, route,
  attempt, 30d retention pruned at worker boot) records every fail/skip/block
  transition — the job row's `last_error` is overwritten by "maintenance
  retry limit reached" at skip time, which had made the live incident
  (repeated analyzer `context deadline exceeded` → 5 retries → learning
  silently skipped) undiagnosable from durable state. The analyzer per-call
  timeout is now `tasks.maintenance_llm_timeout` (default 2m, was a 45s
  const; batch bound = 2× per-call). `/diag` adds a "Learning failures (24h)"
  timeline and a "watch verdict suspect" alert for timed_out watches whose
  recorded output matches a terminal pattern. Tests:
  `control/maintenance_jobs_test.go` (`TestMaintenanceAttemptHistory`).

- **Prompt-cache accounting + provider-safe cache_control (2026-07-17/18, P1).**
  `UsageStats` carries `cache_read_input_tokens` / `cache_creation_input_tokens`;
  `token.updated` adds both plus `billed_input_tokens`. The anthropic adapter
  parses cache usage (non-stream + message_start) and, under the
  `prompt_cache` provider quirk, attaches `cache_control` breakpoints on
  the stable system prefix and a rolling history breakpoint. Prompt layering
  was verified byte-stable across consecutive turns
  (`kernel/prompt_layering_test.go`), so the cacheable prefix is real.
  Measured motivation: ~7.0M input tokens on 2026-07-17, ≥90% replayed
  prefix. Built-in native Anthropic and MiniMax profiles now enable the
  documented cache contract; custom Anthropic-compatible endpoints and direct
  Kimi Coding remain off unless explicitly configured. `/diag context` shows
  cache reads, writes, billed input, and hit rate instead of only estimating
  the stable prefix. Tests: `llm/anthropic_prompt_cache_test.go`,
  `kernel/token_usage_event_test.go`, `modelruntime/resolver_test.go`,
  `httpapi/diag_w2_test.go`.
  **Per-call accounting closure (2026-07-22):** each logical stream or
  non-stream provider invocation now emits one durable `provider.call.usage`
  event with duration, transport, status, cache read/create tokens, and billed
  input. `token.updated` intentionally remains the cumulative run total.
  `/diag context` renders both views, preventing cumulative run usage from
  being mistaken for a single oversized provider request. Tests:
  `kernel/token_usage_event_test.go`, `httpapi/diag_w2_test.go`.

- **Unattended completion + IM recovery closure (2026-07-18, P0/P1).**
  External-watch finalization queue rows now receive a dedicated execution
  profile: safe workspace file tools run without prompting, while shell,
  network, privileged, and out-of-workspace operations fail immediately with
  an instruction to finish `waiting_user`. Approval expiry likewise returns a
  stable rejection instead of a retryable context deadline, preventing the
  observed sequence of three unattended 30-minute approval waits. Weixin
  `ret=-2` / `prepare failed` is classified as session-refresh-required for
  critical notifications; the durable row becomes `pending_session` and the
  next inbound message retries it after refreshing platform context. Startup
  `EnsureJob` also collapses historical duplicate named cron rows, fixing the
  accumulated `skill-pruner` schedules. Tests: `tools/middleware_test.go`,
  `gateway/delivery/catchup_test.go`, `gateway/weixin/client_test.go`,
  `kernel/task/cron/scheduler_test.go`, `httpapi/recovery_notification_test.go`.

- **Turn-efficiency + consistency batch (2026-07-17, P1/P2).** Tool failures
  now classify (`error_class: syntax|auth|timeout|not_found|permission|
  network|environment` + one actionable hint appended to terminal/read_file/
  search_files failures; raw error preserved — `tools/tool_errors.go`);
  measured motivation: 24/352 failed calls on 2026-07-17, each ≈ one full
  replay turn. `finish_run` gains `waiting_user` (prepared, awaiting the
  user's go-ahead) distinct from `blocked`, mapped through outcome
  reconciliation, task cards, dupes, and strategy guidance. `/tasks` open view
  groups cards by deterministic ticket key (display-only,
  `groupTasksByWorkKey`). Skill review cadence stretched to 3× the memory
  nudge interval (15 no-change reviews in one day of CI/CD work). Tests:
  `tools/tool_errors_test.go`, `httpapi/task_workkey_test.go`.

- **P1-6 cross-endpoint recovery + maintenance failover + honest task state -
  shipped 2026-07-16.** Async and cron final answers are now typed as
  `final_result`. An explicit CLI continuation receives a bounded
  `Delivery Continuity` slice when the same task has a recent Weixin/IM final
  answer in `pending_session`, `sent_unconfirmed`, or `failed`, allowing the
  agent to restate the
  missing conclusion without adding a second resend path. Mid-turn guidance
  now records `run.steering_consumed` only after it reaches a model step; raw
  guidance is not copied into the durable event. Post-run maintenance can
  fail over through the explicitly configured
  `tasks.maintenance_fallback_roles` and still never falls back to the primary
  coding model. Smart approval evaluates paths against the active
  `ExecutionScope`, so switching `/ws` no longer makes already-authorized
  reads look outside the daemon's startup directory. `/tasks` uses the newest
  structured run outcome to distinguish daemon recovery, provider transport
  interruption, context overflow, verification incomplete, and
  `waiting_external`. Existing `/diag context`, stable/volatile prompt
  accounting, artifact-backed large tool outputs, duplicate-task suggestions,
  and the already-split gateway files were verified as covering the remaining
  proposed P1/P2 work without another implementation. Tests:
  `httpapi/context_selector_test.go`, `httpapi/run_events_test.go`,
  `httpapi/task_view_test.go`, `control/task_labels_test.go`,
  `tools/workspace_scope_test.go`, `app/post_run_analyzer_test.go`.
  **Maintenance replay hardening (2026-07-17):** normal and provider-error run
  finalization now write the immutable maintenance replay payload in the same
  transaction as the terminal run state and job row; no success/error window
  can leave an empty replay job. Explicit fallback roles are tried when a
  provider returns a nil or whitespace-only response, instead of accepting an
  empty proposal as success. Tests: `control/maintenance_jobs_test.go`,
  `app/post_run_analyzer_test.go`, `httpapi/run_completion_test.go`.
  **Healthy-route replay closure (2026-07-22):** blocked maintenance jobs are
  replayed when any explicitly configured cheap route in the same analyzer
  chain is healthy, even if the failed route remains configured. Replay is
  analyzer-version scoped (`1` post-run, `100` skill review), so one worker
  cannot steal another worker's jobs; if every configured route is open, jobs
  remain blocked. Background review uses the same explicit
  `tasks.maintenance_fallback_roles` chain and still never borrows the
  foreground coding model. Provider circuit health is daemon-scoped (normally
  tenant `default`) while durable jobs retain their person tenant; replay keeps
  those scopes separate and releases matching person-owned jobs across tenants
  only when the daemon chain has a healthy fallback. Tests:
  `control/provider_route_health_test.go`.
  **Kimi transport closure (2026-07-17):** every Kimi Coding Plan role remains
  on the provider-default Anthropic Messages transport, matching Hermes and the
  `/coding` route's wire contract; custom gateways may still override protocol
  per role. Equivalent fallback routes are deduplicated, output budget
  scales with batch size, and retryable aggregate failures are bisected so one
  truncated or malformed run cannot fail the entire maintenance batch. A fatal
  backup-provider error no longer converts a recoverable empty/truncated Kimi
  result into a non-retryable chain failure. Tests:
  `app/post_run_analyzer_test.go`, `httpapi/maintenance_batch_test.go`.
  **Kimi maintenance quota circuit (2026-07-17):** Anthropic-compatible Kimi
  responses now parse direct-string content and turn HTTP-200 semantic empties
  into typed provider errors carrying request/stop metadata. Maintenance roles
  sharing an endpoint and credential are one physical route: the first quota
  403 opens a durable circuit, queued post-run/memory/skill-review jobs block
  without spending retry attempts, one scheduled half-open probe tests
  recovery, and success replays the backlog. Stream-delivered quota errors are
  observed by the same circuit. `/diag` exposes the blocked route and next
  probe. Tests: `kernel/llm/adapters_test.go`,
  `app/post_run_analyzer_test.go`, `control/provider_route_health_test.go`,
  `kernel/background_review_test.go`.

**P2 — engineering governance**

- **P2-1 split server.go / controller.go — ✅ shipped 2026-07-10** (no behavior
  change; same-package function relocation, compiler+tests as the safety net).
  `server.go` 1928→751: `*RunCoordinator` methods → `run_coordinator_lifecycle.go`,
  control-command cluster → `control_commands.go`. `controller.go` 1691→720:
  message loop → `controller_update.go`, view/render → `controller_view.go`,
  transcript mutators → `controller_transcript.go`, constructors →
  `controller_init.go`. All under the 800-line architecture-constraints line.
  Remaining follow-up (STATUS item 11, separable/low-risk): physically delete
  the unmounted dead adapters (`gateway/telegram`, `gateway/wechat`,
  `platform/wechat`, `GatewayDeps.Bridge`) — not done here to avoid a
  cross-package excision under the same change.
- **P2-2 docs — ✅ shipped 2026-07-10.** AGENTS.md carries the daemon-only
  development invariant (no in-process agent path / no fallback / no
  `SELFMIND_TUI_INPROC`); STATUS rows and `docs/tui-terminal-first-hybrid.md`
  updated for the legacy-TUI removal.

One-liner: make cross-endpoint takeover actually reliable → make the eval gate
trustworthy → converge to daemon-only; only then token economics and code
hygiene.

0. **P0 — Work Timeline transition (approved 2026-07-06; canonical design:
   `docs/work-timeline.md` — mandatory reading, includes the full rationale
   and the rejected alternatives).** Context ownership moves from tasks to a
   person-level work spine; `task` keeps its name but demotes to a work
   label/view; disambiguation happens in the agent's turn, never at ingress.
   Control plane (runs/approvals/queue/clarify/recovery) is untouched.
   Packages, one worktree each, owner verifies between packages:
   - **P1 spine: ✅ landed 2026-07-06.** Person-keyed turn-level history
     (slim entries under `kernel.SpineTrajectoryKey`; tool intermediates
     excluded), verbatim summary boundary note on every rendered compaction
     summary, ContextComposer slice formalization
     (`internal/kernel/context_composer.go`, slice ④ reserved for P2 via
     `RuntimeContextBundle.Recall`), and the read-only legacy compat chain
     (`task:<id>` → `PriorChannel` → old channel key). See the Context
     engine row above for the full contract.
   - **P2 recall v1** — ✅ shipped (2026-07-06): automatic recall slice at the
     selector layer (`httpapi/recall.go` `RecallEngine` on `Server.Recall`,
     rendered via `kernel.TaskRuntimeContext.RecallSlices`; ephemeral —
     system-prompt block only, never persisted history). Sources:
     person-partitioned session FTS + task label cards (live read-only
     `control.ListTaskCards` JOIN; artifacts/changed files ride the card's
     work line). Budget ≤3 slices / ~400-char excerpts / work-line dedupe
     (label card beats raw session fragment); control-command and
     short-message skips; `semantic_recall`-role query expansion only when
     the role is explicitly configured (`app.SemanticRecallExpander`, never
     the main model), 3s-bounded, degrades to raw terms; redacted
     `context.recall` task event. Embedding v2 interface reserved
     (`httpapi.RecallSource`). Wired identically in daemon, in-process TUI,
     and eval servers. Tests: `httpapi/recall_test.go`,
     `control/task_cards_test.go`. Details: `docs/work-timeline.md`
     "Semantic recall".
   - **P3 labels & view** — ✅ shipped (2026-07-06): `resolveTask` → harmless
     pre-label guess (explicit task_id / cue / `/resume` pin deterministic; else
     current OPEN label or a new placeholder; request workspace wins for
     guessed turns); async post-run labeler on the memory_extract role
     (`Server.Labeler`, KEEP/MOVE/TITLE one-line contract, fails safe to
     KEEP, `label.assigned` audit events, `Store.ReassignRun` transactional
     re-point + placeholder cleanup); `/tasks` aggregated open-work view with
     run counts + next-step hints (done/archived collapse to counts,
     `/tasks done|archived|all` expand); 2026-07-06: the view renders
     multi-line CARDS (simplified `[running|waiting|paused|<terminal>]`
     bracket, `last:` latest run input + age/interrupted, `file:` primary
     artifact basename, `approvals:`/`questions:` pending counts, `runs:`,
     shortened `id:`; per-card hints replaced by one trailing hint line;
     batched via `LatestRunSummaries`/`LatestHandoffFilesByPerson`/
     `PendingCountsByTask` grouped queries); `/task <id>` detail /
     `runs|rename <name>|archive` subcommands (short `task_xxxxxxxx` ids
     accepted; `archived` is a new terminal status excluded from open lists,
     `ListTaskCards`, `resolveContinueTask`, and the pre-label default —
     explicit `/resume` reopens it); implicit-continuation LLM upgrade
     REMOVED (`intent.continue_window` deprecated no-op). Tests:
     `httpapi/run_labeler_test.go`, `httpapi/task_attach_test.go`,
     `httpapi/task_view_test.go`, `control/task_labels_test.go`.
     2026-07-06 UX fixes: ordinal references everywhere the lists are numbered
     (`/task <n>`, `/resume <n>` against the /tasks open-card order via
     `resolveTaskReference`; `/workspace <n>` against the /workspaces order via
     `resolveWorkspaceReference` — display order = resolution order, approval
     resolver contract); `/resume` accepts the card-displayed short id and its
     success reply names the task's bound workspace; the workspace-escape tool
     error appends a `/resume`//`/workspace` hint; the TUI echoes typed slash
     commands into the transcript (`handleCommand`).
     2026-07-07 TUI session fixes: a successful `/workspace <n|id>` pins a
     session workspace override in the TUI (resolved id/name/path parsed from
     the gateway's control reply; `MessageRequest.WorkspaceID` rides every
     later agent/control turn, so the switch is no longer a no-op for
     subsequent CLI messages), the status bar renders the override
     `<name>:<path>` instead of the launch cwd, and client mode forwards
     `token.updated` usage events (`client.eventToStream` → `MsgTokens`) so
     run token counts tick live instead of sitting at 0 until the final
     response. Tests: `cli/workspace_override_test.go`,
     `cli/token_events_test.go`, `client/client_test.go`.
   - **P4 eval: ✅ shipped 2026-07-06.** `evalcases/timeline/` — five cases
     (iterate/new-topic/ambiguity/cross-endpoint/tasks-view) with committed
     cassettes replaying offline; scenarios 7-10 map to Go tests (see the
     suite README's scenario table). The Work Timeline transition P0-P4 is
     complete.
1. **P0 — G0: Runtime attachment model** (design:
   `docs/identity-continuity.md` "Runtime attachment model"; owner decisions
   2026-07-04, incl. origin-affinity routing). Sub-items in order:
   (0) **Weixin push reliability** — ✅ mechanism landed (2026-07-04): the
   sender DOES check iLink ret/errcode, so the observed loss is
   accepted-but-dropped on a stale context_token. Context tokens now carry
   capture timestamps (legacy files restore as age-unknown = stale);
   `delivery.SenderWithReceipt` lets the weixin adapter report push
   confidence from token freshness (30m conservative window, true window
   undocumented); doubtful sends finalize as `sent_unconfirmed` — terminal
   for the retry queue (resending on the same stale session risks
   duplicates) — with a WARN log. `sent_unconfirmed`/`failed` rows now
   surface in the attach digest (G0-c, 2026-07-04). Remaining: catch-up on
   the peer's next inbound + delivery health in `/diag` — **absorbed into the
   ACTIVE PLAN above (P0-1, 2026-07-10)**; calibrate the window empirically.
   (a) Detached run execution — ✅ resolved (2026-07-04). Runs execute on
   daemon-owned contexts: `ProcessMessage` derives the run ctx with
   `context.WithoutCancel` from the request ctx (values — stream observer,
   steering, scopes — preserved; caller deadlines re-applied as a bound on
   the run), so closing the CLI/dropping the connection mid-turn detaches a
   watcher instead of killing the run. Cancellation is owned by the
   active-run registry: `/stop`, drain `stopAllActive`, and TUI ctrl+c
   (now routed through `/stop` via `requestDaemonStop`); the idle watchdog
   and per-turn deadline still cancel. A sync run whose client vanished
   routes its result like an async one (`deliverAsyncResult` → IM fan-out
   for cli origin); connected clients get the sync answer only. Tests:
   `httpapi/detached_run_test.go`, `cli/cancel_stop_test.go`.
   (b) Two-layer routing — ✅ shipped (2026-07-04): in-memory presence
   registry (`httpapi/presence.go`, 90s TTL; touched by `/v1/message`,
   `/v1/events/stream`, `GET /v1/presence/ping`, and a 30s idle ping
   loop in the client TUI; `accounts.last_seen_at` persisted throttled for
   preferred-endpoint recency). **Presence = recent user INPUT (2026-07-05):**
   an open-but-vacated TUI no longer claims attachment forever. The client
   tracks the last keystroke (`cli.SetInputActivityHook` →
   `client.InputTracker`) and stamps `active=0|1` on its presence ping and
   event stream once input age exceeds `gateway.presence_idle_timeout`
   (duration, default `5m`, `0` = old always-attached behavior; the decision
   is client-side because only the client knows input age — the daemon just
   skips `touchPresence` for `active=0`, `presenceClaimed`). `/v1/message`
   always counts as active (typing a message IS input). Effect: walk away →
   after timeout + TTL the CLI reads detached and CLI-origin approval/result
   pushes route to the preferred IM; return and type → the next beat
   re-attaches. Tests: `httpapi/presence_test.go`
   (`TestPresenceBeatWithActiveZero*`, `TestPresenceIdleBeats*`),
   `client/presence_active_test.go`, `config/presence_idle_test.go`. CLI-origin approval/result pushes: skipped
   entirely while a CLI endpoint is attached (no double notification), else
   delivered to ONE preferred IM endpoint — explicit `/notify <platform|auto>`
   preference (validated against the person's own bound accounts, stored in
   `person_settings`) or the most recently seen IM account. IM-origin
   replies unchanged (origin affinity). Follow-up: reply-endpoint override
   parsed from task text remains open.
   (c) Attach digest — ✅ shipped (2026-07-04). `GET /v1/digest`
   (`httpapi/handlers_digest.go`): person-scoped, anchored on the CLI
   account's `accounts.last_seen_at` (24h fallback when never seen); bounded
   sections — finished tasks (≤10), failed/interrupted tasks (≤10), all
   pending approvals (shared `approvalSummaryLine`), `sent_unconfirmed`/
   `failed` outbound pushes (≤5), and the person's active run. **Active-run
   progress (2026-07-05):** `api.DigestActiveRun` now carries `plan_steps`
   (the SAME `plan.updated` source `/status` uses via `latestPlanForTask`,
   pre-rendered `[x]/[>]/[ ]` lines, bounded to 6 — completed prefix
   collapses, tail truncates with "… N more steps") and `latest_activity`
   (one line from the newest tool/thinking event), rendered indented under
   the "▶ A task is running now" line — reopening the CLI shows where the
   run stands, not just that it exists. The client
   TUI fetches it BEFORE its first presence beat (the beat stamps the
   anchor) and renders one compact "While you were away" block; an empty
   digest renders nothing. Store queries:
   `control.ListTasksByStatusSince`, `control.ListUndeliveredOutbound`.
   Tests: `httpapi/handlers_digest_test.go`, `control/digest_queries_test.go`,
   `cli/attach_digest_test.go`.
   (d) Re-attach to a mid-flight run — ✅ shipped (2026-07-04). When the
   digest reports an active run, the client TUI starts
   `client.WatchActiveRun` without a user turn: baseline event probe (no
   history replay), live tool/thinking/approval events rendered exactly like
   an in-turn poll, run end detected via a fresh `run.finished`/
   `run.cancelled` event (carries the outcome summary) with a
   `/v1/tasks/current` active-run probe as fallback. The controller flips
   the run-active flags (`attachedRun`/`thinking`), so Enter steers the
   daemon run (`/v1/runs/steer`) and ctrl+c shows the background/cancel exit
   prompt — all existing paths. `cli/attach_digest.go`,
   `client.WatchActiveRun`.
2. **P0 — G1+G2: IM routing stack + queue instead of busy.** Queue-instead-of-busy
   is ✅ shipped (2026-07-04, see the Task queueing capability row): a genuinely
   new message while a run is active is enqueued with an honest acceptance and
   auto-started (sync + async drain + boot drain) when the runner frees up;
   continuations never queue (they steer). The full inbound routing priority
   order (bare y/n approval > pending question > continuation cue > NEW task) is
   now in place — the pending-question leg shipped with G3 below. Observability
   export (`selfmind doctor` / `/diag`) also shipped 2026-07-04.
3. **P0 — G3: clarify-over-IM — ✅ shipped (2026-07-04).** `gatewayClarify` is
   no longer a stub: a clarify is now a first-class DB-backed pending question
   modeled exactly on the approval waiter. It creates a `clarify_requests` row
   (`internal/control/clarifies.go`), appends the `clarify.requested` event,
   pushes a presence-aware, single-preferred-endpoint notification via the
   shared `RunCoordinator.routePendingNotification` (same routing as approvals;
   `notifyClarifyRequested` builds the "Question — reply with your answer:" body,
   `delivery.KindClarify`), then blocks polling the row for up to 30 min. An
   answer recorded from ANY endpoint (`Store.AnswerClarifyRequest`) is returned
   verbatim as the tool result; timeout / expiry returns the best-judgment
   fallback sentinel so the run never hangs. Inbound: a plain non-slash reply
   while a question is pending IS the answer (`tryHandleClarifyAnswer`, wired in
   `tryHandleControlCommand` above the new-task/queue logic and below the bare
   y/n approval leg — approvals and clarifies are mutually exclusive per run, but
   y/n-looking input still favors an approval defensively). Orphan hygiene:
   `Store.ExpireOrphanedClarifies` rides `MarkInterruptedRuns` next to the
   approval sweep, so a restart never leaves a dangling question. Surfacing:
   `/status`, `/diag`, and the attach digest (`api.DigestClarify`, CLI
   "N questions waiting") all show pending clarifies. A question now survives the
   CLI closing exactly like an approval (docs/identity-continuity.md "Runtime
   attachment model"). `internal/control/clarifies.go`,
   `internal/gateway/httpapi/{server.go,clarify_inbound.go,diag.go,handlers_digest.go}`,
   `internal/gateway/cli/attach_digest.go`. Tests:
   `control/clarifies_test.go`, `httpapi/clarify_inbound_test.go`.
4. **P2 — G4 (deferred until queues create real multi-task traffic):
   adaptive task tags + targeted messaging.** IM notifications
   carry a short task tag only when >1 task is alive; `/task <n> <text>`
   routes a message to a specific task (steer if running, next-turn input if
   parked).
5. **P0 — Task-attach semantics for parked tasks.** ✅ shipped (2026-07-05);
   **superseded by Work Timeline P3 pre-label semantics (2026-07-06)** — see
   the Task-attach semantics row and the P3 package entry above. Historical
   rule as shipped: attach ONLY on explicit continuation evidence — a caller-supplied
   `task_id`, an `IntentContinue` classification (router cue or the
   short-acceptance upgrade), or the one-shot pin written by `/resume`
   (person_settings key `resume_pin_task`, consumed by the next agent-bound
   message so a stale `/resume` can never capture unrelated work). Any other
   message that reaches the agent — sync, async dispatch, queued-task drain,
   cron — creates its OWN task carrying the request's explicit `workspace_id`
   (else the person's current workspace); parked tasks stay resumable via
   `/resume` or a later continuation cue. The channel-recency fallback
   (`Store.CurrentTaskForChannel`) was removed with it. `resolveTask` +
   `consumeResumePin` in `httpapi/server.go` / `httpapi/continue_resolver.go`.
   Tests: `httpapi/task_attach_test.go` (new-work isolation, 继续 attach,
   one-shot pin, async explicit-workspace, drained-item own task). Eval:
   `evalcases/continuity/continuity-task-attach.yaml` (`require_task_switch`)
   — needs a live cassette recording before it gates (suite README).
6. **P0 — Continuity path polish.** Surface identity: `GET /v1/accounts` +
   `selfmind accounts`, and make "bind a new endpoint → inherit tasks and
   memory" a visible moment.
7. **P1 — Finish daemon-client convergence, then delete the duplicates.**
   **SUPERSEDED by the ACTIVE PLAN above (P0-3, 2026-07-10)** — same content,
   now with deletion preconditions and eval-scope wording. The legacy TUI
   cleanup (`SELFMIND_TUI_LEGACY`, viewport, `controller_mouse.go`,
   `renderCache`, `uiModel` decomposition) and the N>1 soak
   (`SELFMIND_WORKERS`) ride along in that item.
8. **P1 — Stranger-isolation hardening (scenario 3).** Highest item: the
   Weixin owner auto-bind hazard — with `gateway.weixin.owner_person_id` set,
   EVERY sender passing the DM policy is bound to the owner person
   (`weixin/adapter.go` inbound path), and the default `dm_policy: open` +
   empty `allow_from` means any stranger DMing the account becomes the owner.
   Fix: owner auto-bind must apply only to allowlisted senders, or first-DM
   pairing-code confirmation; until then document allowlist as mandatory after
   login. Also: QQ webhook ed25519 signature verification (inbound is
   currently unverified), Feishu encrypt-envelope AES decryption, WeChat OA
   safe-mode crypto.
9. **P2 — Multi-tenant execution isolation hardening.** The Linux single-user
   bubblewrap contract is shipped. Before multi-person sharing, add per-tenant
   containers or equivalent namespace/seccomp/cgroup isolation, resource
   quotas, and stronger egress policy.
10. **P2 — MCP `sampling/createMessage`**, IM voice STT/TTS, remaining adapter
   polish — only as scenario needs dictate.
11. **RESOLVED (2026-07-05) — Unify control-command parsing across endpoints.**
   A single canonical registry `internal/gateway/command` (leaf package, stdlib
   only, no import cycle) now owns detection/help/name-lists/async-hint/suggest.
   Rewired to read it: gateway `/help` → `command.HelpText()`;
   `suggestControlCommand` + the unknown-slash reject gate → `command.Suggest`;
   the two async-hint `isControlCommand` copies (`weixin/adapter.go`,
   `handlers_channels.go`) → `command.IsGatewayControl` (fixes the drift — now
   covers `/queue /diag /mode /notify /help /model`); the three approval-mode
   word lists → `tools.IsKnownApprovalModeWord` / `tools.CanonicalApprovalModes`;
   the TUI `slash_commands.go` now exposes every gateway command (typing
   `/approve` in the TUI relays to the daemon) and unifies its unknown-command
   message on `command.Suggest`. Drift guards: `command.TestKnownMatchesGateway`
   `Contract` + httpapi `TestEveryRegistryGatewayCommandIsHandledBySwitch`. The
   big execute switch (`tryHandleControlCommand`) stays authoritative; only
   metadata was consolidated (no execution behavior changed). Dead fork: the
   unmounted IM adapters (`gateway/telegram`, `gateway/wechat`,
   `platform/wechat` via `channel.Bridge`) route raw text through
   `router.Gateway.Handle`, which has no control detection; `Handle` is now
   documented as in-proc-TUI-only (its live caller intercepts control commands
   first) and marked `// DEAD` — physical removal of the adapters + the unused
   `GatewayDeps.Bridge` wiring is a separate low-risk cleanup left as follow-up.
Resolved from this list: **Approval UX** (was item 1, resolved 2026-07-04) —
see the Approval lifecycle row above for what shipped and the two intentional
remainders (unmounted long-poll telegram adapter; Weixin text fallback).
**H2: LLM triage for `smart` approval mode** (was item 11, resolved 2026-07-04)
— inserted as layer 4 in `SmartApprovalMiddleware`, below the hard floor and
class-grant allowlist and above the human ask; fails SAFE to ESCALATE. See the
Approval modes row for the shipped behavior.

Cron proactive delivery, user profile synthesis, CLI image input, approval
modes, and the self-check/CI gate landed with the Phase-1 work — see
`docs/phase1-modules.md`.

## How To Keep This Accurate

- Treat this file as the index of "what is real." Update the affected row in the
  same PR that changes the behavior.
- Do not add per-feature status notes to the historical roadmap docs; record state
  here instead.

## npm Distribution Lifecycle (2026-07-24)

- The release is distributed as the `selfmind` npm launcher plus Linux and
  macOS x64/arm64 optional native packages. Linux, macOS, and WSL are official
  targets; native Windows fails with an actionable WSL message.
- A bare interactive `selfmind` launch now opens guided model setup when no
  model is configured, before the daemon or TUI starts. Cancellation exits
  cleanly, and non-interactive callers receive actionable commands without an
  interactive prompt. `selfmind setup`, `update`, `uninstall --prepare`, and
  `feedback` remain the explicit lifecycle controls. Existing configs are not
  rewritten when setup has nothing to change.
- Git tags are the single version source for Go build metadata, npm packages,
  and GitHub releases. CI builds all four Linux/macOS architectures,
  smoke-tests packed launchers on Linux and macOS, publishes native packages
  before the launcher, and uses `latest`/`next` dist-tags.
- macOS setup installs a per-user launchd LaunchAgent using the stable npm
  launcher path. Drain-aware restart lets launchd relaunch the newly installed
  version, while doctor reports installed/loaded service state.
- Top-level panics create private local crash reports. Startup reports a crash
  once; reports and diagnostics are uploaded only by explicit user action.
- Release operations and compatibility rules are documented in
  `docs/npm-distribution.md`. npm trusted publishing must be configured for all
  five package names before the first public release.

## Run Finalization and Build Identity (2026-07-20)

- Normal, error, and recovery completion paths materialize the terminal run,
  task card, assistant transcript, handoff, maintenance job, and terminal event
  in one SQLite transaction. A stable terminal event key makes replay safe.
- Durable queue rows bind to the actual run ID. Startup recovery settles queue
  rows whose terminal event was already committed and only reopens work that
  did not reach a durable terminal boundary.
- The daemon rejects unresolved CLI paste placeholders before identity, task,
  or run creation, so display-only paste markers cannot reach the agent.
- Release builds expose version, commit, build time, and a build fingerprint.
  CLI startup, `gateway status`, and `doctor` surface stale daemon binaries.

## Delivery And Verification Closure (2026-07-20)

- Weixin `pending_session` deliveries are now visible in `/diag` and
  `/diag delivery`. Automatic catch-up remains inbound-triggered and bounded by
  the delivery attempt limit. An owner may retry one exact pending row with
  `/diag delivery retry <id-prefix>`; lookup is scoped to the current IM peer,
  or explicitly discard obsolete backlog with
  `/diag delivery dismiss <id-prefix>`. Diagnostics show age and explain why a
  stale row is outside automatic catch-up. `sent_unconfirmed` remains terminal
  to prevent blind duplicate sends.
- Work whose files changed but whose verification failed or did not complete
  now leaves the task in the distinct, resumable `verification_partial` state.
  The run itself is terminal, while `/status`, `/tasks`, digest, and recent
  activity explain that verification remains instead of calling it a generic
  interruption.
- Reliability evals can assert an expected gateway HTTP rejection and prove
  that it happened before task/run creation. The unresolved-paste fixture uses
  this contract, and a verification-partial case records the cross-layer state
  expected after changed-but-unverified work.

## Daily-Driver Routing And Execution Closure (2026-07-22)

- Post-run INBOX proposals now pass a deterministic eligibility guard. Work
  with a work key, changed files, tests, risks, verification evidence, explicit
  next steps, or a non-done outcome remains on its visible task label even when
  the maintenance model requests INBOX. The rejected proposal is still written
  to `label.assigned` for audit; routing mistakes affect display only.
- `/diag context` reports whether the stable prompt prefix changed between the
  two latest turns. Prompt-cache creation is rendered as `n/a` for transports
  such as Responses that do not report that counter, avoiding the false claim
  that zero cache entries were created. `token.updated` remains a cumulative
  run snapshot and must not be summed across events.
- An isolated terminal failure requests one approval-gated host retry only for
  authentication, network, or environment-state failures. Syntax, path, and
  command errors stay in the sandbox correction path. Verification-claim
  reconciliation now requires both a verification cue and a positive result;
  successful read-only provider queries no longer count as tests passing.

## Loop, Recall, And Retention Closure (2026-07-23)

- External watches execute with Bash on Linux and fail fast on deterministic
  shell defects instead of polling the same invalid command until deadline.
  Generic transport errors preserve any durable structured completion evidence
  already recorded by the run.
- `/diag context` aggregates provider-call cache reads, reported creation
  tokens, and billed input across its visible event window. A zero creation
  counter is described as transport-unreported rather than proof that the
  provider created no cache entry.
- Prompt memory selection applies bounded query relevance on top of the
  governance score, including deterministic CJK matching. Post-run maintenance
  compares proposals against the canonical read model, so an existing
  canonical memory is reinforced without recreating a legacy shadow fact.
- Action-tool budgets are provider-agnostic configuration. The defaults are
  `12 + 6` per evidence-gated extension, a hard ceiling of `64`, and at most
  nine extensions; tool-free turns remain tool-free.
- Gateway startup prunes only old terminal outbound history after
  `gateway.outbound_retention` (default 14 days). Pending, retryable,
  unconfirmed, session-recovery, and recoverable critical IM rows are retained.

## Durable Execution And macOS Closure (2026-07-24)

- Task terminal state is reduced from durable blockers before commit: pending
  approvals or questions win over model completion, active runs keep the task
  in progress, external watches keep it waiting externally, and queued watch
  finalization keeps it waiting for finalization. A stale model answer can no
  longer mark blocked work done.
- External-watch success, failure, and timeout all materialize a durable
  finalization intent. A merely attached CLI is not treated as proof that the
  user received the verdict; durable channel delivery or the idempotent
  finalization queue must exist before the watch is marked notified.
- Repeated terminal polling of the same remote CI/CD status is redirected to
  `watch_external`. A one-shot status check remains valid; repeated identical
  non-terminal output is the guardrail trigger.
- Runtime project context comes only from the explicit workspace contract.
  The daemon process cwd is never used as a fallback project root.
- On macOS, launchd owns the long-running gateway. Ordinary `gateway start`
  reuses a running service without replacing it; `gateway restart --drain` is
  the explicit upgrade path. The LaunchAgent has restart throttling, a stable
  npm launcher path, a Homebrew-aware `PATH`, and bounded log rotation.

## Memory Governance Reliability Closure (2026-07-11)

- Post-run maintenance is now a daemon-consumed durable job, not a detached
  best-effort goroutine only. The replay payload and frozen model proposal are
  persisted separately; crashes and apply failures reuse the same proposal.
  Fresh payload-attachment races remain pending and retries stop after a bounded
  attempt count. Empty model replies fail the job instead of becoming success.
- Canonical intake proposal items are idempotent by run, analyzer version,
  decision key, target, scope, and content. Statement lookup/status mutation is
  target+scope+hash scoped, so identical text in two workspaces cannot mutate
  across the boundary.
- `/memory history` joins explicit learning changes with automatic merge/archive
  events. New merge snapshots preserve evidence ownership and can be reverted
  with `/memory undo <event-id>`; legacy incomplete merge snapshots fail safely.
- Background governance defaults to `shadow`, uses the explicitly configured
  maintenance role only, pauses while foreground runs are active, enforces both
  global and per-workspace caps in `full`, and writes private reports with 0600
  permissions. Reports include a dry-run action ledger and projected active
  count, and `/diag memory` surfaces the latest calibration summary.
  `auto_supersede_confidence` is reserved and not yet an automatic consolidation
  apply gate.
- Remaining calibration work: inspect the generated shadow report against real
  legacy history, then deliberately promote `shadow -> merge-only -> full`.
  Automatic consolidation SUPERSEDE/CONFLICT application remains out of scope
  until that evidence exists.
