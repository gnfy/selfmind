---
status: accepted
---

# Separate model and runtime readiness

SelfMind treats the applied Model Manager transaction as the sole authority for
Model Readiness, while onboarding owns Runtime Readiness and first-use progress.
Onboarding must not duplicate provider, model, or verification facts: doing so
creates two authorities that drift after a valid route change and makes an
unrelated runtime repair repeat model setup. Existing valid receipts are
migrated once without prompting, and an incomplete onboarding run resumes only
the readiness stages that remain unsatisfied.

## Considered options

- Updating both model and onboarding state on every route change was rejected
  because crash recovery and direct configuration drift would still require a
  cross-file consistency protocol.
- Keeping only a model-transaction generation in onboarding was rejected for
  the same reason: onboarding does not need to own or acknowledge model state.

## Consequences

Infrastructure failures never invalidate an applied model route. Model,
runtime, and first-use status are reported separately, and legacy onboarding
model fields become migration input rather than ongoing authority.
