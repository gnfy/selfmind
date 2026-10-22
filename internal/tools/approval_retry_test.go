package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

type retryJudge struct {
	calls atomic.Int32
	call  func(context.Context, int) (ApprovalResponse, error)
}

type budgetedRetryJudge struct {
	*retryJudge
	budget time.Duration
}

func (j budgetedRetryJudge) ApprovalJudgeTimeout() time.Duration { return j.budget }

func TestApprovalShortBudgetReservesARealTimeoutRetry(t *testing.T) {
	for _, callerBound := range []bool{false, true} {
		t.Run(fmt.Sprint(callerBound), func(t *testing.T) {
			ctx := context.Background()
			if callerBound {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			j := budgetedRetryJudge{budget: 100 * time.Millisecond, retryJudge: &retryJudge{call: func(ctx context.Context, n int) (ApprovalResponse, error) {
				if n == 1 {
					<-ctx.Done()
					return ApprovalResponse{}, ctx.Err()
				}
				if ctx.Err() != nil {
					return ApprovalResponse{}, ctx.Err()
				}
				return ApprovalResponse{Content: "APPROVE"}, nil
			}}}
			v, a, err := triageApproval(ctx, j, "terminal", "inspect", "review", "inspect")
			if err != nil || v != TriageApprove || j.calls.Load() != 2 || len(a.Response.Attempts) != 2 || a.Response.Attempts[0].ProtocolStatus != "timeout" {
				t.Fatalf("first attempt consumed the retry budget: calls=%d verdict=%v attempts=%v err=%v", j.calls.Load(), v, a.Response.Attempts, err)
			}
		})
	}
}

func TestApprovalFirstAttemptUsesEffectiveBudget(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		configured, caller, want time.Duration
	}{
		{"configured ten seconds", 10 * time.Second, 0, 8 * time.Second},
		{"default capped budget", 30 * time.Second, 0, 10 * time.Second},
		{"shorter caller", 15 * time.Second, 5 * time.Second, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.caller)
				defer cancel()
			}
			var available time.Duration
			j := budgetedRetryJudge{budget: tc.configured, retryJudge: &retryJudge{call: func(ctx context.Context, _ int) (ApprovalResponse, error) {
				deadline, _ := ctx.Deadline()
				available = time.Until(deadline)
				return ApprovalResponse{Content: "APPROVE"}, nil
			}}}
			v, _, err := triageApproval(ctx, j, "terminal", "inspect", "review", "inspect")
			if err != nil || v != TriageApprove || j.calls.Load() != 1 || available > tc.want || available < tc.want-100*time.Millisecond {
				t.Fatalf("first budget=%s want near %s, calls=%d verdict=%v err=%v", available, tc.want, j.calls.Load(), v, err)
			}
		})
	}
}

func TestApprovalCancelledCallerDoesNotStartReview(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := &retryJudge{call: func(context.Context, int) (ApprovalResponse, error) {
		return ApprovalResponse{Content: "APPROVE"}, nil
	}}
	v, _, err := triageApproval(ctx, j, "terminal", "inspect", "review", "inspect")
	if v != TriageEscalate || !errors.Is(err, context.Canceled) || j.calls.Load() != 0 {
		t.Fatalf("cancelled caller dispatched review: calls=%d verdict=%v err=%v", j.calls.Load(), v, err)
	}
}

func (j *retryJudge) Judge(ctx context.Context, prompt string) (string, error) {
	r, err := j.JudgeResponse(ctx, prompt)
	return r.Content, err
}

func (j *retryJudge) JudgeResponse(ctx context.Context, _ string) (ApprovalResponse, error) {
	return j.call(ctx, int(j.calls.Add(1)))
}

func TestApprovalRetriesOnlyTransientUnavailability(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply string
		err   error
		calls int32
	}{
		{"transport", "", io.EOF, 2},
		{"timeout", "", context.DeadlineExceeded, 2},
		{"empty", "", &ApprovalResponseError{Class: "empty_output"}, 2},
		{"provider transient", "", &llm.ProviderError{Class: llm.ProviderErrorTransient}, 2},
		{"provider output limit", "", &llm.ProviderError{Class: llm.ProviderErrorEmptyResponse, StopReason: "length"}, 1},
		{"deny", "DENY", nil, 1},
		{"escalate", "ESCALATE", nil, 1},
		{"malformed", "not a verdict", nil, 1},
		{"truncated", "", &ApprovalResponseError{Class: "output_limit"}, 1},
		{"auth", "", &llm.ProviderError{Class: llm.ProviderErrorAuth}, 1},
		{"unknown", "", errors.New("unknown failure"), 1},
		{"invalid DNS", "", &net.DNSError{Err: "no such host", IsNotFound: true}, 1},
		{"cancelled", "", context.Canceled, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			j := &retryJudge{call: func(_ context.Context, n int) (ApprovalResponse, error) {
				if n == 1 {
					return ApprovalResponse{Content: test.reply}, test.err
				}
				return ApprovalResponse{Content: "APPROVE"}, nil
			}}
			v, _, _ := triageApproval(context.Background(), j, "terminal", "inspect", "review", "inspect")
			if j.calls.Load() != test.calls || test.calls == 2 && v != TriageApprove {
				t.Fatalf("calls=%d verdict=%v, want calls=%d", j.calls.Load(), v, test.calls)
			}
		})
	}
}

func TestApprovalRetryRespectsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	j := &retryJudge{call: func(ctx context.Context, _ int) (ApprovalResponse, error) {
		<-ctx.Done()
		return ApprovalResponse{}, ctx.Err()
	}}
	v, _, err := triageApproval(ctx, j, "terminal", "inspect", "review", "inspect")
	if v != TriageEscalate || !errors.Is(err, context.DeadlineExceeded) || j.calls.Load() > 2 || ctx.Err() == nil {
		t.Fatalf("deadline renewed: calls=%d verdict=%v err=%v", j.calls.Load(), v, err)
	}
}

func TestLateApprovalCannotOverrideSecondAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, done := make(chan struct{}), make(chan struct{})
	j := &retryJudge{call: func(_ context.Context, n int) (ApprovalResponse, error) {
		if n == 1 {
			defer close(done)
			<-release // deliberately ignores cancellation
			return ApprovalResponse{Content: "APPROVE"}, nil
		}
		close(release)
		<-done
		return ApprovalResponse{Content: "DENY"}, nil
	}}
	r := reviewApproval(ctx, j, "review", 10*time.Millisecond, nil)
	if r.err != nil || r.reply != "DENY" || j.calls.Load() != 2 || len(r.response.Attempts) != 2 || r.response.Attempts[0].ProtocolStatus != "timeout" {
		t.Fatalf("late reply replaced decision: %+v calls=%d", r, j.calls.Load())
	}
}

func TestChangedIntentStopsApprovalAndStaleHumanAsk(t *testing.T) {
	for _, firstError := range []error{nil, io.EOF} {
		t.Run(fmt.Sprint(firstError), func(t *testing.T) {
			root := t.TempDir()
			var changed atomic.Bool
			j := &retryJudge{call: func(context.Context, int) (ApprovalResponse, error) {
				changed.Store(true)
				return ApprovalResponse{Content: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"Requested write."}`}, firstError
			}}
			asked, ran := false, false
			scope := ExecutionScope{TenantID: "intent-fence", PersonID: "intent-fence", RunID: "intent-fence", WorkspaceRoot: root, AllowedRoots: []string{root}, ApprovalMode: ApprovalSmart, Judge: j,
				IntentSnapshot: func() RunIntentSnapshot {
					s := RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Write the report."}
					if changed.Load() {
						s.AddedRequirements = []string{"Wait; leave the report unchanged."}
					}
					return s
				},
				Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
					asked = true
					return ToolApprovalDecision{}, nil
				},
			}
			defer SetExecutionScope("intent-fence", scope)()
			exec := SmartApprovalMiddleware(root)(func(map[string]interface{}) (string, error) { ran = true; return "", nil })
			_, err := exec(map[string]interface{}{"_tenant_id": "intent-fence", "_tool_name": "write_file", "path": root + "/report.txt", "content": "report"})
			if ran || asked || j.calls.Load() != 1 || err == nil || !strings.Contains(err.Error(), "requirements changed") {
				t.Fatalf("stale review used: ran=%v asked=%v calls=%d err=%v", ran, asked, j.calls.Load(), err)
			}
		})
	}
}

func TestApprovalRetryPreservesBothAttemptsUsage(t *testing.T) {
	j := &retryJudge{call: func(_ context.Context, n int) (ApprovalResponse, error) {
		r := ApprovalResponse{Content: "APPROVE", ApprovalResponseMetadata: ApprovalResponseMetadata{Version: 2, Usage: &llm.UsageStats{InputTokens: 10 * n, OutputTokens: n, CacheUsageReported: true}, OutputTokens: n}}
		if n == 1 {
			return r, io.EOF
		}
		return r, nil
	}}
	_, a, err := triageApproval(context.Background(), j, "terminal", "inspect", "review", "inspect")
	if err != nil || len(a.Response.Attempts) != 2 || a.Response.Usage.InputTokens != 30 || a.Response.OutputTokens != 3 {
		t.Fatalf("lost retry cost: %+v err=%v", a.Response, err)
	}
}

func TestApprovalRetryKeepsHumanFallbackAndExecutesAtMostOnce(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			root := t.TempDir()
			j := &retryJudge{call: func(_ context.Context, n int) (ApprovalResponse, error) {
				if recover && n == 2 {
					return ApprovalResponse{Content: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"Requested write."}`}, nil
				}
				return ApprovalResponse{}, io.EOF
			}}
			asked, executed := 0, 0
			scope := ExecutionScope{TenantID: "retry-fallback", PersonID: "retry-fallback", RunID: "retry-fallback", WorkspaceRoot: root, AllowedRoots: []string{root}, ApprovalMode: ApprovalSmart, Judge: j,
				IntentSnapshot: func() RunIntentSnapshot {
					return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Write the report."}
				},
				Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
					asked++
					return ToolApprovalDecision{Approved: false, Outcome: ApprovalOutcomeDenied}, nil
				},
			}
			defer SetExecutionScope("retry-fallback", scope)()
			exec := SmartApprovalMiddleware(root)(func(map[string]interface{}) (string, error) { executed++; return "done", nil })
			_, err := exec(map[string]interface{}{"_tenant_id": "retry-fallback", "_tool_name": "write_file", "path": root + "/report.txt", "content": "report"})
			if j.calls.Load() != 2 || recover && (asked != 0 || executed != 1 || err != nil) || !recover && (asked != 1 || executed != 0 || err == nil) {
				t.Fatalf("wrong fallback: calls=%d asked=%d executed=%d err=%v", j.calls.Load(), asked, executed, err)
			}
		})
	}
}

