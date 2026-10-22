package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"selfmind/internal/kernel/llm"
)

const approvalFirstAttemptLimit = 10 * time.Second

type approvalJudgeResult struct {
	reply    string
	response ApprovalResponseMetadata
	err      error
}

// reviewApproval shares one deadline, including retries inside a transport.
// Each attempt has its own result channel: a late, context-ignoring provider
// cannot replace a later decision. Nothing here retries an execution tool.
func reviewApproval(ctx context.Context, judge ApprovalJudge, prompt string, firstLimit time.Duration, current func() bool, requireStructured ...bool) (out approvalJudgeResult) {
	started := time.Now()
	var attempts []ApprovalAttemptMetadata
	var usage *llm.UsageStats
	var output, reasoning int
	usageIncomplete := false
	defer func() {
		out.response.Attempts = attempts
		out.response.DurationMS = time.Since(started).Milliseconds()
		out.response.OutputTokens, out.response.ReasoningTokens = output, reasoning
		out.response.Usage = usage
		out.response.UsageIncomplete = usageIncomplete
		if usageIncomplete && usage != nil {
			usage.CacheUsageReported, usage.CacheCreationReported = false, false
		}
		var responseErr *ApprovalResponseError
		if errors.As(out.err, &responseErr) {
			out.response.ProtocolStatus = responseErr.Class
			responseErr.Metadata = out.response
		}
	}()
	for n := 0; n < 2; n++ {
		if err := ctx.Err(); err != nil {
			out.err = err
			return
		}
		if current != nil && !current() {
			out.err = &ApprovalResponseError{Class: "intent_changed"}
			return
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		if n == 0 {
			cancel()
			// Reserve part of the effective shared budget, including a shorter
			// caller deadline. Otherwise a timeout consumes both attempts at once.
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if reserved := remaining - remaining/5; reserved < firstLimit {
					firstLimit = reserved
				}
			}
			attemptCtx, cancel = context.WithTimeout(ctx, firstLimit)
		}
		ch := make(chan approvalJudgeResult, 1)
		attemptStarted := time.Now()
		go func() {
			if err := attemptCtx.Err(); err != nil {
				ch <- approvalJudgeResult{err: err}
				return
			}
			if structured, ok := judge.(StructuredApprovalJudge); ok {
				r, err := structured.JudgeResponse(attemptCtx, prompt)
				ch <- approvalJudgeResult{reply: r.Content, response: r.ApprovalResponseMetadata, err: err}
			} else {
				reply, err := judge.Judge(attemptCtx, prompt)
				ch <- approvalJudgeResult{reply: reply, err: err}
			}
		}()
		select {
		case <-attemptCtx.Done():
			out = approvalJudgeResult{err: attemptCtx.Err()}
		case out = <-ch:
		}
		// Cancellation/deadline wins even if a result became ready concurrently.
		if err := attemptCtx.Err(); err != nil {
			out.err = err
		}
		cancel()
		if out.err == nil && strings.TrimSpace(out.reply) == "" {
			out.err = &ApprovalResponseError{Class: "empty_output", Metadata: out.response}
		}
		if out.err == nil && len(requireStructured) > 0 && requireStructured[0] {
			if protocolErr := validateStructuredTriageReply(out.reply); protocolErr != nil {
				out.response.ProtocolStatus = protocolErr.Class
				protocolErr.Metadata = out.response
				out.err = protocolErr
			}
		}
		status := out.response.ProtocolStatus
		if out.err != nil {
			status = approvalRetryErrorClass(out.err)
		}
		attempts = append(attempts, ApprovalAttemptMetadata{DurationMS: time.Since(attemptStarted).Milliseconds(), ProtocolStatus: status, Usage: out.response.Usage})
		output += out.response.OutputTokens
		reasoning += out.response.ReasoningTokens
		if u := out.response.Usage; u != nil {
			if usage == nil {
				copy := *u
				usage = &copy
			} else {
				usage.InputTokens += u.InputTokens
				usage.OutputTokens += u.OutputTokens
				usage.CacheReadInputTokens += u.CacheReadInputTokens
				usage.CacheMissInputTokens += u.CacheMissInputTokens
				usage.CacheCreationInputTokens += u.CacheCreationInputTokens
				usage.ReasoningOutputTokens += u.ReasoningOutputTokens
				usage.CacheUsageReported = usage.CacheUsageReported && u.CacheUsageReported
				usage.CacheCreationReported = usage.CacheCreationReported && u.CacheCreationReported
			}
		} else {
			usageIncomplete = true
		}
		if err := ctx.Err(); err != nil {
			out.err = err
			return
		}
		if current != nil && !current() {
			out.err = &ApprovalResponseError{Class: "intent_changed"}
			return
		}
		if out.err == nil || !retryableApprovalFailure(out.err) {
			return
		}
		if status == "invalid_json" || status == "incomplete_decision" {
			prompt += "\n\nIMPORTANT: Your previous response was not a valid or complete JSON object. Reply ONLY with a single valid JSON object containing outcome, risk_level, user_authorization, and rationale."
		}
	}
	return
}

func retryableApprovalFailure(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var responseErr *ApprovalResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Class == "empty_output" || responseErr.Class == "invalid_json" || responseErr.Class == "incomplete_decision"
	}
	if info, ok := llm.ProviderErrorInfo(err); ok {
		stop := llm.ClassifyStopReason(info.StopReason)
		if stop.Continuable() || stop == llm.StopFiltered {
			return false
		}
		return info.Class == llm.ProviderErrorTransient || info.Class == llm.ProviderErrorEmptyResponse
	}
	var transport net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) ||
		errors.As(err, &transport) && (transport.Timeout() || transport.Temporary())
}

func approvalRetryErrorClass(err error) string {
	var responseErr *ApprovalResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if info, ok := llm.ProviderErrorInfo(err); ok {
		return string(info.Class)
	}
	return "provider_error"
}
