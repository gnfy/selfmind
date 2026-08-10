package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"selfmind/internal/modelruntime"
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
