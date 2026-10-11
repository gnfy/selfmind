package kernel

import (
	"fmt"
	"strings"
)

// ExternalEffectContext is a selected runtime blocker, never an execution grant.
type ExternalEffectContext struct {
	ClaimID, RunID, Target, State, Summary, Outcome string
	SameChain                                       bool
}

func externalEffectContextPrompt(items []ExternalEffectContext) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Unresolved External Effects\n")
	b.WriteString("These durable blockers are separate from Plan milestones. This selected snapshot stays fixed during the Run; newer runtime observations supersede it. Address the underlying uncertainty instead of repeating approvals or adding retry steps. A successful unbound verify does not release a claim. Bind one proven read-only terminal or verify observation with effect_claim_id; the same check may prove a Plan step with step_id. Own-chain success releases its effect. A foreign-chain observation permits only one conflicting call and leaves the source unresolved: ask the person to use /effects settle <claim_id> [reason] only if they know its result. In-flight or watched effects must finish first. Approval of another action never settles these claims. Do not replay unknown effects or describe them as released without a recorded release.\n")
	for _, item := range items {
		fmt.Fprintf(&b, "- claim_id=%s state=%s own_chain=%t target=%q source_run=%s call=%q outcome=%q\n",
			item.ClaimID, trimLine(item.State, 40), item.SameChain, item.Target, item.RunID, trimLine(item.Summary, 180), trimLine(item.Outcome, 100))
	}
	return b.String()
}
