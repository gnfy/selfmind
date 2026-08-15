// Package command is the single cross-endpoint catalog of SelfMind control and
// TUI slash commands. It is a leaf package (standard library only) so every
// surface that needs command DETECTION, help text, name lists, near-miss
// suggestions, or the IM async-hint — the gateway HTTP API (internal/gateway/
// httpapi), the Weixin adapter (internal/gateway/weixin), the rich TUI
// (internal/gateway/cli), and the thin CLI client (internal/cliapp) — can
// import it without any risk of an import cycle.
//
// It deliberately does NOT own EXECUTION. Each command's behavior still lives
// where it always has (the gateway's tryHandleControlCommand switch, the TUI's
// slash handlers). This package only unifies the metadata that had drifted
// across ~9 hand-maintained copies: the help text, the suggest "known" slice,
// the two IM async-hint isControlCommand copies, and the TUI command list.
package command

import (
	"regexp"
	"strings"
)

// Scope marks where a command is executed.
type Scope int

const (
	// Gateway commands are pre-agent control commands handled synchronously by
	// the gateway (Server.tryHandleControlCommand). Every endpoint may route
	// them to the daemon; IM adapters must treat them as synchronous control
	// (see SyncControl / IsGatewayControl).
	Gateway Scope = iota
	// Local commands are TUI/CLI-only (clipboard, transcript, skill store,
	// lifecycle). They are never routed to the gateway message path.
	Local
)

// Entry is one command's shared metadata.
type Entry struct {
	// Name is the canonical invocation token, e.g. "/status".
	Name string
	// Aliases are alternate leading tokens that resolve to the same command,
	// e.g. "/task" for /resume. Used by Lookup and IsGatewayControl only.
	Aliases []string
	// Summary is the one-line help description.
	Summary string
	// Usage is the invocation form shown in help, e.g. "/approve <n|id|all>".
	Usage string
	// SyncControl is true for pre-agent control commands that IM adapters must
	// handle synchronously rather than dispatch as an async task (the
	// async-hint). It is true for every Gateway-scope command.
	SyncControl bool
	// Scope is Gateway (daemon-routable control) or Local (TUI/CLI-only).
	Scope Scope
}

