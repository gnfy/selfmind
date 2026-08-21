package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// =============================================================================
// Web Search Tool
// =============================================================================
//
// Backend model (2026-07-12 rebuild): local HTML scraping (DuckDuckGo) is
// unreliable on many egresses — anti-bot 202 challenges, GFW, rate limits — so
// a hosted search API is the quality path, mirroring codex (server-side
// search) and hermes (managed Firecrawl / provider registry). Backends are
// selected by an explicit config choice, else the first one with a credential,
// else DuckDuckGo scraping as a best-effort fallback.
//
// Honesty invariant: a backend failure (non-200, anti-bot page, missing
// credential) returns an ERROR, never "No results found". Reporting a backend
// outage as an empty result made the model burn calls rephrasing and invent
// negative conclusions ("this car does not exist") — the exact failure this
// rebuild fixes. "No results found" is reserved for a backend that genuinely
// ran and returned zero hits.

// WebSearchOptions carries the resolved web-search backend + its credential
// into the tool without the tools package importing the config package. One
// backend, one key — no fallback chain (a configured backend that fails
// returns an error, it does not silently switch engines).
type WebSearchOptions struct {
	Backend string // tavily|brave|serper|firecrawl|searxng|duckduckgo; "" = duckduckgo
	APIKey  string // credential for Backend (searxng: instance URL; duckduckgo: unused)
}

// WebSearchTool searches the web through a configured or auto-selected backend.
type WebSearchTool struct {
	BaseTool
	opts WebSearchOptions
}

func NewWebSearchTool() *WebSearchTool {
	// Env-only construction path (backward compatible). The daemon uses
	// NewWebSearchToolWithOptions with resolved config.
	return NewWebSearchToolWithOptions(WebSearchOptions{
		Backend: strings.ToLower(strings.TrimSpace(os.Getenv("WEB_SEARCH_PROVIDER"))),
		APIKey:  os.Getenv("SELFMIND_WEB_SEARCH_API_KEY"),
	})
}

func NewWebSearchToolWithOptions(opts WebSearchOptions) *WebSearchTool {
	return &WebSearchTool{
		BaseTool: BaseTool{
			name:        "web_search",
			description: "Search the web and return titled results with URLs and snippets. Use for current events, product/spec lookups, and any fact that may have changed since training.",
			metadata:    ToolMetadata{ReadOnly: true, RiskLevel: ToolRiskLow, Category: "network", OperationClasses: []OperationClass{OpClassNetwork}},
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "The search query.",
					},
					"num_results": {
						Type:        "integer",
						Description: "Number of results to return (default 5, max 10).",
						Default:     5,
					},
				},
				Required: []string{"query"},
			},
		},
		opts: opts,
	}
}

type searchResult struct {
	Title string
	URL   string
	Desc  string
}

// resolveBackend returns the chosen backend, defaulting to duckduckgo when
// none is configured (best-effort, no key).
func (o WebSearchOptions) resolveBackend() string {
	if b := strings.ToLower(strings.TrimSpace(o.Backend)); b != "" {
		return b
	}
	return "duckduckgo"
}

func (t *WebSearchTool) Execute(args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	numResults := 5
	switch n := args["num_results"].(type) {
	case int:
		if n > 0 {
			numResults = n
		}
	case float64:
		if n > 0 {
			numResults = int(n)
		}
	}
	if numResults > 10 {
		numResults = 10
	}

	backend := t.opts.resolveBackend()
	var (
		results []searchResult
		err     error
	)
	switch backend {
	case "tavily":
		results, err = t.searchTavily(query, numResults)
	case "brave":
		results, err = t.searchBrave(query, numResults)
	case "serper":
		results, err = t.searchSerper(query, numResults)
	case "firecrawl":
		results, err = t.searchFirecrawl(query, numResults)
	case "searxng":
		results, err = t.searchSearxng(query, numResults)
	case "duckduckgo", "ddg":
		results, err = t.searchDuckDuckGo(query, numResults)
	default:
		return "", fmt.Errorf("unknown web_search backend %q (configure web.search_backend: tavily|brave|serper|firecrawl|searxng|duckduckgo)", backend)
	}
	if err != nil {
		// Backend failure — surface it as an error so the model treats it as
		// diagnostic evidence, not as "this information does not exist".
		return "", fmt.Errorf("web_search backend %q failed: %w", backend, err)
	}
	if len(results) == 0 {
		return fmt.Sprintf("No results found for: %s (backend %q ran successfully but returned zero hits — try different terms)", query, backend), nil
	}
	return formatSearchResults(query, backend, results), nil
}

