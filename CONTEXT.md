# SelfMind Domain Language

SelfMind is an always-on personal AI gateway whose foreground work and bounded
background work share one person's durable state without sharing raw channel
transcripts.

## Readiness

**Model Readiness**:
The state in which the effective Main and Background routes have been validated
and accepted for use.
_Avoid_: Model setup completion, configured models

**Runtime Readiness**:
The state in which the selected workspace, safety policy, and requested
background operating mode are available.
_Avoid_: Setup readiness, model readiness

**First-Use Completion**:
The milestone reached after a person completes the first real SelfMind task.
_Avoid_: Installation completion, runtime readiness

**Runtime Degraded**:
A state in which foreground work remains available but the requested always-on
background guarantee is not currently met.
_Avoid_: Ready, healthy

## Models

**Main**:
The model route that owns conversations, planning, tool use, and final answers.
_Avoid_: Primary, chat model, foreground model

**Background**:
The default model route for bounded maintenance roles such as memory,
summarization, review, and Skill curation.
_Avoid_: Auxiliary, secondary model, cheap model

**Model Manager**:
The sole interactive surface for selecting, validating, and applying Main,
Background, and optional role-specific routes.
_Avoid_: Model setup wizard, model subcommands

## Background Operation

**Managed Background Service**:
A Gateway whose lifecycle is verifiably owned by the selected platform service
manager.
_Avoid_: Reachable Gateway, loaded service definition

**Service Ownership**:
The verifiable relationship between a running Gateway instance and the platform
service-manager installation expected to supervise it.
_Avoid_: Process found, endpoint healthy

**Onboarding**:
The resumable workflow that establishes missing readiness without redoing a
readiness stage already satisfied elsewhere.
_Avoid_: Model configuration authority, one-shot setup wizard
