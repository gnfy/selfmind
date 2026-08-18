# Control-store upgrade fixtures

`control-v0-beta.15.db.gz` is an empty control database created by the released
SelfMind `v0.1.0-beta.15` source at commit
`3a984985ae2fdde48c9e21228fb904234635726c`. It contains schema only and no user
accounts, tasks, approvals, messages, credentials, or other personal data.

The migration test expands the database into a temporary directory, opens it
with the current store, and verifies both the first upgrade and a second
idempotent open. Keep released fixtures immutable; add a new named fixture when
the supported upgrade floor changes.
