# ExternalAuthManager — Design

Status: **shipped** — manager (`internal/modelruntime/authmanager.go`,
race-tested) + codex & minimax migrated through it (their existing OAuth tests
still pass = behavior preserved); Kimi/static keys are pass-through. Worker-pool
wiring (inject the shared manager + shared per-provider http.Client) lands with
W1b. Prerequisite for the worker pool (see
`docs/STATUS.md` concurrency row). Design note, not a rule set; `AGENTS.md`
stays canonical.

## 1. Context & problem

External-CLI auth reuse (Codex ChatGPT OAuth, MiniMax OAuth) works today, but the
refresh path is **per-closure**, not per-auth-file:

- `internal/modelruntime/codex_oauth.go:34` creates `var mu sync.Mutex` **inside
  each `ResolveCredential` call**. `minimax_oauth.go:61/68` is the same shape.
- The resolver wires those closures into the adapter as
  `Runtime.TokenGetter`/`TokenRefresher` (`resolver.go:171`, consumed at
  `internal/app/agent.go:330`). No caching / single-flight at the resolver.
- Refresh **rotates** the refresh token (`codex_oauth.go` writes
  `payload["refresh_token"]`; `minimax_oauth.go:323`) and **writes the file**
  back. Codex uses a plain write; MiniMax already does an atomic temp+rename
  (`minimax_oauth.go:468`).
- On failure the closures **return `""`** (`codex_oauth.go:43/47`) — no
  transient/permanent distinction, no quarantine.

Today a single shared Agent + global `runMu` (`agent.go:423`) serializes all
runs, so the per-closure lock is incidentally safe. **The moment we add a worker
pool** (multiple Agents/adapters, each resolving its own closure), the
per-closure lock no longer serializes across workers:

- N workers see an expired token → each refreshes with the **same** refresh
  token → OAuth rotation makes the 1st succeed and the rest fail with
  `refresh_token_reused` / `invalid_grant` → **broken login** until re-login.
- Concurrent file writes (codex, non-atomic) → **corrupted `auth.json`**.

Static-API-key providers (Kimi `sk-kimi-…`, `profile.go:258`; generic
OpenAI-compatible keys) have **no refresh and no rotation**, so they carry **no
auth concurrency hazard** — only provider-side rate limits and HTTP client
sharing (handled by the worker pool, not here).

## 2. Goals / non-goals

Goals:
- One process-global authority per auth source; all workers share it.
- **Single-flight refresh**: N concurrent callers trigger **one** refresh.
- Atomic file writes; consistent in-memory snapshot.
- Permanent-failure **quarantine** + **actionable** errors (never raw provider
  JSON).
- External-login awareness (`codex login` mid-run picks up + clears quarantine).
- Transparent migration: adapters keep consuming `TokenGetter`/`TokenRefresher`.

Non-goals:
- Not a 1:1 port of Codex's Rust `AuthManager`.
- Not a general secrets vault; not interactive login (still external
  `codex login` / `minimax` OAuth flow).
- Not changing the wire protocol (`store=false`/stream-only stays in profiles).

## 3. Identity & registry

```
type AuthKind int
const ( AuthStaticKey AuthKind = iota; AuthOAuthFile )

// AuthRef identifies one external auth source.
type AuthRef struct {
    Provider string // "codex-cli", "minimax-oauth", "kimi-coding", …
    Kind     AuthKind
    Path     string // absolute auth-file path (OAuth); "" for static key
}
func (r AuthRef) key() string // = Kind==OAuthFile ? abs(Path) : "static:"+Provider
```

The manager holds `map[string]*authEntry` keyed by `AuthRef.key()`, guarded by a
top-level `sync.Mutex` (or `sync.Map`) for entry lookup/creation only. **The
registry key is the absolute auth-file path**, so every worker resolving the
same `~/.codex/auth.json` shares one entry regardless of pool topology.

## 4. Data structures

