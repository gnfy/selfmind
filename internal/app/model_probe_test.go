package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"selfmind/internal/kernel/llm"
	"strings"
	"testing"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestProbeResolvedModelValidatesOpenAIToolSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []map[string]interface{} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		function := request.Tools[0]["function"].(map[string]interface{})
		parameters := function["parameters"].(map[string]interface{})
		if required, exists := parameters["required"]; exists {
			t.Fatalf("required must be omitted, got %#v", required)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "deepseek",
		Protocol: modelruntime.ProtocolOpenAICompatible,
		Model:    "deepseek-v4-flash",
		BaseURL:  server.URL,
		APIKey:   "test-key",
		Quirks: modelruntime.ProviderQuirks{
			SupportsTools: true,
		},
	})
	if probe.Err != nil {
		t.Fatal(probe.Err)
	}
	if !probe.NativeToolsTested {
		t.Fatal("native tool schema was not tested")
	}
}

func TestProbeResolvedModelReportsUnexpectedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{}"}}]}}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "onelinkai", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key",
		Quirks: modelruntime.ProviderQuirks{SupportsTools: true},
	})
	if probe.Err == nil || !strings.Contains(probe.Err.Error(), "unexpected tool call") {
		t.Fatalf("probe error = %v", probe.Err)
	}
}

func TestProbeResolvedModelValidatesMaintenanceContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxTokens int                      `json:"max_tokens"`
			Tools     []map[string]interface{} `json:"tools"`
			Thinking  map[string]interface{}   `json:"thinking"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.MaxTokens < postRunAnalyzerMaxTokens {
			t.Fatalf("maintenance probe max_tokens=%d", request.MaxTokens)
		}
		if len(request.Tools) != 0 {
			t.Fatalf("maintenance probe must not carry agent tools: %#v", request.Tools)
		}
		if request.Thinking["type"] != "disabled" {
			t.Fatalf("maintenance probe thinking=%#v, want disabled", request.Thinking)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"task_decision\":\"KEEP\",\"memory_decisions\":[]}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModelForRole(t.Context(), modelruntime.Runtime{
		Provider: "deepseek", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key", MaxTokens: 16000,
		ReasoningEffort: "high", Thinking: map[string]interface{}{"type": "enabled"},
		Quirks: modelruntime.ProviderQuirks{ThinkingMode: modelruntime.ThinkingModeDeepSeek, SupportsTools: true},
	}, "memory_extract")
	if probe.Err != nil || !probe.MaintenanceContractTested || !probe.MaintenanceContractPassed {
		t.Fatalf("probe=%+v", probe)
	}
	if probe.NativeToolsTested {
		t.Fatal("native tools were reported as tested even though the maintenance contract intentionally omitted them")
	}
}

func TestProbeResolvedModelValidatesDeepSeekThinkingToolLoop(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []struct {
				Role             string `json:"role"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		switch requests {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		case 2:
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"reasoning_content":"call the required check","tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_thinking_check","arguments":"{\"value\":\"ping\"}"}}]}}]}`)
		case 3:
			if len(request.Messages) < 2 || request.Messages[len(request.Messages)-2].ReasoningContent == "" {
				t.Fatal("second request did not replay reasoning_content")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "deepseek", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key",
		ReasoningEffort: "xhigh", Thinking: map[string]interface{}{"type": "enabled"},
		Quirks: modelruntime.ProviderQuirks{
			ThinkingMode: modelruntime.ThinkingModeDeepSeek, UserIdentityField: "user_id", SupportsTools: true,
		},
	})
	if probe.Err != nil || !probe.ThinkingToolLoopTested || !probe.ThinkingToolLoopPassed || requests != 3 {
		t.Fatalf("probe=%+v requests=%d", probe, requests)
	}
}

func TestResolveModelRuntimeUsesAuxiliaryAndRoleOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		"memory_extract": {Model: "memory-model"},
	}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Normalize()

	auxiliary, err := ResolveModelRuntime(context.Background(), cfg, "auxiliary")
	if err != nil {
		t.Fatal(err)
	}
	if auxiliary.Model != "aux-model" {
		t.Fatalf("auxiliary model = %q", auxiliary.Model)
	}
	memory, err := ResolveModelRuntime(context.Background(), cfg, "memory_extract")
	if err != nil {
		t.Fatal(err)
	}
	if memory.Provider != "openai" || memory.Model != "memory-model" {
		t.Fatalf("memory runtime = %+v", memory)
	}
}

func TestResolveModelRuntimeDoesNotApplyAuxiliaryToVision(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Normalize()

	if _, err := ResolveModelRuntime(context.Background(), cfg, "vision"); err == nil {
		t.Fatal("vision must require an explicit capability-specific role")
	}
}

// TestModelProbeBudgetFitsAReasoningModel pins the output cap on the probe that
// GATES MODEL CHANGES. Reasoning tokens are output tokens, so this cap is what
// the model must finish thinking inside before it can answer at all. Measured
// against DeepSeek V4 at reasoning_effort=xhigh, in this probe's exact shape,
// five attempts used 14, 64, 22, 23 and 38 completion tokens — one hit the old
// ceiling of 64 exactly and returned nothing. A false failure here rolls the
// person's model choice back and parks their queued work, which is why setting
// a model behaved like a coin flip.
func TestModelProbeBudgetFitsAReasoningModel(t *testing.T) {
	req := modelProbeRequest(modelruntime.Runtime{Model: "probe-model"}, false, false)
	if req.MaxTokens < 128 {
		t.Fatalf("plain probe budget = %d; a reasoning model can spend more than that before saying anything",
			req.MaxTokens)
	}
}

// TestModelProbeContentErrorNamesTruncation: an empty answer that was CUT OFF
// is a different diagnosis from a route that answered with nothing, and the
// finish reason is the only thing separating them.
func TestModelProbeContentErrorNamesTruncation(t *testing.T) {
	err := modelProbeContentError(&llm.ChatResponse{FinishReason: "length"})
	if err == nil {
		t.Fatal("a truncated empty answer must still fail the probe")
	}
	for _, want := range []string{"finish_reason=length", "budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("probe error must name %q so a log shows the cause: %q", want, err)
		}
	}

	if err := modelProbeContentError(&llm.ChatResponse{FinishReason: "content_filter"}); err == nil ||
		!strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("a non-truncation empty answer should name its finish reason: %v", err)
	}
	if err := modelProbeContentError(&llm.ChatResponse{}); err == nil ||
		!strings.Contains(err.Error(), "unset") {
		t.Fatalf("an absent finish reason should read as unset: %v", err)
	}
	if err := modelProbeContentError(&llm.ChatResponse{Content: "OK", FinishReason: "stop"}); err != nil {
		t.Fatalf("a normal answer must pass: %v", err)
	}
}
