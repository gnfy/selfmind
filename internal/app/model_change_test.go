package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestValidateModelChangeProbesIndependentRoutesConcurrently(t *testing.T) {
	var active atomic.Int32
	var overlapped atomic.Bool
	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if active.Add(1) >= 2 {
				overlapped.Store(true)
			}
			defer active.Add(-1)
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		}))
	}
	first, second := newServer(), newServer()
	defer first.Close()
	defer second.Close()

	cfg := &config.Config{
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"probe-one": {BaseURL: first.URL, Protocol: modelruntime.ProtocolOpenAICompatible, APIKey: "test-key"},
			"probe-two": {BaseURL: second.URL, Protocol: modelruntime.ProtocolOpenAICompatible, APIKey: "test-key"},
		},
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			string(modelchange.RouteBackgroundReview): {Provider: "probe-one", Model: "model-one"},
			string(modelchange.RouteSkillCurator):     {Provider: "probe-two", Model: "model-two"},
		}},
	}
	cfg.Normalize()
	results := ValidateModelChange(t.Context(), cfg, []modelchange.Route{
		modelchange.RouteBackgroundReview, modelchange.RouteSkillCurator,
	})
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("probe results = %+v", results)
	}
	if !overlapped.Load() {
		t.Fatal("independent read-only model probes ran serially")
	}
}

