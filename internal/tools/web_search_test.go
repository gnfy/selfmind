package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveBackend: explicit config wins; else first credentialed backend;
// else duckduckgo.
func TestResolveBackend(t *testing.T) {
	cases := []struct {
		name string
		opts WebSearchOptions
		want string
	}{
		{"explicit-tavily", WebSearchOptions{Backend: "tavily", APIKey: "x"}, "tavily"},
		{"explicit-brave", WebSearchOptions{Backend: "brave", APIKey: "x"}, "brave"},
		{"explicit-searxng", WebSearchOptions{Backend: "searxng", APIKey: "http://localhost:8080"}, "searxng"},
		{"none", WebSearchOptions{}, "duckduckgo"},
		{"key-without-backend-defaults-ddg", WebSearchOptions{APIKey: "x"}, "duckduckgo"},
	}
	for _, tc := range cases {
		if got := tc.opts.resolveBackend(); got != tc.want {
			t.Errorf("%s: resolveBackend()=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestMissingCredentialIsError: a selected hosted backend with no key returns
// an actionable ERROR (never "No results found").
func TestMissingCredentialIsError(t *testing.T) {
	for _, backend := range []string{"tavily", "brave", "serper", "firecrawl"} {
		tool := NewWebSearchToolWithOptions(WebSearchOptions{Backend: backend})
		out, err := tool.Execute(map[string]interface{}{"query": "anything"})
		if err == nil {
			t.Fatalf("%s: missing key must be an error, got output %q", backend, out)
		}
		if !strings.Contains(err.Error(), "config.yaml") {
			t.Errorf("%s: error must point at config: %v", backend, err)
		}
		if strings.Contains(err.Error(), "No results found") {
			t.Errorf("%s: backend failure must not read as empty results", backend)
		}
	}
}

// TestDuckDuckGoAntiBotIsError: a 202 challenge (or a 200 with no SERP markers)
// is reported as a backend failure, not an empty result — the core fix.
func TestDuckDuckGoAntiBotIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // 202 challenge
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head></head><body>challenge</body></html>"))
	}))
	defer srv.Close()

	tool := NewWebSearchToolWithOptions(WebSearchOptions{Backend: "duckduckgo"})
	// Point the DDG fetch at the stub by exercising the parser+status logic
	// directly (Execute hits the real host); assert the status branch.
	_, err := tool.searchDuckDuckGoAt(srv.URL, "aion n60", 5)
	if err == nil || !strings.Contains(err.Error(), "anti-bot") {
		t.Fatalf("202 challenge must be an anti-bot error, got: %v", err)
	}
}

// TestDuckDuckGoParsesHighlightedTitles: the parser tolerates nested <b>
// highlight tags (the old [^<]+ regex dropped every highlighted result) and
// unwraps the redirect URL.
func TestDuckDuckGoParsesHighlightedTitles(t *testing.T) {
	html := `<div class="result">
	<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Faion&amp;rut=x">AION <b>N60</b> 官方</a>
	<a class="result__snippet" href="#">广汽埃安 <b>N60</b> 参数与价格</a>
	</div>`
	results := parseDuckDuckGoResults(html, 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Title != "AION N60 官方" {
		t.Errorf("title with <b> highlight not recovered: %q", results[0].Title)
	}
	if results[0].URL != "https://example.com/aion" {
		t.Errorf("redirect URL not unwrapped: %q", results[0].URL)
	}
	if !strings.Contains(results[0].Desc, "参数与价格") {
		t.Errorf("snippet not captured: %q", results[0].Desc)
	}
}

// TestTavilyBackendParsesResults: a healthy hosted-backend response yields
// snippet-bearing results (checks the parse + format path end to end).
func TestTavilyBackendParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"AION N60","url":"https://aion.com.cn/n60","content":"广汽埃安 N60 纯电 SUV"}]}`))
	}))
	defer srv.Close()

	tool := NewWebSearchToolWithOptions(WebSearchOptions{Backend: "tavily", APIKey: "k"})
	results, err := tool.searchTavilyAt(srv.URL, "aion n60", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Desc == "" {
		t.Fatalf("expected one snippet-bearing result: %+v", results)
	}
}
