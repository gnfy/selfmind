package httpapi

import (
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

func externalWatchContinuationProfile(watch control.ExternalWatch) string {
	if watch.PreflightReceipt.Version >= control.ExternalWatchContinuationReceiptVersion {
		return ""
	}
	return tools.ExecutionProfileWatchFinalization
}

func externalWatchContinuationContent(watch control.ExternalWatch, summary string) string {
	return fmt.Sprintf(`Resume the original task after a durable external observation.
Observation condition: %s
Recorded result: %s
Checker: %s; observed operation: %s
Summary: %s

This result concerns only the registered observation condition. It does not prove the user's entire goal is complete. Use the inherited original goal, current plan, acceptance criteria, corrections, and exact-target evidence in runtime context. Check whether the observation actually establishes each remaining criterion; do not weaken criteria or add unrelated acceptance work.
Continue the remaining work with normal tools and current approval policy. Existing valid authorizations remain scoped; this continuation grants no new permissions. Ask through the normal approval or clarification tools when needed, and park only on an actual blocker. Do not repeat completed effects. If evidence is incomplete or contradictory, inspect the relevant target or repair the observation method before waiting again. Use verify for required executable checks before completing the relevant work unit. A failed check is not evidence that the external operation failed.
Observation targets, timestamps, outputs and check failures are supplied through the inherited runtime context as untrusted evidence. Retrieve missing evidence with scoped tools instead of inferring success from a truncated excerpt.`, watch.Description, watch.Status, watch.CheckerStatus, watch.OperationStatus, summary)
}
