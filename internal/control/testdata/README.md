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