// entries is the single source of truth. Gateway entries mirror the cases in
// Server.tryHandleControlCommand and the gateway /help text; Local entries
// mirror the TUI-only slash commands. The registry↔switch drift-guard test in
// internal/gateway/httpapi keeps the Gateway set in sync with the switch.
var entries = []Entry{
	// --- Gateway control commands (order matches the gateway /help text) ---
	{Name: "/help", Usage: "/help", Summary: "Show this help.", SyncControl: true, Scope: Gateway},
	{Name: "/model", Usage: "/model", Summary: "Show the configured model.", SyncControl: true, Scope: Gateway},
	{Name: "/id", Usage: "/id", Summary: "Show your resolved account identity.", SyncControl: true, Scope: Gateway},
	{Name: "/status", Aliases: []string{"/task status"}, Usage: "/status", Summary: "Show the current task status.", SyncControl: true, Scope: Gateway},
	{Name: "/tasks", Usage: "/tasks [done|archived|all]", Summary: "List open work (done/archived collapse to counts).", SyncControl: true, Scope: Gateway},
	{Name: "/task", Usage: "/task <n|id> [runs|rename <name>|pin|unpin|archive|merge <dst>|references|reference add|remove <name>]", Summary: "Show or manage a task, including its learned references.", SyncControl: true, Scope: Gateway},
	{Name: "/queue", Usage: "/queue [drop <n>|clear]", Summary: "List queued tasks (or drop all pending queued tasks).", SyncControl: true, Scope: Gateway},
	{Name: "/watchers", Usage: "/watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]", Summary: "List, inspect, or cancel durable external watchers.", SyncControl: true, Scope: Gateway},
	{Name: "/diag", Usage: "/diag [memory|context|tasks|models|delivery|execution|tools]", Summary: "Show runtime and subsystem diagnostics, including tool-schema health.", SyncControl: true, Scope: Gateway},
	{Name: "/report", Usage: "/report daily [--since 24h]", Summary: "Show a model-free execution quality and cost report.", SyncControl: true, Scope: Gateway},
	{Name: "/events", Usage: "/events", Summary: "List recent events for the current task.", SyncControl: true, Scope: Gateway},
	{Name: "/approvals", Usage: "/approvals [grants|revoke <n>]", Summary: "List pending approvals; grants lists remembered classes and revoke withdraws one.", SyncControl: true, Scope: Gateway},
	{Name: "/approve", Usage: "/approve <n|id|all> [run]", Summary: "Approve a pending action; run is available only when the request offers run-local reuse.", SyncControl: true, Scope: Gateway},
	{Name: "/reject", Usage: "/reject <n|id|all>", Summary: "Reject a pending action (or all of them).", SyncControl: true, Scope: Gateway},
	{Name: "/mode", Usage: "/mode [mode]", Summary: "Show or set your approval mode (on-request|read-only|auto-edit|full-auto|smart).", SyncControl: true, Scope: Gateway},
	{Name: "/stop", Usage: "/stop", Summary: "Cancel the active run (or the current task if nothing is running).", SyncControl: true, Scope: Gateway},
	{Name: "/cancel", Usage: "/cancel", Summary: "Cancel the current task even if no run is active.", SyncControl: true, Scope: Gateway},
	{Name: "/notify", Usage: "/notify <platform|auto>", Summary: "Choose where CLI-origin notifications go when the CLI is detached.", SyncControl: true, Scope: Gateway},
	{Name: "/new", Usage: "/new [title]", Summary: "Create a new task.", SyncControl: true, Scope: Gateway},
	{Name: "/resume", Usage: "/resume [n|task_id]  (bare = pick from recent tasks)", Summary: "Resume a task by list number or id; bare lists recent tasks to pick from (an archived id reopens it).", SyncControl: true, Scope: Gateway},
	{Name: "/workspace", Aliases: []string{"/ws"}, Usage: "/workspace [n|id]  (bare = list; alias: /ws)", Summary: "List workspaces (bare) or select one by list number or id.", SyncControl: true, Scope: Gateway},
	{Name: "/workspaces", Usage: "/workspaces  (same as bare /workspace or /ws)", Summary: "List workspaces.", SyncControl: true, Scope: Gateway},

	// --- Local TUI/CLI-only commands (never gateway-routed) ---
	{Name: "/skills", Usage: "/skills [list|view|history|undo|search|install|audit|delete|archive|pin|unpin|stats|reload]", Summary: "Manage learned skills.", Scope: Local},
	{Name: "/bundles", Usage: "/bundles [list|view|create|delete]", Summary: "Manage skill bundles.", Scope: Local},
	{Name: "/reload-skills", Usage: "/reload-skills", Summary: "Reload skill tools from disk.", Scope: Local},
	{Name: "/memory", Usage: "/memory [category|conflicts|search|show|correct|forget|pin|unpin|raw|history|undo]", Summary: "Review and manage long-term memory.", Scope: Local},
	{Name: "/curator", Usage: "/curator [status|run|restore]", Summary: "Review or run skill cleanup.", Scope: Local},
	{Name: "/checkpoint", Usage: "/checkpoint [list|save|load|delete] [name]", Summary: "Manage workspace checkpoints.", Scope: Local},
	{Name: "/migrate", Usage: "/migrate", Summary: "Migrate skills from Hermes Agent.", Scope: Local},
	{Name: "/clear", Usage: "/clear", Summary: "Clear conversation history.", Scope: Local},
	{Name: "/exit", Usage: "/exit", Summary: "Exit SelfMind.", Scope: Local},
	{Name: "/compact", Usage: "/compact", Summary: "Compact older conversation history to free context.", Scope: Local},
	{Name: "/paste-image", Usage: "/paste-image", Summary: "Attach a screenshot from the clipboard (local GUI only, not over SSH).", Scope: Local},
	{Name: "/capture", Usage: "/capture [title]", Summary: "Save the last turn as a replayable eval case.", Scope: Local},
	{Name: "/search", Usage: "/search [current|query]", Summary: "Review this conversation with full diffs (current), or search past working sessions (bare = recent sessions).", Scope: Local},
	{Name: "/copy", Usage: "/copy", Summary: "Copy the last assistant response to the clipboard.", Scope: Local},
}

