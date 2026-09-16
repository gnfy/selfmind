package tools

import (
	"context"
	"errors"
	"testing"
)

type responseJudge struct {
	response ApprovalResponse
	err      error
}

func (j responseJudge) Judge(context.Context, string) (string, error) {
	return j.response.Content, j.err
}
func (j responseJudge) JudgeResponse(context.Context, string) (ApprovalResponse, error) {
	return j.response, j.err
}

func TestApprovalProtocolFailureRemainsUnavailable(t *testing.T) {
	for _, tc := range []struct{ reply, class string }{
		{`{"outcome":`, "invalid_json"},
		{`{"outcome":"approve"}`, "incomplete_decision"},
		{`{"outcome":"deny","risk_level":"unknown","user_authorization":"high","rationale":"No"}`, "incomplete_decision"},
	} {
		t.Run(tc.class+tc.reply, func(t *testing.T) {
			judge := responseJudge{response: ApprovalResponse{Content: tc.reply, ApprovalResponseMetadata: ApprovalResponseMetadata{Version: 1, FinishReason: "stop", ResponseBytes: len(tc.reply), OutputTokens: 128}}}
			verdict, assessment, err := triageApprovalWithIntent(context.Background(), judge, "write_file", "artifact.txt", "effect authorization", RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Create artifact.txt"})
			var failure *ApprovalResponseError
			if verdict != TriageEscalate || !errors.As(err, &failure) || failure.Class != tc.class || assessment.Response.ProtocolStatus != tc.class || assessment.Response.OutputTokens != 128 {
				t.Fatalf("verdict=%v assessment=%+v err=%v", verdict, assessment, err)
			}
		})
	}
}
