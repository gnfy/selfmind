package llm

// Current plan evidence precedes stale IDs retained in older context. Live
// time-based tool-result aging and instant replay may retain different amounts
// of that old context; first-ever-occurrence indexing shifts current step IDs.
// Version zero preserves existing cassettes' historical binding contract.
func vcrPlanStepIDs(messages []Message, version int) []string {
	if version == 0 {
		return vcrOpaqueIDs(messages, vcrPlanStepIDPattern)
	}
	newest := make([]Message, len(messages))
	for i := range messages {
		newest[i] = messages[len(messages)-1-i]
	}
	return vcrOpaqueIDs(newest, vcrPlanStepIDPattern)
}
