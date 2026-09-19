package tools

import (
	"context"
	"fmt"
	"selfmind/internal/kernel/llm"
)

// ApprovalResponse retains protocol facts without persisting model text or
// private reasoning. A complete transport response is not an approval verdict.
type ApprovalResponse struct {
	Content string `json:"-"`
	ApprovalResponseMetadata
}

type ApprovalResponseMetadata struct {
	Restriction     *RestrictionEvidence `json:"restriction,omitempty"`
	Usage           *llm.UsageStats      `json:"usage,omitempty"`
	Model           string               `json:"model,omitempty"`
	Role            string               `json:"role,omitempty"`
	DurationMS      int64                `json:"duration_ms,omitempty"`
	Version         int                  `json:"version,omitempty"`
	ToolCallID      string               `json:"tool_call_id,omitempty"`
	FinishReason    string               `json:"finish_reason,omitempty"`
	ResponseBytes   int                  `json:"response_bytes"`
	OutputTokens    int                  `json:"output_tokens"`
	ReasoningTokens int                  `json:"reasoning_tokens"`
	ProtocolStatus  string               `json:"protocol_status,omitempty"`
}

// StructuredApprovalJudge is implemented at the provider adapter. Legacy
// injected judges remain supported without inventing missing telemetry.
type StructuredApprovalJudge interface {
	JudgeResponse(context.Context, string) (ApprovalResponse, error)
}

type ApprovalResponseError struct {
	Class    string
	Metadata ApprovalResponseMetadata
}

func (e *ApprovalResponseError) Error() string {
	return fmt.Sprintf("automatic approval unavailable: %s (finish=%s, bytes=%d, output_tokens=%d, reasoning_tokens=%d)",
		e.Class, e.Metadata.FinishReason, e.Metadata.ResponseBytes, e.Metadata.OutputTokens, e.Metadata.ReasoningTokens)
}

func (e *ApprovalResponseError) ApprovalErrorClass() string { return e.Class }