```
type authSnapshot struct {
    Token        string
    RefreshToken string
    ExpiresAt    time.Time
    AccountID    string    // ChatGPT account_id; "" if none
    FileMtime    time.Time // mtime when this snapshot was loaded
    LoadedAt     time.Time
}

type quarantine struct {
    Reason      string    // "refresh_token_reused" | "refresh_token_expired" | "invalid_grant"
    Since       time.Time
    FileMtime   time.Time // mtime at quarantine time; a newer file clears it
    Actionable  string    // e.g. "Codex login expired — run `codex login`, then retry."
}

type authEntry struct {
    ref   AuthRef
    mu    sync.Mutex          // guards snap + quarantine + file write
    sf    singleflight.Group  // key "refresh" — collapses concurrent refreshes
    snap  authSnapshot
    quar  *quarantine         // nil = healthy
    // provider hooks (set at registration; pure I/O, no locking of their own):
    load    func(path string) (authSnapshot, error)             // read+parse file
    refresh func(prev authSnapshot) (authSnapshot, *authError)  // rotate+atomic-write
}

type authError struct {
    Permanent  bool
    Reason     string
    Actionable string
    cause      error
}
```

## 5. Manager API

```
type ExternalAuthManager interface {
    // Register an auth source (idempotent; first writer wins). Static-key
    // sources register with a nil refresh and a constant snapshot.
    Register(ref AuthRef, load LoadFunc, refresh RefreshFunc)

    // Token returns a currently-valid token, refreshing single-flight if near
    // expiry. Returns *authError (Permanent) on quarantine.
    Token(ctx context.Context, ref AuthRef) (string, error)

    // ForceRefresh is the 401-replay path: refresh once (single-flight),
    // return the new token or a permanent error.
    ForceRefresh(ctx context.Context, ref AuthRef) (string, error)

    // AccountID returns the provider account id without refreshing.
    AccountID(ref AuthRef) string
}
```

## 6. Core flows

