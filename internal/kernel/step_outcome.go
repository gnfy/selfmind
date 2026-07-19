package kernel

import "strings"

// Loop Engineering ACTIVE PLAN P0-B: typed turn-completion classification.
//
// The turn loop historically ended through several scattered branches, each
// hand-emitting its own turn.completed event with an ad-hoc
// status/reason/resumable triple. That duplication is where "ended too early"
// and "ran an extra repair round" bugs hid. StepOutcome names what a loop
// iteration decided; resolveTurnCompletion is the SINGLE source of truth for
// how a finished turn is reported. The hard iteration cap is demoted to a pure
// safety backstop: hitting it no longer discards the collected answer, it
// finalizes from evidence like any other bounded stop.

// StepOutcome is the decision a single loop iteration reached. This slice
// applies it to the terminal classification; the mid-loop
// continue/execute/compact transitions remain inline (tracked follow-up).
type StepOutcome string

const (
	StepContinueModel  StepOutcome = "continue_model"  // re-prompt (output-limit / budget / plan repair)
	StepExecuteTools   StepOutcome = "execute_tools"   // model asked for tools
	StepCompactContext StepOutcome = "compact_context" // over-budget window compacted mid-turn
	StepCompleteTurn   StepOutcome = "complete_turn"   // final answer produced
	StepFailTurn       StepOutcome = "fail_turn"       // unrecoverable transport/tool failure
)

// turnCompletion is the typed, single-sourced result of a finished turn.
type turnCompletion struct {
	Status    string // "completed" | "incomplete"
	Reason    string // completed | output_limit | tool_budget_exhausted | plan_unresolved | max_iterations
	Resumable bool
}

// completionSignals is the loop state that decides how a turn is reported.
type completionSignals struct {
	FinishStatus        string // structured finish_run status, "" if none
	ToolBudgetExhausted bool
	PlanUnresolved      bool
	OutputLimited       bool // model stopped for output length AND no room to continue
	IterationCapped     bool // hit the hard safety iteration ceiling
}

// resolveTurnCompletion maps loop state to the reported completion. Precedence
// mirrors the pre-refactor branch order exactly so existing turns classify
// identically; the only behavioral change lives at the call site, where the
// iteration-cap path now returns the collected answer instead of a stub
// string. A structured finish_run status is authoritative for "the model
// declared done" and suppresses the tool-budget-incomplete signal.
func resolveTurnCompletion(s completionSignals) turnCompletion {
	switch {
	case s.OutputLimited:
		return turnCompletion{Status: "incomplete", Reason: "output_limit", Resumable: true}
	case s.ToolBudgetExhausted && strings.TrimSpace(s.FinishStatus) == "":
		return turnCompletion{Status: "incomplete", Reason: "tool_budget_exhausted", Resumable: true}
	case s.PlanUnresolved:
		return turnCompletion{Status: "incomplete", Reason: "plan_unresolved", Resumable: true}
	case s.IterationCapped:
		return turnCompletion{Status: "incomplete", Reason: "max_iterations", Resumable: true}
	default:
		return turnCompletion{Status: "completed", Reason: "completed", Resumable: false}
	}
}
