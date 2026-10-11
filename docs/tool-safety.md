# Tool Execution and Safety Contract

This document is the detailed source of truth for tool execution, workspace
scope, approval, network egress, delegation, and result presentation. Read it
before changing `internal/tools`, execution middleware, or kernel tool dispatch.

## Execution Scope

- File, terminal, patch, process, and workspace-aware memory tools execute
  inside the request's `ExecutionScope`. Scope comes from the active logical
  workspace and allowed roots, not the daemon process cwd.
- Scope checks must resolve path semantics safely, including wrapper commands
  and symlink-sensitive operations. A workspace switch must affect subsequent
  tool execution without restarting the daemon.
- A repeatable local-CLI `--add-dir` is a run-scoped root binding, not a
  workspace mutation or trust grant. The local control socket, which only your
  own CLI can use, carries it, and the daemon canonicalizes it before admission; queue and run rows retain the frozen
  binding; tools, project-context discovery, sandbox mounts, and overlapping
  path scheduling consume the same typed roots. An external additional root
  makes the aggregate scope untrusted, while a nested root already covered by
  a trusted workspace does not reduce that workspace's trust.
- A call resolves the scope of the run it belongs to: the run key on its
  context, or the key named by its trusted `_invocation_scope`, which a
  delegated sub-agent's forked context still carries. A call that names a run
  never falls back to the person-level scope, which may belong to another
  execution, and a confined filesystem or process call whose run scope is gone
  is refused rather than run against the daemon's directory. Calls without a
  run, such as local helpers, resolve by person only when there is no ambiguity
  between distinct active Runs; otherwise dispatch fails closed. Registering
  a second Run cannot overwrite the first Run's scope, and either cleanup
  removes only its own registration. If a trusted context and invocation name
  different Run keys, neither key is accepted.
- `vision_analyze`'s local-path branch is a filesystem read and obeys the
  scope like `read_file` (`WorkspaceScopeMiddleware`); its http(s) branch
  stays with the tool's SSRF check. Any new tool that reads a caller-supplied
  path must join the middleware's scoped-tool list and `runScopedToolCall`,
  never `os.ReadFile` raw.
- Message attachments (e.g. TUI clipboard-pasted images) enter a run only via
  the gateway import channel (`httpapi/attachments.go`): files are copied into
  the person-partitioned `<data>/attachments/<person>/<run>/` store and that
  person's partition joins the run's `AllowedRoots`. Arbitrary out-of-scope
  client paths (OS temp dirs, other users' files) stay unreadable; do not
  widen scope to accommodate an attachment path directly.
- Tools that do not need filesystem or process access must not receive broader
  scope as a convenience.

## Execution Environment

An execution environment belongs to a run, not to the channel that submitted
the message. CLI, IM, cron, and HTTP may continue the same work without
silently changing its filesystem, toolchain, credentials, or network view.
Future remote runners must preserve this rule through a durable
`EnvironmentLease`.

The lease stores policy and references only:

- workspace and environment profile identity;
- capability decisions such as shared network or host escape;
- `CredentialRef` values that identify a credential source and principal;
- a principal/source fingerprint that survives ordinary token rotation.

Raw credential bytes must never be persisted in a lease, run event, approval
grant, artifact, diagnostic response, or task metadata. A missing environment
parks work as `waiting_user` with structured reason
`environment_unavailable`; installing a compatible environment may create a
new run and lease through the existing resume path. Do not add a new top-level
task state for this condition.

### Child process environment

Every tool-owned child process, including terminal, verification, code
execution, and stdio MCP servers, constructs `cmd.Env` through
`BuildProcessEnv`. Direct `os.Environ()` inheritance at an execution callsite
is forbidden.
Daemon-owned Git admission and managed-view commands use the same core filter
before applying their Git-specific environment restrictions; they do not pass
SelfMind control variables into those children.

The current compatibility phase preserves the operator's normal toolchain and
Agent CLI login environment while removing SelfMind control-plane identity,
gateway addresses, tokens, tenant/person/task ids, and channel state. This
lets existing CLIs keep using their normal login mechanisms without exposing
the daemon's own authority. Later credential snapshots may narrow this set,
but must do so through data-driven profiles rather than per-vendor branches.

The immutable snapshot is projected into each invocation's actual network
view. An isolated invocation receives no standard proxy variables. A shared
invocation preserves remote proxies and reachable loopback proxies; when a
loopback listener is no longer reachable, the runtime omits it from that child
environment and records `proxy_mode` in the sandbox plan and execution event.
This decision is runtime-owned and generic: models do not need to remember
shell prefixes such as `unset HTTPS_PROXY`, and the execution engine has no
provider, project, or command-specific proxy branch.

Ordinary commands inherit that runtime route. Literal removal of only standard
proxy variables (`unset` or `env -u`) is separate from external mutation proof:
every remaining command must still be a proven observation. PATH changes,
credential removal, substitutions, unknown wrappers and writes remain gated.
In smart mode, removal of a proxy configured in the Run's frozen environment
receives the existing authorization review even for a read-only query; a broad
observation grant cannot release that route change. The judge receives variable
names and the current intent, never proxy addresses or credentials. Removal
detection names programs through the same identification as the hard floor
(see Hard Safety Floor), because a case-insensitive filesystem runs `Env -u`,
`BASH -c 'unset …'` and `/usr/bin/ENV` as the ordinary programs; it sees
through every known wrapper (`exec env -u`, `nohup env -u`) and into `-c`
scripts, and an empty assignment (`HTTPS_PROXY= gh …`) is a removal too. A
path-qualified wrapper is still never proof. The route change is appended to
the review reason: a dangerous-operation or explicit-deny reason stays in the
judge prompt and the person's request. Full-auto
retains its existing authority contract. Commands are never silently rewritten.
Typed process results expose network mode, proxy projection (`not_configured`,
`inherited`, or an omission reason), and literal requested proxy removals to the
model separately from raw output and the compact user preview. Requested
removals describe the command, not proof of the route an arbitrary program used.

Bubblewrap receives the constructed environment through `cmd.Env`. Never put a
credential into bwrap arguments with `--setenv`: process arguments are visible
through process listings and `/proc/*/cmdline`.

### Secret redaction

One runtime `SecretRegistry` feeds the existing `RedactSensitive` boundary.
The same redactor protects new durable writes and logical read-time views such
as model context, IM, artifacts, diagnostics, and logs. Do not build a second
redaction implementation or physically rewrite historical audit evidence.

Every detector reports the byte ranges it would mask in the original text, and
the union of all of them is masked once. One match therefore never hides part
of another: two registered secrets that overlap are masked as one range, not
the longer one first with the other's remainder left visible.

Redaction keeps a JSON tool result valid JSON whenever that is safe, because
the plan gate, the model and every formatter parse it. Inside JSON text an
assignment's escaped quotes (`TOKEN=\"…\"`, or `\\\"` for JSON inside a JSON
string) delimit the masked value, and a value never ends inside an escape
sequence. A valid document (whitespace JSON does not allow, such as a form
feed or a no-break space, and a leading byte order mark may surround it) is
also matched string by string on its decoded
values and keys, and rewritten in place: only a string that changed is
re-encoded, every other byte keeps the producer's layout. If text masking
would break a valid document, the original is masked value by value instead:
every scalar under a key whose decoded name is a credential name, and every
key that is itself a secret; two keys that mask alike stay distinct. That form
is used only when it shows nothing the text pass masked; otherwise the
text-masked output is kept, security over validity, and the result is flagged
`RedactionUnparseable`, which tells the model to read it as text; that text
mask also covers every string whose decoded value the value-by-value form
masks. A document in which the raw form of a registered secret holding a JSON structure
character (`[]{}":,`) or whitespace matched is never masked value by value:
such a secret may have matched across tokens, and the value-by-value form can
split it into leaves that a pattern masks only in part (a cookie header, a URL
with a password parameter), so the text mask is kept. A match of one of its
JSON-escaped forms does not count: escapes occur only inside a JSON string, so
that match cannot span tokens. Otherwise the check looks for each masked value
as written and as a JSON string writes it, in one indexed pass over the
document. Every registered secret is checked; as a second net, one that is
itself JSON is also checked by its compact re-encoding and by each key, string
and number in it. The only value passed over is a pattern match made of JSON
brackets, literals and masks alone; it opens a container whose strings and
numbers the value-by-value form masks.

At present the registry knows daemon-configured secrets and stripped
control-plane values, plus shape-based token patterns. Operator credential
values become registrable when an execution snapshot is materialized. Values
remain registered for the process lifetime so rotation cannot reveal an older
secret in historical output. FTS may still reveal that a matching row exists
even when content is masked; treat that as a documented existence leak until
indexes support redacted projections.

Exact identifier-like values (letters, digits, `_`, or `-`) are masked only at
identifier boundaries. This prevents a short registered value such as a tool
or account name from corrupting a longer resource name that merely contains
the same characters. Overlapping or touching occurrences of such values count
as one run whose ends must be boundaries; an escape sequence such as the `\n`
before a token on a new line of a JSON string, and the edge of another
registered secret's occurrence, are boundaries too. Structured secrets
containing punctuation still use exact-value replacement.

What is covered, exactly:

- **Registered secrets.** The raw value; its JSON string body as encoding/json
  writes it, with HTML escaping (`\u003c`, `\u003e`, `\u0026`) and without; the
  same body with every control character as `\u00xx`; each of those escaped
  bodies with uppercase hex digits (`\u00XX`, `\u003C`); a multi-line value
  with either line ending, since line-by-line output carries LF and a file
  read whole keeps its own; and a value that is a JSON object or array
  whatever its whitespace, so a secret registered compact is found
  pretty-printed and the reverse. In a valid JSON document every string and
  key is matched decoded, so any producer's escaping is covered (.NET's
  `\u0022` and `\u002B`, PHP's `\/`, Python's `ensure_ascii` `\u00e4`,
  mixed-case hex), and JSON inside a JSON string is redacted as a document of
  its own, to eight levels. A value is also looked for in the text without
  terminal color codes and invisible characters, so neither can split it.
