package tools

import (
	"context"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// Cancellation provenance at the plan boundary. The runtime records which
// person input a cancellation quote matched; models and clients see only the
// input it names, and a model can never supply either.

// runtimeOwnedPlanStepFields are step fields that only the runtime writes:
// the identity of the input a cancellation quote matched, and the display of
// that input, which plan results and work_inspect carry.
var runtimeOwnedPlanStepFields = []string{"cancellation_quote_source", "cancellation_authorized_by"}

// NormalizeArguments drops runtime-owned step fields before the schema check.
// A complete snapshot echoed from an earlier result then proceeds, and nothing
// a model supplies there reaches the plan: the runtime records the provenance
// of each quote itself.
func (t *PlanTool) NormalizeArguments(args map[string]interface{}) (map[string]interface{}, error) {
	coerced, err := coerceValue("plan", args["plan"], "array")
	items, ok := coerced.([]interface{})
	if err != nil || !ok {
		return args, nil
	}
	var plan []interface{}
	for i, item := range items {
		step, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		owned := false
		for _, key := range runtimeOwnedPlanStepFields {
			_, present := step[key]
			owned = owned || present
		}
		if !owned {
			continue
		}
		if plan == nil {
			plan = append([]interface{}(nil), items...)
		}
		kept := make(map[string]interface{}, len(step))
		for key, value := range step {
			kept[key] = value
		}
		for _, key := range runtimeOwnedPlanStepFields {
			delete(kept, key)
		}
		plan[i] = kept
	}
	if plan == nil {
		return args, nil
	}
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		out[key] = value
	}
	out["plan"] = plan
	return out, nil
}

// PlanQuoteOrigins describes, per recorded quote source on runID's plan, the
// person input that authorized a cancellation: the request a Run started with
// or guidance sent while it ran, when it arrived, and whether an earlier Run of
// the same work received it. An input no longer stored is described by none.
// A failed read describes none rather than failing the caller's plan output.
func PlanQuoteOrigins(ctx context.Context, store *control.Store, tenantID, runID string, steps []control.RunPlanStep) map[string]*kernel.PlanQuoteOrigin {
	origins, err := store.CancellationQuoteOrigins(ctx, tenantID, steps)
	if err != nil {
		log.Warn("plan quote origin lookup failed", "run_id", runID, "error", err)
		return nil
	}
	described := make(map[string]*kernel.PlanQuoteOrigin, len(origins))
	for source, origin := range origins {
		input := kernel.PlanQuoteFromRequest
		if origin.Kind == control.CancellationQuoteFromSteering {
			input = kernel.PlanQuoteFromGuidance
		}
		described[source] = &kernel.PlanQuoteOrigin{Input: input, ReceivedAt: origin.ReceivedAt.Format(time.RFC3339), EarlierRun: origin.RunID != runID}
	}
	return described
}

// DisplayedPlanStep is a durable step as it leaves the runtime for a client or
// a model: the person input that authorized its cancellation in place of the
// id the runtime recorded.
type DisplayedPlanStep struct {
	control.RunPlanStep
	CancellationAuthorizedBy *kernel.PlanQuoteOrigin `json:"cancellation_authorized_by,omitempty"`
}

// DisplayedPlanSteps prepares runID's durable steps for display.
func DisplayedPlanSteps(ctx context.Context, store *control.Store, tenantID, runID string, steps []control.RunPlanStep) []DisplayedPlanStep {
	origins := PlanQuoteOrigins(ctx, store, tenantID, runID, steps)
	out := make([]DisplayedPlanStep, 0, len(steps))
	for _, step := range steps {
		origin := origins[step.CancellationQuoteSource]
		step.CancellationQuoteSource = ""
		out = append(out, DisplayedPlanStep{RunPlanStep: step, CancellationAuthorizedBy: origin})
	}
	return out
}
