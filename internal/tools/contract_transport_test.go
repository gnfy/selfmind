package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// Test by protocol rather than model name: Qwen, DeepSeek and Gemini's
// compatible endpoint all share the OpenAI Chat contract. The same registered
// builtin must survive every native adapter without acquiring new authority.
func TestRegisteredFinishContractAcrossNativeTransports(t *testing.T) {
	type chat func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error)
	protocols := []struct {
		name, schemaKey string
		newChat         func(string) chat
		response        func(string) string
	}{
		{"openai-chat", "parameters", func(url string) chat { a := llm.NewOpenAIAdapter("test"); a.BaseURL = url; return a.Chat },
			func(args string) string {
				encoded, _ := json.Marshal(args)
				return fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"id":"call-contract","type":"function","function":{"name":"finish_run","arguments":%s}}]}}]}`, encoded)
			}},
		{"anthropic", "input_schema", func(url string) chat { a := llm.NewAnthropicAdapter("test"); a.BaseURL = url; return a.Chat },
			func(args string) string {
				return fmt.Sprintf(`{"content":[{"type":"tool_use","id":"call-contract","name":"finish_run","input":%s}],"stop_reason":"tool_use"}`, args)
			}},
		{"responses", "parameters", func(url string) chat { return llm.NewResponsesAdapter("test", url, "test").Chat },
			func(args string) string {
				encoded, _ := json.Marshal(args)
				return fmt.Sprintf(`{"output":[{"type":"function_call","call_id":"call-contract","name":"finish_run","arguments":%s}]}`, encoded)
			}},
	}
	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			for _, valid := range []bool{true, false} {
				t.Run(fmt.Sprint(valid), func(t *testing.T) {
					registry := NewRegistry()
					registry.Register(NewFinishRunTool())
					defs := registry.ToolDefinitions()
					encoded, _ := json.Marshal(defs)
					// Registry definitions are the canonical source; adapters never get
					// an independently maintained copy of this tool's schema.
					function := defs[0]["function"].(map[string]interface{})
					definition := llm.ToolDefinition{Name: function["name"].(string), Description: function["description"].(string), Parameters: function["parameters"].(map[string]interface{})}
					args := `{"status":"waiting_user","final_response":"First paragraph.\n\nSecond paragraph."}`
					if !valid {
						args = `{"run_id":"invented","final_response":"ready"}`
					}
					var wire map[string]interface{}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
							t.Error(err)
						}
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, protocol.response(args))
					}))
					defer server.Close()
					response, err := protocol.newChat(server.URL)(context.Background(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "finish"}}, Tools: []llm.ToolDefinition{definition}})
					if err != nil {
						t.Fatal(err)
					}
					tool := wire["tools"].([]interface{})[0].(map[string]interface{})
					if protocol.name == "openai-chat" {
						tool = tool["function"].(map[string]interface{})
					}
					schema := tool[protocol.schemaKey].(map[string]interface{})
					if !reflect.DeepEqual(schema["required"], []interface{}{"status", "final_response"}) || schema["additionalProperties"] != false {
						t.Fatalf("wire weakened required/closed contract: %#v", schema)
					}
					if !reflect.DeepEqual(schema, definition.Parameters) {
						t.Fatalf("wire changed the registered contract: got %#v; want %#v", schema, definition.Parameters)
					}
					if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "call-contract" {
						t.Fatalf("call identity lost: %+v", response)
					}
					var actual, expected map[string]interface{}
					if err := json.Unmarshal([]byte(response.ToolCalls[0].Args), &actual); err != nil {
						t.Fatal(err)
					}
					_ = json.Unmarshal([]byte(args), &expected)
					if !reflect.DeepEqual(actual, expected) {
						t.Fatal("adapter repaired or dropped arguments")
					}
					_, err = registry.DispatchResult(response.ToolCalls[0].Function, actual)
					if valid && err != nil {
						t.Fatal(err)
					}
					if !valid && (err == nil || !strings.Contains(err.Error(), "missing required parameter: status") || !strings.Contains(err.Error(), "unknown parameter: run_id")) {
						t.Fatalf("malformed call acquired authority: %v", err)
					}
					after, _ := json.Marshal(defs)
					if string(after) != string(encoded) {
						t.Fatal("adapter mutated registered schema")
					}
				})
			}
		})
	}
}
