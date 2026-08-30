---
status: accepted
---

# Verify ownership before declaring a managed Gateway healthy

SelfMind declares a Managed Background Service healthy only when the platform
service-manager job and the responding Gateway prove the same installation
generation, version, and configuration path. Endpoint reachability or a loaded
job alone is insufficient because an orphaned or on-demand Gateway can mask a
failing service-manager restart loop. A compatible reachable Gateway may keep
foreground work available as Runtime Degraded, but setup must not describe that
state as ready.

## Consequences

Service reconciliation is graceful and bounded: it drains active work, waits
for the previous owner and runtime lock to exit, retries a transient bootstrap
only after absence is proven, and never force-kills an active task. Failure to
establish ownership remains actionable runtime-repair state rather than a model
failure or a request for administrator privileges.