// Model configuration is where a person should learn how their approval model
// turns reasoning off, not from later triage timeouts. The validation the
// Model Manager runs must carry the finding for the approval route only; other
// roles run at their configured reasoning and are not second-guessed.
func TestValidateModelChangeReportsHowTheApprovalRouteTurnsReasoningOff(t *testing.T) {
	const verdict = `{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"Bounded read-only check.\"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string                 `json:"reasoning_effort"`
			ResponseFormat  map[string]interface{} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		reasoning, content := 300, "OK"
		if body.ReasoningEffort == "none" {
			reasoning = 0
		}
		if body.ResponseFormat != nil {
			content = verdict
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"%s"},"finish_reason":"stop"}],"usage":{"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
			content, reasoning+10, reasoning)
	}))
	defer server.Close()

	cfg := &config.Config{
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"lab": {BaseURL: server.URL, Protocol: modelruntime.ProtocolOpenAICompatible, APIKey: "test-key"},
		},
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			string(modelchange.RouteFastClassifier): {Provider: "lab", Model: "lab-model"},
			string(modelchange.RouteSemanticRecall): {Provider: "lab", Model: "lab-model"},
		}},
	}
	cfg.Normalize()
	results := ValidateModelChange(t.Context(), cfg, []modelchange.Route{
		modelchange.RouteFastClassifier, modelchange.RouteSemanticRecall,
	})
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("probe results = %+v", results)
	}
	approval, recall := results[0], results[1]
	if approval.ThinkingMode != modelruntime.ThinkingModeEffortNone || !strings.Contains(approval.Notice, "turned it off") {
		t.Fatalf("approval route result = %+v, want the proven effort_none encoding", approval)
	}
	if recall.ThinkingMode != "" || recall.Notice != "" {
		t.Fatalf("semantic_recall runs at its configured reasoning and must carry no finding: %+v", recall)
	}
}

// Choosing a model already proved it, so applying the same draft must not
// make the person wait for the same probes again. Evidence is keyed by the
// exact request and credential, never outlives its short window, and a
// failure is never remembered.
func TestModelChangeValidatorReusesPassingEvidence(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("content-type", "application/json")
		if body.Model == "broken-model" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	draft := func(model, key string) *config.Config {
		cfg := &config.Config{
			ProviderProfiles: map[string]config.ProviderEndpoint{
				"lab": {BaseURL: server.URL, Protocol: modelruntime.ProtocolOpenAICompatible, APIKey: key},
			},
			Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
				string(modelchange.RouteSemanticRecall): {Provider: "lab", Model: model},
			}},
		}
		cfg.Normalize()
		return cfg
	}
	validator := NewModelChangeValidator()
	clock := time.Now()
	validator.now = func() time.Time { return clock }
	validate := func(cfg *config.Config, wantRequests int32) modelchange.ProbeResult {
		t.Helper()
		results := validator.Validate(t.Context(), cfg, []modelchange.Route{modelchange.RouteSemanticRecall})
		if len(results) != 1 {
			t.Fatalf("results = %+v", results)
		}
		if got := requests.Load(); got != wantRequests {
			t.Fatalf("provider requests = %d, want %d (result %+v)", got, wantRequests, results[0])
		}
		return results[0]
	}

	if first := validate(draft("lab-model", "key-1"), 1); !first.OK || first.Reused {
		t.Fatalf("first validation = %+v", first)
	}
	if again := validate(draft("lab-model", "key-1"), 1); !again.OK || !again.Reused {
		t.Fatalf("the identical draft must reuse the passing probe: %+v", again)
	}
	validate(draft("other-model", "key-1"), 2)
	validate(draft("lab-model", "key-2"), 3)
	clock = clock.Add(modelValidationEvidenceTTL + time.Second)
	if stale := validate(draft("lab-model", "key-1"), 4); stale.Reused {
		t.Fatalf("expired evidence was reused: %+v", stale)
	}
	if failed := validate(draft("broken-model", "key-1"), 5); failed.OK {
		t.Fatalf("broken model passed: %+v", failed)
	}
	validate(draft("broken-model", "key-1"), 6)
}

func TestValidateModelChangeSerializesAndDeduplicatesOnePhysicalEndpoint(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			prior := maxActive.Load()
			if current <= prior || maxActive.CompareAndSwap(prior, current) {
				break
			}
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var system string
		for _, message := range request.Messages {
			if message.Role == "system" {
				system = message.Content
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("content-type", "application/json")
		switch {
		case strings.Contains(system, "additional human confirmation"):
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"The bounded status check is authorized.\"}"},"finish_reason":"stop"}]}`)
		case strings.Contains(system, "memory_decisions"):
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"task_decision\":\"KEEP\",\"memory_decisions\":[]}"},"finish_reason":"stop"}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()

	cfg := &config.Config{
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"shared": {BaseURL: server.URL, Protocol: modelruntime.ProtocolOpenAICompatible, APIKey: "test-key"},
		},
		Models: config.ModelsConfig{Auxiliary: config.ModelSelectionConfig{Provider: "shared", Model: "one-model", Reasoning: "high"}},
	}
	cfg.Normalize()
	results := ValidateModelChange(t.Context(), cfg, []modelchange.Route{modelchange.RouteAuxiliary})
	if len(results) != 7 {
		t.Fatalf("probe results = %+v", results)
	}
	for _, result := range results {
		if !result.OK {
			t.Fatalf("probe failed: %+v", results)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("physical requests = %d, want maintenance, approval, and background-text once each", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("same endpoint had %d concurrent probes", got)
	}
}

func TestClassifyModelProbeFailureOnlyBlamesDeterministicRouteFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want modelchange.FailureClass
	}{
		{name: "semantic contract", err: errors.New("maintenance contract failed"), want: modelchange.FailureModel},
		{name: "invalid request", err: &llm.ProviderError{Class: llm.ProviderErrorInvalidRequest}, want: modelchange.FailureModel},
		{name: "auth", err: &llm.ProviderError{Class: llm.ProviderErrorAuth}, want: modelchange.FailureModel},
		{name: "rate limit", err: &llm.ProviderError{Class: llm.ProviderErrorRateLimit}, want: modelchange.FailureInfrastructure},
		{name: "transient", err: &llm.ProviderError{Class: llm.ProviderErrorTransient}, want: modelchange.FailureInfrastructure},
		{name: "deadline", err: context.DeadlineExceeded, want: modelchange.FailureInfrastructure},
		{name: "unknown provider error", err: &llm.ProviderError{Class: llm.ProviderErrorUnknown}, want: modelchange.FailureInfrastructure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyModelProbeFailure(test.err); got != test.want {
				t.Fatalf("class = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExpandedModelValidationRoutesCoversOnlyBackgroundRolesThatInherit(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
		string(modelchange.RouteMemoryExtract): {Provider: "anthropic", Model: "claude-role"},
	}}}
	got := expandedModelValidationRoutes(cfg, []modelchange.Route{modelchange.RouteAuxiliary})
	want := []modelchange.Route{
		modelchange.RouteAuxiliary,
		modelchange.RouteFastClassifier,
		modelchange.RouteBackgroundReview,
		modelchange.RouteSkillCurator,
		modelchange.RouteSemanticRecall,
		modelchange.RouteSummarizer,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}
