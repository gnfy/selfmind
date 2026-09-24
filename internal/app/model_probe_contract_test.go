package app

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
)

// The maintenance contract demands a JSON object, and the probe mirrors the
// analyzer's request field for field. Both must ask for JSON mode: relying on
// the system prompt let deepseek-v4-flash answer with a YAML-shaped echo of
// the request on 10 of 10 attempts, so only the largest model in the family
// could be the background route. The plain OK probe must stay unconstrained.
func TestMaintenanceProbeAsksForJSONModeAndPlainProbeDoesNot(t *testing.T) {
	rt := modelruntime.Runtime{Model: "any"}
	maintenance := modelProbeRequest(rt, false, modelProbeContractMaintenance)
	format, _ := maintenance.Options["response_format"].(map[string]interface{})
	if format["type"] != maintenanceResponseFormat || maintenanceResponseFormat != "json_object" {
		t.Fatalf("maintenance probe response_format = %#v", maintenance.Options["response_format"])
	}
	if _, ok := maintenance.Options["temperature"]; ok {
		t.Fatalf("maintenance probe must leave provider temperature policy intact: %#v", maintenance.Options)
	}
	approval := modelProbeRequest(modelruntime.Runtime{Model: "any", ReasoningLevels: []string{"low", "medium", "high"}}, false, modelProbeContractApproval)
	if _, ok := approval.Options["temperature"]; ok {
		t.Fatalf("approval probe must leave provider temperature policy intact: %#v", approval.Options)
	}
	plain := modelProbeRequest(rt, true, modelProbeContractPlain)
	if _, ok := plain.Options["response_format"]; ok {
		t.Fatalf("the plain probe must not constrain output: %#v", plain.Options)
	}
}

// scriptedProvider answers with a fixed sequence of responses.
type scriptedProvider struct {
	responses []*llm.ChatResponse
	requests  []llm.ChatRequest
}

func (p *scriptedProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return &llm.ChatResponse{}, nil
	}
	next := p.responses[0]
	p.responses = p.responses[1:]
	return next, nil
}

func (p *scriptedProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "", nil
}

func (p *scriptedProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return nil, nil
}

// reasoning_content is a vendor extension the model MAY attach to a tool call;
// a trivial request often gets none, and the same model returned it on one run
// and not the next. The probe used to fail on the empty field alone, so a
// healthy route validated one day and not the next. The interoperability
// question is whether the follow-up turn is accepted, and that is the test.
func TestNativeToolLoopAcceptsAToolCallWithoutReasoningContent(t *testing.T) {
	provider := &scriptedProvider{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Function: "selfmind_model_check", Args: `{"value":"ping"}`}}},
		{Content: "called it, ok"},
	}}
	rt := modelruntime.Runtime{Model: "deepseek-v4-flash-vision-exp"}
	if err := probeNativeToolLoop(context.Background(), provider, rt); err != nil {
		t.Fatalf("a tool call without reasoning_content must not fail the probe: %v", err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("the replay turn was not attempted: %d requests", len(provider.requests))
	}
	// Whatever came back is what gets replayed — here, nothing.
	replayed := provider.requests[1].Messages[1]
	if replayed.Role != "assistant" || replayed.ReasoningContent != "" || len(replayed.ToolCalls) != 1 {
		t.Fatalf("replay turn = %+v", replayed)
	}
}

// A model that never makes the call is still a broken route: the relaxation is
// about an optional field, not about the tool call itself.
func TestNativeToolLoopStillRequiresTheToolCall(t *testing.T) {
	provider := &scriptedProvider{responses: []*llm.ChatResponse{{Content: "I will not call tools."}}}
	if err := probeNativeToolLoop(context.Background(), provider, modelruntime.Runtime{Model: "m"}); err == nil {
		t.Fatal("a response with no tool call passed the tool-loop probe")
	}
}
