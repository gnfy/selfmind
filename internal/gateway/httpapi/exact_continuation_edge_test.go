package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/gateway/api"
)

// RUQX-1272 lost its plan progress at every approval answer. Only the runtime's
// own resume edges keep progress; a person's turn never does.
func TestOnlyTheRuntimesOwnResumeEdgesAreExactContinuations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason taskAttachReason
		origin string
		want   bool
	}{
		{"approval answer", taskAttachApprovalResume, "", true},
		{"clarification answer", taskAttachClarifyResume, "", true},
		{"approval answer by origin", taskAttachContinuation, runOriginApproval, true},
		{"watcher result", taskAttachContinuation, runOriginWatch, true},
		{"recovery", taskAttachContinuation, runOriginRecovery, true},
		{"resource wake-up", taskAttachContinuation, runOriginResource, true},
		{"provider wait", taskAttachContinuation, runOriginProviderWait, true},
		{"person continues work", taskAttachContinuation, "", false},
		{"person replies to a run", taskAttachReplyToRun, "", false},
		{"cron turn", taskAttachContinuation, "cron", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attach := newTaskAttach(tc.reason, "", false, false)
			if got := exactContinuationEdge(context.Background(), api.MessageRequest{Origin: tc.origin}, attach); got != tc.want {
				t.Fatalf("exactContinuationEdge = %v, want %v", got, tc.want)
			}
		})
	}
}
