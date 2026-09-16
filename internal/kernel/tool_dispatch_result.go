package kernel

import "strconv"

// ToolDispatchResult carries runtime observations independently of presentation.
// It contains no Task/Thread identity or authorization. The invocation context
// and durable ledger remain the authority for scope, ownership and replay.
type ToolDispatchResult struct {
	Output string
	// Nil means a compatibility backend cannot prove whether its body ran.
	Invoked  *bool
	Process  *ToolProcessResult
	Evidence []RunEvidence
}

// ToolProcessResult describes the observed process, not the tool's success.
// A missing exit status must never turn into a successful exit code of zero.
type ToolProcessResult struct {
	Started         bool   `json:"started"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	SandboxMode     string `json:"sandbox_mode,omitempty"`
	RecoveryOutcome string `json:"recovery_outcome,omitempty"`
}

// ToolResultBackend is implemented by the production dispatcher. Legacy
// backends enter through DispatchToolResult without inventing execution facts.
type ToolResultBackend interface {
	DispatchResult(string, map[string]interface{}) (ToolDispatchResult, error)
}

func DispatchToolResult(backend ToolBackend, name string, args map[string]interface{}) (ToolDispatchResult, error) {
	if typed, ok := backend.(ToolResultBackend); ok {
		return typed.DispatchResult(name, args)
	}
	output, err := backend.Dispatch(name, args)
	return ToolDispatchResult{Output: output}, err
}

func applyDispatchFacts(envelope *ToolResultEnvelope, result ToolDispatchResult) {
	envelope.Invoked = result.Invoked
	envelope.Process = result.Process
	if envelope.ErrorCategory != "" && result.Process != nil {
		exit := "unknown"
		if result.Process.ExitCode != nil {
			exit = strconv.Itoa(*result.Process.ExitCode)
		}
		envelope.ModelContent = "Process observation: started=" + strconv.FormatBool(result.Process.Started) + "; exit_code=" + exit + ".\n" + envelope.ModelContent
	}
	for _, evidence := range result.Evidence {
		if evidence.ToolCallID != "" {
			envelope.EvidenceRefs = append(envelope.EvidenceRefs, evidence.ToolCallID)
		}
	}
}
