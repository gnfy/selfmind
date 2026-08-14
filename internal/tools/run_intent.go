package tools

import "strings"

// OperationClass names what an action can DO, not which tool performs it. A
// prohibition and a pending call are compared in this vocabulary so "do not
// modify files" stops a write and leaves a read-only probe alone.
type OperationClass string

const (
	OpClassWrite  OperationClass = "write"
	OpClassDelete OperationClass = "delete"
	// OpClassObserve is a statically classified read-only command. Keeping it
	// distinct prevents a request such as "inspect first, do not execute yet"
	// from blocking the inspection that makes the later decision possible.
	OpClassObserve OperationClass = "observe"
	// OpClassExec is a deny-side parent: an unqualified "do not run commands"
	// covers both execution shapes below.
	OpClassExec OperationClass = "exec"
	// OpClassExecInTurn is the agent running a command itself, now.
	OpClassExecInTurn OperationClass = "exec.in_turn"
	// OpClassExecDelegated hands a command to the daemon to run later, unattended.
	OpClassExecDelegated OperationClass = "exec.delegated"
	OpClassNetwork       OperationClass = "network"
	// OpClassDangerous is a call-side fallback for the dangerous-op heuristic when
	// no more specific class applies. Prohibitions never carry it: a deny must
	// name something, and "this looked risky" is not something the person said.
	OpClassDangerous OperationClass = "dangerous"
)

// DenyScope is one prohibition the person expressed, bound to the clause it
// appeared in. Empty Targets means "every target of these classes".
type DenyScope struct {
	// Marker is the phrase that triggered detection, kept so a human-facing
	// explanation can quote the person's own words.
	Marker string `json:"marker,omitempty"`
	// Clause is the bounded sentence the marker appeared in. A prohibition
	// binds to its own clause: "probe read-only, do not modify files" forbids
	// modification, not probing.
	Clause  string           `json:"clause,omitempty"`
	Classes []OperationClass `json:"classes,omitempty"`
	// Targets are literal paths or command fragments named in the clause.
	Targets []string `json:"targets,omitempty"`
	// Repetition marks a prohibition qualified as "not again" (rerun, retry,
	// 重新). It forbids a SECOND execution, not the first, so it does not force
	// a human ask for the call the person just asked for. Repeated identical
	// calls remain bounded by ToolGuardrails.
	Repetition bool `json:"repetition,omitempty"`
	// Resolved is false when a prohibition was detected but could not be
	// classified. Such a deny keeps the old blanket effect: narrowing applies
	// to what we can read, never to what we cannot.
	Resolved bool `json:"resolved,omitempty"`
}

// RunIntentSnapshot separates durable control-plane facts from user-authored
// evidence before an approval judge sees them. GoalSummary is advisory context;
// none of these fields bypasses deterministic approval floors or stored grants.
type RunIntentSnapshot struct {
	RawUserText   string   `json:"raw_user_text,omitempty"`
	GoalSummary   string   `json:"goal_summary,omitempty"`
	WorkKey       string   `json:"work_key,omitempty"`
	WorkspaceID   string   `json:"workspace_id,omitempty"`
	Source        string   `json:"source,omitempty"`
	ExplicitAllow []string `json:"explicit_allow,omitempty"`
	ExplicitDeny  []string `json:"explicit_deny,omitempty"`
	// DenyScopes is the structured form of ExplicitDeny. When it is empty but
	// ExplicitDeny is not, the snapshot came from a caller that predates
	// scoping and keeps the old blanket behavior.
	DenyScopes []DenyScope `json:"deny_scopes,omitempty"`
}

// UserAuthored reports whether RawUserText came from a person in the current
// turn. System prompts may describe desired work, but they are never current
// human authorization for a side effect.
func (s RunIntentSnapshot) UserAuthored() bool {
	source := strings.ToLower(strings.TrimSpace(s.Source))
	return source == "" || source == "direct" || source == "continuation"
}

// HasExplicitDeny reports whether the person forbade anything at all this
// turn. It answers "was there a prohibition", not "does it apply here" — use
// DenyBlocks for the second question. The judge prompt still shows every deny,
// applicable or not, so smart mode can weigh what the person said.
func (s RunIntentSnapshot) HasExplicitDeny() bool {
	for _, item := range s.ExplicitDeny {
		if strings.TrimSpace(item) != "" {
			return true
		}
	}
	return false
}

// DenyBlocks reports whether any prohibition covers a call of these classes
// against these targets. A deny remains a reason to ask or reject, never an
// instruction the model may reinterpret — but it constrains only the operation
// it actually points at, so "do not modify files" no longer stops a read-only
// probe from running.
func (s RunIntentSnapshot) DenyBlocks(classes []OperationClass, targets []string) bool {
	if len(classes) == 0 {
		return false
	}
	if len(s.DenyScopes) == 0 {
		// Pre-scoping caller: keep the blanket effect rather than silently
		// dropping a prohibition we simply failed to parse.
		return s.HasExplicitDeny()
	}
	for _, scope := range s.DenyScopes {
		if scope.blocks(classes, targets) {
			return true
		}
	}
	return false
}

func (d DenyScope) blocks(classes []OperationClass, targets []string) bool {
	if !d.Resolved {
		return true
	}
	if d.Repetition {
		// "Do not rerun it" authorizes the first execution. Blocking it inverts
		// the instruction, and nothing here can tell a first call from a second.
		return false
	}
	if !anyClassMatches(d.Classes, classes) {
		return false
	}
	if len(d.Targets) == 0 {
		return true
	}
	return anyTargetMatches(d.Targets, targets)
}

func anyClassMatches(deny, call []OperationClass) bool {
	for _, d := range deny {
		for _, c := range call {
			if classMatches(d, c) {
				return true
			}
		}
	}
	return false
}

func classMatches(deny, call OperationClass) bool {
	if deny == call {
		return true
	}
	// An unqualified execution ban covers both shapes; a ban qualified as
	// "directly" resolves to OpClassExecInTurn and leaves delegation alone.
	return deny == OpClassExec && (call == OpClassObserve || call == OpClassExecInTurn || call == OpClassExecDelegated)
}

// anyTargetMatches compares a named target against the call's targets by
// containment in either direction: the person writes "config.yaml" while the
// call carries "/repo/config.yaml", and a command fragment appears inside a
// longer command line.
func anyTargetMatches(deny, call []string) bool {
	for _, d := range deny {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		for _, c := range call {
			c = strings.ToLower(strings.TrimSpace(c))
			if c == "" {
				continue
			}
			if strings.Contains(c, d) || strings.Contains(d, c) {
				return true
			}
		}
	}
	return false
}