- **Credential names.** The value assigned to a name that ends in a
  credential word, in any case and style. Singular words: `token`, `secret`,
  `password`, `passwd`, `passphrase`, `authorization`, `_pass` and `-pass`
  (`SMTP_PASS`), `SharedAccessSignature`, and `key` after `api`, `access`,
  `account`, `private`, `secret`, `client`, `auth`, `signing`, `encryption`,
  `app` or `master`, also followed by `-data` (`API_KEY`, `Secret Key`,
  `client-secret`, `AccountKey`, `APP_KEY`, `MASTER_KEY`, kubeconfig's
  `client-key-data`, `//registry.npmjs.org/:_authToken`). `credential(s)` is a
  weak name: what it opens is a credential container, but a bare plain word
  after it is a setting (`credentials: include`, `persist-credentials:
  false`). Plural words (`secrets`, `passwords`, `apiKeys`) mark a value only
  when it looks like a credential: a count, a list marker or a list of
  `name:` references (`imagePullSecrets`, a ServiceAccount's `secrets`) is not
  one. `auth` marks a value only when it is base64 of `user:password`, as
  docker's `config.json` and npm's `_auth` write it. Not credential names:
  `tokens` (`max_tokens` counts tokens), `PWD` (the working directory) and the
  cursors a model needs to page through an API (`nextPageToken`, `NextToken`,
  `continuationToken`, `--starting-token`).

  The forms are `name=value`, `name: value`, `name := value`,
  `name == value` and `name != value` (env, YAML, HTTP headers such as
  `X-Api-Key`, dotenv, `export`, INI, TOML, a query string), `name => 'value'`
  (a PHP array), `x["name"] = value`, a typed declaration
  (`const T: &str = "x"`, `var name string = "x"`,
  `password: Optional[str] = "x"`), a call with the name as a string and a
  literal after it (below), the `value` field beside a name (below), an
  element's text (`<password>x</password>`, also with its text on the next
  line), a JSON member, a command-line flag (`--api-key value`,
  `--password=value`) and `.netrc`'s `password value`. A value is looked for in
  a window after its name that holds 512 bytes that are not blanks, so any run
  of blanks between them is read past; a value longer than 512 bytes is a
  credential whatever it looks like.

  A **call** that names the credential in a string and gives a literal after
  it (`os.Setenv("API_TOKEN", "x")`, `req.Header.Add("X-Api-Key", "x")`,
  `os.environ.get("API_KEY", "x")`, `getenv(…)`, `config.set("auth",
  "password", "x")`, `args.add_argument("--token", default="x")`) masks the
  literal when the call sets or reads a value by name (`set…`, `put…`,
  `define…`, `export…`, `assign…`, `store…`, `save…`, `write…`, `add…`,
  `append…`, `get…`, `insert…`, `push…`, `header…`) and the literal is not a
  keyword, not itself the name of a credential or of an environment variable
  (`fields.set("token", "access_token")` maps a name to a name), not a flag
  (`-t`) and not an encoding (`utf8`). Any other call, and a tuple, masks only
  a literal that does not read as a name (`_first_env("A_TOKEN", "B_TOKEN")`
  and `("api_key", "base_url")` pair names; `("api_key", "Xk9ab12Cd")` does
  not).

  The **value field beside a name** is found after the name, on a later line
  of the same list entry (a comment, blank lines and the entry's other fields
  may lie between; up to 8 lines) or, for a name that is the value of a field
  called `name` or `key`, before it: `- value: x` above `name: DB_PASSWORD`,
  `{"value": "x", "name": "DB_PASSWORD"}`, `<add value="x" key="DbPassword"/>`.
  The stream holds a line that is `value: …` until the next line, which may
  turn out to name a credential.

  A **quoted** value is masked: the quote is evidence of a literal, whatever
  the literal starts with (`"$3cr3tP@ss"`, `"<Xk92mQzpR7>"`, `"two words"`). The
  quote may be doubled or tripled (`''x''`, `"""x"""`), a backtick, and
  escaped inside JSON text; a doubled quote inside a literal of that quote is
  one (`'it''s'`, `"a""b"`). The closing quote is looked for on the line, up to
  64 KB of it, and on up to two following lines that start with blanks (a
  folded YAML scalar); a call, a declaration, a triple quote and a backtick
  look for it on later lines too, within 512 bytes (64 KB for a triple quote
  and a backtick). A literal that is not closed is masked to the end of its
  line, of its unspaced run if that is longer, or, for a triple quote and a
  backtick, to the end of the window of 512 bytes. A quote that opens
  nothing is no literal: `""`, `''` before text that is no value
  (`--token='': help`), and a backtick before punctuation (`` `?token=` ``
  in prose). A quoted value is a
  reference only when it is one whole: `"${DB_PASSWORD}"`, `"$(gcloud auth
  print-access-token)"`, `"{{ .Values.password }}"`, `"$GITHUB_TOKEN"`,
  `"<your-key-here>"`, `` `cat token.txt` `` and a template string with
  its words before the interpolation (`` `Bearer ${token}` ``) are references;
  `"${DB_PASSWORD}x9"` and `"$3cr3tP@ss"` are literals.

  An **unquoted** value is not masked when it is a reference by its shape:
  `${…}`, `$(…)`, `$NAME` or `%NAME%` whose name is ALL_CAPS or lowercase
  letters only (also `$NAME/path`), a `<placeholder>` without a digit; when it
  is a boolean or null (`false`, `None`); a type annotation (`password:
  string`, `Optional[str]`); the opening of a container or template (`{`, `[`,
  `{{ … }}`); a `{name}` interpolation; the next YAML key; the empty value of
  `NAME= command`; the name of a docstring's class or function (`class
  Token:` then `"""…"""`); or the word a name is compared with after `==`.
  A name closed by `]` (`x[password] = value`) is a variable used as a key,
  and its bare value is another. Otherwise an unquoted value is code only with
  positive evidence on the name's own line (a window of 256 bytes before the
  name, 512 after it), and a literal without it. The evidence exempts what
  code is made of, listed below, and never a value that has a digit among its
  letters, a symbol or a dotted path with digits (`Xk9ab12Cd`, `hunter2`,
  `s.rGSC2pP…`), unless it is a call or an index:
  - strong evidence, a construct only code has: a declaration keyword or type
    before the name (`var password = pw`, `String password = …`), and `:=`
    after a name that is not ALL_CAPS (`token := tok`). It exempts a call whose
    arguments are words and punctuation of code (`os.Getenv(`,
    `loadToken(cfg)`), an index, and an identifier without a digit (`tok`,
    `API_KEY`, `hunter`);
  - statement evidence, which a line of configuration can be written to show:
    member access with a spaced `=` (`c.token = tokValue`; `db.password =
    secret` is a `.properties` line), `==` or `!=`, a value that ends its
    statement with `;`, a name that ends its line with `:` after `if`,
    `while`, `def` or `case`, or that closes a quote (`case "x_token":`). It
    exempts a call of restricted arguments, an index, a dotted path of letters
    alone, a camelCase or snake_case variable and a statement keyword
    (`return`, `raise`, `null`, `self`, …), not a single word: `if token ==
    tok`, `c.token = tok` and `password: hunter;` are literals;
  - medium evidence: an unclosed `(`, `[` or `{` earlier on the line
    (`connect(host, password=db_password)`), a previous line that ends with
    `{`, `(`, `[` or `,`, or a value followed by `,`, `)` or `}` (`apiKey:
    someVar,`). It exempts what statement evidence does, except the keywords,
    and the name's own echo;
  - none: a call, an index and a dotted path of letters alone are code only
    after a spaced `=` (`password = os.environ["X"]`, `token = settings.token`),
    never when the `=` touches the name (`PASSWORD=os.getenv(x)`) and never
    after `name: value`. A bare `apiKey: someVar` on a line of its own and
    `api_key = api_key` are therefore masked.

  `credential(s)` is a weak name: a bare value after it that is a setting
  word (`include`, `omit`, `same-origin`, `default`, `none`, `prod`, …), or one
  that is followed by more words on its line (prose), or that touches its colon
  (`credential:read`), is not a credential, unless it has a digit or a symbol
  among its letters (`credentials: Xk9mQz2pLw4r and more`); any other is
  (`credentials: mysecretvalue`). A name that ends in `_pass` or `-pass` after a word that
  counts or selects a pass (`first_pass`, `since_pass`, `selected_pass`) is
  not a credential name.

  The stream reads the same lines the result does, so a line is judged with
  the lines before it, not alone. Names are read as a person sees them:
  invisible format characters (zero-width spaces and joiners, the soft hyphen,
  bidirectional controls) and JSON `\uXXXX` escapes of letters, digits, `_`
  and `-` do not hide one. A value on the line after its name is masked. An
  `Authorization` value keeps its scheme (`Basic`, `Bearer`, `Digest`, …) and
  masks the credentials after it.
- **Credential containers.** Under a singular credential name, every string
  and number of the container it opens is masked at any depth
  (`"credentials": {"pass": "…"}`, `"api_key": [1234]`). Under a plural name
  its strings are, and so are the strings of the objects it lists, except
  those under a key that names a reference (`name`, `ref`, `id`, `kind`,
  `type`, `namespace`, `path`, `url`, `key`, …). A valid document is read by
  its decoded keys, other text and a stream by its quoted keys, to 64 levels
  of nesting; scalars deeper than that keep the mode of the deepest container
  that was followed, so what a credential's name opened stays masked at any
  depth. Every string of a container is masked, a reference included, since
  a quoted string is a literal.
- **Credential shapes**, with no name: `sk-` provider keys; GitHub tokens
  (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), Slack tokens
  (`xoxb-`, `xoxp-`, …), Stripe keys (`sk_live_`, `rk_live_`), Google API keys
  (`AIza…`), GitLab tokens (`glpat-`) and Hugging Face tokens (`hf_`), whose
  prefix stays visible; JWTs, three base64url segments the first of which
  starts `eyJ`; `Bearer` credentials, but not the words of prose that follow
  the scheme's name; `Basic` credentials that decode to `user:password`; and
  an AWS secret access key written next to its `AKIA` or `ASIA` key id. The
  key id is an identifier and stays visible. A shape must start a word, so
  `task-…` names, git SHAs, sha256 digests, UUIDs and base64 image data are
  not masked.
- **Formats.** A private key's PEM body (RSA, EC, OPENSSH, ENCRYPTED and PGP
  blocks): the run of base64 and header lines after its BEGIN line, so prose
  that mentions the armor masks nothing, and the armor lines stay. The lines
  of a body may share a prefix, which the first line sets and which may differ
  from line to line in its digits only: an indent (a YAML block scalar), a
  `| `, `# ` or `> ` marker, a log line's timestamp or pod name. After a
  prefix of words a body starts with a whole line of base64 (32 characters or
  more), so a sentence that ends in a word after the armor is not one. A key
  squeezed into one line, its chunks apart by spaces
  (`KEY="-----BEGIN … KEY----- MIIE… -----END … KEY-----"`), is masked from
  its first chunk to its last when each chunk but the last is 16 characters or
  more. The password of a URL's userinfo (`scheme://user:password@host`; the
  user name and host stay). The values of a `Cookie:` header and the cookie
  value of `Set-Cookie:` (its attributes stay), also in JSON. The password of
  `mysql -p<password>`, `curl -u user:password` and `--user`. Azure's
  `AccountKey=` and `SharedAccessSignature=` in a connection string. The
  signature of a signed URL: `sig=` and `Signature=` as a query parameter,
  `X-Amz-Signature=`, `X-Goog-Signature=`, and the `Signature=` of an AWS
  `Authorization` header. The secret of an incoming webhook's address: a Slack
  hook's last path segment, a Discord webhook's token, the segment after
  `IncomingWebhook` in a Teams connector's.
- **Terminal color codes.** Detection runs on the text without ANSI escape
  sequences, so `jq -C` and `grep --color` output is read as the text it
  colors; a mask covers the original range, codes inside it included.
- **Preparation refusals.** Arguments refused before a tool body runs, by
  schema coercion or validation or because they are not one JSON object, can
  be quoted by the refusal; it is redacted like a tool failure before the
  model, events or records see it.
- **Streamed output.** Command output streamed live as `tool.output` lines
  reaches clients before the result middleware sees the whole output, so the
  execution path masks each line itself, with a lookback of the shown text
  before it on the same stream (256 bytes that are not blanks, and as much as
  a registered JSON secret needs, with up to 64 KB of blank lines between a
  name and its value). A line is masked by what its window shows, not by
  itself, so the evidence that a value is code lies in the lines before it as
  in the result; only a line that is a JSON document is also read whole, to
  stay JSON. The stream masks what the result masks. The command a run shows
  is masked once per call and marked (`kernel.RedactedText`), so the event
  redactor does not mask it again for every line of output. The captured
  result merges the two streams in arrival order, so what one stream prints
  with the other stream's lines between its own can be invisible to the
  result middleware: a registered multi-line secret, a name and its value on
  separate lines, an AWS key id and its secret, a private key's body, the
  strings of a credential container. When both streams wrote, each is read on
  its own with every detector, and what the merged text does not show to a
  detector is masked in the capture, line by line where it lies. A line that
  is `value: …` waits for the next line before it is shown, which may be the
  `name:` that makes it a credential. Two kinds of state follow the stream
  line by line, because their extent is not a registered value's: a private
  key's body between its armor lines, and the scalars inside a credential
  container (a pretty-printed `"api_keys": [` with its strings on the lines
  after it); a stream that stops looking like JSON for two lines ends the
  container. Lines that may
  begin a registered secret spanning lines (a PEM key, a pretty-printed JSON
  secret) are held until it completes, when all its lines are masked, or
  diverges, when they are released unchanged. A secret that opens with lines
  of punctuation alone (`{`) is looked for from the first line with more in
  it, so a line that merely ends with `{` is not held, and a JSON secret is
  followed whatever its whitespace. What is held is bounded by the longest
  registered secret plus a small slack, and everything held is released when
  the stream ends, by exit, timeout or cancellation.
- **Previews.** A preview is cut after masking, never before, so a cut cannot
  leave the start of a secret no longer recognizable as one: the arguments and
  result preview a tool event keeps in control.db, an eval recording's input,
  output, tool and content records, an approval's code preview, and a logged
  tool result.

Not covered: encodings of a secret such as base64, hex or URL encoding (but
`Basic`, docker's and npm's `auth` are base64 of `user:password` by
definition); secrets the model splits across separate tool calls or
arguments; a registered multi-line secret printed only in part, indented, or
behind a log prefix on every line, and a registered JSON secret printed with
more whitespace than its layout can take; characters inserted into a
registered secret's own value; Unicode case folding that only looks like a
credential name; credential containers written as YAML block lists
(`api_keys:` then `- k1`) or with unquoted keys; a call whose name is further
than one argument from the credential's (`f(a, b, "password", "x")`); more
than 64 KB of blank lines between a name and its value in a stream (the
result reads any number); and credentials with neither a credential name, a
known shape nor a known format.

The remaining residual of the code rule and of the pairing rules is exactly
these classes; a value that is a literal can be left visible only in them:

1. A camelCase or snake_case variable shape, the name's own echo or a dotted
   path of letters alone, where medium evidence reads it as code: inside an
   unclosed `(`, `[` or `{` of its line, on a line after one that ends with
   `{`, `(`, `[` or `,`, or before a `,`, `)` or `}`
   (`connect(host, password=db_password)`, `{"password": someVar}`,
   `{` then `password: correct_horse_battery`, `{"password": hunter.two}`).
2. A call of restricted arguments, an index or a dotted path of letters alone
   after a spaced `=` or `:=` with no evidence (`token = hunter.two`,
   `password = Xk9(2mQz`).
3. An identifier without a digit after a declaration keyword or type, or after
   `:=` on a name that is not ALL_CAPS, where strong evidence reads it as
   code: `var password = hunter`, `let token = tok`, `token := tok`.
4. A weak name's bare value that is a setting word, or reads as a word
   (letters joined by `_ . : / -`, no digit) and is followed by more words or
   touches its colon: `credentials: default`, `credential:read`,
   `credentials: mysecretvalue and more`.
5. In a call, a literal that reads as a name or a flag: `x.set("token",
   "access_token")`; and, in a call that neither sets nor reads a value by
   name and in a tuple, a literal of letters alone
   (`vault.read("API_KEY", "mysecretvalue")`).
6. A multi-line literal beyond what is read: one that opens with a triple
   quote or a backtick and is not closed within 64 KB, past its first 512
   bytes; a call's string that runs over lines, past 512 bytes; and, in a
   stream, any multi-line literal past the 256 bytes of lookback after its
   opening quote.
7. A paired value field beyond the result parser's bounded name/value window
   (8 lines or 512 bytes; reverse XML lookup is bounded separately). Streaming
   holds unresolved scalar fields within its line and byte budgets; budget
   exhaustion masks that scalar rather than releasing it. A value-first block
   scalar is masked conservatively while its name is unknown, because already
   emitted lines cannot be recovered. Name-first fields outside the bounded
   association window remain a residual; this is not a complete format parser.

Two committed matrices measure the first two classes, each cell checked in
the result and in the stream: 620 cells (31 config syntaxes × 20 values,
`TestConfigMatrixMasksLiterals`), of which 33 are in the residual and the rest
masked (28 are a variable shape inside the brackets of `(PASSWORD=x ./run)`,
`$(… DB_PASSWORD=x …)`, `{ export PASSWORD=x; }` and `auth: {password: x}`,
five a call-shaped `Xk9(2mQz` after a spaced `=`), and 528 cells (33 contexts
× 16 values, `TestProbeMatrixMasksLiterals`), of which 24 are (7 bare JSON
members, 7 keyword arguments and 10 dotted paths after a spaced `=` or `:=`).
`TestEvidenceNeverExemptsAValueThatLooksLikeACredential` holds 58 spoofed
lines (a trailing `;`, a declaration word, `x.` before the name, `:=`, `==`,
`!=`, a block label with the value on the next line, a previous line that ends
in a comma or an opening bracket, a single word after member access, a
comparison, a `;` or a label) whose value must be masked, and
`TestReviewerAdversarialList` holds the 117 cases of the review of round 3.
Each class has a test that pins it, so that a change in either direction must
update this list: classes 1 and 2 `TestConfigMatrixMasksLiterals` and
`TestProbeMatrixMasksLiterals` and `TestDocumentedResidualOfTheCodeRule`,
classes 3 to 5 `TestDocumentedResidualOfTheCodeRule`, classes 6 and 7
`TestDocumentedResidualOfLiteralsAndPairs`. A false-positive table in the same
tests holds what stays unmasked (`token := os.Getenv("GITHUB_TOKEN")`,
`c.token = tok`, `case "x_token":`, `automountServiceAccountToken: false`,
`credentials: include`, a Go `Password string` field, `--token string` in
help output, Helm `{{ .Values.password }}`, a prompt such as
`input("Password: ")`, lists and tuples of names, type annotations, quoted
whole references, `first_pass = 0`).

### The local control channel and CLI identity

CLI identity is the person's own terminal, and only that terminal may wield it.
"Authenticated local CLI" throughout this document means a request the daemon
verified on its local control socket. A token, a `platform` field, or a loopback
address is not that proof.

**Channel.** The CLI and TUI reach a local daemon over a Unix domain socket,
`<dataDir>/gateway/control.sock`, or, when that path is too long for a socket
address, `/tmp/selfmind-<uid>/<hash of the data directory>.sock`. Its directory
is `0700` and owned by the daemon's user: the client refuses a socket whose
directory another user owns or can enter, and the daemon repairs the mode at
start. A socket file left by a killed daemon is replaced at start; anything else
at that path is never removed, and the daemon does not start without its socket.
The TCP listener stays for IM webhooks and HTTP integrations. Nothing that needs
CLI identity falls back from the socket to TCP, and the earlier shared
`local_control.token` file, which any tool running as the same user could read,
is deleted at start and no longer accepted. A daemon that predates the socket
answers health checks but cannot serve the CLI; `EnsureRunning`,
`selfmind gateway status`, and `selfmind doctor` name it and the restart that
fixes it. Stopping the daemon and reading its status are the one exception: the
CLI asks a daemon on this host on its socket first, and over TCP only when no
socket exists, which only a daemon that predates it answers; a current daemon
refuses that request unless it carries the gateway token.

**Verdict.** The daemon judges a connection from the kernel's record of who
connected (macOS `LOCAL_PEERCRED` with `LOCAL_PEERPID`, Linux `SO_PEERCRED`),
taken once per connection at the first request that claims CLI identity, while
that client waits for its answer. The peer must run as the daemon's user, must
not be the daemon or one of its descendants (the ancestry walk fails closed when
a process cannot be read), and must not belong to a live process group the
daemon started for a tool. Credentials that can no longer be read because the
peer left before it was judged refuse. Nothing a request sends counts toward the
verdict.

**What needs it.** A request that claims `platform:"cli"` (an omitted platform
is that claim) over TCP or from a refused peer is answered 403, with no
fallback: `/v1/message` (including `--add-dir`), `/v1/dispatch`, approval
responses, run steering, every stream and query that resolves a person,
workspace registration, trust, capability, observation-profile and
effect-profile grants, model change and probe, the provider catalogue probe,
schedule adoption, and account binding. Durable IM ingress cannot claim `cli`
either. IM, webhook, and HTTP integrations keep their own authentication over
TCP and act as the non-CLI identity they name; a configured `gateway.token`
authenticates them and never grants CLI identity. A CLI pointed at a remote
gateway therefore cannot act as its person there. "On this host" is one
predicate shared by the CLI and the runtime: `localhost`, a loopback address,
and a wildcard address such as `:8765`, `0.0.0.0:8765` or `[::]:8765`, which a
daemon binds on every interface, loopback included. The refusal tells a person
whose own CLI got it to check `gateway.url` and `SELF_GATEWAY_URL`, and to run
`selfmind` from their own terminal; it does not say where the socket is, because
it goes to a caller that has not proved it may know. The CLI derives the path
from its own data directory and appends it to the error it shows. The daemon
warns about a refused socket peer's CLI claim and about any refused attempt to
administer it (a CLI claim over TCP is refused without a warning) once per peer
and kind of refusal per minute (a TCP peer by address, a socket peer by
process), counts what it held back, keeps at most 256 peers, and writes at most
20 such warnings a minute across all peers: what that ceiling holds back is one
summary line with `suppressed=<count>`, written with the first refusal after the
minute ends. A process that varies its address, or many processes, cannot fill
`gateway.log`. A status
read by a caller the daemon did not accept is marked `liveness_only`, and
`selfmind gateway status` says so and says where the rest is.

**Administering the daemon.** `POST /v1/gateway/shutdown` stops the daemon and
`GET /v1/gateway/status` describes it: run summaries and identities, the data,
runtime, and configuration paths and the socket's location, environment
fingerprints, tool and MCP diagnostics. With no `gateway.token` set, the TCP
listener answers every local process, so these two answer only a caller that
proves more than reaching a port: the verified local CLI on the socket, or the
holder of a configured gateway token (a remote operator). A stop request from
anyone else is refused `403` with a message that names the socket and
`gateway.token`; a status request is answered with liveness only (state,
draining, how many runs are active, build identity), which is all restart and
doctor poll for. `/health` stays open to every caller. The model-change restart helper the daemon starts is
a descendant of the daemon and so never the verified CLI; it proves itself with a
random capability that exists only in the daemon's memory and the helper's
environment (`SELF_GATEWAY_RESTART_CAPABILITY`, never its arguments), good for
stopping the daemon for that model change and for nothing else.

**Host header.** A page that points its own domain at `127.0.0.1` (DNS rebinding)
is same-origin to the browser and sends no `Origin`, but its `Host` is still its
own name. When the daemon listens on loopback only, every TCP request except
`/health` must name `localhost`, `127.0.0.1`, or `[::1]`, on the daemon's port or
none; the refusal names the address the daemon expects. A request without a
`Host` (HTTP/1.0) cannot be a browser and is accepted, and a socket request is
not judged by its `Host`. A daemon bound beyond loopback skips the check, since
the names its clients use are not known; its `gateway.token` is the boundary
there. Two setups that used to work need a change: a tunnel that maps the daemon
to a different local port must keep the daemon's port, and a reverse proxy on
the same host that forwards the public name as `Host` (`proxy_set_header Host
$host`) must forward the daemon's own address instead
(`proxy_set_header Host 127.0.0.1:8765`) or have the daemon bind beyond
loopback with a `gateway.token`. An allowed-hosts setting is not provided.

**Browser requests.** A web page can reach a loopback listener, and the daemon
has no browser client. Every route except `/health` refuses a request carrying an
`Origin` or a `Sec-Fetch-Site` header (a browser adds the latter to every
request, including a same-origin GET after DNS rebinding that carries no
`Origin`; Node's `fetch` sends only `Sec-Fetch-Mode`, so HTTP integrations written
with it are unaffected), and a state-changing request must declare
`Content-Type: application/json`, which a cross-origin page cannot send without a
CORS preflight the daemon never answers. IM webhook bodies keep the type their
platform sends; the header check still applies to them.

**Process-tree residuals.** Refusing the daemon's descendants is only as strong
as the daemon's knowledge of them.

- Linux: the daemon is a child subreaper (`PR_SET_CHILD_SUBREAPER`), so a
  double-forked or `setsid` tool is reparented to the daemon instead of to init
  and stays a descendant; it reaps adopted orphans that outlive their parents.
  This path compiles for Linux and its pieces are unit-tested, but it was not
  run on a Linux host in this change.
- macOS has no subreaper. The daemon records the process group of every tool
  command it starts (foreground, background, isolated or host) and refuses a peer
  in a live recorded group, which catches an ordinary detached child. A host-mode
  command that double-forks **and** calls `setsid` before connecting leaves both
  the tree and the group; against a scratch daemon on macOS that peer was
  accepted as the person's CLI.
- Host mode is uncontained by definition. On both platforms an approved host
  command can also ask a service manager outside the daemon's tree (launchd,
  cron, `systemd-run --user`) to start a process, which then looks like your own
  terminal. Sandboxed execution, the default, is not exposed to any of this: it
  cannot reach the socket or the listener on macOS at all, and cannot reach the
  socket on Linux (below). Approving a host command approves your user authority.

### Daemon private state is unreachable from a confined tool

A tool runs as the same OS user as the daemon, so file modes alone do not keep it
away from the daemon's records, credentials, or control listeners. The daemon
derives what is private from the data-directory layout and its own listeners and
installs it for the process:

- the runtime directory (state, lock, pid, log, socket) and the backups
  directory, and the socket's own directory when it is not inside the runtime
  directory (the `/tmp/selfmind-<uid>` fallback, shared by every daemon of the
  person, so a confined command reaches none of their sockets either);
- the control store and its `-wal`, `-shm`, and `-journal` sidecars;
- the configuration file the daemon loaded (`--config`, `SELF_CONFIG`, or the
  default), which holds every provider key and chat token, together with the
  copies written beside it: `config.yaml.tmp` while it is saved and
  `config.yaml.bak-<time>` after an edit, including ones that appear after the
  daemon started;
- the credentials file (`auth.credentials_file`) and every file beside it that
  begins with its name (`auth.json.<random>.tmp` from the OAuth refresh,
  `auth.json.lock`), the credential store's `.auth-*` temporary files, and the
  chat accounts' credentials (`~/.selfmind/weixin/accounts`);
- the control socket, and the daemon's TCP port (a daemon bound beyond loopback
  is denied on every address of that port).

Every isolated backend applies it after the command's own writable roots and
network mode, so a trusted workspace that contains the data directory (a home
directory), or a shared network, cannot reopen it. A policy entry that is not an
absolute, clean path refuses the whole policy instead of being dropped, which
degrades to approval-gated host execution.

**The names above the state.** A private name guards the state only while it
keeps naming it. A command that may write the data directory's parent could
otherwise move the directory aside (`mv ~/.selfmind ~/.x`, a rename swap, or a
rename plus a symlink at the old name) and read or connect to everything
through a name no rule mentions, or leave the daemon with nothing at the paths
it uses. So every directory above private state that lies strictly inside one of
the command's writable roots is pinned against being renamed, swapped, or
removed, and a private socket against being renamed or removed; a writable root
is already pinned, and nothing above a root is writable. Reads and writes of
everything else in those directories are unaffected: the rest of the data
directory and the SelfMind home stay as writable as the root they lie in. A hard
link to a private path is refused by the sandbox itself for any path that has a
denial on it. What is not private stays reachable by design: the per-person
memory databases, `cron.db`, attachments, tool output, prompts, and skills are
user content a trusted home-directory workspace can read and write.

**Symlinks on the way.** A person who keeps `~/.selfmind` in a dotfiles
repository, or `config.yaml` as a link into one, reaches state through a link,
while every private name is the real path it leads to. Replacing the link would
send the daemon, the CLI, and the next configuration load to a tree the command
made, with every private name still guarding the old one. The daemon therefore
also names every symlink met on the way to the paths it and the CLI use (the
data directory, the runtime directory and socket, the configuration, the
credentials file, the chat accounts). It walks each path as the operating system
does: a link's target is read relative to the link's own directory and the links
met on the way through it are named too, so a link to a link, a link into a
synced folder (Dropbox, stow) that is itself a link, and a relative chain are
all covered, up to 40 links so that a cycle ends. Each is named as the link's
own name in its real directory, and the sandbox denies renaming, swapping, or
removing it, and pins the directory it stands in. If you move state behind a
link while the daemon runs, restart it; until then the new location is not
private. The names are taken at start, so they still point at the old locations,
and a confined command can read `control.db` and `config.yaml` through the new
link and by their new real path.
The configuration's and credentials file's temporary files and copies are
created beside the link, not beside its target, so both are private prefixes.
Seatbelt enforces this and it is tested against each of: a symlinked data
directory, a symlinked configuration file, a symlinked ancestor, a link to a
link, a chain of configuration links, a link into a synced folder that is
itself a link, and a relative chain. Bubblewrap
cannot: a link is a directory entry, not a mount point, so a confined command
with a home-wide root can replace the link. The directory it leads to stays
pinned and hidden, so the command cannot read through the link or move the
state, but it can make the daemon's next start, the CLI's next dial, and the
next configuration load follow its own tree. Resolving the data directory to
its real path at start would not help the CLI, which resolves the name itself on
every run, and would change the identity paths the service files and workspace
records hold. On Linux either keep `~/.selfmind` and the configuration real
files and directories, or keep the home directory out of the writable roots.

**Writers beside private files.** The stores that rewrite a secret in place used
to write `<file>.tmp` and rename it. A confined command with a home-wide root
could plant a symlink at that predictable name, and the daemon wrote the secret
through it: the OAuth refresh put a refresh token in the command's file and left
`auth.json` pointing at it. The private prefixes now stop a confined command
from creating any such name, and the writers no longer rely on that: they
create their temporary file exclusively (`O_EXCL`, a random name beginning with
the destination's, mode `0600`) and rename it over the destination
(`internal/platform/atomicfile`), so a planted name is never written through.
Changed: the OAuth auth store (`writeAuthStore`, the MiniMax and Codex
refreshes), the saved configuration (`config.SaveConfig` and `selfmind config`
rewrites), the chat accounts' credentials and context tokens, and the
timestamped configuration backup (created `O_EXCL`, the next free name on a
collision). Also changed, although they hold no secret, because a planted name
let a command poison what the agent reads: the model catalogue cache
(`model_cache.json`), archived skills (`<skill>.md`), and the skill usage
sidecar. The credential store, the auth manager's refresh write, the account
sync cursor, the model change state, and the onboarding state already used
`os.CreateTemp`. Not changed: the learning audit, crash markers, feedback
reports, and the gateway state file (inside the private runtime directory),
none of which holds a secret or steers the agent; they still write a fixed
`.tmp` name and are a follow-up.

- macOS (Seatbelt) appends `deny` rules after every `allow`: all file access to
  a private directory, file, or file prefix, connecting to a private socket or to
  any socket inside a private directory, connecting to the daemon's port (by
  address and by `localhost`), and unlinking, which is what rename, swap, and
  rename-over check, on every pinned directory and on a private socket. A
  confined `cat` of `control.db`, `gateway.pid`, `gateway.log`, the configuration
  and its backups, the credentials file, and a chat account's credentials, a
  listing of or write into the runtime directory, a `curl` to the daemon's port
  and to its socket, a `mv` or rename swap of the data directory or any directory
  above it, and a hard link to a private file all fail, while a workspace read,
  any other file in the same directories, and a connection to an unrelated
  loopback port still work. This is covered by real `sandbox-exec` tests and by
  a scratch daemon driven through its real tool engine, in both socket layouts.
  The hard-link refusal is the sandbox's own behavior, observed on macOS 27 and
  pinned by a test, not a rule SelfMind writes.
- Linux (bubblewrap) masks each private directory with a `tmpfs`, each private
  file by binding `/dev/null` over it, and each private socket's directory the
  same way, and each file that exists beside a private prefix when the command
  starts. A mount point cannot be renamed, swapped, or removed, so each pinned
  directory is an `rw` bind of itself (`--bind-try`) placed after the writable
  roots and before the overlays and masks: it changes no access inside a root
  and is never bound for a directory outside every root, under `/tmp`, or inside
  a synthesized or overlaid path. A path under `/tmp` is never masked, because
  `/tmp` is the run's own scratch there, and a path that does not exist is
  dropped, because bubblewrap cannot create a mount point under the read-only
  root; a copy of the configuration written after a command started is therefore
  masked from the next command on. Residual: with a shared network
  (`exec_sandbox.allow_network`, the default) bubblewrap keeps the host network
  namespace, so a confined command can still reach the daemon's TCP listener. It
  cannot claim CLI identity there (TCP never carries it), reach the socket
  (masked), stop the daemon, or read its status beyond liveness while
  `gateway.token` is unset, but it can still act as any non-CLI identity it can
  name, which is not harmless: see "Known Gaps". Set `gateway.token`, or
  `exec_sandbox.allow_network: false`. These argv rules are unit-tested; the
  masks and mount points were not run under a real bubblewrap in this change.
- Private paths are symlink-resolved before they enter a policy, because
  Seatbelt matches the real path.
- In-place credential-cache writes (macOS; the declared `host_state_writes`,
  see "Tool environment profiles") compose with all of this. The policy lists
  its writable roots and the in-place subpaths with their own protections (the
  canary, the read-only runtime records) first and the private denials last, so
  the cache allowance never reopens a private name and the private rules never
  break the cache write. An in-place subpath is a writable subpath like a root,
  so a directory or link between it and private state is pinned as one inside a
  root is; only flat caches are declared in practice, which hold neither, so
  that is depth rather than a reachable case. The runtime's records
  (`<runtime>/host-state`) sit beside the data directory, not inside a private
  directory: they stay read-only to a confined command, not hidden. Covered
  under real `sandbox-exec` with the whole policy a call carries: the cache write
  works while the data directory, its ancestors and links, the configuration, its
  copies and the credentials' temporary names stay unreachable, each against a
  control without the rule under test, and reordering the rules or dropping the
  in-place subpaths from the pins fails them.

### Workspace trust and capabilities

Workspace trust is enforced as a durable owner-controlled boundary:

- only an authenticated local CLI may grant or revoke workspace trust; IM,
  cron, and remote HTTP cannot. Use `selfmind ws trust [id]` and
  `selfmind ws untrust [id]`;
- first-use setup is one such explicit local trust act: it shows the canonical
  project path and trust consequence before calling the local-control-only
  registration endpoint. The filesystem root and user home directory are never
  accepted as implicit defaults. Declining or cancelling setup grants nothing;
- workspaces created by older versions are migrated to untrusted with
  `migration_review_required`; newly discovered or remote workspaces also
  start untrusted. `selfmind doctor` lists migrated paths that need review;
- untrusted workspaces default to isolated filesystem access and no network,
  but `network:shared` is an approvable, workspace-scoped, time-bounded
  capability rather than forcing users to mark the whole workspace trusted.
  A task grant lasts one hour and a person grant lasts eight hours. Marking a
  workspace untrusted revokes its active execution capabilities;
- network is a permission, never a reason. Workspace trust under an operator
  policy that allows egress (`exec_sandbox.allow_network`), a stored or run
  `network:shared` grant, and a one-time approval each PERMIT the daemon's
  network; none of them hands it to every command. A call receives it only
  when it also needs it: its command reaches a remote service by name, or the
  model set `network: true`. One predicate answers the first question for a
  trusted workspace and a grant alike (`commandPlausiblyNeedsEgress`): the
  explicit egress programs, the fetch, install, publish and remote verbs of
  package managers, VCS remotes, infrastructure and cluster tools, and the
  credential-bearing service CLIs. The verbs are a table
  (`internal/tools/network_commands.go`), read through the shared command
  identification, so assignments, wrappers, paths, case and compound commands
  read as the shell reads them and a compound command qualifies when any
  command in it does. Building, testing, linting, formatting and inspecting
  (`go test`, `cargo build`, `npm test`, `mvn test`, `make`, `git status`,
  `rg`) are not remote services by name: they run offline, contained, and
  claim no external effect target. So is installing the module in front of
  you (`go install ./cmd/tool`, `cargo install --path .`), which is a build;
  `go install pkg@version` and `cargo install ripgrep` fetch a remote package.
  `exec_sandbox.allow_network: false` still wins over trust: trust then
  permits nothing, and only a grant or an approval a person gave opens the
  network, as before. An untrusted workspace needs a grant or an approval under
  either policy setting;
- the same table holds the clients whose usual invocation is a remote service:
  `npx`, `bunx`, `uvx`, `uv run`, `bun install`, `pipx`, `conda`, `rustup`,
  `dotnet restore`, `corepack`, `npm init <initializer>`, `pnpm create`,
  `yarn create`, `psql -h`, `ping`, `dig` and `nslookup`. Their local halves
  stay local (`npm init -y`, `psql` on a socket, `uv run --no-sync`);
- an explicit offline or local form is not a remote service. The program's own
  strictly-offline switch (`npm install --offline`, `pip install --no-index`,
  `cargo fetch --offline`), a transfer whose remote is a path on this machine
  (`git clone ../repo`, `git fetch /srv/mirror.git`, a `file://` URL), a request
  for help or version (`kubectl --help`, `curl --version`), and
  `cargo install --list` are read as local. `--prefer-offline` is not one: it
  still goes to the network on a cache miss. A transfer is local only when
  nothing widens it past its first operand (`git clone --recurse-submodules`
  reaches the submodules' remotes, `git fetch --multiple` takes every operand as
  a remote). A switch given a false value (`--offline=false`, `--offline No`) is
  not the switch. `--version` is a version request only before the program has
  been given anything to do: after a subcommand or an operand it is an option
  with a value (`cargo install ripgrep --version 13.0.0`, `helm pull
  repo/chart --version 1.2.3`) and the command stays remote; `--help` keeps its
  reach past a subcommand. A runner hands every option after the program it
  runs to that program (`npx pkg --offline`, `docker run alpine --help`,
  `pnpm dlx tool --offline`). The veto belongs to one invocation, so it never
  hides another segment of a compound command, and it never applies when the
  invocation names a remote target (a URL or `user@host:path`) or the call set
  `network: true`. It lives in the network decision only: the egress
  classifier of the approval funnel still reads `curl --version` as an egress
  program;
- `terminal`, `verify` and `execute_code` take an optional boolean `network`:
  "this command (or script) needs network access" (a dependency download on a
  cold cache, a remote service its name does not show, Python that calls an
  API). It asks and never grants. With a permission the call is networked;
  without one, the one-time `network:shared` ask runs exactly as for a
  clearly-network command, before the command runs. A call whose hard-floor
  verdict is a block is not asked about network (or credentials) first. No
  other tool reads the parameter, and underscore arguments a model supplies
  stay stripped. A call whose network is shared only because it asked, while its
  command says it is local (`ls`, `git status`), is not released by the
  observation-only rule: the model's request is not a proof that the network
  belongs to the observation;
- a call that receives the daemon's network is a `network` operation for a
  prohibition. "Do not use the network" forces a human on `npm install`,
  `git push` and any call with `network: true`, as it does on `curl`, whatever
  made the network shared; a local command is not touched by it;
- a command that ran offline and failed with a network-class error is handled
  by what permits it. Where a permission exists the runtime does not ask again:
  it returns the typed failure `network_withheld_by_policy`, keeps the
  command's output, its classification and its effect certainty (no
  `effect_state` is invented, so the retry stays possible), says the command was
  not replayed, says that retrying with `network: true` runs it networked, and
  warns that a command that changes state before it reaches the network has
  already run those steps and repeats them on a retry, so its state should be
  inspected first. Where nothing permits network, the earlier flow stays: the
  capability is asked once after the failure, the middleware never replays the
  command, and the answer tells the model to retry with `network: true` with the
  same warning. Only the command's own failure takes either path: a rejection,
  a safety-triage denial whose rationale mentions the sandbox network, a
  parked approval, a hard-floor block or a call that was not dispatched is an
  outcome with its own meaning and is returned as it is;
- every execution call records its network decision in its process facts and
  its `tool.sandbox` event: `shared`; `withheld: not network-bound by policy`
  (permitted, not needed); `withheld: policy disabled` (the policy closes the
  trusted path and nothing else opens it); `withheld: no grant`. It records the
  permission that applied beside it (`trust`, `grant` or `approval`). A
  verification check that passed offline although a network GRANT permits it
  (trust is a standing default, not something a person granted), and whose
  command can fetch dependencies when it runs, is stated in the `verify` result
  the model reads, in the Run's verification summary and in the person's
  closing notice, so a pass cannot silently claim coverage of anything that
  needs a remote service. What can fetch is a table of named programs
  (`go build|test|run|mod`, `cargo build|test|check|run|clippy|doc`,
  `npm test|run|ci`, `yarn`, `pnpm`, `bun`, `pip`, `poetry`, `uv`, `mvn`,
  `gradle`, `sbt`, `bundle install`, `composer`, `dotnet build|test|run|restore`,
  `bazel build|test|fetch`, `deno run|test|cache|install`, `mix deps.get|test|compile`,
  `swift build|test|package`, `flutter pub get|test|build`, `tox`, `nox`,
  `conda`, `mamba`, `micromamba`); a `go vet`, a linter, a formatter, a package
  script named lint, format or typecheck, `bundle exec`, `pytest` or `make`
  adds no sentence. A check that needs its dependencies sets `network: true`;
- operator credential state is reached through `credential:read`, which is
  approvable, workspace-scoped and time-bounded on the same terms as
  `network:shared`. It is asked for BEFORE execution and only when the command's
  program set matches a profile declaring operator credentials, so a trusted
  workspace and a command that touches no credential CLI are never interrupted.
  Declining leaves the command runnable with an empty state overlay;
- access is never granted to all of `~/.config`. A profile names the files and
  subdirectories it needs, bounded by `copy_in` limits, and the operator's own
  files are never modified;
- once credentials are in scope, every program in the payload must be unable to
  emit one, because that is how a credential leaves. The per-program flag is too
  coarse for a filter, so the invocation is judged instead: a catalogued filter
  invoked with no file operand can only have read the previous command's stdout,
  which is why `… | grep tag` is released while `grep -r secret ~/.aws`,
  `head ~/.aws/credentials` and `grep -f patterns.txt` are not. The argument
  grammar is per program and fails closed on any flag, bundle or operand it does
  not fully model, so it can only ever widen. `rg` is excluded because with no
  path operand it walks the working directory, and `set`/`export` are excluded
  because with no operands they print the whole variable environment.

Private package installation in an untrusted workspace requires a shared-network
capability, and credential-backed registries additionally require
`credential:read`. Do not bypass either boundary with a vendor-specific
exception.

Trusted workspaces may register a local observation script with
`selfmind ws observe`. This is deliberately narrower than trusting an
interpreter or shell command family: the durable approval key binds the
workspace ID, canonical in-workspace path, script content hash, argument
prefix/trailing policy, network mode, and credential mode. Symlinks resolving
outside the workspace are rejected, changed scripts stop matching, and only an
your own CLI on the local control socket can create the grant. A call redeems the profile
only when the shell reaches the registered file: the kernel follows a symlink
before it applies a later `..`, so `link/../scripts/inspect.sh` reaches the
script beside the link's target, and that call is approved like any other
command. A profile cannot be registered through such a path. The runtime still
uses the normal execution scope, sandbox, environment lease, network,
credential, and secret-redaction layers. Treat `--all-args` as an explicit
owner assertion that the unchanged script cannot turn arbitrary arguments into
mutation.

## Dispatch and Delegation

- Native tool calls preserve their provider call id through the result. The
  text tool protocol is a compatibility fallback only.
- Compatibility normalization, scalar coercion, and recursive schema validation
  finish before recovery policy, durable dispatch claim, or `tool.started`.
  Repeating the same malformed call is blocked until its arguments or the
  tool's catalogue generation changes. A prepared call retains the exact Tool
  and middleware snapshot it validated, so a concurrent catalogue refresh
  affects the next call rather than invalidating an already claimed one.
- Run-lifecycle, steering, and human-wait controls use closed recursive schemas. Unknown fields fail
  before dispatch with their exact argument path; they are never silently
  dropped and therefore cannot turn a malformed control request into a
  different valid action. Open compatibility and external schemas preserve
  undeclared fields unless their own schema closes the object.
- Clearly read-only calls may run in parallel. Terminal execution, writes,
  patches, process control, memory or skill mutation, delegation, and unknown
  tools run sequentially by default.
- At a multi-Run capacity (the default is two), a tool that can affect an external system
  reserves its targets in `control.db` after approval and before dispatch.
  Trusted built-in adapters may prove exact target identities; without that
  proof the claim uses a person-wide unknown target, and external tool
  metadata cannot narrow it; only an operator's or the person's explicit
  read-only marking of the exact MCP tool removes the claim (see External MCP
  tools). A conflicting call is never dispatched. Target sets are acquired
  and released atomically. A terminal call can affect an external system only
  when it runs with the daemon's network, operator credentials or the host. A
  command that ran offline (a failing `go test`, a build) claims nothing and
  leaves nothing uncertain for another Run to wait on or be asked about; a
  command that ran with network (`npm install`, `git push`, anything with
  `network: true`) claims like any networked call.
- The claim follows the call's outcome. A foreground command that exits 0, or
  a synchronous tool that returns without error, settles its claim at return
  with an observation ref naming the call (`tool:completed:<call>`). An adapter
  that declares asynchronous completion, or an effect profile that names an
  observation command, keeps the claim for that observation. A background
  start never settles at start: the daemon settles it when it observes exit 0
  (`process:exited:0:...`) and leaves it uncertain on any other exit. A
  non-zero exit, timeout, cancellation, kill, external tool error, or a crash
  after dispatch leaves the claim uncertain; finishing or restarting a Run
  never releases it.
- In flight and left uncertain are distinct, durable states. Before the tool
  body starts, an internal `external_effect.dispatched` Run event records a
  bounded, redacted summary of the call; a return without proof of success
  records `external_effect.returned` with how it ended. Both are ordered by
  the shared effect sequence of claims, watchers, and events. A dispatched
  foreground call, or a reservation about to dispatch, is in flight while no
  return is recorded and its owner Run is running.
  Boot recovery marks a crashed daemon's running Runs interrupted, so their
  unreturned calls are left uncertain. A Run's own foreground call is never in
  flight to that Run, which is calling again. The disposition is derived on
  read and never stored.
- A background start is in flight until its exit is recorded for as long as
  the daemon process that started it lives, even after its owner Run stops
  running: only that process observes the exit, so no observation or
  settlement can release the claim first, and a long-lived process holds its
  claimed targets until it exits or is stopped: by its own Run's `process`
  tool while that Run runs, at its time limit (2 hours by default), or by a
  restart. `/effects settle` refuses it with exactly that. A later daemon process cannot
  observe that exit; boot recovery records the start as returned with an
  unknown exit, once, which leaves the claim uncertain.
- A lost return record fails closed. When `external_effect.returned` cannot
  be written (the failure is logged), a foreground call stays in flight for as
  long as its owner Run stays `running`: other Runs wait the grace and then
  park, another Run's bound observation is refused, and `/effects settle` is
  refused. It is left uncertain once the owner Run stops running, by finishing
  or by boot recovery. A background start whose exit record is lost stays in
  flight until the daemon that started it stops. The owner's own chain is not
  held by a lost foreground record: it is calling again, so it treats the
  call as returned and must observe the result read-only before another
  conflicting effect.
- The Run's own exact chain (the Run and the Runs it continues) is refused
  before its next conflicting effect while its claim is unresolved. The typed
  `external_effect_unresolved` refusal names the claim, the redacted command
  summary, its exit status, and the two release paths; nothing waits on
  itself. (a) A successful read-only observation that the same chain makes
  after the claim and binds with `effect_claim_id`: a proven read-only
  `terminal` or `verify` command or owner-approved observation script, or `watch_external`,
  which releases at once when its first check succeeds and otherwise when the
  watcher's success is finalized. The model judges whether the observation
  shows the result; the runtime checks the person, the chain, that the
  observation started after the claim and the call's return by sequence, the
  read-only proof, that the command reads some state, and success (a
  terminal check's process facts must show exit status 0, not merely a call
  without an error; a watcher's verdict must be success), and records an
  observation ref naming the observing call or watcher. A command
  whose every program reads nothing beyond its own literal arguments, the
  environment, or the clock (`true`, `:`, `cd .`, `echo`, `printf`, `sleep`,
  `date`, `pwd`, `test` without a file test, where a lone operator such as
  `test -f` is a string test, `jq -n` or a jq reading only the environment, a
  filter of its standard input or of `/dev/null`, or a client-only command
  such as `helm version` or `kubectl version --client`, alone or piped
  together) is refused before it runs as `effect_observation_reads_no_state`;
  the rule follows from identifying each program and reading its arguments as
  the program does, not from a list of command strings. (b) The person's `/effects settle`. The failing
  call's own result already names the claim and both paths. The chain's own
  background process that is still running is refused as
  `external_effect_in_flight` until its exit is observed.
- `verify` exposes `effect_claim_id` alongside native `step_id`: one proven
  read-only check may observe an effect and establish a milestone. Normal
  observation proof, process success, sequence and identity checks apply.
  Typed approval records retain conflicting claim IDs. Selected context names
  bounded blockers; final results expose unresolved claims of the exact lineage
  or its typed approval/observation references. Unrelated same-person effects
  are not reported as that Run's result. Model prose and approval of a different
  action do not release them; only existing observation/settlement paths do.
- Another Run's in-flight claim, or any claim a bound watcher can still
  release (the Run's own chain's included), is waited for in place for up to
  30 seconds (`ExternalEffectInFlightGrace`).
  The wait ends with the Run's cancellation and dispatches as soon as the claim
  settles; past the grace the Run parks as `external_resource_wait` exactly as
  before. Lock order: the wait runs after approval and before the tool body,
  holds no `control.db` transaction, and precedes any exclusive execution
  lane; the waiter holds only its worker and admitted roots, which the owner
  does not need to return. A park on the Run's own watched effect says so: its
  pause and status name the claim and its watcher, and if the watcher ends
  without releasing it, `/resume` first observes that effect read-only. Only
  another Run's left-uncertain claim is ever passed by asking. The gate waits
  before it asks: when another Run's left-uncertain claim also holds the
  target, the Run first waits for the claims in flight or watched, and its
  pause and status name that other claim as the decision left for after the
  wait instead of promising an automatic resume. The parked Run stays waiting
  until nothing is left to wait for, then becomes blocked as below.
- Another Run's left-uncertain claim turns this Run's conflicting call into one
  human approval of that exact call, naming the claim (run, redacted command
  summary, state). Approval passes only this call, stores no grant, and leaves
  the other claim unresolved; a rejection is final for the call. When the mode
  already asks about the call, the decision joins that ask. A claim known by
  the precheck skips the judge and reusable grants: neither can remove this
  mandatory one-time human decision. The hard safety floor and current
  restrictions still apply; an exact resumed decision covers only the same
  action and conflicting claims. A claim appearing after the precheck is
  still checked atomically before dispatch. Instead, a
  successful read-only observation this Run made after that effect's call
  returned and bound to its claim, under the same checks as above, lets the
  next conflicting call pass once without asking; it never settles the other
  Run's claim. A parked approval that is answered later
  authorizes the regenerated call only against the same claims. In full-auto
  and smart mode this is the only parallel-specific human ask for external
  effects: shared-network and credential-bearing commands are approved as in a
  single Run, so smart still asks a person when its judge escalates, fails, or
  times out.
- A parked Run that is left with a decision (its own chain's unobserved claim,
  or, once nothing is waited for, another Run's left-uncertain claim) becomes
  blocked through the existing finalization transaction, with a resumable
  outcome and notice that name the claims. Once every conflicting claim is released (settlement,
  bound observation, or a successful return), the resource wait pass queues
  its exact continuation once; the wakeup never replays the refused call.
  Corrections written before this contract carry no wake marker and stay with
  the person's `/resume`. The resource-wait projection is shared by admission,
  wakeup, `/status`, `/diag`, and the daily report; historical blocked waits
  cannot starve active wakeups.
- Before a judge or human approval, a read-only projection refuses the Run's
  own unresolved effects, because approval cannot establish their result, and
  names another Run's left-uncertain claims so an ask the mode makes anyway
  can include them. This also precedes network and credential capability
  asks, projecting the requested route from existing tool profiles without
  granting it. The precheck reserves nothing; the post-approval claim decides
  again atomically.
- `/effects settle <claim_id> [reason]` releases the effect group on the
  person's word with an audit row naming the actor account, channel, and
  reason; it refuses a call that is still in flight. `/effects resolve
  <claim_id> <watch_id>` relates a finalized watcher nobody bound: the
  watcher must come from the claim's Run, or an exact same-person continuation
  with an explicit v3 binding, and follow the claim by sequence (strictly later
  timestamps only when both rows predate the sequence). Historical receipts
  remain same-Run only. The observer uses its own approved environment
  binding, not the old effect's credentials or permissions.
- `/effects` shows each hold's creation time and explains that an unknown
  target conflicts with all of the person's external targets. Missing legacy
  lifecycle records remain an evidence gap, not proof of non-dispatch or failure.
  Age and approval of a different action do not release the hold. Resolved tool
  ledger entries tied to an unresolved effect remain past normal retention;
  once observed, they become eligible for normal pruning again.
- Parallel Runs use the same approval modes and capability boundaries as one
  Run. Optional isolation that cannot enforce a call is presented as host
  execution before approval; explicit isolation requirements still refuse it.
  Host commands hold the person's exclusive execution lane, excluding other
  terminal and file mutations; isolated commands and file edits share the lane.
  Waiting at an uninvoked call can checkpoint and park, retaining its admitted
  writable roots. A checkpoint failure retains a cancellable worker. Smart
  host calls receive fresh cheap-role review and create no reusable decision;
  full-auto retains the floor, while on-request keeps its existing human/grant
  contract. A proven observation can still inspect an occupied target. A Run
  keeps its frozen network policy, including inside a managed Git view; an
  untrusted view must first obtain the normal network capability.
  Typed provider target adapters and real restart/transport acceptance remain
  open. Default capacity is two; explicit capacity one retains serialized
  admission and takes no claims.

An authenticated local owner can register one exact script invocation with
`selfmind ws effect`. The revocable grant binds a trusted workspace, script
path and content hash, complete literal argv, network and credential modes,
and every asserted external target. It also binds the rest of the invocation.
The script is bound twice: as the file it resolves to, by content, and as the
path the call names, resolved against the call's directory without following
symlinks. That second path is what the script sees as `$0`, so a symlink alias
of the registered script is another invocation. The file is the one the shell
reaches: the kernel follows a symlink before it applies a later `..`, so
`link/../scripts/deploy.sh` reaches the script beside the link's target. A
call, input file or observation command that reaches another file this way is
another invocation, and a profile cannot be registered through such a path.
`--cwd` names the workspace
directory the call runs from; the default is the workspace root. The directory
is matched both as written, which the script sees as `$PWD`, and by its
resolved physical path, so a symlinked directory is another invocation too.
`--env NAME=VALUE` declares each literal assignment written before the script.
The one declared standard input is either `--stdin TEXT`, passed as a
here-string, or `--stdin-file PATH`, a workspace file redirected with `<` and
bound by its resolved path and content hash. Declared values are stored only as
hashes. At dispatch the runtime rechecks the profile and standing-grant cutoff;
the executable script must be invoked
directly rather than through a selectable interpreter. One matching profile narrows only the
target claim. The same script from another directory, with different or extra
arguments, or with other assignments or input is a different invocation.
Missing, stale, ambiguous, or changed profiles fall back to the person-wide
unknown target. A profile registered before invocations were bound means the
workspace root, the script's physical path, and no declared inputs. The
profile is an operator assertion about that script's effects, not a
model-supplied target, tool approval, or completion proof. A profile without
an observation command describes a foreground script, whose exit 0 settles its
claim like any command; naming one keeps the claim held for that observation.

An effect profile may additionally name one exact observation script command
with `--observe-command`. The command runs from the profile's directory and
declares no assignments or input of its own; a watcher started elsewhere,
through a symlink alias of the script, or with other arguments does not bind
the claim. The owner also registers that unchanged script with `ws observe`,
which is the separate read-only authority. When a `status_json.v1` watcher
from the same Run or a later Run of its exact chain binds the one
already-dispatched claim with exactly the asserted targets, the durable
successful watcher event can settle that claim automatically. Registration
freezes the script digest;
each poll and finalization recheck it, and the release transaction rechecks
the owner's still-active effect grant, exact effect identity and target set.
A changed or revoked script, ambiguous claim, failed check, or ordinary
regex watcher keeps the claim occupied for explicit review. This is an owner
assertion about the script's meaning, not model-provided proof of the target.

Managed Git views are writable by the Run, including their local `.git`
directory, while daemon-side Git runs on the host outside every sandbox.
View-writable state must never choose what that Git executes:

- Every daemon-side command on a view pins `GIT_DIR`, `GIT_COMMON_DIR`, and
  the work tree to the view, disables replace objects, and overrides
  `core.fsmonitor`, `core.hooksPath`, and `core.untrackedCache` on the command
  line. Every daemon-side Git command, on a view or the source, also leaves
  commit-graphs, multi-pack indexes, and reachability bitmaps unread, as
  defense in depth: no decision that could lose work relies on Git reading
  them, since each either hashes the commits and trees it relies on or reads
  only the view's own object directory, as described below. Only a view's
  validity check (`merge-base`), whose wrong pass makes the verified walk list
  more, and an informational count would read them otherwise.
  A view whose `.git` is not a plain directory, contains `commondir`,
  `gitdir`, `config.worktree`, `info/grafts`, or `shallow`, or binds any object
  store other than its admitted source is unavailable; its files are kept.
- The view's `.git/config` may hold only the passive keys installed at
  creation. Includes and every key that can name a program or redirect Git
  make the view unavailable without deleting its files.
- Reads of the checkout and index (status, ignored and hidden entries) use a
  daemon-written Git directory carrying the validated passive keys, so a
  later edit of `.git/config` cannot define a filter for them. Status never
  descends into submodules, diffs pass `--no-ext-diff --no-textconv`, and
  while a Run may still write the view (its own Run or another running Run
  is bound to it, or an admission into it is in flight) `/views` reports
  committed work only and `/apply` refuses.
- Status compares the index with HEAD's trees as Git reads them, unhashed,
  and a tree the view does not store itself (a gc in the view leaves out what
  the source holds) comes from the source. So an index that status calls
  clean must also record exactly HEAD's tree: `write-tree --missing-ok` on a
  copy of the index, which reads no tree object, must name the tree of HEAD's
  commit as the daemon reads and hashes it. Otherwise the inspection fails, and
  a tree a writer of the source planted cannot hide a staged change from
  archive and prune. Git also settles a symlink whose size still matches, or
  whose index entry carries no stat data (after `read-tree`, or a write in the
  same second as the link), by reading its blob the same way. So the daemon
  reads each symlink's target from the checkout, hashes it as a blob, and
  counts a symlink whose id differs from its index entry as uncommitted work,
  reading no blob from either store. A regular file needs no such check: Git
  hashes the file itself, and the one blob it reads for it, under `text=auto`,
  only decides whether CRLF endings are normalized.

A first direct-writer Run's sandbox binds the real checkout's `.git`
read-write, so a Run can rewrite the source repository's own configuration and
object-binding files, also while daemon-side Git is working there. The
guarantee is narrower than "source files never matter", and stated precisely:
no daemon-side Git command on the source reads a program, hook, or transport
from source configuration, even if that configuration changes after it was
screened. Source commands fall into four kinds:

- Identity probes (`rev-parse --show-toplevel` and `--git-common-dir`) resolve
  the repository the way the person's own Git would, so a changed topology is
  noticed and refused. Baseline freeze admits only a checkout whose common
  directory is its own `.git`, with no `commondir` or `gitdir` file in it.
- Config, object, and index reads (`config`, `cat-file`, `ls-files`), the
  object import of delivery (`index-pack`), and the source-side check of a
  created ref (`rev-parse`) have no hook event. Each runs with `GIT_DIR` and
  `GIT_COMMON_DIR` pinned to the admitted `.git`, all transports blocked (empty
  `GIT_ALLOW_PROTOCOL`), promisor lazy fetch disabled (`GIT_NO_LAZY_FETCH`),
  replace refs ignored, and the hooks path and fsmonitor overridden on the
  command line; system and global configuration are ignored. Each first
  refuses a source `.git` that has gained a `commondir` or `gitdir` file.
- Ref reads and ref creation go through a daemon-written Git directory instead.
  Pinning keeps configuration and objects in the admitted `.git`, but Git
  still resolves refs through a `commondir` file it finds there, so a file
  planted after the check would make a pinned ref read answer from another
  repository, for example that undelivered work was delivered. The
  daemon-written directory has no `commondir`. It links the source's `refs`,
  `packed-refs`, and `reftable`, uses the source object directory, and carries
  daemon configuration that repeats only the source's repository format, read
  the way Git reads it (the repository's own config file, without includes):
  `files` or `reftable` ref storage. Any other ref storage is refused before
  anything is written, and baseline freeze does not admit SHA-256
  repositories yet. None of the source configuration is read there, so
  `update-ref` cannot run a configuration-defined `hook.<name>`
  reference-transaction hook, which Git runs even when `core.hooksPath` is
  `/dev/null`. Ref creation (the `/apply`
  branch and the prune keep-ref) is compare-and-swap against an absent ref; an
  existing ref that names the same commit is an identical earlier creation.
  No reflog is written for a created ref. Success is reported only after the
  ref resolves to the expected commit both through the daemon-written
  directory and through a pinned read of the source with its own
  configuration, so a ref written where the source's Git does not look is a
  refusal, not a delivery.
- The checkout status of baseline freeze reads through a daemon-written Git
  directory carrying only passive comparison keys (file mode, case, line
  endings, stat precision), so a filter or fsmonitor added after the screen
  cannot run.

Baseline freeze follows the same rules: after its two identity probes, the
configuration screen, the submodule check, and the read of comparison keys
(`config`, `ls-files`) are pinned, HEAD is read through the daemon-written ref
directory, and status through daemon-written configuration.

Delivery uses object plumbing, never a transport: a pack of exactly the
objects the daemon's verified walk (below) lists for the delivered HEAD is
built in the pinned, allowlisted view and streamed through a pipe between the
two Git processes into the source object store with `index-pack --strict`, so
the daemon never holds the pack; cancellation or a failure on either side stops
both, and each side reports its own error. A fetch
would consult `url.*.insteadOf`, `core.sshCommand`, `core.gitProxy`,
`credential.*`, `protocol.*`, a promisor remote, and `core.alternateRefsCommand`,
each of which can reach a program. Before baseline freeze, view creation,
delivery, and prune, the source configuration is screened (flattened, so
includes are covered) and refused when it defines a configuration-based hook,
an alternate-refs program, a pack-protocol program
(`uploadpack.*`/`receive.*`/`receivepack.*`), a redirected work tree, or an
executable content driver; keys ordinary in a developer's checkout
(`core.sshCommand`, `credential.*`, `url.*.insteadOf`, `core.gitProxy`,
`protocol.*`) are left alone and made harmless by removing the transport. A
source that borrows objects through an `alternates` or `http-alternates` file is
refused, so a delivered or protected commit cannot resolve against
attacker-controlled objects.

A ref is not delivered work, and neither is an object file: a Run that can
write the source could copy the view's commit and trees there, put an empty
file or another object at each blob's path, and point the branch at the work.
Work counts as delivered, and may be protected for prune, only while the source
holds every object the work's HEAD adds over the baseline with content that
hashes to its id, and the source's own Git still resolves the branch. Each
listed object is read back from the source with `cat-file --batch` through
the daemon-written directory and hashed by the daemon, one buffer reused for
all of them, with replace refs ignored and an object store borrowed through
alternates refused before and after the read. Git does not hash what it reads
and need not read one object from one copy: when a pack (the newest or most
recently used) and a loose file both hold an id, Git 2.54 announces a blob's
size from the pack copy but streams its content from the loose copy, and
reads a commit or tree whole from the pack copy. So every copy of a listed
object must hash: a loose
file beside a packed one, the copy in each pack whose index names the id, read
through an object directory that holds only that pack, and the copy a
multi-pack index resolves when the source has one, since the person's own Git
reads through that index by default. A substitute Git does not read today
would otherwise wait for a repack. `/views`, archive, and prune count a
delivery branch only with that proof.

What counts as the work, and what as already in the baseline, is decided by a
walk the daemon runs itself. Git reads objects without hashing them, so a pack
the Run writes into its own view, with an index that names a commit of the
baseline's history but holds other content, would otherwise make work commits
look like part of that history; Git's own walk cannot be trusted here, whatever
its configuration. The daemon reads each commit and tree of the view or archive
through `cat-file --batch` and accepts it only when it hashes to its id; it
walks commits newest first from the work's HEAD and from the baseline, the way
`rev-list` limits a range, and counts a commit as baseline only when a verified
chain of parents leads to it from the baseline. Ordering affects only how far
the walk reads; it never excludes work. Work commits are read parents first,
and each contributes the trees and blobs it holds at paths where none of its
parents holds the same object; a tree met again is not read again, since
everything under it was listed then or is held by a parent read earlier or by
the baseline. Listing more than `rev-list --objects` would is safe, since the
source must hold those objects anyway; a test compares the walk with Git's own
reachability on generated histories (merges, renames, mode and type changes,
symlinks, gitlinks, reverts, moves back, clock skew) and requires it never to
list less. Every tree-mode entry is read as a tree, and an id a work tree names
both as a directory and as a file is refused. Grafts, shallow files, replace
refs, commit-graphs, and refs other than HEAD play no part, and a view whose
objects do not hash cannot be listed, so archive, `/apply`, and prune refuse
it. A walk holds at most 100000 commits it does not yet know to be the
baseline's, so more work than that fails closed. A commit may name at most
1024 parents, far above any octopus merge Git writes (a parent named twice is
kept once, in a slice of exactly the parents kept), and a tree may lie at most
2048 directories below its commit's root, Git's own default
`core.maxTreeDepth`; the walk reads trees on an explicit stack. It keeps no
commit's text and holds each id once. What it must retain (the commits read,
the ids it listed, named, or introduced) and the trees it is going through are
charged, at the sizes Go allocates them, against 512 MiB, a tree before it is
built; trees it is done with, and their name indexes, stay cached within 64 MiB
and are read again, hash-verified, once dropped. Its own work is charged
against 2^28 steps: 256 for each object it reads plus one per 16 bytes of it (a
tree read again after the cache dropped it is charged again), 4 for each tree
entry it parses, and for each entry it compares with its parents' trees one per
parent, plus one per 64 bytes of the name; an entry already introduced or
listed is not compared again. The walk checks its context as it goes, so a
deadline or cancellation ends it as an interruption. Past any of these bounds
it refuses and names the bound. A blob where the
walk expects a commit or tree, and any object over 64 MiB, is refused at once
and its read abandoned: Git announces a blob's size from one copy and may
stream another, so waiting would end only at the deadline and read as an
interruption. The view's own store is not trusted for any of this, including
the copy of the baseline's history made when the view was created; `git gc`
or a repack inside the view changes nothing. A writer of the source reaches the
listing only through objects the work shares with the source that the view
never stored itself; those are verified the same way.

Measured on a loaded machine (macOS, Go 1.27). After a walk the live heap was
never more than 1.03 times what it had charged, so within an eighth in every
case; the peak, which includes garbage Go has not freed yet, was between 1.1 and
2.1 times, and 3.2 times when the walk read the same 1 MiB trees over and over.
Spending the 2^28 steps took the daemon between 4 s and 40 s of CPU, depending
on what they were spent on: about 5 s on comparisons or on reading 1 MiB trees,
16 s on commits with 1,024 parent lines, 36 s (90 s of wall time) on reading a
million tiny trees again, 40 s on one-parent commits (extrapolated from
99,000). A head under 1,024 parents whose 64 MiB root repeats
one directory entry completes in 0.8 s; distinct names those parents lack are
refused after 6 s. A work of 30,000 commits that each name their parent 1,024
times is listed in 8 s (14 MiB charged, 11 MiB live), 100,000 of them are
refused after 21 s (16 s CPU), 300 parents whose roots point from 1,000
directories at 300 distinct 1 MiB trees under a head with 1,000 distinct
subtrees are refused after 6 s, as are 2,000 commits sharing 1,023 parents with
200 KiB trees (9 s), and 12,000 commits sharing 100 parents with tiny trees,
nothing cached, are refused after 90 s (36 s CPU). Honest work passes with
room: 300 commits that each change one file of an 8,000-file directory use 7%
of the steps and charge 66 MiB (1.1 s); 152,000 objects in 500 commits use 2%
(0.7 s); classifying a 300,000-commit baseline history under old-dated work
uses 30% and charges 123 MiB (55 s of wall time, 13 s of CPU).

`/apply` and prune import the work from the view or its archive every time
before proving it: pack-objects packs exactly the listed objects.
`index-pack --strict` compares each object it receives with a loose copy the
source already holds, but Git 2.54 does not compare it with a packed one, so
the import first checks every copy the source already holds of the work's
objects, as above, and refuses a substitute instead of shadowing it with a
newer pack. A repeated `/apply`, and the prune after a delivery, therefore
leave a duplicate pack in the source until Git repacks it. For a pruned view,
whose archive is gone, the same verified walk reads the source's own objects.
A proof, walk, import, or ref check that times out or is cancelled reports
that it did not finish, never missing work or a changed ref, and a failed Git
command carries Git's own message. Objects reachable from the baseline are the
source's own history and are not proven again.

Prune's check that every object the view stores itself is reachable from the
delivered HEAD reads the view's own objects only, through a daemon-made object
directory that links the view's loose and pack directories and has no
alternates or other info files. Through its alternates Git would also read the
source, where a writer could plant a commit whose parents lead to work the
delivered HEAD left out. A commit the view does not store itself (after a gc in
the view) ends the walk there, which can only keep the archive. The view's own
store is its Run's: a Run that forges objects there can hide its own
unreachable work from this check, as it could delete that work outright.

`/apply` and `/views prune` also wait while the source checkout can still
change underneath them, and name what they wait for: a Run of the person being
admitted or running with a writable binding on (or above) the checkout, a
parked Run that can resume there, or a still-running background process that
such a Run started (or that an earlier daemon recorded in the checkout).

Residuals:

- A child process that escaped a sandboxed foreground command without being
  registered as a background process can outlive its Run on macOS, where
  seatbelt has no PID namespace (bwrap's PID namespace prevents this on Linux).
  The wait above cannot see it, and it keeps the Run's write access to the
  checkout.
- The wait for writers of the source checkout is a check made before delivery
  or prune, not a lock held against Run admission (unlike the view move
  below): a Run admitted, or a queued request drained, while `/apply` or
  prune is working in the source is not seen.
- The wait compares paths with the shared path comparison, which does not yet
  treat spellings of one directory that differ only in letter case as the same
  path on a case-insensitive file system (the macOS default). A Run whose
  writable binding names the checkout that way is not seen. The fix belongs in
  that shared comparison, with the admission path work.

None of these can make daemon-side Git run a program, by the rules above. Each
can still change the checkout's refs and objects during `/apply` or prune;
compare-and-swap ref creation, `--strict` import, the verification of every
created ref, and the proof that the source holds the work turn such
interference into a refused operation rather than a wrong branch or a deleted
archive. Objects removed from the source after that proof are outside it. The
guarantee for plain reads rests on those commands having no
configuration-driven program in the Git version verified (2.54); a Git release
that adds one would need this review again.

Branch delivery imports committed work under a new ref and leaves the source
checkout untouched, even when another Run has advanced its HEAD or left
unrelated work there. Source repository and baseline object identity, view
cleanliness, and the branch compare-and-swap remain the delivery authority.
Archive and prune refuse work Git status cannot show: index entries marked
skip-worktree or assume-unchanged, linked worktrees under `.git/worktrees`,
and submodule repositories, besides uncommitted, untracked, ignored, and
undelivered work. An explicit prune may reclaim a retired view only after
checking the exact owner Run, unfinished references, extra Git refs, and that
every object the view stores itself, loose or packed, is reachable from the
delivered HEAD without reflogs; objects borrowed from the source through
alternates are not the view's and are excluded. The delivered work is
protected by a separate source ref, created only once the work has been
imported from the archive and the source proven to hold every object it adds
over the baseline with matching content, before a durable prune record
authorizes checkout deletion. That ref is verified against the source again
inside the guarded move, immediately before the checkout leaves restore's
path, and a pruned view is reported as protected only while the source still
holds the work. A failed proof or verification preserves the archive; no
age-based automatic deletion runs.
The rename that takes a view out of its Run's path is atomic with Run
admission: every admission registers its frozen roots in the gateway registry
before inserting its Run, and the move holds the same lock across a registry
check, a durable re-check of unfinished references and of every Run bound to
the view since the first check, and the rename. A Run admitted after the move
parks with the archive or retained-ref explanation, and restore is available
to any owned Run bound to the view regardless of its status.
- Delegation depth is enforced structurally. `buildDelegateSubBackend` builds
  the child dispatcher as a `Subset` of the parent's registry and removes
  `delegate_task` at the configured depth limit. A subset has fewer tools but
  the same middleware chain, clarify handler, and attribution observer, so a
  sub-agent's calls meet the parent's safety floor, approvals, workspace scope,
  and guardrails. Never expose or mutate the shared parent dispatcher. Fan-out
  remains bounded by `max_subtasks` and `max_concurrent`. A delegated batch
  uses `max_concurrent` only when every cloned child catalogue contains
  proven built-in read tools. Read-only clones omit nested delegation so that
  capability cannot widen mid-batch. If any child can write, execute a shell,
  delegate again, or invoke an unknown/external tool, the batch runs one
  sub-agent at a time in the shared parent view. Separate isolated child
  views are required before write-capable fan-out can be enabled.
- A sub-agent is one step of the parent run. Its calls act as the parent run's
  person and are claimed in the parent run's tool ledger under the delegating
  call's namespace, as are the call ids its loop generates for a provider that
  sends none, so its calls never collide with the parent's or a sibling's. Only
  its tool activity, evidence, and usage reach the parent's event stream,
  marked `delegated`. Every approval surface names its asks as a sub-agent's,
  and a human wait that ends its turn, such as an unanswered approval, parks
  the parent run on the same wait instead of returning as its report; a batch
  keeps the other goals' results for the parent.

### External MCP tools

MCP tools are external code and fail closed unless the operator or the person
explicitly marked the exact tool read-only. The registry injects the actual
schema origin after model-argument validation; approval must use that origin
rather than infer trust from a provider-visible name. An unclassified MCP call
is sequential and requires a once-only human decision in every approval mode,
including `full-auto`. It cannot use a stored grant or smart-model approval.
The hard safety floor still runs first.

A read-only marking names one exact remote tool of one configured server; no
pattern, prefix or default can mark a family of tools. The operator lists exact
names in `mcp.servers[].read_only_tools`; the loader rejects patterns and
inexact names, and a name the connected server does not expose is a warning in
gateway status, `selfmind doctor`, `/diag tools` and `/tools readonly`, never a
failed server. A person marks a tool for themselves with the model-free
`/tools readonly <server>/<tool> on|off`, from any bound endpoint; bare
`/tools readonly` lists both kinds. The command accepts only a tool the
connected server exposes now. A server name may contain `/`; a reference that
splits into more than one configured server and tool is refused with the
readings listed, never resolved by guessing. Only a turn the person typed
changes a marking. A scheduled prompt, a watcher finalization or any other
daemon-started turn that carries the command changes nothing, whichever
endpoint it names. Each marking records who made it, when, and from which
endpoint; unmarking withdraws it under every identity of that server and keeps
the audit row. A marking applies only to its own person, and it is a standing
decision: like a remembered approval class, a scheduled Run carries only the
markings that existed when its schedule was authorised.

What the server says about its own tools never counts: an MCP `readOnlyHint`
or any other annotation is ignored, so a writing tool that claims to be
read-only stays unclassified. A person's marking is bound to the server's
configured identity: the normalized transport plus the configured command and
arguments for stdio, or the URL for HTTP. A server configured under the same
name with another transport, command, argument list or URL does not inherit
the marking; it is listed as inactive in `/tools readonly` and counted in
`/diag tools` until the person marks it again. The identity leaves out other
things that change what a server does, and a marking survives a change in any
of them:
- environment values, including what `env_filter` passes through;
- headers and credentials;
- the program a bare command resolves to through `PATH`;
- the daemon working directory a relative command or argument is read from;
- an upgrade of the binary or package behind an unchanged command line;
- a server that redefines a tool's behavior or schema under the same name.

Withdraw a marking when one of these changes what the tool can do. Operator
names live in the server's own configuration entry.

A marked call gets a read-only execution policy for that call's person. It is
approved like any read-only tool in the Run's frozen mode: no human ask and no
judge. At parallel capacity it claims no external target. It may join a
read-only concurrent batch, because the kernel asks for each call with that
call's trusted Run scope. An isolated execution view admits it because there is
nothing to claim, and its ledger effect class is an observation, a look rather
than work. It keeps the network operation class, as `web_search` does, so the
person's explicit "do not use the network" still reaches it and asks, even in
full-auto. A call reads its marking once and carries that answer through its
batch decision, approval, effect claim and recorded metadata, so an unmark
during dispatch cannot give one call two policies; the next call reads again.
Its origin stays external, so blind replay, recovery and every other
external-tool rule are unchanged. A lookup failure counts as unmarked.
The provider catalogue is static, so verification-only recovery turns still
omit marked MCP tools from the catalogue; the dispatch-time verification-only
check reads the call's marking. An unmarked MCP tool behaves exactly as above.

A control database that an earlier development build stamped as schema v26
before the marking table existed opens without migrating. Schema health in
gateway status and `selfmind doctor` names the missing `mcp_read_only_marks`
table. `/tools readonly` explains how to restore the backup taken before that
upgrade with `selfmind maintenance restore-control`, so this build can apply
v26 completely. Until then person markings neither apply nor change.

Only provider-visible input arguments cross the MCP boundary. Dispatcher scope,
callbacks, registries, storage handles, and every other top-level
underscore-prefixed argument remain inside SelfMind. An external schema that
declares a top-level underscore parameter is quarantined because it would make
that boundary ambiguous. Nested business fields are preserved.

MCP server and tool names are normalized for provider compatibility and carry
a stable identity suffix so truncation or punctuation normalization cannot make
one server overwrite another. Registry collisions are rejected, never
last-writer-wins. Current connection and catalogue failures are exposed through
gateway status and `selfmind doctor`; they are not recoverable only from logs.

### Model catalogue exposure

Tool exposure is a catalogue concern, separate from execution authority:

- `direct` definitions are present in every native-tool request;
- `deferred` definitions are absent until a successful `tool_search` result
  activates the named tool for the current work unit;
- `hidden` definitions are never provider-visible and cannot be activated.

Activation is monotonic inside one work unit: the definition set may only grow
until a successful `update_plan` crosses to another top-level unit, where the
set resets before any later tool call executes. Resume reconstructs only the
latest unit's activations from the ordered tool ledger.
Execution still passes through the same registry, scope, approval, sandbox,
and safety-floor checks; discovery never grants authority. Automatic external
deferral has an empty reviewed allowlist and remains code-gated until the active
daily-driver plan has at least seven days of trustworthy usage and request-
fingerprint evidence. Catalogue size or a tool-name hash is not evidence that a
tool is cold. This rollout gate is not public configuration. Explicit exposure
metadata remains enforceable and is covered by mechanism tests.

## Result Envelope and Artifacts

One tool result has three distinct surfaces:

1. bounded raw capture for diagnostics and artifact persistence;
2. model-facing content with explicit truncation and recovery instructions;
3. a concise, UTF-8-safe user preview for CLI, IM, and run events.

Do not use one truncated string for all three surfaces. Large output is spooled
through the per-run artifact sink and referenced by an artifact id. The model
uses the read-only `tool_output_view` tool to retrieve omitted ranges instead
of repeating the original command. Spooling failure degrades to a bounded
result note and must not turn a successful tool call into a failure. Aged tool
results may shrink only when an addressable artifact retains the evidence.

Failed tools preserve a separate bounded diagnostic excerpt, byte count, and
content hash in the durable event payload. This evidence is independent of the
short user preview, so a failure can be classified and reviewed without
persisting an unbounded stream. Exit status and known interface-drift failures
are recorded as structured fields where they can be derived deterministically.
An error classification already attached by the tool layer remains the single
model-visible classification; the kernel reuses it instead of appending a
second, potentially contradictory hint. Raw database-driver signatures are
hidden only for trusted built-in tools whose failure exposes SelfMind storage.
External/MCP database errors remain actionable model evidence, selected from
trusted registry origin metadata rather than inferred from the tool name.

Normal UI output summarizes tools, plans, and failures. Raw JSON is reserved
for an explicit protocol/debug request.

## Approval Layers

Approval evaluation is ordered. A lower layer cannot override a higher one:

1. unbypassable hard safety floor;
2. explicit approval mode and enforced execution containment;
3. matching run/task/person authorization;
4. optional smart-mode cheap-model triage;
5. human approval prompt.

A Run's mode is frozen when its work is admitted, and a person's later tightening
of `/mode` narrows work that waited: queued work and work parked on the model
provider, an approval or a question runs under the narrower of its admitted mode
and the persisted mode, only when the person set that mode after the work was
first admitted, never widening and never rewriting the admitted snapshot (see
the admission snapshot in `docs/work-timeline.md`). A parked approval the person
already answered keeps releasing exactly its action under the narrower mode: its
authorization is keyed on the action, not the mode. A Run that is running keeps
its mode.

An approval rejection or cancellation is a user decision, not a transient tool
failure. Kernel rejection detection and middleware error strings are a shared
contract: rejection tells the model not to repeat the operation. A hard safety
block is a policy decision and remains distinguishable from user rejection.

Approval memory uses bounded daemon-derived rules (for example a command
prefix, one network host, or one write root), not opaque client-defined classes
or raw exact command strings. The proposed rule text travels alongside its
opaque key and is shown verbatim in every approval surface. Hard-floor denials
and content-level denials are never grantable. Numbered approval references
resolve in the gateway with the same order used by every client.

A remembered exec class is a token prefix that reaches the subcommand verb, not
the program alone and not the whole command. Stopping at the program conflated
a read with a write in the same class; taking the trailing tokens minted one
class per invocation, because those tokens are build ids and resource names.
Pipeline plumbing contributes no class only under its exact catalogued name
(`./ls` or `LS` is an operation), and a payload whose subcommand is not
identified is not grantable. A subcommand that performs any HTTP method carries
the method and a bounded resource path, so a remembered read cannot release a
delete. Programs that run whatever they are handed — shells, interpreters,
`git`, `find`, `make`, arbitrary network clients — never mint a class at any
scope. Nor does a payload whose class would name a program that may not be the
one that runs, or not as the person approved it:

- an assignment that redirects resolution or loads code: `PATH`, a
  code-injection variable (see Arbitrary Code and Network Egress) or
  `PYTHONPATH`, before the program, through `env`, exported or set by
  `printf -v`, or `env -P`. Any other assignment changes what the program reads
  (`KUBECONFIG=./kc`, `export AWS_PROFILE=prod`) and mints nothing either,
  unless the run's environment snapshot already holds exactly that value;
- a program or wrapper word that is not its exact bare lowercase name: a path
  runs whatever file sits there and a case-sensitive filesystem may hold a
  different `KUBECTL`, so `./kubectl get pods`, `bin/kubectl`, `KUBECTL` and
  `/tmp/x/timeout 5 kubectl` mint nothing (the plumbing rule already required
  the exact name);
- an option that points the program at configuration, credentials, a server,
  a proxy or a trust anchor the command chooses, or turns verification off:
  `--kubeconfig`, `--server`, `--token`, `--as`, `--insecure-skip-tls-verify`,
  `--endpoint-url`, `--ca-bundle`, `--no-verify-ssl`, `docker --config`/`-H`/
  `--tls`, `podman --remote`, `doctl --api-url`, `gcloud --flags-file`,
  `gh --hostname`, `ssh -o`/`-F`/`-i`, `rsync -e`/`--password-file`,
  `ncat --proxy`. A kubeconfig, a Docker config and an ssh option can each run
  another program, and a redirected or unverified server receives the
  operator's token. An option that names an entry of the operator's own
  configuration (`--profile`, `--context`, `--account`) is instead the identity
  the call acts as and joins the class
  (`aws codebuild batch-get-builds --profile=cw3`), so a class remembered for
  one profile never releases another. Where the program's parser reads any
  unique prefix of a long option — argparse (`aws`, `az`, `gcloud`), Python's
  getopt (`gsutil`) and getopt_long (`ncat`, `netcat`, `nc`) — a prefix counts
  as every option it could name: `--endpoint-ur` drops the class, `--prof cw3`
  joins it as `--profile=cw3`, and one that could name more than one option
  drops it. Cobra and pflag (`kubectl`, `helm`, `docker`, `gh`) read only exact
  names;
- an escape anywhere in the payload: `\python3` is python3 and `-\X DELETE` is
  a DELETE;
- any wrapper except those that change neither who runs the command, its
  privilege, nor how its words are read (`exec`, `time`, `command`, `builtin`,
  `nice`, `ionice`, `nohup`, `timeout`, `stdbuf`). `sudo kubectl get pods`,
  `env FOO=1 kubectl get pods` and `bash -lc 'gcloud builds list'` mint
  nothing, so a class remembered for `kubectl get` never releases the root
  form. The same rule withholds prefix and host rules and declared bundles.

The remembered key IS that class, for a sandboxed call (`exec:<tool>|class=…`)
as for a host one (bound to the workspace and the class). It is never built from
the approval reason, which describes why this one call asked: in on-request
mode the reason is the mode text and in smart mode it may be empty, so a key
built from it named the whole tool, and remembering one sandboxed
`kubectl get pods` released `kubectl delete` and `terraform destroy` while the
prompt showed the mode text or a `kubectl get` class. The remember option is
offered only when the floor minted a class, and its label names exactly that
class. A payload naming a raw disk or a `/dev/tcp`/`/dev/udp` pseudo-device mints
no class either: it is dangerous for what it touches, not for which program
touches it.

The boot-time review applies the same floor to stored keys. A stored key is
never rewritten to its normalized form, which could match more than the person
approved: an exec key the current derivation would not mint is withdrawn. That
covers a family holding an escape or naming a program the floor bans, a host key
minted under another reason, and every sandboxed key built from a reason, a mode
or the tool name alone (`exec:terminal requires approval in on-request mode`,
`exec:terminal`, `exec:invokes dangerous command: chmod`): no current call looks
such a key up, and the tool-wide ones covered every command.

Host execution no longer forces a one-shot decision by itself; the floor
decides. A class the floor minted may be remembered for one workspace, which is
narrower than person scope and carries no deadline: what bounds it is that it is
narrow, listed by `/approvals grants`, revocable, and re-checked against the
floor. A payload the floor refuses keeps its one-shot decision, including
`execute_code`, which has no class narrower than arbitrary code.

A command the dangerous-op heuristic flags for its own effect also stays
one-shot on the host. Those classes — chmod, chown, kill, pkill, killall — are
left out of the floor's banned set so they can be remembered under an enforced
sandbox, where the blast radius is the workspace; the host offers no such
bound. `mv` is not among them: the heuristic does not flag it, so on the host
it is an ordinary host request whose class may be remembered. The
heuristic's REASON decides this, not its boolean: it reports every host request
as dangerous, so reading the flag alone would return every host call to a
one-shot decision. Such a program (chmod, kill, ssh) also has no host class at
all, so no stored host key, however old, releases it, and the boot review
withdraws one.

Only the person's own input answers an ask. `/approve`, `/reject`, a bare `y`
or `好`, and a menu letter count only when the person sent them from one of
their endpoints. A turn the daemon started — a schedule firing today; watcher,
recovery, approval, and wakeup continuations drain from the queue — carries
text stored earlier, often in the model's own words, and the gateway reads it
as task input only: never an answer, a command, a numbered choice, or a
continuity control, and never presence at an endpoint. A schedule whose text
begins with a command is refused rather than handed to the agent, which would have
to improvise around a control the gateway will not run: nothing starts, and the
notice goes where the job's result would — its IM chat, or for a CLI job, whose
results are never pushed, the daemon log. The model therefore cannot approve
its own action by scheduling the words, and the once-only asks that full-auto
keeps stay with the person.

The model-facing `cron` tool binds every job to the person whose Run creates
it: the job runs as one of that person's own bound accounts — with nothing
named, the account the request came from — and delivers to that account's
direct chat. Another person's account, an unbound account, a shared chat,
another tenant, and a prompt that begins with a command are refused with an
error the model can act on, never rewritten; the error names only the
platforms the person's accounts are on, never their ids. List, remove, and
enable reach only the person's own jobs, and list says why a job of theirs does
not run. A direct chat is the account's user id, or, on platforms whose chat id
differs (Feishu and Lark `oc_` chats, QQ `c2c:` chats), the chat the platform
itself marked as direct on that account's last such message.

Each fire checks the stored job again, because a job stored before the tool
checked it may name anything. A fire never creates an account. A job that
names an unbound or inactive account, or a chat its person does not own, runs
nothing: it is turned off so it is not refused again at every fire, logged,
and, for a chat its person does not own, that person is told once in their own
chat. It stays visible with its reason in the cron tool's list and in
`selfmind doctor`, which shows it to its owner with how to remove it and counts
other people's jobs without showing them.

A job stored by an older release may name no account: channel `cli` and no
`deliver_to`, which that release ran as `cli:local`. The command line now acts
as `cli:$USER`, so the row belongs to whoever created it, and the evidence
decides who that is. It is not safe to assume the local command line did: that
release's cron tool filled in `cli` whenever the model named no platform, for a
Run started from any chat, a stranger's under an open DM policy included, and it
stored every job under the default tenant. The creator is the person whose own
Run, in any tenant, recorded the tool's receipt for that job
(`Cron job <id> (<name>) added.`, a `tool.completed` event of the Run). A
receipt from a Run the daemon started — a schedule firing, a watcher, a
recovery — names no one, and neither does one from the `cli:local` person an
older executor created, because that executor ran other people's jobs as it.
With no such receipt, the job belongs to the installation's command-line person
— the one person holding command-line accounts other than `cli:local` — only
when no one else has ever held an account here, in any tenant, or when it is
the bare liveness canary, whose turn is fixed text. Several command-line persons
leave the command line unknown. The owner's own command-line account runs the
job, which reaches only that person.

The decision is made once, the first time the daemon reads the row, and kept
in the control store. Its evidence only shrinks: a work-history reset deletes
every Run and receipt, and an account can be rebound or cleaned up. A decision
remade later from less evidence could move a job to the command line, so it is
never remade. A job no one can be shown to own, or whose owner has no
command-line account, does not run. One no one owns is listed to command-line
users, and when it is refused the installation's command-line person is told
once, in their own chat, which jobs those are; the notice names the jobs,
never their prompts. That person may remove such a job, or adopt it with
`selfmind cron adopt <id>`, which shows its prompt and schedule and, with
`--yes`, records that it is theirs. Adoption is not a tool, a slash command,
or a daemon turn: it reaches the daemon only through the local-control
endpoint that workspace trust uses, so it is exactly as strong as that channel
(see [Workspace trust and capabilities](#workspace-trust-and-capabilities)) and
no stronger. The request must name a command-line account that already exists,
it never creates one, and of racing adoptions one wins. A job another person
created can be neither shown, not even its prompt, nor adopted.

The daemon registers its liveness canary under a system key, so it updates
only its own row. The row an earlier release registered by name is taken over
only when it has the canary's exact name and bare prompt; another job named
`canary` is left alone.

A run may USE a remembered class; only a person answering an ask creates one.
Automatic triage records at most a run-scoped reuse of the exact action.
Scheduled work is authorised when the person creates the schedule, so it carries
the classes that existed at that instant and no later ones; a schedule with no
recorded authorisation consumes none and asks. A fire that waits in the queue
behind the person's work stays scheduled work when it drains, and keeps the
instant its schedule was authorised in the row's admission snapshot, so it
consumes the classes that existed then and no others; a row written without a
snapshot has no instant to restore and consumes none. Daemon-started
continuations of a person's own work — an answered approval, a finished
watcher, a recovered run — carry that person's decisions, because the work is
theirs and already underway. Who authorised the work is read from its
admission snapshot, which every continuation restores from its exact parent, so
a continuation of scheduled work keeps the schedule's instant whatever
triggered it (`work-timeline.md`).

Containment is assessed on three independent axes: filesystem
(`isolated|host|unknown`), network (`none|shared`), and credentials
(`none|selected`). On an enforceable isolated filesystem (bubblewrap on Linux,
Seatbelt on macOS), a call with no network or credentials may release an
ordinary exec call in smart mode. When shared
network or selected credentials are present, only operations in the declarative
observation catalog may be released. Unknown programs, arbitrary scripts,
`execute_code`, secret reads, mutations, and unparseable command forms remain
approval-gated. Extending the catalog is a reviewed data change with focused
tests; it must not become a permissive shell heuristic. The network axis
describes the network the call actually receives, not the network its workspace
permits: a stored grant or a trusted workspace leaves `go test` at
`network=none`, so it is released as contained, while `npm install` under the
same grant is `network=shared` and is reviewed.

Sandbox reads are not confined. Both backends give a command a broad read-only
view of the host filesystem (`--ro-bind / /` under bubblewrap,
`(allow file-read*)` under Seatbelt), so an isolated command can read the
operator's files, credential files included. "Contained" therefore means
confined writes, no network and no attached credentials — not confidentiality
of what the host holds. The credential axis reports the operator credential
state the call's sandbox is prepared with, and it is the decision preparation
makes, not a second one: the credential CLIs the command names, plus, when the
payload hides what it runs (an interpreter, a script, `make`, `npm run`,
`find -exec`), every operator profile the host has state for, handed over only
when workspace trust or `credential:read` allows it. In a trusted workspace on
a host that holds such state, `npm run build` is therefore
`credentials=selected` and is not released as contained, while
`go build ./...`, which names no credential tool, still is.
The once-only rule below keys on the narrower
`credential:read` decision for a credential CLI the call names: inventory state
an undecidable payload receives without a network route reaches no remote
target, and the reads above already reach the same files.

The boundary is decided once, before any capability ask, judge or person, and
execution is pinned to it. The decision includes whether the platform's backend
can enforce THIS call's plan, not only whether the platform has a sandbox. When
it cannot, optional isolation (`auto`) is presented, judged and asked as host
execution with the reason, and isolation that was demanded — an explicit
`isolated` request or `exec_sandbox.required` — is refused
before any approval with a typed `not_dispatched` failure. The feasibility
check assumes a pending `credential:read` ask could be granted, because the
writes withheld credentials need are a subset. On a backend that writes
credential state in place (see Tool environment profiles), the containment
summary every judge and person sees also names those host paths:
`host_credential_writes=<path>;<path>`. Those paths are part of the approved
boundary, so a changed set asks again: every reusable key — the remembered
class, each offered rule, a phase bundle's declared effect — is bound to a
digest of the write set (`|host_writes=…`), and the exact-run and decision keys
and a parked approval's resume authorization carry it through the containment
summary. A class or rule remembered while a command wrote nothing in place
therefore never releases it once its configuration needs writes, while one
remembered with the same set is reused. A phase bundle mints each declared
command's key already bound to the writes that command makes once its
credential access is granted, names them in the command's label, and leads its
ask with them, so a bundle approved before the writes appeared never releases
them. In an untrusted workspace the `credential:read` ask is what turns those
writes on, and a contained safe observation it enables is then released with
no second ask; that ask therefore leads with the same fact, and the run grant
it leaves is bound to the write set like the other keys.
The boot review keeps a bound key whose class still passes the floor, and
`/approvals grants` says it holds only while the same caches are written. The
reason every surface truncates (an IM ask to 120 characters, the terminal panel
to two lines) leads with the boundary fact — `writes credential caches in
place: ~/.aws/sso/cache, ~/.aws/cli/cache`, or `runs on the host: <why>` — so no
approver can miss it. The external-effect claim boundary's own once-only ask for
another Run's left-uncertain effect releases the same exact call, so it leads
with the same fact and carries the containment summary, and the authorization a
parked answer leaves is bound to the write set through that summary. A
background start refused before its process launches reports that nothing
started, so the claim it took is released rather than left uncertain.

The one-time ask distinguishes a known conflicting target from an unknown
target scope. Unknown scope means independence cannot be proved; it does not
prove that two operations actually touch the same remote resource. The ask
names the claim and the existing `/effects settle` control, conditional on the
person knowing the original outcome. `/effects` explains why approving one
action can leave later actions asking again. Neither notice changes authority
or settles a claim.

Execution uses exactly the approved boundary. An approval — a person's, the
judge's, a stored or run-local grant's, or a parked approval's resumed
capability — that assumed isolation is never widened at dispatch: if the
backend cannot enforce that plan then (configuration changed, a state path
moved out of its root), the call is not dispatched and the model receives a
typed `sandbox_boundary_unavailable` or `sandbox_plan_unsupported` failure
saying it did not run on the host. This holds at capacity 1 as well as in
parallel work, and for durable watcher polls, which have no approval at poll
time. A boundary approved as host runs on the host. Optional isolation the
backend cannot enforce resolves to host execution before approval, never by
widening an approved isolated boundary at dispatch.
The approved boundary includes its in-place credential writes: preparation
refuses, before creating anything, a plan whose writes the releasing decision
did not cover (`approved_host_writes_changed`), because the host's
configuration changed between approval and dispatch. A durable watcher poll has
no releasing decision of its own, so it is held to the write set its
registration's approval covered, which registration records on the watch's
execution binding (`host_state_writes`, part of the existing binding JSON). A
poll whose plan writes more is refused the same way, before anything is
created, and the watch parks as an environment failure; a binding that recorded
no writes, including one from before bindings existed, covers none. A background start is
host execution by contract and follows the same rule: a policy that requires
isolation refuses it. A watch registers only when its unattended polls, which
resolve `auto` with no approval of their own under the policy its registration
froze onto the watch (narrowed, never widened, by the daemon's current policy;
see Durable execution identity), will run under the boundary its registration
was approved for; otherwise nothing runs and registration is refused
(`watch_boundary_mismatch`), so a watch can never register and then be unable
to poll, or poll outside what was approved. The two holds do not depend on each
other's inputs staying put. Disabling the sandbox after registration cannot move
a poll onto the host, where no in-place plan exists to hold it to its write set;
and a stricter configuration that narrows a watch registered on the host into
isolation does not grant it in-place writes, because its registration covered
none: such a poll is refused until the watch is registered again.

A filter joins the catalog only when every operand it takes is an INPUT. A
program whose second operand is an output file writes with no flag to reject,
and the catalog's rule shape cannot express "no positional output" — which is
why `uniq`, `tee` and `xxd` are excluded while `od` is not. This matters beyond
auto-approval: the same catalog decides what a durable watcher may re-run
unattended.

The catalog vouches for the program a bare name resolves to, never for the file
at a path. A path-qualified program (`./cat`, `bin/git`, `/tmp/x/od`), including
a path-qualified `sh -c`, `timeout`, or `command` wrapper, is not a proven
observation: it may be a workspace script that borrows a catalogued basename.
The prover reads a shell's command string with the same grammar as
identification and proves only one `-c` operand, with no positional
parameters, that every shell the name stands for reads alike; a shell that
also reads a startup file (`bash --rcfile FILE -ic 'git status'`) is never
proven.
A relative or workspace-internal entry on the daemon's PATH could still shadow a
bare name, so keep such entries out of the daemon environment. The same proof
records the call's ledger effect class as `observation`, which keeps a read
from closing a continuation's selection window or counting as work; replay,
recovery, and approval keep their own classes.

Where one subcommand can perform every HTTP method, the catalog and the class
derivation must resolve that verb with the SAME parser. Two parsers accepting
different spellings of one flag is how `-X=DELETE` and `-XDELETE` were read as
ordinary GETs by the catalog while the class recorded them as reads. A verb that
cannot be resolved statically is a mutation. For `gh api` that parser reads gh's
own option grammar (pflag): short options bundle (`-if title=x`) and take an
attached value (`-ftitle=x`). A request body (`-f`, `-F`, `--field`,
`--raw-field`, `--input`) makes the call a POST unless `--method` names another
verb, and it disqualifies an observation whatever the verb; an unknown option
fails closed, and `--hostname` is no observation because another host may
receive an enterprise token.

An exec payload that is too opaque to mint a safe class authorization may offer
an exact-run decision. Its key hashes the raw action plus run, workspace,
environment, and containment metadata; raw code is never persisted. The grant
exists only in memory for that run, releases only a byte-identical repeat, and
cannot be promoted to task or person scope. `/diag` reports containment,
class/rule grant hits, exact-run hits, judge outcomes, and human asks as funnel
events rather than pretending they are unique operation counts.

When Main can name a bounded phase before it starts, `request_permissions` may
combine its path and host rules with up to eight exact, statically known command
effects in one human decision. Each command is normalized through its registered
schema from a closed `tool` plus `arguments_json` declaration before the ask and
is bound to the live Run, workspace, identity,
environment generation, and effective filesystem boundary. The actual call is
normalized again and still passes the hard floor, current explicit restrictions,
execution capabilities, and workspace scope. Changed arguments or boundaries
ask again. Arbitrary code, opaque shell scripts, arbitrary network clients,
destructive host operations, external tools, and commands that depend on an
earlier result stay on the ordinary single-call path. `/diag` reports successful
declared-command reuse separately as `bundle-hit` so daily-driver evidence can
measure the interaction reduction rather than infer it from approval counts.

Credential reads outside the observation catalog, explicit-deny overrides, and
high-risk or unclassified requests are once-only decisions and never create a
reusable grant from the interactive prompt. An exec call that is not provably
isolated is judged by the same rule as the host above: it needs a class the
floor minted, and an unresolved sandbox mode is not evidence of containment.
Ordinary bounded operations offer a run-local reuse choice and, where a class
exists, the workspace choice; the exact server-issued choice set is shared by
CLI and IM. Historical task and person grants remain readable until they expire
and can be inspected or revoked with the approval and workspace grant commands,
but new interactive prompts do not mint grants at those scopes.

The model-visible `sandbox: auto|isolated|host` arguments are a compatibility
surface. Internally they may map to capability requests such as
`network:shared`, `credential:read`, or `host:escape`, but do not change the
tool schema and invalidate eval cassettes in the same patch. Any future
`with_additional_permissions` request carries a justification and the concrete
capability; it never grants an unspecified "more access".

## Hard Safety Floor

`hardlineToolCall` executes before every bypass, including full-auto, grants,
and model triage. Keep this set narrow and limited to operations that should
never be legitimate, including destructive operations against filesystem or
protected roots, filesystem formatting, raw-device overwrite, fork bombs, and
host shutdown/reboot. Formatting includes `newfs` and macOS `newfs_*`;
shutdown includes `telinit 0|6` and systemd's `systemctl reboot`, `poweroff`,
`halt`, `kexec` and `soft-reboot` (or starting or isolating their targets);
erasing a disk includes macOS `diskutil eraseDisk`, `eraseVolume`, `reformat`,
`zeroDisk`, `randomDisk`, `partitionDisk` and `secureErase` other than of free
space.

The classifier inspects the executable payload used by terminal, shell, and
code-execution tools. It refuses what runs, not how it is spelled. Every
analysis that recognizes a program by name — this floor, the dangerous-operation
heuristic, the observation prover, environment dependency analysis and
proxy-removal detection — names it through one identification
(`identifyCommand`):

- leading `NAME=value` assignments are skipped whatever the value
  (`PATH=/usr/bin rm`, `FOO="a b" rm`);
- the known prefix wrappers are peeled with their own options and operands:
  `env` (including `-u`, `-C`, `-P` and assignments), `sudo`, `doas`, `command`,
  `exec`, `builtin`, `time`, `nice`, `ionice`, `nohup`, `setsid`, `stdbuf`,
  `timeout`, `xargs`, `chroot ROOT`, `flock FILE`, `busybox`, `toybox`,
  polkit's `pkexec`, util-linux `taskset MASK`, `nsenter`, `runuser -u` (without
  `-u` it reads like `su`) and `setpriv`, macOS `arch` (whole-word options, its
  `-e NAME=value` recorded as an assignment) and `caffeinate`, and GNU
  parallel's `sem`. `taskset -p` and `setpriv --dump` run no command;
- a launcher's options are read with its own parser's rules, from a grammar
  that is the union of the BSD tools macOS ships and the GNU coreutils,
  findutils, util-linux, procps and GNU parallel ones: a bundle of short
  options ends at the first that takes a value (`sudo -iu root`,
  `env -iu NAME`, `xargs -0n 1`), a long option takes `=value` or the next word
  and may be abbreviated to a unique prefix (`timeout --kill 5`), BSD-only
  values are values (`chroot -u user`, `xargs -J %`), `timeout` reads any
  DURATION strtod accepts (`1e3`, `inf`), and `ionice -p`, `busybox --install`
  or `toybox --long` run no command;
- the command strings a shell runs (`sh`, `bash`, `zsh`, `dash`, `ksh`,
  `mksh`, `csh`, `tcsh`, `fish`, and busybox's `ash` and `hush`), `su -c`
  (attached, bundled or abbreviated, before or after the user), `flock FILE -c`,
  `eval`'s arguments, and the command `watch` or `parallel` runs are read as
  nested payloads, as is each command `find -exec`, `-execdir`, `-ok` or
  `-okdir` runs. `find` ends -exec and -execdir at `;` or at a `+` right after a
  word holding `{}`, so `-exec xargs -E + cmd ;` runs cmd; -ok and -okdir end
  only at `;`; BSD find also ends at any word beginning with `;`. `parallel`
  with no command before `:::` runs its arguments as command lines, and
  `--arg-sep` renames the separator. With a command, each combination of
  literal arguments is composed into it as parallel does — shell-quoted, in
  place of `{}`, `{N}` or an `-I` string, or appended when it holds none
  (`parallel rm -rf ::: /` runs `rm -rf /`) — and the command is read as
  written too. Other replacement strings (`{.}`, `{//}`, `{= expr =}`) and
  arguments from files (`::::`) are not composed;
- a shell is read with its own option grammar. A POSIX shell's `-c` is a flag:
  the command string is the first operand after all of its options, which may
  follow `-c` and end at `--` or a lone `-` (`sh -c -- 'cmd'`,
  `bash -c -e 'cmd'`). `-o` and `+o` take a value — the next word in bash and
  dash, the rest of their cluster in zsh and ksh (`-opipefail`) — as do bash's
  `-O`, mksh's `-T` and ksh93's `-R`; bash reads its long options
  (`--rcfile FILE`, `-norc`) only before its first single-letter option, and
  zsh's `-b` ends its options. `sh`, which is bash, dash, ash, ksh or zsh as the
  host installs it, is read with each of their grammars, and every command
  string any of them runs is classified. csh and tcsh run the word after the
  cluster holding `c`; fish runs every `-c` and `-C`. `su` hands `-c COMMAND`,
  or the words after the login (macOS), to the target's login shell, read with
  every shell's grammar. A shell that also runs a script file, its input, or a
  startup file an option names (`bash --rcfile FILE -i`) is opaque, and the
  command string it runs is still read;
- `env -S` splits its string into env's own arguments as env does — quotes,
  `\_` and the other escapes, a `#` comment, `\c` — and reads them in place of
  the option as options, assignments and the utility (`env -S '-i rm -rf /'`).
  A `${NAME}` it substitutes in the utility word is chosen at run time and
  opaque, and a string env refuses runs nothing it can show and is opaque;
- a program word a launcher replaces with its input is chosen at run time and
  is opaque, like `$CMD`: `xargs -I {} {}`, `parallel {1}`, or a script one of
  them runs (`xargs -I {} sh -c '{} -f'`). A brace group (`{ cmd; }`) is shell
  syntax, not a replacement string;
- escapes and path qualification are removed and names compare lowercased,
  because a case-insensitive filesystem runs `RM`, `\rm` and `/BIN/RM` as rm.
  A name Homebrew gives a GNU program is that program: the coreutils,
  findutils, time, inetutils, sed, tar, grep, make, awk, indent, which,
  libtool, units and ed sets installed with a `g` prefix (`grm`, `gtimeout`,
  `genv`, `gxargs`, `gfind`, `gtelnet`, `gsed`, `gmake`), so the bare
  program's rules apply: `gsed` and `gmake` back no class, and the boot review
  withdraws a key that names one. Only the deny side folds case and prefix:
  proof and remembered classes require the exact name.

The floor and the dangerous heuristic act on either of two readings: the
shell's own parse, which keeps a quoted value in one word and sees into control
structures, substitutions and functions, and the conservative field reading,
which still finds a command quoted inside code (`os.system('rm -rf /')`). A
property test applies these respellings, alone and composed, to the whole
forbidden set. Targets are compared as the shell passes them: an `rm` target
after `~`, `$HOME` or `${HOME}` resolves, the path is cleaned and trailing
globs collapse (`rm -rf ~/` and `rm -rf /*/*`), and a `dd of=` operand or an
output redirection is read unquoted (`of="/dev/sda"`, `> '/dev/disk0'`). The
raw-disk prefixes include the macOS whole-disk nodes `/dev/disk*` and
`/dev/rdisk*`. `find -delete` is a dangerous delete. A command `find` runs is
classified like the same command run directly, so `find … -exec gofmt -l {} +`
is ordinary while `-exec chmod`, `-exec curl` and a program `find` cannot name
(`-exec {} ;`, `-exec "$TOOL"`) are dangerous. A `find` is refused as what it
amounts to when its expression — read with find's own `-a`, `-o`, `!`,
parentheses and commas — lets an action reach nearly every entry: a
`-delete`, unless `-maxdepth 0` or `1` bounds it, as `rm -rf` of its start
paths (`find / -delete`), and each command its `-exec` family runs with every
start path in place of `{}`, or past `-mindepth` every entry under it
(`find / -exec rm -rf {} +`). Only a real test narrows what an action reaches:
an option, an action, `-true`, a name or path pattern of only `*` and the
regular expression `.*` hold for every entry, a command's exit status may hold
or fail for every one, and what a negated test or an earlier `-o` branch
leaves is still nearly everything (`find / -path /proc -prune -o -delete`).
A find a real test narrows (`find / -name '*.tmp' -delete`), or one with
`-quit` or `-files0-from`, stays a dangerous operation.

A wrapper whose command cannot be extracted (`bash deploy.sh`, `su root`,
`chroot ROOT` alone) is opaque: ordinary dangerous approval rather than a false
hard block. Known residuals, each degrading to approval rather than passing:

- a program name decided at run time — a substitution (`$(printf rm)`), ANSI-C
  quoting (`$'\x72m'`), a variable (`$CMD`) or a decoded payload piped to a
  shell — is not identified. The shell's reading marks such a command opaque,
  so it is dangerous and asks; a decoded payload reaches a shell with no `-c`
  script, which is opaque too. Inside code the shell cannot parse
  (`execute_code`), only the field reading applies and arbitrary-code approval
  remains the bound;
- nesting is bounded: eight wrappers on one command and four nested scripts.
  Anything deeper is opaque. Proof stops at three levels and is refused beyond;
- the prover's reject lists match option words literally, so a write option
  inside a short-option cluster or an abbreviated long option (`sort -uo out`,
  `sort --out=out`) can be misread as a read. Proof releases a call only inside
  enforced isolation, whose writes stay within the workspace; elsewhere the
  mode's ordinary approval applies.

Two residuals pass rather than ask. A replaced value that lands in an argument
of a script a launcher runs (`xargs -I {} sh -c 'echo {}'`) can inject commands
no reading sees; only a replaced program word is opaque. A launcher outside the
table above (the coreutils multi-call binary's `--coreutils-prog=PROG`, or a
program that execs one of its arguments by its own convention) is read as an
ordinary program, so the command it runs is not identified. The floor cannot
refuse either; enforced containment or the mode's ordinary approval is the
bound.

Do not promote an ordinary dangerous operation into the hard floor without an
explicit product decision and focused tests.

## Arbitrary Code and Network Egress

- Arbitrary terminal or code execution asks for approval in on-request and
  smart modes, independent of a narrow dangerous-command heuristic. Full-auto
  may bypass ordinary approval but never the hard floor.
- A code-injection variable assigned anywhere in the payload — before a
  program, through `env` or `sudo`, exported, or set by `printf -v` — loads
  code into whatever program runs next: the loader's `LD_PRELOAD`,
  `LD_LIBRARY_PATH`, `LD_AUDIT` and `DYLD_*` library and framework paths, and
  the interpreter and shell startup controls `NODE_OPTIONS`, `PYTHONSTARTUP`,
  `PERL5OPT`, `PERL5LIB`, `RUBYOPT`, `BASH_ENV`, `ZDOTDIR` and
  `GIT_EXEC_PATH`. `PYTHONPATH` counts when the payload runs a Python
  interpreter, and `ENV` only when it runs an interactive POSIX shell (`sh -i`,
  `ksh -i`, or a shell reading commands from standard input), the only program
  that reads it; `ENV=production node app.js` is ordinary. Injected code runs
  inside whatever confines the program it is injected into. Under enforced
  containment — writes confined to scope, no network or credential escape, the
  same proof that lets smart mode run arbitrary code unprompted — it does not
  widen the blast radius, so it does not block the containment release. Host
  or otherwise uncontained execution treats it as a dangerous operation
  whatever the program, and approval follows the mode; full-auto keeps its
  documented bypass. In every case it never mints a reusable class, prefix or
  host rule, or declared bundle.
- Network egress is a first-class dangerous category. Detect common transfer,
  remote-shell, socket, and `/dev/tcp` or `/dev/udp` forms after wrapper
  unwrapping. Keep the classifier named and independently tested.
- Network egress is approvable, not hardline. Do not silently change full-auto
  semantics. Git traffic to configured remotes remains excluded to avoid
  routine approval fatigue unless the policy is explicitly revised. That
  classifier asks whether an operation is dangerous. Whether a command is
  given the daemon's network is a separate question, answered by the
  remote-service predicate (see Workspace trust and capabilities): `git push`
  and `npm install` receive the network where it is permitted, and still do
  not become dangerous egress.

## Smart Approval Triage

Smart-mode triage is below the hard floor and above the human prompt.
Historical approval contracts may reuse their earlier class grants. It uses a configured cheap role, not the run's coding model, and may
return APPROVE, DENY, or ESCALATE.

The judge receives typed `RunIntentSnapshot` evidence: the person's original
request, the complete preceding proposal within a bounded quotation budget,
and ordered later corrections. Workspace/source/work-key facts remain separate;
task summaries and system wake-ups are not human authorization. New version-3
snapshots do not infer permissions or prohibitions with word lists, sentence
splitting, or operation-class matching. Main follows the person's constraints.
Version-3 smart execution reviews writes and process operations even when an
older capability grant would otherwise bypass triage. Enforced containment is
the one exception, because it is layer 2 of the ordered funnel above and a
lower layer cannot override it: a call the runtime already proves harmless —
isolated, enforced, no egress and no credentials, or a declaratively proven
observation — runs without a judgement about whether it was asked for. Reviewing
those anyway made containment unreachable in practice, since every gateway run
carries model authorization; the funnel reported zero contained releases while
the judge escalated calls its own rationale described as read-only. A dangerous
operation, an explicit deny, an unclassified external effect, a write tool, and
every uncontained execution keep their review. The cheap judge resolves references, exceptions, timing,
and scope from that evidence; only an unchanged action/evidence decision is
reused. A clear applicable refusal is respected without
asking to override it; uncertainty escalates with the missing evidence stated.
The judge makes the smart-mode operation decision; a risk flag is a reason for
review, not proof that a separate human grant is missing. Explicit execution
restrictions and the control-plane safety floor remain independently enforced.
The provider request enables structured JSON output and always selects the
lowest latency tier declared by the resolved model, whatever reasoning the
configuration names: disabled when supported, then `minimal` or `low`, with the
compatibility-safe disabled request for unknown capabilities. Output and
timeout budgets still bound the call. Model-change validation sends this same
approval contract before activating the route and, when the model keeps
reasoning anyway, records the provider's proven disabled encoding (see
[Provider Runtime](provider-runtime.md)).
Version-3 review requires
all four decision fields; malformed or incomplete output is an auditable model
failure and falls back to the human, never a verdict inferred from prose. New
audit records identify the policy as `smart-v3`.

A version-1 `approval.response` event retains finish reason, response byte count,
output/reasoning token usage, and protocol status without storing model text or
private reasoning. Output-limit termination is unavailable even when the partial
text parses. Empty output, unexpected tool calls, malformed JSON, incomplete
decisions, and provider failures stay distinct from a valid `escalate` decision.
All unavailable paths still require human confirmation; transport success alone
never authorizes execution. Existing injected legacy judges remain supported
without fabricating response metadata.

Version-2 response diagnostics also retain full provider usage (including cache
reporting flags), model, role, latency, and a bounded restriction citation.
Historical missing usage stays unknown. The daily report separates Main,
approval, and maintenance usage rather than counting approval continuations as
judge calls. An incomplete action is not sent for a prefix-only review.
New provider-backed denials cite an attributed quotation and explain its
applicability to the actual effect. The runtime validates quotation provenance;
the model still decides meaning. Missing or fabricated restriction evidence is
an unavailable decision requiring human review, never automatic approval.
Necessary observation does not require verbatim command authorization, while
applicable restrictions, hard floors, and unanswered-approval parking remain.

An unanswered approval returns a typed Run pause: kernel records `waiting_user`
and stops the remaining calls before another model turn. The model cannot treat
a parked human decision as an ordinary tool failure and proceed with a variant.

Version-1/2 persisted prohibitions retain their original deterministic guards
when historical work resumes; upgrading the daemon does not widen old pending
permissions. Explicit human approval/rejection, ownership, workspace bounds,
and the unconditional safety floor remain control-plane facts.

Each human quotation is bounded to 16 KiB, with 48 KiB for the complete evidence
bundle and at most six ancestor quotations. Omitted evidence, including overflow
or unavailable ancestry, is marked incomplete at collection and prompt building.
A partial bundle cannot yield automatic model approval: escalation identifies
the missing context instead of silently ignoring a trailing restriction.

- APPROVE from smart triage may reuse a decision only within the same Run for
  identical action arguments, authorization evidence, and execution environment.
  It never grants a command class; changed user requirements invalidate reuse
  without needing a matching prohibition keyword. Host execution always gets
  fresh judgment; neither an earlier judge decision nor a stored grant releases
  a later smart host call.
  A human ask is narrower: it approves once, or records one explicitly offered
  run-local rule. Host escape, credential access, explicit-deny overrides, high
  risk, and unavailable triage never create remembered authority.
- DENY uses the user-rejection contract and must not trigger retry.
- ESCALATE asks the human.
- Missing judge, timeout, provider failure, malformed output, or an unknown
  decision always escalates. Triage never fails open.

At an uninvoked native call, a human ask gets a short grace period for an
in-place answer. Otherwise the exact call, paired prior results, pending request,
Run and checkpoint are atomically parked before releasing the worker. A shorter
configured wait or caller deadline uses that same boundary. Checkpoint failure,
an entered tool other than effect-free `clarify`, or opaque delegation retains
cancellable occupancy; `clarify` may checkpoint its exact question.
None may turn a control-store failure into an answer. A parked approval stays
answerable for seven days; a clarification follows its origin Run's resumable
state and ends on answer, cancellation or expiry. Answering atomically records
the decision and enqueues one exact continuation under the frozen scope and
current authority narrowing. An approval supplies a one-shot capability only
for the same regenerated action and stable workspace/environment/principal/
credential fingerprints; the floor and a current explicit deny still run first.
A committed human park releases execution-root occupancy as well as the worker.
The answer must reacquire the roots against currently executing work. Other
same-person work using overlapping writable roots since the checkpoint makes
cached pending calls non-dispatchable: Main receives a typed
`resume_revalidation_required` result and reassesses the affected facts before
submitting fresh calls through normal policy. Missing activity evidence also
requires revalidation. Completed call/result pairs and clarification answers
remain intact, and existing unresolved external effects keep their claims.

Reachability is not inferred from keyboard silence or a 24-hour account-age
window. A live process heartbeat counts as attended even when the IM route's
latest delivery is unhealthy. Without a live process, no routable IM account or
a latest IM state of `pending_session`, `failed`, or `sent_unconfirmed` selects
the unattended resource budget. Defaults are 30 minutes for
`agent.approval_wait` and 30 seconds for
`agent.approval_wait_unattended`; the caller's remaining deadline may shorten
either. These budgets bound legacy waits, while checkpointable asks release
their worker after the grace period. A rapid park is therefore intentional
resource release, not rejection or expiry: the request remains answerable for
seven days and a later answer resumes the task. If the caller already expired
or a legacy ask has no budget, an `approval.skipped_no_budget` event records it
and work pauses without creating a misleading row. Explicit Run cancellation
expires a pending request; daemon or waiter loss parks it. On restart, an approval
already answered but not yet consumed gets one idempotent queued continuation.
None of this changes the OUTCOME contract: unanswered is never rejection and
never approval.

Daemon-generated continuation prose is not a user prohibition. New Run start
events freeze version-3 approval intent, including the preceding proposal shown
to the person. Exact-parent, same-workspace/root continuations retain bounded,
attributed human requests and accepted-proposal context, prohibitions, and later
user requirements. This supplies judgment evidence, never inherited grants.
The judge weighs scope and effects rather than command-word matches, and explains
which human evidence applies or which execution permission remains missing.
Version-1 evidence remains readable without fabricating missing proposals. Missing or
unsupported historical intent fails closed to confirmation. This evidence never
grants new execution authority or replaces the current approval mode.

Foreground command cancellation terminates the managed Unix process group.
After cancellation, output draining has a 250 ms bound before readers close;
detached descendants cannot keep a Run waiting for pipe EOF. Normal completion
still drains before reaping so short commands retain their complete output.

Treat command text as untrusted data in the judge prompt. Strip irrelevant
comments and delimit the command rather than interpolating it as an instruction.

`/diag` reports the last 24 hours of triage outcomes from a durable, bounded
projection. The projection stores the run/tool-call identity, outcome, risk,
authorization assessment, grant key, provider route, latency, policy version,
rationale, and a short redacted provider error. It never stores the command,
arguments, prompt, or credentials. Records are retained for 14 days. A failure
to write diagnostics must not block approval, so the foreground write has a
short deadline and is best-effort. The judge has a five-second foreground
deadline; its 4096-token output budget accommodates providers that still spend
hidden reasoning tokens before returning the compact structured verdict.

The declarative read-only catalog may bypass a human only when both the command
shape and the credential-bearing tool profile are recognized. Trusted workspace
status does not turn local observations such as `git diff`, `rg`, or `jq` into
credentialed operations. Unknown commands, scripts, and mutating cloud calls
remain gated.

Observation proof uses a dedicated quote-aware shell AST parser; it does not
reuse the broader dangerous-command tokenizer. Static pipelines and quoted
provider format expressions can therefore be proved without mistaking `|`,
parentheses, or redirections inside quotes for shell control operators. Dynamic
expansion, command/process substitution, heredocs, assignments, opaque scripts,
unknown global options, privilege wrappers, and writes outside `/dev/null`
remain unprovable and continue through normal approval. Tool-specific global
flags are skipped only from a fail-closed catalog, and credential-bearing files
or environment reads never become automatic merely because a command is
otherwise read-only.

The prover names the program through the same identification as the hard
floor, so it can never prove a program other than the one every other analysis
names. Each wrapper it accepts (`command`, `timeout`, a proxy-only `env -u`, a
bash-family `-c`) must also reach the same inner command under the proof
grammar, or the call is unprovable. Proof vouches for the exact catalogued
name of every wrapper and program: a case-sensitive filesystem may hold a
different `LS` or `Env`, so those are unprovable although the deny side reads
them as the programs. Words are read as the shell passes them, escapes
removed, so `sort -\o out` and `gh api -\X DELETE` are the write and the
delete they are. Any assignment keeps a command unprovable, including one a
builtin makes (`printf -v PATH …`, `command export PATH=…`): `PATH` chooses
the binary a catalogued name runs and a code-injection variable loads code
into it.

## Failure Recovery

Dispatch retry classification consumes trusted built-in registration metadata,
so read-only composite tools receive the same treatment as their constituent
reads. Metadata-free compatibility backends retain the conservative legacy
catalogue; unknown and external tools never gain blind retry from model input
or an external read-only declaration. Replay safety and effects are separate:
trusted built-in runtime attribution has effect class `run_bookkeeping`, which
does not count as target execution or close implicit continuation. This includes
Skill activation, viewing and fallback, never their scripts or publication.
The classification grants no approval, parallelism or blind retry. Missing,
external and historical unclassified effects remain conservatively guarded.

Completion precondition failures are typed separately from storage failures.
One failed `finish_run` may be retried after a genuinely changed plan or a
successful `verify` call; unchanged retries, rejection, and unknown failures do
not unlock it. Successful completion remains final and the retry is bounded.
An identical rejected `update_plan` is retried only after its requested
completed steps receive new successful bound verification or the durable Plan
version changes. A check for a sibling step that the update did not complete
does not unlock it; the Plan store still revalidates every transition.
Closing a work unit with declared required verification is rejected before
commit if its checks have not passed. The plan and evidence window stay open
so Main can verify and resubmit the same completed snapshot. The rejection
derives each blocked step's next action from its bound evidence: a step with no
bound check can be verified directly with its server-issued `step_id`, including
while pending; a stale or failed bound check is rechecked with `replaces` and
keeps its obligation. A named criterion or target without `step_id` is standalone
and cannot satisfy an unrelated Plan step.
Every plan-bound check records the server-issued step id. Main can select it
explicitly. For unnamed legacy checks, the active required step wins; otherwise
the earliest pending required step in the active work unit is selected from
durable plan order. Selection never matches model prose and never crosses
the next work-unit boundary. One required step represents one distinct acceptance
gate; the command is evidence for that step rather than another gate.
For that one durable step, criterion, target, cwd, and declared dependency set,
the latest attempt is the effective verdict; earlier failures remain immutable
evidence. Historical bindings whose runtime target is an issued step id are
projected through the same rule. Evidence for another step or standalone target
cannot satisfy the obligation.
`verify` exposes criterion, target, replacement metadata, and method dependencies
as flat arguments. A compatibility adapter lifts the previously published
`check` object before strict validation. A replacement keeps criterion, target,
and cwd; omitted dependencies inherit, while an explicit dependency list records
the corrected observation method. It retains the original step identity while
that step remains in an active work unit, even if Main changes its step status.
If the step was replanned away, or its owning work unit is already frozen, Main
must explicitly select an owned current `step_id` for the recheck; Plan position
never silently reassigns it. Standalone replacements remain standalone even
when a Plan exists, unless Main explicitly selects its step. Source references,
completion verdicts and freshness checks share the exact Work Unit evidence
window. Later observations outside that window cannot replace its proof, and
every effective check must remain fresh against its own declared inputs.
Failure feedback keeps the same open obligation in place;
successful replacement feedback identifies a separate retry step as redundant.
When a role has `verify` and a sufficiently large action envelope, two calls
inside the existing hard ceiling are reserved for verification. A bounded
notice asks Main to finish the current scope and reconcile its plan first.
This does not widen tool availability, execution authority, or approval grants.
Calls excluded by a strategy, completion reserve, or lifecycle attempt cap
retain their call IDs and receive paired typed `not_dispatched` refusals.
They consume no dispatch claim or action budget. A refusal cannot become an
empty-response completion: one bounded correction allows honest closure, and
unresolved admission without a recorded outcome remains resumable.

`verify` records deliberate checks; ordinary `terminal` calls remain command
evidence. A check runs offline unless its command reaches a remote service or
it sets `network: true` (see Workspace trust and capabilities). The check's
network decision is part of its evidence: a check that passed without network
although a network grant permits it still passes, and when its command can
fetch dependencies (a build or a test, not a vet or a formatter) the `verify`
result the model reads, the Run's verification summary and the person's closing
notice say so, and that the criterion can be re-checked with `network: true`
when it depends on remote data. A replacement check bound to the original obligation
may set `network: true`; the networked attempt is then the effective one.
Missing structured verification must not be described as proof that
no command ran. A recorded refusal or `process.started=false` is a blocked
check, not a failing test; missing dispatch/exit facts in typed observations
also remain blocked. Observed nonzero exits stay failed, and historical rows
without dispatch facts retain their historical meaning. File tools and terminal
tools share the exact lease's temporary directory; other leases and credential
state directories receive no implicit file-tool access.
On supported POSIX hosts, verification uses Bash with `-e`
and `pipefail` so an unhandled failing command or pipeline cannot be hidden by
a successful footer. Explicit shell conditionals can handle expected failures;
the runtime does not infer verification success from output prose.

Watcher static validation distinguishes invalid shell syntax from an
unsupported observation shape. A syntax error permits corrected input before
execution; corrected commands still pass the same read-only proof, approval,
environment binding, and preflight gates.

A tool error is evidence, not an automatic stop or a reason to repeat the same
call. Before retrying, inspect the relevant cwd, project markers, environment,
authentication state, runtime, and package-manager/workspace configuration.
Change the next command only after identifying a plausible cause. Generic tools
must not embed project-specific environment overrides such as forcing a Go
workspace mode for every repository.

Long-running Agent CLIs are not ordinary shell commands. `terminal`, `verify`,
and `execute_code` accept a vendor-neutral `execution_class`:

- `standard`: the ordinary tool default, capped at 15 minutes;
- `long-running`: 30-minute default, two-hour hard cap, 10-second heartbeat;
- `interactive`: one-minute default, 15-minute hard cap, one-second heartbeat.

An explicit shorter timeout still wins. The profile is generic execution
metadata; do not solve one vendor's timeout by hard-coding its binary name in
execution middleware.

## Execution engine

`docs/execution-engine.zh-CN.md` owns the mechanics: environment snapshots and
fingerprints, run scratch, the tool environment profile catalog, the five state
primitives, and the sandbox-gated failure classes. This section fixes only the
cross-cutting rules that other domains must not violate.

### Three layers, and the floor between them

Execution policy is layered, and each layer has exactly one home:

- **L1 built-in behaviour data lives in Go source**, versioned with the binary
  and reviewed in a pull request: the reusable-grant floor
  (`tools/grant_floor.go`), the hard-floor protected roots and dangerous/egress
  binaries (`tools/middleware.go`), the prefix-wrapper grammar and loader
  variables (`tools/command_identity.go`), failure-class rules
  (`tools/tool_errors.go`), and the tool environment profile catalog. These
  define behaviour, not preference. Do not move them into configuration and do
  not add a per-vendor branch in engine code — a profile is data in the
  catalog.
- **L2 remembered rules are bounded and visible.** New interactive approvals
  can reuse a bounded rule only within the current run. The daemon publishes
  both its opaque enforcement key and its human-readable rule; clients display
  that exact rule and never reconstruct authority from option prose. Durable
  `approval_grants` and `execution_capability_grants` remain the compatibility
  and administrative ledger for historical or explicitly managed grants; all
  such entries must be listable, time-bounded, and revocable.
- **L3 user configuration stays small.** `exec_sandbox` keeps `enabled`,
  `required`, and `allow_network`. New execution behaviour does not earn a new
  configuration key.

**L1 is the floor on L2.** A class may be remembered only when it actually
bounds what can run: payloads that reach an interpreter or shell, a
general-purpose exec facility, an irreversible operation, shell control flow, or
any redirection/substitution/expansion/wildcard are approvable once and never
remembered. Ordinary dangerous operations remain approvable classes — that
distinction is the same one the hard floor draws. A boot-time review re-applies
the current floor to already-stored keys and withdraws what no longer qualifies;
it only ever removes authority and is idempotent.

### One execution entry

`tools.Execute(ctx, ExecutionRequest, args)` is the only place a sandboxed
command is constructed, asserted by test. `terminal`, `verify`, `execute_code`
and `watch_external` all pass through it. `background: true` shares the same
prepared material (environment binding, scratch, tool profiles) but is not
routed through `Execute`: a detached process has no streamed output or exit code
to return. It is bounded by a ceiling so a wedged process cannot hold its
process slot, scratch, and copied credential state until the daemon restarts,
and a durable wait belongs in `watch_external` instead.

`SandboxPlan` is serializable and carries the environment binding (snapshot id,
generation, scratch handle) plus the filesystem view; `ProcessMaterial` carries
the values and has no exported fields, no `String`, and no JSON marshaller, so
an environment cannot be logged or shipped by accident. Execution policy travels
on the request (`ExecutionScope.SandboxPolicy`) rather than being read from a
process global at execution time.

Cancellation is part of the same boundary. Tools that can block on I/O or spend
meaningful time in local computation implement `tools.ContextTool`; the
dispatcher supplies the authenticated run context, never a context accepted from
model arguments. Loops must check it at bounded intervals. A tool must not use an
unbounded approximate-search fallback: `patch`, in particular, accepts a unique
exact or bounded same-line-count normalized match and otherwise fails without
writing. `/stop` records a cancellation request, while terminal run state is
owned by the goroutine after the execution body exits.

Known optional typed properties supplied as JSON `null` are normalized to omission
at the argument adapter, recursively. Required typed properties and typed array
elements reject null; unknown closed-schema fields still reject, and open
extension data retains null. No provider-name branch repairs argument spelling.

### Tool environment profiles

A profile declares what host state a command-line tool needs. The engine
understands five primitives and nothing about any vendor: `copy_in` (bounded,
selective, staged then atomically renamed), `map_ro`, `map_rw`, `map_rw_at`
(a writable state directory bound OVER a host path, for state whose location the
tool does not let us configure — the AWS SSO token cache derives from `HOME`),
and `env_redirect`. `write_back` is a reserved slot and is not implemented, so a
permanent identity change (`gcloud auth login`, `docker login`, `gh auth login`)
does not persist from inside the sandbox and belongs in approved host execution.

A dependency between profiles is either inherent (`RequiresProfiles`) or proven
(`ConditionalRequires`, which pulls a credential helper in only when the tool's
own configuration names it). Assuming a dependency is a defect in both
directions: it breaks the tools that do not need it and widens credential
exposure for commands that never touch that provider.

Environment dependency analysis distinguishes non-executing interpreter modes
from script execution without changing read-only proof or approval authority.
A leading `VAR=value` assignment changes what a program sees, not which program
runs, so `TZ=UTC date` or `CGO_ENABLED=0 go build` prepares exactly like the
bare command. The program is read from the shell's own parse through the same
identification as the hard floor; the field reading is only a fallback for a
payload the shell cannot parse. A value with a path or spaces
(`KUBECONFIG=/x kubectl …`, `FOO="a b" aws …`, `PATH=/x go build`) therefore
cannot move the program word. An inline selector the snapshot cannot resolve
(`AWS_PROFILE=x aws …`, `env -u AWS_PROFILE …`) is a configuration override,
including inside a `-c` script: it keeps that tool's conservative state
requirement without pulling in the host inventory.
Mount-backed state may be conditional on bounded, selected configuration keys.
Literal command options and snapshot variables select INI sections. A partial
long option that could select a configuration section retains conservative
state requirements: without the CLI's complete grammar it cannot prove which
section an abbreviation selects. `--` ends option interpretation. Opaque
scripts, dynamic selectors, configuration overrides and unreadable/malformed
configuration retain conservative requirements. Proven static authentication retains
read-only state without synthetic roots or writable cache mounts. Preparation
cache keys include the resolved mount requirements. Backend inability remains
an explicit non-dispatch refusal, never permission to drop required state or
escape to host. Model-facing execution notes use the exact Run's frozen policy;
an explicit isolation refusal is a policy restriction, while an occupied host
lane is an exact resumable execution wait.

Mount-backed state follows the backend that will enforce the call; preparation
asks the backend (`MountsToolState`) rather than the platform. Bubblewrap binds
a private, seeded directory over each `map_rw_at` path inside a synthesized
state root, so the host copy is never written. Seatbelt (macOS) cannot mount,
so each such requirement becomes an in-place write allowance on the real host
path: exactly the declared subpath (`~/.aws/sso/cache`, `~/.aws/cli/cache`),
created inside the declared state root when missing, one real directory at a
time. Never the state root itself (`~/.aws`), never the read-only configuration
and credential files, and never through a symbolic link below the root,
wherever it points: a link inside the root can be wider than the declared
subpath, and whoever can repoint it steers later grants.

Only a cache the profile declares flat (`MapRWAt.Flat`: files alone, as the
AWS SSO and CLI caches hold one JSON file per token) is written in place; a
requirement not declared flat is refused at planning. Its repair is then a
listing of one directory the sandbox cannot replace, so nothing a sandboxed
process exchanges underneath it can redirect the repair. What a host tool may
meet in such a cache is a single-link regular file. Four layers keep it so:

- The seatbelt policy refuses, by the entry an operation names: creating a
  symbolic link, FIFO, socket, device node or directory in the cache (made in
  place, renamed in or cloned in), and any hard link there (a hard link to a
  regular file is itself a regular file, so the type rule alone would let one
  alias a file elsewhere).
- A policy cannot refuse what an operation does not name: an exchange
  (`renamex_np` with `RENAME_SWAP`) of a workspace link, pipe or directory with
  a cache file. So every command that writes a cache in place is guarded on
  both sides. Just before it starts, whatever the cache already holds that a
  host tool must not meet — from an earlier run, another writer or anything
  else — is moved out and the command runs; planning never refuses for it.
  From a deferred step that runs however the attempt ends (success, failure,
  timeout, cancellation, or a panic after the start), the engine stops the
  command's process group, then every process still running under that one
  command's sandbox: each command's seatbelt profile carries a per-command
  canary (`com.selfmind.exec.` and 24 random hex digits: a unique mach global
  name only that profile may look up, with no service behind it), and
  `sandbox_check` over this user's processes finds every process under it,
  whatever process group or session it moved to (`setsid`, a daemon). Each pid
  is checked again right before it is signalled. The check is confirmed first:
  a probe started under the same canary must be found, and a process the
  check cannot answer for is never taken for one outside the canary. A stop
  that cannot be confirmed is reported as such in the tool result and its
  evidence, never as success. Only then is the cache listed again.
- The daemon sweeps at startup, before any command: it stops every process
  still under a canary recorded by a command that never finished (the daemon
  crashed or was killed under it, or the command's own stop could not be
  confirmed, which keeps the record) and repairs only those commands' recorded
  cache write sets. Versioned, bounded private records are synced before any
  command effect; failure to save the record prevents execution. Changed roots
  or ancestors and unsupported records are reported without widening scope.
  Legacy empty markers permit process cleanup only; missing cache scope is
  reported in logs and `/diag execution`, never inferred from the current
  environment. An unresolved repair keeps its record for recovery. Records are per daemon — named by its data directory, since daemons
  whose data directories sit side by side share a runtime root — and only a
  record in the generated canary form is acted on. On shutdown, whatever the
  reason, the daemon waits up to 15 seconds for interrupted commands to
  finish their repair before exiting.
- The runtime's records (`<runtime>/host-state/`: canary records and the
  quarantine) are read-only to every seatbelt command, even one whose writable
  root contains the runtime root.
- A repair moves every entry a host tool must not meet (link, special file,
  hard-linked file, directory) into a quarantine under the runtime root
  (`<runtime>/host-state/quarantine/…`, never under the tool's state root). A
  cache path that is no longer a real directory, or a cache with more than
  4096 entries, is moved whole and the tool recreates it. An entry that
  vanishes while the cache is listed (a writer's temporary file renamed into
  place) is skipped; it never makes the repair move the live cache. An entry
  that cannot be moved is removed when it holds no data of its own (a link, a
  special file, a hard-linked file) and otherwise left in place and reported.
  The tool result says what was repaired and what was stopped, listing at
  most ten entries ("and N more"), each path quoted with control characters
  and brackets escaped; the call's evidence records the repair
  (`tool.environment` with `state_repairs`, at most twenty listed and the full
  count). Quarantined copies can hold the operator's tokens, so each
  quarantine is deleted after seven days or once twenty newer ones exist,
  swept at startup and after every repair; `/diag execution` shows the
  quarantine, its retention and the last startup sweep.

The guarantee is therefore: an entry a sandboxed command plants is removed,
and every process it left under its sandbox stopped, before the call
returns. A host tool can meet a planted entry only while such a command is
running (the window is the command's duration, up to the scan after it), or,
if the daemon dies under a command, from then until the next startup sweep —
the command's processes keep their write access until that sweep stops them.
If the stop cannot be confirmed — the probe is not found, `sandbox_check`
cannot answer for a process, or a process under the canary survives repeated
kills — the call says so instead of claiming the cache is guarded. On Intel
Macs (darwin/amd64) the stop has never been exercised, so it is not attempted
there: leftover processes are not stopped, every in-place call says the stop
could not be confirmed on this architecture, and no record is kept for a
startup sweep that could not act on it either; only the before and after
scans apply. What
the sandbox can always leave is ordinary regular-file content (see the
poisoning caveat below). The cache directory itself is an authority boundary
like a writable root: it cannot be removed or exchanged, and its mode, flags,
owner, extended attributes and ACL cannot be changed, while the files inside
stay writable.
This is deliberately weaker than the Linux form: the operator's real cache is
written and its other contents stay readable, so a sandboxed command can
refresh, replace or poison cached tokens there. Only a call whose operator
credentials the policy allows gets the allowance; with credentials withheld the
host cache stays read-only and visible (seatbelt cannot hide it either), rather
than becoming the private empty cache Linux provides. A requirement subpath
writes cannot express (a cache that resolves outside its root, a non-directory,
a write that would cover read-only state, no declared root) is refused with
that reason, before approval when possible and at dispatch otherwise; it never
falls back to the host. `/diag execution` lists, per credential profile on this
host, whether it is sandbox-runnable under the configured policy on this
platform (a disabled `exec_sandbox` runs everything on the host) and which host
paths a sandboxed run writes in place or shadows with a private overlay — paths
only.

Credential access is a POLICY decision, never a catalog one. A trusted workspace
may use operator credential state; an untrusted one needs the `credential:read`
capability, which is asked for before execution and only when the command's
program set actually matches a profile that declares operator credentials.
Declining is not an error: the command still runs with an empty state overlay and
the tool reports its own "not logged in", which is the honest diagnosis.

### Durable execution identity

As of 2026-08-01, new watches carry a versioned, secret-free
`executionenv.Binding` derived from the creating run's lease. It freezes the
environment profile, credential references, workspace trust level, effective
capability names, and the provenance of each capability together with the
snapshot id/generation and three fingerprints. The same binding is the future
Runner request contract; node-local secret values remain in
`Snapshot`/`ProcessMaterial` and are never serialized.

Preflight and background polls resolve the same binding. An in-process exact
snapshot wins even if the daemon later samples another shell; after restart, a
snapshot id is only an index and all three fingerprints must still match before
the current environment may be rebound. Capabilities are frozen at
registration: later grants cannot widen a running watch, while trust withdrawal
or a persisted grant's revocation/expiry stops it before the next command. A
one-shot registration approval is bounded by the watch deadline and can be
withdrawn by cancelling that watch. Pre-binding watches remain on the legacy
path so an upgrade does not strand already-running work. The binding also
freezes the sandbox policy (enabled, required) the registering call ran under
(`sandbox_policy`), the boundary its approval resolved (`approved_boundary`,
`isolated` or `host`), and the in-place credential writes that approval covered
(`host_state_writes`); a poll is described by the binding alone, so the
execution engine derives its policy from it, whoever runs the poll. Network is
not frozen: a poll's network is its binding's `network:shared` capability,
checked against the policy in force before every poll. Every poll runs under
the frozen policy narrowed, never widened, by the current configuration, so
disabling the sandbox after registration cannot move a poll onto the host.
Every poll is also pinned to the approved boundary like any approved call: one
approved as contained that the backend can no longer enforce is not dispatched
(`sandbox_boundary_unavailable`, an environment failure) rather than run on the
host, and one approved for the host is refused once the policy requires
isolation. A host pin survives the operator merely enabling the sandbox: the
approval covered the host, so only `required` narrows it, and the watch must be
registered again to be approved for isolation. Every poll is held to the
recorded writes. All three fields are optional in the stored JSON and absent
from a row that never recorded them: a watch registered before the policy was
recorded follows the current configuration, one that recorded no boundary
resolves under the policy alone (a vanished backend then means the host, as it
always did), and one that recorded no writes covers none, so such a row's poll
is refused wherever the configuration makes it write credential caches in
place. A boundary this binary cannot interpret refuses the poll rather than
being guessed at, and a network flag an earlier version stored in the policy is
ignored.

A `watch_external` check outlives its run and survives restarts, so it records
the environment identity it was registered under (snapshot id, generation, and
the three fingerprints — never values) and verifies it before every check. The
failure mode being prevented is not an error but a check that SUCCEEDS against a
different account. A watch whose identity no longer matches is stopped with
`environment_changed`; watches registered before identity existed are
grandfathered rather than stranded.

A successful host preflight records its actual shared-network execution boundary.
When the host approval already covers that boundary, the watcher retains it as
registration-scoped authority expiring at its deadline; this also applies in an
untrusted workspace. It creates no workspace grant or credential permission.
Trust- and grant-derived capabilities keep their existing revocation rules.

Registration also proves that the frozen check is runnable before ownership
moves to the daemon. The first check must exit 0 and emit a non-empty bounded
state. Non-zero exits, timeouts, missing commands, and check-definition output
such as a swallowed Python traceback are rejected in the foreground. A clean
exit whose output matches no terminal/target pattern is the normal pending
case and may register. This contract is deliberately stricter than an ordinary
interactive status probe: the background loop has no model available to repair
the command later.

The frozen polling command is observation-only. It must either match the
declarative read-only command catalog or be an operator-approved, content-hash-
pinned observation script; shell writes, mutation subcommands, unknown clients,
and mixed observe/mutate pipelines are refused before registration. This limit
does not prevent final state recording: writeback is a separate, one-shot
watch-finalization run after the external observation reaches a terminal state.

The active-turn guard recognizes repeated observation of external state from
the same read-only command catalog, plus a guard-only table: provider-native
waits such as `kubectl rollout status`, `kubectl wait`, `argocd app wait`, and
`gh run watch`, and the AWS and Azure CLI read-operation conventions. The table
only restricts; its forms still require approval. A loop over one unchanged
target, including a finite retry loop, must move to a durable watcher, a single
provider-native wait, or an actionable handoff. A bounded read of distinct
targets remains an ordinary batch. The guard does not classify a mutation as a
status check.

Successful registration is itself a trusted lifecycle handoff. The kernel
records a structured `waiting_external` outcome and ends the foreground turn
without asking the model to call `finish_run` or produce another response.
Multiple watcher registrations in the same tool batch are allowed, while later
non-watcher calls are held at the handoff boundary. The durable polling interval
therefore has no active person run and no model-token cost; a new user task can
start immediately. Terminal finalization is still real daemon work under the
one-active-run policy, so clients label that short interval as background
finalization and queue concurrent input honestly instead of displaying a
foreground elapsed-time clock. During finalization the CLI shows elapsed time,
accepted plan progress, and a bounded action count in its status line; it does
not replay background tool output into the transcript. An interrupted
finalization remains visibly unfinished.
Terminal watch products are idempotent and compensated on ordinary worker
ticks as well as startup. A terminal group has one winning member that owns
the child Run and notification; non-winning members settle their own bookkeeping
without emitting a second result. A crash after enqueue but before the winning
member's finalized mark therefore does not leave a permanent backlog or create
a duplicate child.

### Failure classification

"Was there a sandbox" is a precondition, not one cue among many. A command that
ran isolated is classified for filesystem denial FIRST
(`sandbox_fs_denied`, `credential_state_readonly`), before the generic
taxonomy, and shell-level rejections (exit 2/126/127) are never sandbox
denials. A denied write to a tool's own state directory must never be reported
as an authentication problem, and must never carry guidance that forbids the
only correct remedy.

Event `error_category` comes from the structured class the tools layer already
appended, not from re-reading the prose hint. Do not add a second classifier
that parses enriched error text.
An active-turn polling block or repeated no-progress refusal is a
`policy_redirect` with `effect_state: not_dispatched`, not a malformed model
protocol call. Daily reports count these separately by `error_code` so model
quality comparisons do not absorb runtime policy decisions.

A command that ran offline, in a workspace that permits network, and failed for
want of it is a tool failure that keeps the class its text already carries
(`sandbox_no_network` for an isolated command; the typed result sets no
category of its own) and adds the code `network_withheld_by_policy`, retryability
`corrected_input` and phase `execution`. It is not a refusal and not a
non-dispatch: the command ran, so no `effect_state` is claimed and recovery
policy does not treat the retry as an exhausted strategy; the one allowed
correction is the same command with `network: true` (`execute_code` takes the
same parameter, so its retry is networked too), after inspecting any state the
command changed before it reached the network. An error that already carries
`not_dispatched`, `user_decision`, `rejected` or `human_wait`, or that is a
hard-floor block, is never re-typed this way, whatever words its rationale
uses.

Human waits and human decisions use the same structured class. A parked
approval reaches the model and the event stream as `human_wait` (code
`approval_parked`, `effect_state: not_dispatched`); a refusal by the person, by
safety triage, or by a capability policy is `rejected`, with a code naming the
source (`approval_rejected`, `triage_denied`, `capability_denied`,
`approval_scope_rejected`). Neither is a tool failure: the recovery policy does
not count them against the strategy, and the `operation rejected` wording stays
the cross-package do-not-retry contract kernel matches.

## Known Gaps

- The CLI-identity guarantee is complete for sandboxed execution and for host
  commands that stay in the daemon's process tree or process group. It is not
  complete for an approved host-mode command that deliberately leaves them (a
  double fork with `setsid` on macOS, or a request to a service manager on any
  platform); see "The local control channel and CLI identity".
- **The original exploit is not closed on Linux with a shared network, nor for
  an approved host-mode command on any platform: bare-TCP identity naming.**
  While `gateway.token` is unset, a request over the TCP listener may name any
  platform identity, and the daemon does not ask it to prove it. If it names an
  IM account already bound to the person (the platform and the user id the
  person's own Telegram or WeChat account has), the daemon resolves it to that
  person and serves what an IM message from that account may do, which includes
  starting work, answering pending approvals, and changing the approval mode:
  `/mode full-auto` sent that way succeeds (it is not CLI-reserved, and this is
  the original exploit, measured). Only the controls reserved for the verified
  CLI stay refused (`/v1/message` as `cli`, workspace trust, `--add-dir`, model
  changes and the rest in "What needs it"). A macOS sandbox
  cannot reach the listener at all, which is why it is closed there; a
  bubblewrap sandbox with a shared network, and a host-mode command, can. A
  configured `gateway.token` closes it for any caller that does not hold it,
  which a confined command does not (its environment drops `SELF_*` and the
  configuration is private). The mitigation holds only while the token lives
  there, in the private configuration or the daemon's own environment: a token
  also written to a readable dotfile, a shell rc, or a repository, where a
  confined command can read it, hands that command the identity back. Making the listener refuse non-CLI identities
  without a token, and generating a token by default, changes what existing IM
  and HTTP integrations must send; it is a separate work package (WP-L2), not
  part of this one. Until then set `gateway.token`, or
  `exec_sandbox.allow_network: false`. The sandbox can no longer stop the gateway
  or read its detail, but that is not this.
- Crash logs under `~/.selfmind/crashes/*.log` are not private: a confined command
  can read them. They hold stack traces and process metadata, not credentials, but
  they can name paths and models; delete them or keep the home out of the writable
  roots if that matters.
- Pinning a directory stops it being moved or removed, not changed in place: a
  confined command that may write the data directory's parent can still change
  the mode or flags of the directories above the daemon's state, fill the disk,
  or delete what is not private, which can stop the daemon from working but
  gives the command neither the state nor the person's authority. Keep the data
  directory out of the writable roots a tool runs with, or accept that risk
  with the home-directory workspace.
- The data directory beyond the private entries (per-person memory databases,
  `cron.db`, attachments, tool output, prompts, skills) and a second daemon of
  the same user in another data directory are not private to a confined command:
  the former is user content a trusted home-directory workspace can read and
  write, and with a shared network the latter's socket inside its own data
  directory is reachable and, because the command is not that daemon's
  descendant, answers it as the person's CLI (the shared `/tmp` fallback
  directory is private). Run one daemon per user, or
  `exec_sandbox.allow_network: false`.
- A durable `EnvironmentLease` is materialized once per run and reused by
  replay of that run. It stores workspace/profile identity, capability names,
  credential references, and a non-secret principal fingerprint only. A
  continued task creates a new run and therefore a fresh lease.
- Missing local workspace environments finish the attempted run as
  `waiting_user` with reason `environment_unavailable`. Automatic wake-up when
  a compatible remote environment is later installed remains follow-up work.
- Egress classification currently covers exec tools; full-auto retains its
  documented bypass for egress in single and parallel Runs. Cross-Run
  protection comes from the external-effect claim, not from a parallel ask.
- The remote-service predicate reads command names and subcommands, not what a
  program will do. A build that downloads on a cold cache (`cargo build`,
  `mvn test`, `gradle build`), a script, or a tool the table does not know runs
  offline first. If it fails for want of network, the typed
  `network_withheld_by_policy` result (or the one-time ask where nothing
  permits network) names the retry, `network: true`. Extending the table is a
  reviewed L1 data change in `network_commands.go` (the offline notice's table
  of commands that can fetch is `network_dependency_fetch.go`) with a focused
  test, not a configuration key. `make test` may run any of those programs,
  which a command line cannot show, so it adds no offline notice.
- Workspace skills are excluded for untrusted active workspaces, skill roots
  come from `ExecutionScope` rather than daemon cwd, and credential-shaped
  environment passthrough declarations fail closed. User-installed trusted
  skills remain available.
- Credential-aware private HOME views and `credential:read` approval are
  implemented through data-driven tool profiles. Refreshed tokens written by a
  CLI stay in the run/watch overlay, except mount-backed caches under seatbelt,
  which are written in place on their declared subpaths; permanent login
  changes still require an approved host flow because `write_back` is
  intentionally not implemented.
- Durable execution falls back to a private per-binding toolchain cache when a
  person-level cache cannot be materialized. Cache availability must never turn
  a valid watcher into an environment failure.
- `/diag execution` reports backend/network policy (a permission, with what a
  command needs to receive it), workspace trust, active capability names, the
  current run's lease/profile and recorded network decisions, and only the
  count of hidden credential references. It never displays credential names or
  values.
- The cron store records no creating person. A job stored before the cron tool
  bound its endpoint and naming another person's existing account still runs
  as that person, and a job whose account no longer exists has nobody to tell;
  recording the creator on the job is the follow-up that closes both.
- Ownership decisions about older cron rows live in control.db while the jobs
  live in cron.db. If control.db is recreated, the decisions and the account
  history behind them are lost: the first command-line person on the new
  database is then the only account holder, so each older row with no receipt
  is decided as theirs. Keeping the decision with the job needs a cron.db
  column, which belongs to the same creator-recording follow-up.

## Required Tests

Changes in this domain need focused coverage for:

- active workspace and allowed-root enforcement;
- symlink and wrapper-command behavior;
- program identification under every respelling (case, path, escape,
  assignment, wrapper, script) agreeing across the floor, the dangerous
  heuristic, observation proof, environment analysis and proxy detection, and
  under every option form a launcher's documented parser accepts;
- remembered exec keys equal to the class the prompt names, and the
  containment credential axis equal to what preparation attaches;
- hard-floor precedence over every mode and grant;
- rejection versus safety-block semantics;
- answers, commands, and choices accepted only from the person's own input,
  through the real scheduled-job path, under every casing and spacing;
- schedules bound to their creator's own account and direct chat, at creation
  and at every fire, older rows attributed only from evidence and decided once
  so a work-history reset never moves them, adoption only from the local CLI,
  and a refused job turned off and still visible to its owner;
- explicit Thread and Run ids from another person reading exactly as unknown
  ids on every path that accepts one;
- code-execution approval;
- three-axis containment and the observation-only catalog;
- the boundary decided before any ask, judge or person, and execution pinned
  to it: an approved isolated boundary the backend cannot enforce at dispatch
  is not dispatched, never run on the host;
- in-place state writes on a non-mounting backend confined to declared
  subpaths inside their state root, with read-only configuration staying
  read-only;
- reusable approvals bound to the in-place write set they were given for, and
  dispatch refusing writes the releasing decision did not cover;
- exact-run authorization isolation and byte-identical reuse;
- egress classification;
- network as permission versus need: a stored or run grant, workspace trust
  and the operator policy permit and never decide; the remote-service predicate
  and the explicit `network` request do, across package managers, VCS remotes,
  infrastructure tools, wrappers and compound commands, with local commands,
  quoted mentions and a disabled policy as the constraints that change the
  result; the typed `network_withheld_by_policy` failure keeping its output,
  class and effect certainty, and never replacing a decision or a wait; the
  retry it names being networked for `terminal`, `verify` and `execute_code`
  in a trusted workspace, under a stored grant and after an approval; the
  recorded decision on every execution path, forged values cleared on the early
  returns; explicit offline and local forms that never hide a remote target; a
  prohibition on the network reaching every networked call; the hard floor
  answering before any capability ask; offline verification despite a grant
  stated to the person and the model only for commands that can fetch; and, at
  parallel capacity through the real claim middleware, a failing local
  command leaving no claim while a failing networked one still does;
- the external-effect claim lifecycle: settlement on proven success, uncertain
  claims after every failure and crash, in-place waits for in-flight calls,
  one-time cross-Run approval, and bound observations ordered by sequence;
- child-process environment construction and daemon-secret exclusion;
- exact-value redaction without persisting raw secrets;
- local-CLI-only workspace trust and capability revocation on untrust;
- CLI identity on every endpoint that resolves a person: refused over TCP and
  from a refused socket peer whatever the platform spelling, gateway token, or
  retired token header, served for the verified peer, with IM and HTTP
  identities unchanged over TCP; the verdict taken once per connection and only
  when a request claims CLI identity; browser-originated and non-JSON
  state-changing requests refused;
- daemon private state denied under the real sandbox, with the workspace and an
  unrelated loopback port still reachable, a path under `/tmp` never masked, and
  every way a tool command starts reporting its process group;
- the names above that state pinned: under the real sandbox, renaming,
  swapping, replacing with a symlink, or removing the data directory, any
  directory above it, a private entry, or the socket (and its directory in the
  `/tmp` fallback layout) fails, nothing is readable through a new name, hard
  links to private files are refused, and ordinary work in the same writable
  root still succeeds, each against an unprotected control; bubblewrap's pins
  rendered after the roots and before the overlays and masks, and never under
  `/tmp` or inside a synthesized or overlaid path;
- the configuration, its timestamped copies (including one written after the
  policy), the credential store's files, and the chat account credentials denied
  under the real sandbox, the daemon naming the file it loaded rather than a
  default, and nothing creatable beside the credentials file under any suffix;
- every secret writer refusing to write through a symlink planted at its old
  temporary name, and every directory on every private path (the data
  directory, the configuration, the credentials file, a socket outside the
  data directory, each on its own branch of the tree) pinned against a rename
  and a swap, under the real sandbox and each against an unprotected control;
- a symlinked data directory, configuration file, and ancestor not replaceable
  (rename, remove, repoint, rename another link over it, swap), under the real
  sandbox against an unprotected control, and the daemon naming those links;
- every link in a chain (link to link, a link into a synced folder that is
  itself a link, a relative chain, a configuration link chain) not replaceable
  and the directories they stand in not movable, each against an unprotected
  control, and a cycle or an endless chain ending within the depth cap, with a
  mutation that follows only the first link caught;
- the model catalogue cache, archived skills, and the skill usage sidecar never
  written through a planted `.tmp` symlink;
- refusal warnings limited per peer and kind with a suppressed count, bounded in
  memory, capped across all peers with one summary line (500 refusals from 500
  addresses write 20 warnings, then one summary line), and no refusal body
  disclosing the socket path, with the CLI deriving it;
- a policy that also lets the command write a credential cache in place still
  denying every move, swap, removal, read, plant, and link replacement above,
  while the cache write works, under real `sandbox-exec` and against controls,
  with the private rules last and the in-place subpaths counted as writable for
  the pins;
- the CLI and the runtime agreeing on which gateway addresses are this host's,
  for every wildcard form and a genuinely remote host, and the Host check
  following the listener's real address;
- `/v1/gateway/shutdown` refused to TCP callers without the gateway token and to
  refused socket peers, accepted for the verified CLI, a token holder, and the
  restart helper holding the capability only for its own model change;
  `/v1/gateway/status` detail withheld likewise; `/health` open; a loopback
  daemon refusing a rebinding-style `Host`, with a scratch daemon driven through
  all of it in both socket layouts;
- immutable per-run leases and expiry of execution capabilities;
- secret-free durable binding round trips, restart fingerprint validation,
  frozen capability sets, and revocation before the next poll;
- untrusted workspace skill fencing and credential-shaped declaration denial;
- long-running timeout bounds and heartbeat behavior;
- context cancellation of command execution and CPU-bound tool loops;
- bounded, ambiguity-safe patch misses on large files;
- host grants scoped by workspace and command family;
- smart triage fail-closed behavior;
- delegation depth and parent-backend isolation, with delegated calls kept
  under the parent's policy chain and run scope;
- external read-only markings bound to the exact tool, its person, and the
  server's configured identity, never to server annotations, with marked calls
  claiming nothing, batching and approving like reads, and unmarked calls
  unchanged;
- effect profiles matching only their exact invocation: workspace, resolved
  cwd, literal argv, declared assignments and standard input;
- UTF-8-safe previews, artifact spooling, and model truncation recovery.
