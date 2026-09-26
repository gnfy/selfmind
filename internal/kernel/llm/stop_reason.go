package llm

import (
	"net/http"
	"strings"
)

// StopReason is the protocol-neutral reason a model reply ended. Adapters pass
// the provider's raw finish or stop reason through unchanged; this is the one
// place that maps each protocol's vocabulary onto what the agent loop acts on.
type StopReason string

const (
	// StopComplete: the model ended its turn, possibly by requesting tools.
	StopComplete StopReason = "complete"
	// StopLength: the output budget ran out before the reply finished.
	StopLength StopReason = "length"
	// StopInterrupted: the provider halted generation for its own reasons,
	// such as capacity, a paused long turn, or an upstream error.
	StopInterrupted StopReason = "interrupted"
	// StopFiltered: the provider withheld the rest of the reply under its
	// content policy.
	StopFiltered StopReason = "filtered"
	// StopMissing: the stream reached its protocol terminator without a
	// reason. Several providers omit it, so this alone is no evidence of a cut.
	StopMissing StopReason = "missing"
	// StopOther: a reason this table does not know.
	StopOther StopReason = "other"
)

// ClassifyStopReason maps a raw OpenAI, OpenAI-compatible, Anthropic or
// Responses reason onto a StopReason. Matching ignores case, which also covers
// Gemini's upper-case values.
func ClassifyStopReason(raw string) StopReason {
	reason := strings.ToLower(strings.TrimSpace(raw))
	switch reason {
	case "":
		return StopMissing
	case "stop", "end_turn", "stop_sequence", "tool_calls", "tool_use", "function_call", "completed":
		return StopComplete
	case "length", "max_tokens", "max_output_tokens", "output_limit", "model_context_window_exceeded":
		return StopLength
	case "content_filter", "safety", "recitation", "refusal", "blocklist", "prohibited_content", "spii":
		return StopFiltered
	case "insufficient_system_resource", "aborted", "pause_turn", "incomplete", "error":
		return StopInterrupted
	}
	if strings.Contains(reason, "max_token") || strings.Contains(reason, "length") {
		return StopLength
	}
	return StopOther
}

// Continuable reports whether the reply stopped early in a way that asking the
// model to continue can repair.
func (r StopReason) Continuable() bool {
	return r == StopLength || r == StopInterrupted
}

// providerErrorCodeStreamUnterminated marks a stream that closed before its
// protocol's terminal event without having reported a stop reason. What arrived
// is a prefix that may be cut anywhere, so it is retryable transport damage and
// never a finished answer.
const providerErrorCodeStreamUnterminated = "stream_unterminated"

func streamUnterminatedError(provider string) error {
	return &ProviderError{Provider: provider, Class: ProviderErrorTransient, StatusCode: http.StatusOK,
		Code: providerErrorCodeStreamUnterminated, Message: "stream ended before its terminal event and without a stop reason"}
}

// IsStreamUnterminated reports the error above, whichever adapter raised it.
func IsStreamUnterminated(err error) bool {
	info, ok := ProviderErrorInfo(err)
	return ok && info.Code == providerErrorCodeStreamUnterminated
}