### Token(ref)
1. `lock`.
2. **mtime reload**: if file mtime changed since `snap.FileMtime`, `load()` →
   replace snap (picks up external `codex login`; consistent snapshot otherwise
   — we do NOT re-read on every request like today's getter).
3. **quarantine check**: if `quar != nil` and file mtime unchanged since
   `quar.FileMtime` → `unlock`, return permanent `authError(quar.Actionable)`
   **without any network call**. If file changed → clear `quar`, continue.
4. static key → `unlock`, return `snap.Token`.
5. token valid (`ExpiresAt - skew > now`) → `unlock`, return `snap.Token`.
6. stale → `unlock`, fall through to refresh (below).

### refresh (single-flight)
```
v, err, _ := entry.sf.Do("refresh", func() (any, error) {
    entry.mu.Lock(); defer entry.mu.Unlock()
    // re-check: a prior flight may have already refreshed
    if fresh(entry.snap) { return entry.snap.Token, nil }
    next, aerr := entry.refresh(entry.snap)   // rotate token + atomic temp+rename
    if aerr != nil {
        if aerr.Permanent { entry.quar = &quarantine{...from aerr...} }
        return nil, aerr
    }
    entry.snap = next
    return next.Token, nil
})
```
`singleflight.Group.Do` guarantees **one** in-flight refresh per key; all
concurrent callers receive the same token (or the same error). This is the fix
for the rotation race — the refresh token is consumed exactly once.

### ForceRefresh(ref) — 401 replay
Skips the validity check, goes straight to the single-flight refresh. Adapters
call this when the backend returns `token_expired`/401, then replay the request
once (matching the existing AGENTS.md "refresh + replay once" contract).

## 7. transient vs permanent classification

`refresh()` maps the OAuth response to `*authError`:

| Condition | Class |
|---|---|
| network error, timeout, HTTP 5xx, 429 | transient (caller may retry; not quarantined) |
| HTTP 400 `invalid_grant`, body `refresh_token_expired` / `*_reused` | **permanent** → quarantine |
| HTTP 401 `invalid_client` | permanent → quarantine |
| missing `access_token` in 200 body | permanent (provider contract broken) |

Quarantine is cleared automatically when the auth file mtime advances (a fresh
`codex login`). No retries are attempted while quarantined — this is exactly the
"avoid repeatedly breaking login state" behavior.

## 8. Atomic file write

All OAuth writebacks use temp-in-same-dir + `os.Rename` + mode `0600` — MiniMax
already does this (`minimax_oauth.go:468`); **Codex must adopt it** (currently a
plain write). Centralized in the manager's refresh helper so every provider gets
it.

## 9. Provider matrix

| Provider | Kind | load | refresh | Notes |
|---|---|---|---|---|
| codex-cli | OAuthFile | read `~/.codex/auth.json` | rotate refresh_token, atomic write | account_id → `chatgpt-account-id` header; store=false/stream-only stay in profile |
| minimax-oauth | OAuthFile | read state file | rotate, atomic write (already) | own refresh endpoint |
| kimi-coding | StaticKey | env/key | nil | no refresh; never quarantine; HTTP/1.1 transport shared |
| generic API key | StaticKey | env/config | nil | same as Kimi |

## 10. Migration / compatibility (low blast radius)

The adapter contract is **unchanged** — it keeps calling
`Runtime.TokenGetter`/`TokenRefresher`. Only the resolver wiring changes:

- `codex_oauth.go` / `minimax_oauth.go` stop building a closure-local
  `mu`+`state`; instead they `Register(ref, load, refresh)` with the manager and
  return a `Credential` whose `Getter`/`Refresher` are thin wrappers:
  - `Getter = func() string { t, _ := mgr.Token(ctx, ref); return t }`
  - `Refresher = func() string { t, _ := mgr.ForceRefresh(ctx, ref); return t }`
- Static-key providers: `Getter` returns the key (no manager needed, or a
  pass-through entry).
- The manager is a process-global singleton created in `internal/app` wiring and
  shared across all Agent workers. `AccountID` continues to flow into
  `chatgpt-account-id` (`resolver.go:153`).

Because `Getter`/`Refresher` still return `string`, and the manager swallows the
error into the wrapper for the legacy string contract, we should ALSO expose the
typed error to the adapter for actionable messaging — add an optional
`Runtime.TokenError func() error` (or have adapters call the manager directly
where available) so permanent failures surface as "run `codex login`" instead of
an empty token + opaque downstream 401. (Adapter-side change is small and
additive.)

## 11. Worker-pool concurrency contract (co-design)

- One shared `ExternalAuthManager` injected into all workers.
- One shared `http.Client`/transport per provider — **never per worker**;
  Kimi's HTTP/1.1 (ALPN-restricted) transport especially must be reused, or
  connection churn can re-trigger the HTTP/2 `unexpected EOF` documented in
  `AGENTS.md`.
- Per-provider concurrency semaphore + 429 backoff (pool/transport layer):
  Kimi has no auth hazard but will be rate-limited under parallelism.
- account_id always attached for codex requests.

## 12. Testing & eval

Unit:
- Concurrent `Token()` with an expired token and a refresh hook that counts
  calls → **exactly one** refresh; all callers get the new token.
- Permanent error → quarantine → subsequent `Token()` returns the actionable
  error with **zero** further network calls; bumping the file mtime clears it.
- Refresh writes are atomic (no partial/corrupt file under concurrent attempts).
- Static-key source: `Token()` returns the key, never refreshes, never
  quarantines.

Eval (offline, `internal/eval`):
- codex `store=false` / stream-only / `chatgpt-account-id` header present
  (regression guards).
- N parallel runs on the same provider produce no auth failure.

## 13. Rollout

1. Build `ExternalAuthManager`; route codex + minimax-oauth through it
   (transparent — adapters unchanged), kimi/static stay pass-through.
2. Verify with the unit/eval tests above (single-flight, quarantine, atomic).
3. THEN build the worker pool + per-person/workspace queue on top of the shared
   manager and shared transports.
