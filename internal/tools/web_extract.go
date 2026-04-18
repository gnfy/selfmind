package tools

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"time"
)

// =============================================================================
// Web Extract Tool
// =============================================================================

// WebExtractTool 提取网页内容
type WebExtractTool struct {
	BaseTool
}

func NewWebExtractTool() *WebExtractTool {
	return &WebExtractTool{
		BaseTool: BaseTool{
			name:        "web_extract",
			description: "提取网页的文本内容，支持 markdown 格式",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"url": {
						Type:        "string",
						Description: "要提取的网页 URL",
					},
					"query": {
						Type:        "string",
						Description: "Optional: specific information to extract",
					},
				},
				Required: []string{"url"},
			},
		},
	}
}

func (t *WebExtractTool) Execute(args map[string]interface{}) (string, error) {
	targetURL, _ := args["url"].(string)
	if targetURL == "" {
		return "", fmt.Errorf("url is required")
	}

	if err := checkSSRF(targetURL); err != nil {
		return "", fmt.Errorf("SSRF blocked: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("redirect to different host denied")
		}
		return nil
	}}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoBot/1.0; +https://example.com/bot)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "text/plain") && !strings.Contains(contentType, "application/xhtml") {
		return "", fmt.Errorf("only HTML content supported, got: %s", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024)) // 2MB limit
	if err != nil {
		return "", fmt.Errorf("read failed: %w", err)
	}

	html := string(body)
	text := extractTextFromHTML(html)

	if query, ok := args["query"].(string); ok && query != "" {
		return fmt.Sprintf("URL: %s\n\nExtracted content:\n%s", targetURL, text), nil
	}

	return fmt.Sprintf("URL: %s\n\nExtracted content:\n%s", targetURL, text), nil
}

func extractTextFromHTML(html string) string {
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")

	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	commentRe := regexp.MustCompile(`<!--.*?-->`)
	html = commentRe.ReplaceAllString(html, "")

	blockRe := regexp.MustCompile(`(?is)</(p|div|h[1-6]|li|tr|br|hr)>|<(p|div|h[1-6]|li|tr|br|hr)[^>]*>`)
	html = blockRe.ReplaceAllString(html, " ")

	tagRe := regexp.MustCompile(`<[^>]+>`)
	text := tagRe.ReplaceAllString(html, "")

	text = cleanHTML(text)

	spaceRe := regexp.MustCompile(`\s+`)
	text = spaceRe.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}

// checkSSRF redeclared, using existing tool internal implementation or helper.
// Removing local declaration to resolve conflict with vision.go.