func formatSearchResults(query, backend string, results []searchResult) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Search results for %q (via %s):\n\n", query, backend)
	for i, r := range results {
		fmt.Fprintf(&out, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if d := strings.TrimSpace(r.Desc); d != "" {
			fmt.Fprintf(&out, "   %s\n", truncateSnippet(d, 300))
		}
		out.WriteString("\n")
	}
	return out.String()
}

func truncateSnippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// missingKeyErr is the actionable error for an unconfigured backend.
func missingKeyErr(backend, signupURL string) error {
	return fmt.Errorf("%s backend selected but no credential configured; set web.api_key in config.yaml (get a free key at %s)", backend, signupURL)
}

// -----------------------------------------------------------------------------
// Tavily — AI-native search API, free tier, snippet-rich results.
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchTavily(query string, numResults int) ([]searchResult, error) {
	return t.searchTavilyAt("https://api.tavily.com/search", query, numResults)
}

func (t *WebSearchTool) searchTavilyAt(endpoint, query string, numResults int) ([]searchResult, error) {
	key := strings.TrimSpace(t.opts.APIKey)
	if key == "" {
		return nil, missingKeyErr("tavily", "https://tavily.com")
	}
	payload := map[string]interface{}{
		"api_key":      key,
		"query":        query,
		"search_depth": "basic",
		"max_results":  numResults,
	}
	body, status, err := postJSON(endpoint, "", payload)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, httpStatusErr(status, body)
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode tavily response: %w", err)
	}
	var out []searchResult
	for _, r := range parsed.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Desc: r.Content})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Brave Search API — independent index, free tier.
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchBrave(query string, numResults int) ([]searchResult, error) {
	key := strings.TrimSpace(t.opts.APIKey)
	if key == "" {
		return nil, missingKeyErr("brave", "https://brave.com/search/api/")
	}
	endpoint := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(query), numResults)
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	body, status, err := doRequest(req)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, httpStatusErr(status, body)
	}
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode brave response: %w", err)
	}
	var out []searchResult
	for _, r := range parsed.Web.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Desc: cleanHTML(r.Description)})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Serper — Google results via API, generous one-time free credits.
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchSerper(query string, numResults int) ([]searchResult, error) {
	key := strings.TrimSpace(t.opts.APIKey)
	if key == "" {
		return nil, missingKeyErr("serper", "https://serper.dev")
	}
	payload := map[string]interface{}{"q": query, "num": numResults}
	body, status, err := postJSON("https://google.serper.dev/search", key, payload)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, httpStatusErr(status, body)
	}
	var parsed struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode serper response: %w", err)
	}
	var out []searchResult
	for _, r := range parsed.Organic {
		out = append(out, searchResult{Title: r.Title, URL: r.Link, Desc: r.Snippet})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Firecrawl — search endpoint (self-hosted or cloud).
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchFirecrawl(query string, numResults int) ([]searchResult, error) {
	key := strings.TrimSpace(t.opts.APIKey)
	if key == "" {
		return nil, missingKeyErr("firecrawl", "https://firecrawl.dev")
	}
	payload := map[string]interface{}{"query": query, "limit": numResults}
	body, status, err := postJSON("https://api.firecrawl.dev/v1/search", key, payload)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, httpStatusErr(status, body)
	}
	var parsed struct {
		Data []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode firecrawl response: %w", err)
	}
	var out []searchResult
	for _, r := range parsed.Data {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Desc: r.Description})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// SearXNG — self-hosted metasearch (JSON API). Not a default; for users who
