package kernel

import "strings"

// taskExecutionGuidance encodes the work-quality discipline that separates a
// polished result from a quick-and-dirty one: explore before writing, prefer
// precise edits, and verify your output before claiming done. It is language-
// and domain-agnostic (the project's own tooling drives verification) and is
// injected only on tool-bearing turns.
func taskExecutionGuidance() string {
	return `# WORK QUALITY & VERIFICATION
- Explore before you write: list the working directory and read related or existing files so you do not overwrite or duplicate existing work. Pick a non-colliding filename when one already exists.
- Prefer precise edits (the patch tool) over rewriting a whole file when changing existing code.
- VERIFY before declaring done. Identify the project's ecosystem from its manifest/build files (e.g. go.mod, package.json, pyproject.toml/requirements.txt, Cargo.toml, pom.xml/build.gradle, composer.json, Gemfile, Makefile, *.csproj, or CI config) and run that project's own syntax/build/test/lint check with the terminal. If nothing is runnable, re-read what you produced and sanity-check the key parts (entry points and the specific behavior requested). Treat a failed check as work to fix, not a place to stop.
- Do not claim completion you have not verified. If you could not verify, say so plainly and give the exact command the user can run.`
}

// progressNarrationGuidance asks the model to keep the user oriented with short
// Codex-style preambles before tool batches. These notes stream as ordinary
// assistant text, and the CLI/TUI persists each one as its own message, turning
// an otherwise opaque tool run into a readable step trajectory. It is injected
// only on tool-bearing turns (alongside taskExecutionGuidance); pure
// direct-answer turns expose no tools and need no narration.
func progressNarrationGuidance() string {
	return `# PROGRESS NARRATION (keep the user oriented)
- Before a group of tool calls, write ONE short sentence (about 8-20 words), in plain language and present tense, saying what you are about to do and why; then make the calls. Group related actions under a single preamble instead of narrating every trivial read.
- Do not use headings, bullet lists, or code fences for these notes; they are short status lines, not sections.
- When you change approach after a failure, or move from one phase of the work to the next, say so in one short line before continuing.
- Skip narration entirely for a direct answer that uses no tools.`
}

// frontendQualityGuidance is injected ONLY when the task looks like UI/frontend
// work (see isFrontendTask). It must not be part of the always-on guidance —
// for backend, data, infra, or CLI work it is irrelevant and misleading.
func frontendQualityGuidance() string {
	return `# FRONTEND / UI QUALITY (this task involves a UI or page)
- Aim for an intentional, polished result; avoid generic "AI slop" and safe, average layouts.
- Define CSS variables; use purposeful typography (avoid default system stacks); add a few meaningful animations; use gradients/shapes/patterns for atmosphere instead of a flat single-color background; ensure it works on both desktop and mobile.
- Exception: inside an existing project or design system, match its established patterns instead.`
}

// isFrontendTask is a lightweight, multilingual signal for whether design/UI
// quality guidance is relevant. It is advisory prompt content only (a false
// positive wastes a little prompt budget; a false negative just omits design
// hints) — not tool gating or agent routing.
func isFrontendTask(input string) bool {
	lower := strings.ToLower(input)
	signals := []string{
		// English
		"frontend", "front-end", "front end", "ui", "ux", "web page", "webpage",
		"website", "web app", "html", "css", "canvas", "react", "vue", "svelte",
		"tailwind", "landing page", "dashboard", "animation", "button", "layout",
		"responsive", "component", "game",
		// Chinese
		"前端", "网页", "页面", "界面", "样式", "布局", "动画", "按钮", "可视化",
		"小游戏", "游戏", "网站", "组件", "响应式", "落地页",
	}
	for _, s := range signals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func selfImprovementGuidance() string {
	return `# Persistent Learning Guidance

You have three durable learning surfaces:
- memory: save compact declarative facts about the user, stable project conventions, and durable environment details.
- session_search: use it when the user refers to a past conversation or when cross-session context may reduce repeated steering.
- skill_manage: save reusable procedures, debugging paths, workflow corrections, and tool-use patterns as skills.

Memory rules:
- Save user preferences and stable project facts.
- Do not save one-off task progress, completed-work logs, PR numbers, issue numbers, file counts, or temporary state.
- Do not save transient provider outages, failed command guesses, or temporary tool failures unless the user turns them into a durable rule.
- Write memories as facts, not commands to yourself.

Skill rules:
- Prefer search/read/patch of an existing skill before creating a new one.
- Patch a skill immediately when it is outdated, incomplete, wrong, or when the user corrects your workflow.
- Create new skills at the class-of-task level, not for a single session artifact.
- Put session-specific detail in a support file under references/ and link to it from SKILL.md.
- Do not create duplicate skills for the same workflow.
- Treat manual and pinned skills as user-owned; patch only when the correction is clear, and never archive them automatically.`
}
