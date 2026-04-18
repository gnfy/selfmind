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

// WebSearchTool 搜索网页
type WebSearchTool struct {
	BaseTool
	provider string // "duckduckgo", "firecrawl", "tavily"
}

func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{
		BaseTool: BaseTool{
			name:        "web_search",
			description: "搜索网页并返回结果摘要",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "搜索查询词",
					},
					"num_results": {
						Type:        "integer",
						Description: "返回结果数量，默认 5",
						Default:     5,
					},
				},
				Required: []string{"query"},
			},
		},
		provider: getWebSearchProvider(),
	}
}

func getWebSearchProvider() string {
	if p := os.Getenv("WEB_SEARCH_PROVIDER"); p != "" {
		return p
	}
	return "duckduckgo"
}

func (t *WebSearchTool) Execute(args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	numResults := 5
	if n, ok := args["num_results"].(int); ok {
		numResults = n
	}

	switch t.provider {
	case "firecrawl":
		return t.searchFirecrawl(query, numResults)
	case "tavily":
		return t.searchTavily(query, numResults)
	default:
		return t.searchDuckDuckGo(query, numResults)
	}
}

func (t *WebSearchTool) searchDuckDuckGo(query string, numResults int) (string, error) {
	searchURL := fmt.Sprintf(
		"https://html.duckduckgo.com/html/?q=%s&kl=wt-wt",
		url.QueryEscape(query),
	)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoBot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	results := parseDuckDuckGoResults(string(body), numResults)
	if len(results) == 0 {
		return fmt.Sprintf("No results found for: %s", query), nil
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("Search results for: %s\n\n", query))
	for i, r := range results {
		out.WriteString(fmt.Sprintf("%d. %s\n   %s\n\n", i+1, r.Title, r.URL))
	}
	return out.String(), nil
}

type searchResult struct {
	Title string
	URL   string
	Desc  string
}

func parseDuckDuckGoResults(html string, limit int) []searchResult {
	var results []searchResult
	snippetRe := regexp.MustCompile(`<a class="result__a" href="([^"]+)">([^<]+)</a>`)
	matches := snippetRe.FindAllStringSubmatch(html, -1)

	urlRe := regexp.MustCompile(`<a class="result__url" href="([^"]+)">`)
	urlRe.FindAllStringSubmatch(html, -1)

	for i, m := range matches {
		if i >= limit {
			break
		}
		if len(m) > 2 {
			results = append(results, searchResult{
				Title: cleanHTML(m[2]),
				URL:   m[1],
			})
		}
	}
	return results
}

func cleanHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.TrimSpace(s)
}

func (t *WebSearchTool) searchFirecrawl(query string, numResults int) (string, error) {
	apiKey := os.Getenv("FIRECRAWL_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("FIRECRAWL_API_KEY not set")
	}

	endpoint := "https://api.firecrawl.dev/v0/search"
	payload := map[string]interface{}{
		"query":      query,
		"pageLimit":  numResults,
		"scrapeOptions": map[string]interface{}{
			"formats": []string{"markdown", "html"},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("firecrawl request failed: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

func (t *WebSearchTool) searchTavily(query string, numResults int) (string, error) {
	apiKey := os.Getenv("TAVILY_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("TAVILY_API_KEY not set")
	}

	endpoint := "https://api.tavily.com/search"
	payload := map[string]interface{}{
		"query":       query,
		"search_depth": "basic",
		"max_results": numResults,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tavily request failed: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}
