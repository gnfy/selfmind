package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Tool execution ledger (Loop Engineering ACTIVE PLAN P0-B, first slice). The
// dangerous window is: a side-effectful tool succeeds (a build is triggered, a
// file is written) and the daemon crashes BEFORE that outcome is durably
// recorded. Boot recovery previously re-ran whole turns, which could fire the
// same external side effect twice. The ledger records each dispatch with a
// retry class so recovery can VERIFY an uncertain side effect instead of
// blindly repeating it.
//
// The kernel owns no control-plane storage, so the ledger is an injected seam
// (mirrors ToolArtifactSink). A nil ledger is valid — recording degrades to a
// no-op and the loop is unaffected.

// ToolRetryClass grades how safe a tool is to re-run after an uncertain crash.
type ToolRetryClass string

const (
	// ToolRetryReadOnly: provably read-only; a repeat is free and safe.
	ToolRetryReadOnly ToolRetryClass = "read_only"
	// ToolRetryIdempotent: mutates but converges (same inputs → same state);
	// a repeat is safe but not free.
	ToolRetryIdempotent ToolRetryClass = "idempotent"
	// ToolRetrySideEffect: may cause an external, non-reversible effect
	// (deploy, network POST, arbitrary shell). An uncertain entry of this
	// class MUST be verified, never blindly repeated.
	ToolRetrySideEffect ToolRetryClass = "side_effect"
)

// readOnlyLedgerTools mirrors the parallel-safe / idempotent read set. Only
// tools proven read-only earn blind-retry; everything else fails safe toward
// verification.
var readOnlyLedgerTools = map[string]struct{}{
	"read_file": {}, "cat": {}, "ls_r": {}, "list_files": {}, "search_files": {},
	"grep": {}, "web_search": {}, "web_extract": {}, "session_search": {},
	"get_current_time": {}, "process_list": {}, "process_poll": {}, "tool_output_view": {},
	"skill_view": {}, "skills_list": {},
}

// idempotentLedgerTools mutate local state but converge on replay.
var idempotentLedgerTools = map[string]struct{}{
	"update_plan": {}, "finish_run": {}, "write_file": {}, "patch": {}, "edit": {},
}

// ClassifyToolRetry grades a tool. The default is the SAFEST assumption
// (side-effect requiring verification), so an unknown or new tool never earns
// a blind re-run by omission.
func ClassifyToolRetry(name string) ToolRetryClass {
	name = strings.TrimSpace(name)
	if _, ok := readOnlyLedgerTools[name]; ok {
		return ToolRetryReadOnly
	}
	if _, ok := idempotentLedgerTools[name]; ok {
		return ToolRetryIdempotent
	}
	return ToolRetrySideEffect
}

// ToolLedgerEntry is one dispatch record.
type ToolLedgerEntry struct {
	RunID                 string
	ToolCallID            string
	ToolName              string
	ArgsHash              string
	RetryClass            ToolRetryClass
	EffectID              string
	PlanVersion           int
	PlanStepID            string
	Strategy              string
	EffectClass           string
	EnvironmentGeneration int64
}

// ToolDispatchDecision is the durable claim result returned before execution.
// Execute is true for one caller only; a duplicate returns the existing state
// and must not be replayed.
type ToolDispatchDecision struct {
	Execute bool
	Status  string
}

// ToolLedger persists tool-dispatch lifecycle for crash recovery. It must
// tolerate concurrent use. For non-read-only tools, failure to persist the
// pre-execution claim is safety-critical and prevents execution.
type ToolLedger interface {
	// ClaimDispatch durably claims a tool BEFORE execution begins.
	ClaimDispatch(ctx context.Context, entry ToolLedgerEntry) (ToolDispatchDecision, error)
	// RecordOutcome marks the dispatched tool completed (ok or failed) AFTER
	// execution returns, closing the uncertain window.
	RecordOutcome(ctx context.Context, runID, toolCallID string, ok bool) error
}

// ToolLedgerOutcomeRecorder is the v1 recovery-contract extension. The result
// reference is a content hash only; raw tool output remains in its existing
// bounded event/artifact surfaces.
type ToolLedgerOutcomeRecorder interface {
	RecordOutcomeWithRef(ctx context.Context, runID, toolCallID string, ok bool, resultRef string) error
}

// ToolArgsHash is the stable dispatch fingerprint used to correlate a ledger
// entry with the model's tool call across a restart.
func ToolArgsHash(args string) string {
	sum := sha256.Sum256([]byte(args))
	return hex.EncodeToString(sum[:16])
}

func ToolEffectID(runID, toolCallID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(runID) + "\x00" + strings.TrimSpace(toolCallID)))
	return "effect_" + hex.EncodeToString(sum[:12])
}

func ToolResultReference(raw string, ok bool) string {
	state := "failed"
	if ok {
		state = "completed"
	}
	sum := sha256.Sum256([]byte(state + "\x00" + raw))
	return "result_" + hex.EncodeToString(sum[:12])
}

func ToolExecutionStrategy(name string, retryClass ToolRetryClass) string {
	switch strings.TrimSpace(name) {
	case "update_plan", "finish_run", "clarify":
		return "interact"
	case "verify":
		return "verify"
	case "watch_external", "process_poll":
		return "wait"
	}
	if retryClass == ToolRetryReadOnly {
		return "observe"
	}
	return "mutate"
}

type toolLedgerKey struct{}

// WithToolLedger installs the per-run tool ledger.
func WithToolLedger(ctx context.Context, ledger ToolLedger) context.Context {
	if ctx == nil || ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, toolLedgerKey{}, ledger)
}

// ToolLedgerFromContext returns the run's ledger, or nil.
func ToolLedgerFromContext(ctx context.Context) ToolLedger {
	if ctx == nil {
		return nil
	}
	ledger, _ := ctx.Value(toolLedgerKey{}).(ToolLedger)
	return ledger
}
