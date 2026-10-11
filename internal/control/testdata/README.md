# Control-store upgrade fixtures

`control-v0-beta.15.db.gz` is an empty control database created by the released
SelfMind `v0.1.0-beta.15` source at commit
`3a984985ae2fdde48c9e21228fb904234635726c`. It contains schema only and no user
accounts, tasks, approvals, messages, credentials, or other personal data.

The migration test expands the database into a temporary directory, opens it
with the current store, and verifies both the first upgrade and a second
idempotent open. Keep released fixtures immutable; add a new named fixture when
the supported upgrade floor changes.

`control-v10-schema.sql` is the schema-only DDL of a real schema-version-10
control database (`sqlite3 <backup> .schema`, with the internal
`sqlite_sequence` line removed). It contains no rows: no accounts, tasks,
approvals, messages, credentials, or other personal data. The v10-to-v11
migration test builds a temporary database from it, records the version-10
migration ledger, seeds synthetic Thread/Run history, migrates it with the
current store, and verifies row counts, parent edges, visibility and kind
mapping, referential integrity, and an idempotent second open. Regenerate it
only from a backup whose ledger stops at version 10, and grep the result for
`INSERT` before committing.

The `*-rows.sql` fixtures are complete `sqlite3 .dump` outputs of databases
that an older tree created with its own `OpenStore` and then seeded through its
own store API with synthetic rows: persons, accounts, Threads, Runs (finished,
interrupted, parked, resumed, waiting on watchers, another person's), queue rows
of every continuation key shape, watchers, a parked approval with the loop
checkpoint that asked it, a pending clarification, a multi-step Plan, and
inbound message receipts (first-seen rows at schema v17; pending, uncertain,
accepted, and another person's uncertain receipt from schema v22). From
schema v23 they also hold external effect claims (uncertain, observed,
reserved) and a pending resource wait; from schema v24 the Plan has a later
version whose cancelled steps carry that release's dispositions and quotes.

- `control-v17-beta.27-rows.sql`: released `v0.1.0-beta.27`, commit
  `d90f4ff2c4f6`, schema v17.
- `control-v24-beta.28-rows.sql`: released `v0.1.0-beta.28`, commit
  `5e4be677d30b`, schema v24 (the same release as `control-v24-schema.sql`,
  which keeps an operator database's migrated physical shape instead).
- `control-v25-rows.sql`: the unreleased integration branch at commit
  `33b5d601d352`, schema v25.

They were generated from `git archive` exports of those commits in a scratch
directory, never from an operator data directory. The v26 upgrade tests load
each dump, migrate it with the current store, and verify that every row is
preserved and that no historical row gains an admission snapshot, an effect
sequence, a settlement, a human-wait binding, a cancellation quote source, or
an inbound claim generation. Regenerate a fixture only from the same commit,
and check the dump for paths and names before committing.
