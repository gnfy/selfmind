package kernel

import "strings"

// Facts travel separately from captured stdout and the user preview. Keep the
// reference exact; a long id is omitted instead of truncated into a false one.
func modelObservationHint(evidence []RunEvidence) string {
	for _, fact := range evidence {
		if fact.Version != 1 || fact.ToolCallID == "" || len(fact.ToolCallID) > 256 || fact.Invoked == nil || !*fact.Invoked {
			continue
		}
		if fact.Kind == "command" || fact.Kind == "observation" {
			id := fact.ToolCallID
			if strings.ContainsAny(id, "\r\n") {
				continue
			}
			return "\nObservation reference: " + id + ". Main may explicitly associate this observation with a Plan condition at finish; it is not a completion verdict."
		}
	}
	return ""
}