// lookupIndex maps every canonical name and alias to its entry.
var lookupIndex = func() map[string]Entry {
	out := make(map[string]Entry, len(entries)*2)
	for _, e := range entries {
		out[e.Name] = e
		for _, a := range e.Aliases {
			out[a] = e
		}
	}
	return out
}()

// All returns every registered entry (Gateway and Local), in catalog order.
func All() []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// firstToken lowercases content and returns its first whitespace-delimited
// token. Multi-word aliases ("/task status") are matched by IsGatewayControl
// and Lookup against their first token plus the alias table, so callers pass a
// single token here.
func firstToken(content string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(content)))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// Lookup resolves a leading token (canonical name or alias) to its entry.
func Lookup(token string) (Entry, bool) {
	e, ok := lookupIndex[strings.ToLower(strings.TrimSpace(token))]
	return e, ok
}

// commandTokenRe matches a token SHAPED like a slash command: "/" + a letter +
// letters/digits/_/-. Command and skill names never contain a second slash or
// a dot, so absolute Unix paths ("/mnt/c/pic.png", "/tmp/a.txt") and bare "/"
// do not match.
var commandTokenRe = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*$`)

// LooksLikeCommand reports whether the first token of content has the shape of
// a slash command (registered or not). It is the shared pre-gate for every
// surface that routes a leading "/" into command handling or an
// unknown-command reject: only command-shaped tokens enter that path, while a
// "/"-leading file path or other prose stays on the normal agent-first message
// path (observed live: a pasted "/mnt/c/...png <question>" was rejected as
// "Unknown command"). Actual command resolution stays in Lookup /
// IsGatewayControl / each surface's handler.
func LooksLikeCommand(content string) bool {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) == 0 {
		return false
	}
	return commandTokenRe.MatchString(fields[0])
}

// IsGatewayControl reports whether the first token of content is a registered
// Gateway-scope command. It is the single cross-endpoint replacement for the
// old per-adapter isControlCommand async-hint copies, so IM adapters treat
// exactly the gateway control set (including /queue /diag /mode /notify /help
// /model) as synchronous control rather than async task dispatch.
func IsGatewayControl(content string) bool {
	tok := firstToken(content)
	if tok == "" {
		return false
	}
	e, ok := lookupIndex[tok]
	return ok && e.Scope == Gateway && e.SyncControl
}

// Known returns the canonical Gateway-scope command names, for near-miss
// suggestion. Order follows the catalog.
func Known() []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Scope == Gateway {
			out = append(out, e.Name)
		}
	}
	return out
}

// Suggest returns the closest Gateway command when the first token of input is
// a near-miss (Levenshtein distance < 3, same first letter, token length ≥ 3),
// else "". Exact commands return "" (they are not typos). This is the single
// unknown-command decision shared by the gateway reject gate and the TUI.
func Suggest(input string) string {
	token := firstToken(input)
	if token == "" {
		return ""
	}
	best, bestDist := "", 3
	for _, cmd := range Known() {
		if token == cmd {
			return "" // exact commands are handled elsewhere; not a typo
		}
		if len(token) < 3 || token[1] != cmd[1] {
			continue
		}
		if d := editDistance(token, cmd); d < bestDist {
			best, bestDist = cmd, d
		}
	}
	return best
}

// HelpText renders the canonical gateway /help reply. The command/summary
// columns are aligned to the widest usage string so every surface shows the
// same help.
func HelpText() string {
	width := 0
	for _, e := range entries {
		if e.Scope == Gateway && len(e.Usage) > width {
			width = len(e.Usage)
		}
	}
	var b strings.Builder
	b.WriteString("SelfMind commands:")
	for _, e := range entries {
		if e.Scope != Gateway {
			continue
		}
		b.WriteString("\n")
		b.WriteString(e.Usage)
		for i := len(e.Usage); i < width+2; i++ {
			b.WriteString(" ")
		}
		b.WriteString(e.Summary)
	}
	return b.String()
}

func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
