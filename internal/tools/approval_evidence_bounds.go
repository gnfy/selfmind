package tools

import "selfmind/internal/platform/textutil"

// Bound complete quotations generously enough to retain numbered proposals and
// their qualifications. Any omitted evidence is explicit, including an earlier
// layer's omission; downstream prompt construction cannot silently erase it.
const approvalQuotationBytes = 16 * 1024
const approvalEvidenceBytes = 48 * 1024

func BoundApprovalEvidence(intent RunIntentSnapshot) RunIntentSnapshot {
	remaining := approvalEvidenceBytes
	bound := func(value string) string {
		limit := min(approvalQuotationBytes, remaining)
		if len(value) > limit {
			intent.AuthorizationEvidenceIncomplete = true
			value = textutil.TruncateBytes(value, limit)
		}
		remaining -= len(value)
		return value
	}
	intent.RawUserText = bound(intent.RawUserText)
	intent.PriorAssistantOffer = bound(intent.PriorAssistantOffer)
	intent.AddedRequirements = append([]string(nil), intent.AddedRequirements...)
	for i := range intent.AddedRequirements {
		intent.AddedRequirements[i] = bound(intent.AddedRequirements[i])
	}
	intent.AuthorizationEvidence = append([]AuthorizationEvidence(nil), intent.AuthorizationEvidence...)
	if len(intent.AuthorizationEvidence) > MaxAuthorizationEvidence {
		intent.AuthorizationEvidence = intent.AuthorizationEvidence[:MaxAuthorizationEvidence]
		intent.AuthorizationEvidenceIncomplete = true
	}
	for i := range intent.AuthorizationEvidence {
		intent.AuthorizationEvidence[i].UserText = bound(intent.AuthorizationEvidence[i].UserText)
		intent.AuthorizationEvidence[i].AcceptedOffer = bound(intent.AuthorizationEvidence[i].AcceptedOffer)
	}
	return intent
}
