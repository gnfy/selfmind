package tools

import "fmt"

func sandboxPreparationError(backend string, cause error) error {
	message := fmt.Sprintf("isolated execution is unavailable (%s cannot enforce this plan): %v", backend, cause)
	return newStableToolRecoveryError(fmt.Errorf("isolated execution is unavailable (%s cannot enforce this plan): %w", backend, cause),
		"sandbox_plan_unsupported", "environment", message,
		"No process was started. Inspect the execution plan and required tool state. Changing command spelling or repeating the same preparation cannot fix an unsupported requirement; use an exposed file inspection tool where sufficient, or a backend that enforces the required plan.",
		"preparation", "after_environment_change", "not_dispatched", false)
}