// run their own instance.
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchSearxng(query string, numResults int) ([]searchResult, error) {
	base := strings.TrimRight(strings.TrimSpace(t.opts.APIKey), "/")
	if base == "" {
		return nil, fmt.Errorf("searxng backend selected but web.api_key (instance URL) is not set")
	}
	endpoint := fmt.Sprintf("%s/search?q=%s&format=json", base, url.QueryEscape(query))
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Accept", "application/json")
	body, status, err := doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach SearXNG at %s: %w", base, err)
	}
	if status != 200 {
		return nil, httpStatusErr(status, body)
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode searxng response: %w", err)
	}
	var out []searchResult
	for i, r := range parsed.Results {
		if i >= numResults {
			break
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Desc: r.Content})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// DuckDuckGo HTML scraping — best-effort fallback only. Detects anti-bot
// challenge pages and reports them as failures instead of empty results.
// -----------------------------------------------------------------------------
func (t *WebSearchTool) searchDuckDuckGo(query string, numResults int) ([]searchResult, error) {
	return t.searchDuckDuckGoAt("https://html.duckduckgo.com/html/", query, numResults)
}

func (t *WebSearchTool) searchDuckDuckGoAt(base, query string, numResults int) ([]searchResult, error) {
	searchURL := fmt.Sprintf("%s?q=%s&kl=wt-wt", base, url.QueryEscape(query))
	req, _ := http.NewRequest("GET", searchURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	body, status, err := doRequest(req)
	if err != nil {
		return nil, err
	}
	// DDG returns 200 for a real SERP; 202 (and sometimes 200 with no result
	// anchors) is an anti-bot challenge. Treat "reachable but no parseable
	// results AND no SERP markers" as a backend failure, never empty.
	if status != 200 {
		return nil, fmt.Errorf("anti-bot challenge (HTTP %d) — DuckDuckGo scraping is blocked on this network; configure a hosted backend (web.search_backend: tavily|brave|serper) for reliable search", status)
	}
	results := parseDuckDuckGoResults(string(body), numResults)
	if len(results) == 0 && !strings.Contains(string(body), "result__a") {
		return nil, fmt.Errorf("no SERP markers in DuckDuckGo response — likely an anti-bot challenge; configure a hosted backend (web.search_backend: tavily|brave|serper)")
	}
	return results, nil
}

func parseDuckDuckGoResults(html string, limit int) []searchResult {
	var results []searchResult
	// Title may contain nested <b> highlight tags, so capture everything up to
	// the closing </a> and strip tags afterwards (the old [^<]+ dropped every
	// highlighted result).
	linkRe := regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	snippetRe := regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	links := linkRe.FindAllStringSubmatch(html, -1)
	snippets := snippetRe.FindAllStringSubmatch(html, -1)
	for i, m := range links {
		if i >= limit {
			break
		}
		if len(m) < 3 {
			continue
		}
		r := searchResult{URL: decodeDDGURL(m[1]), Title: cleanHTML(m[2])}
		if i < len(snippets) && len(snippets[i]) > 1 {
			r.Desc = cleanHTML(snippets[i][1])
		}
		results = append(results, r)
	}
	return results
}

// decodeDDGURL unwraps DuckDuckGo's redirect wrapper (//duckduckgo.com/l/?uddg=…).
func decodeDDGURL(raw string) string {
	if idx := strings.Index(raw, "uddg="); idx >= 0 {
		enc := raw[idx+len("uddg="):]
		if amp := strings.IndexByte(enc, '&'); amp >= 0 {
			enc = enc[:amp]
		}
		if dec, err := url.QueryUnescape(enc); err == nil {
			return dec
		}
	}
	return raw
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func cleanHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return strings.TrimSpace(s)
}

// -----------------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------------

func postJSON(endpoint, bearer string, payload map[string]interface{}) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		// Serper uses X-API-KEY; Tavily/Firecrawl take the key in the body or
		// a bearer. Callers that need a bearer pass it here; Serper's header is
		// set by its own path. Default to X-API-KEY which Serper needs and the
		// others ignore.
		req.Header.Set("X-API-KEY", bearer)
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doRequest(req)
}

func doRequest(req *http.Request) ([]byte, int, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func httpStatusErr(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	switch status {
	case 401, 403:
		return fmt.Errorf("authentication failed (HTTP %d) — check the API key; response: %s", status, msg)
	case 429:
		return fmt.Errorf("rate limited or quota exhausted (HTTP %d); response: %s", status, msg)
	default:
		return fmt.Errorf("HTTP %d: %s", status, msg)
	}
}