func TestApprovalRetryUsesOneBoundedBudget(t *testing.T) {
	var deadlines []time.Time
	j := &retryJudge{call: func(ctx context.Context, n int) (ApprovalResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return ApprovalResponse{}, errors.New("missing deadline")
		}
		deadlines = append(deadlines, deadline)
		if n == 1 {
			return ApprovalResponse{}, io.EOF
		}
		return ApprovalResponse{Content: "APPROVE"}, nil
	}}
	start := time.Now()
	v, _, err := triageApproval(context.Background(), j, "terminal", "inspect", "review", "inspect")
	if err != nil || v != TriageApprove || len(deadlines) != 2 {
		t.Fatalf("retry missing: %v %v %v", v, err, deadlines)
	}
	if deadlines[0].After(start.Add(10*time.Second+time.Second)) || deadlines[1].After(start.Add(15*time.Second+time.Second)) {
		t.Fatalf("unbounded review deadlines: %v", deadlines)
	}
}

func TestApprovalStructuredJSONExtractionAndSingleRetryRecovery(t *testing.T) {
	t.Run("extracts valid JSON after wrapper braces in one attempt", func(t *testing.T) {
		j := &retryJudge{call: func(_ context.Context, _ int) (ApprovalResponse, error) {
			return ApprovalResponse{Content: "<think>evaluating {target} safety</think>\n```json\n{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"Read-only inspection.\"}\n```"}, nil
		}}
		v, a, err := triageApprovalWithIntent(context.Background(), j, "terminal", "inspect", "review", RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Inspect workspace."})
		if err != nil || v != TriageApprove || j.calls.Load() != 1 || len(a.Response.Attempts) != 1 || a.Response.ProtocolStatus != "valid_decision" {
			t.Fatalf("unexpected extraction result: calls=%d verdict=%v response=%+v err=%v", j.calls.Load(), v, a.Response, err)
		}
	})

	for _, firstReply := range []struct {
		name   string
		reply  string
		status string
	}{
		{"invalid_json", "I think we should approve this request.", "invalid_json"},
		{"incomplete_decision", `{"outcome":"approve"}`, "incomplete_decision"},
	} {
		t.Run(firstReply.name, func(t *testing.T) {
			var secondPrompt string
			j := &retryJudge{call: func(_ context.Context, n int) (ApprovalResponse, error) {
				if n == 1 {
					return ApprovalResponse{Content: firstReply.reply, ApprovalResponseMetadata: ApprovalResponseMetadata{OutputTokens: 32}}, nil
				}
				return ApprovalResponse{Content: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"Authorized inspection."}`, ApprovalResponseMetadata: ApprovalResponseMetadata{OutputTokens: 48}}, nil
			}}
			v, a, err := triageApprovalWithIntent(context.Background(), promptCapturingJudge{retryJudge: j, onPrompt: func(n int, p string) {
				if n == 2 {
					secondPrompt = p
				}
			}}, "terminal", "inspect", "review", RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Inspect workspace."})
			if err != nil || v != TriageApprove || j.calls.Load() != 2 || len(a.Response.Attempts) != 2 {
				t.Fatalf("expected retry recovery: calls=%d verdict=%v attempts=%+v err=%v", j.calls.Load(), v, a.Response.Attempts, err)
			}
			if a.Response.Attempts[0].ProtocolStatus != firstReply.status || a.Response.ProtocolStatus != "valid_decision" || a.Response.OutputTokens != 80 {
				t.Fatalf("unexpected attempt metadata: %+v", a.Response)
			}
			if !strings.Contains(secondPrompt, "Reply ONLY with a single valid JSON object") {
				t.Fatalf("expected strict JSON retry reminder on second attempt, got %q", secondPrompt)
			}
		})
	}
}

type promptCapturingJudge struct {
	*retryJudge
	onPrompt func(int, string)
}

func (j promptCapturingJudge) JudgeResponse(ctx context.Context, prompt string) (ApprovalResponse, error) {
	n := int(j.calls.Add(1))
	if j.onPrompt != nil {
		j.onPrompt(n, prompt)
	}
	return j.call(ctx, n)
}
