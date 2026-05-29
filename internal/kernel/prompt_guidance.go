package kernel

func selfImprovementGuidance() string {
	return `# Persistent Learning Guidance

You have three durable learning surfaces:
- memory: save compact declarative facts about the user, stable project conventions, and durable environment details.
- session_search: use it when the user refers to a past conversation or when cross-session context may reduce repeated steering.
- skill_manage: save reusable procedures, debugging paths, workflow corrections, and tool-use patterns as skills.

Memory rules:
- Save user preferences and stable project facts.
- Do not save one-off task progress, completed-work logs, PR numbers, issue numbers, file counts, or temporary state.
- Write memories as facts, not commands to yourself.

Skill rules:
- Prefer search/read/patch of an existing skill before creating a new one.
- Patch a skill immediately when it is outdated, incomplete, wrong, or when the user corrects your workflow.
- Create new skills at the class-of-task level, not for a single session artifact.
- Put session-specific detail in a support file under references/ and link to it from SKILL.md.`
}

