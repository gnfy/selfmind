package tools

import (
	"errors"
	"fmt"
)

func sandboxPreparationError(backend string, cause error) error {
	message := fmt.Sprintf("isolated execution is unavailable (%s cannot enforce this plan): %v", backend, cause)
	return newStableToolRecoveryError(fmt.Errorf("isolated execution is unavailable (%s cannot enforce this plan): %w", backend, cause),
		"sandbox_plan_unsupported", "environment", message,
		"No process was started. Inspect the execution plan and required tool state. Changing command spelling or repeating the same preparation cannot fix an unsupported requirement; use an exposed file inspection tool where sufficient, or a backend that enforces the required plan.",
		"preparation", "after_environment_change", "not_dispatched", false)
}

// isolationRequiredError refuses a call that demanded isolation — an explicit
// isolated request or a required policy — on a host that cannot provide it.
// Nothing started, and nothing ran on the host instead.
func isolationRequiredError(cause error) error {
	return newStableToolRecoveryError(cause,
		"sandbox_isolation_unavailable", "environment",
		fmt.Sprintf("%v. This call requires isolated execution; it was not dispatched and did not run on the host.", cause),
		"No process was started. Repeating the call cannot change the host's isolation capability or policy; see /diag execution.",
		"preparation", "after_environment_change", "not_dispatched", false)
}

// approvedBoundaryUnavailableError refuses a call whose approval — by a person,
// the judge, or a remembered grant — assumed the isolated boundary, when that
// boundary cannot be enforced at dispatch. Running it on the host would
// execute something nobody approved.
func approvedBoundaryUnavailableError(cause error) error {
	return newStableToolRecoveryError(fmt.Errorf("approved isolated boundary is unavailable: %w", cause),
		"sandbox_boundary_unavailable", "environment",
		fmt.Sprintf("This call was approved to run inside the isolated sandbox, but that boundary cannot be enforced now (%v). It was not dispatched and did not run on the host.", cause),
		"No process was started. The approval covered isolated execution only; repeating the unchanged call cannot restore it. A host run, where policy allows one, needs its own approval.",
		"preparation", "after_environment_change", "not_dispatched", false)
}

// unenforceableBoundaryError refuses, before any judge or person is asked, a
// call whose isolated plan the backend cannot enforce. No approval is
// requested for a boundary that would not be the one enforced.
func unenforceableBoundaryError(reason string) error {
	cause := errors.New(reason)
	return newStableToolRecoveryError(fmt.Errorf("isolated execution cannot be enforced for this call: %w", cause),
		"sandbox_plan_unsupported", "environment",
		fmt.Sprintf("Isolated execution cannot be enforced for this call (%s). It was not dispatched, and no approval was requested.", reason),
		"No process was started. Changing command spelling or repeating the call cannot fix an unsupported requirement; inspect /diag execution for the boundary this host can enforce.",
		"preparation", "after_environment_change", "not_dispatched", false)
}
