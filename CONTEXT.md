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

## Work History

**Interaction**:
One user request and its answer, retained as searchable history whether or not
it represents ongoing work.
_Avoid_: Task, memory

**Conversation**:
The endpoint-local transcript visible in one channel or client.
_Avoid_: Thread, shared transcript, session

**Run**:
One accountable agent execution attempt and the sole owner of execution state.
_Avoid_: Task status, thread status

**Active Work Buffer**:
A bounded view of the current Run, endpoint-local tail, and unconsolidated Work
Spine evidence used for immediate understanding before background organization.
_Avoid_: Shared transcript, current task

**Task Capsule**:
A reversible semantic summary that groups evidence from related work across
Runs; it does not own execution state or authority.
_Avoid_: Thread, task lifecycle, conversation, context boundary

**Task Evidence**:
A sourced, correctable relationship between a Task Capsule and a Run, Work
Unit, or evidence slice.
_Avoid_: Resume edge, parent relationship

**Resume Source**:
The exact prior Run whose unfinished execution a new Run resumes, recorded by
`resumes_run_id`; it does not express general semantic relatedness.
_Avoid_: Parent Run, current task, related task

**Attention**:
The work that currently needs the person or agent to act, derived from Runs and
pending control objects rather than stored as a Task Capsule status.
_Avoid_: Task status, inbox

**Work Obligation**:
An unresolved condition with a named owner and acceptance evidence that a
later Run may satisfy without pretending to resume the source Run.
_Avoid_: Task status, waiting label, next-step prose

**Work Evidence**:
Durable proof that a Run did work: a plan, a non-lifecycle side-effect tool
record, an approval, clarification, or watcher, a parent edge, or next steps.
_Avoid_: Any tool call, lifecycle tool result, message length

**Memory**:
A stable person preference or correction that should influence future work.
_Avoid_: Work history, transcript archive, project status
