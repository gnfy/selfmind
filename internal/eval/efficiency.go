package eval

import "encoding/json"

// Efficiency reports observed cost, not wasted work. Closing starts with the
// model call that first attempts finish_run and includes subsequent recovery.
// Judge time is deliberately not added to model/tool time: those intervals nest.
type Efficiency struct {
	UsageAvailable        bool  `json:"usage_available"`
	MainCalls             int   `json:"main_calls"`
	MainMS                int64 `json:"main_ms"`
	MainOutputTokens      int64 `json:"main_output_tokens"`
	ReasoningTokens       int64 `json:"reasoning_tokens"`
	OtherRoleCalls        int   `json:"other_role_calls"`
	UnknownRoleCalls      int   `json:"unknown_role_calls"`
	LifecycleCalls        int   `json:"lifecycle_calls"`
	VerifyCalls           int   `json:"verify_calls"`
	FinishAttempts        int   `json:"finish_attempts"`
	FinishRuns            int   `json:"finish_runs"`
	FinishFirstPassRuns   int   `json:"finish_first_pass_runs"`
	ClosingAvailable      bool  `json:"closing_available"`
	ClosingModelCalls     int   `json:"closing_model_calls"`
	ClosingModelMS        int64 `json:"closing_model_ms"`
	ClosingIncomplete     bool  `json:"closing_incomplete"`
	FinishIdentityMissing bool  `json:"finish_identity_missing"`
}

type efficiencyRun struct {
	lastMS       int64
	lastObserved bool
	closing      bool
	firstFinish  string
	firstResult  bool
}

type efficiencyAccumulator struct {
	Efficiency
	runs    map[string]*efficiencyRun
	started map[string]bool
	caseID  string
}

func (a *efficiencyAccumulator) observe(e JSONLEvent) {
	if a.runs == nil {
		a.runs = map[string]*efficiencyRun{}
		a.started = map[string]bool{}
	}
	if e.Type == "case_started" {
		a.caseID = e.CaseID
	}
	key := a.caseID + "/" + e.RunID
	r := a.runs[key]
	if r == nil {
		r = &efficiencyRun{}
		a.runs[key] = r
	}
	if e.Type == "provider.call.usage" {
		a.observeUsage(e, r)
		return
	}
	if e.Type != "tool_started" && e.Type != "tool_finished" && e.Type != "tool_failed" {
		return
	}
	call := key + "/" + e.ToolCallID
	newCall := e.Type == "tool_started"
	if e.ToolCallID != "" {
		newCall = !a.started[call]
	}
	if newCall {
		a.started[call] = true
		if isLifecycleTool(e.Tool) {
			a.LifecycleCalls++
		}
		if e.Tool == "verify" {
			a.VerifyCalls++
		}
		if e.Tool == "finish_run" {
			a.FinishAttempts++
		}
	}
	if e.Tool == "finish_run" && (e.RunID == "" || e.ToolCallID == "") {
		a.FinishIdentityMissing = true
		a.ClosingIncomplete = true
	}
	if e.Tool != "finish_run" || e.RunID == "" || e.ToolCallID == "" {
		return
	}
	if !r.closing {
		r.closing = true
		r.firstFinish = call
		a.FinishRuns++
		if r.lastObserved {
			a.ClosingAvailable = true
			a.ClosingModelCalls++
			a.ClosingModelMS += r.lastMS
		} else {
			a.ClosingIncomplete = true
		}
	}
	if call == r.firstFinish && e.Type != "tool_started" && !r.firstResult {
		r.firstResult = true
		if e.Type == "tool_finished" && e.OK != nil && *e.OK {
			a.FinishFirstPassRuns++
		}
	}
}

func (a *efficiencyAccumulator) observeUsage(e JSONLEvent, r *efficiencyRun) {
	a.UsageAvailable = true
	if dispatched, ok := e.Metadata["provider_dispatched"].(bool); ok && !dispatched {
		return
	}
	role, _ := e.Metadata["role"].(string)
	delegated, _ := e.Metadata["delegated"].(bool)
	if role == "" {
		a.UnknownRoleCalls++
		return
	}
	if role != "coding_agent" || delegated {
		a.OtherRoleCalls++
		return
	}
	a.MainCalls++
	ms := metricInt(e.Metadata["duration_ms"])
	a.MainMS += ms
	a.MainOutputTokens += metricInt(e.Metadata["output_tokens"])
	a.ReasoningTokens += metricInt(e.Metadata["reasoning_output_tokens"])
	_, timed := e.Metadata["duration_ms"].(float64)
	if _, ok := e.Metadata["duration_ms"].(int); ok {
		timed = true
	}
	if _, ok := e.Metadata["duration_ms"].(int64); ok {
		timed = true
	}
	r.lastMS, r.lastObserved = ms, timed
	if r.closing && e.RunID != "" {
		if !timed {
			a.ClosingIncomplete = true
		}
		a.ClosingModelCalls++
		a.ClosingModelMS += ms
	}
}

func metricInt(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
