package tools

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func checkSSRF(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid scheme: %s", u.Scheme)
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("private/local IP blocked: %s", ip)
		}
	}
	return nil
}

// =============================================================================
// Vision Tool
// =============================================================================

// VisionTool 图片分析工具
type VisionTool struct {
	BaseTool
	// llmProvider is set via RegisterVisionTool
	llmProvider VisionLLM
}

// VisionLLM 定义视觉分析所需的 LLM 接口，避免循环依赖
type VisionLLM interface {
	Analyze(imageBase64, mimeType, question string) (string, error)
}

// RegisterVisionTool sets the LLM provider for vision analysis.
func RegisterVisionTool(t *VisionTool, provider VisionLLM) {
	t.llmProvider = provider
}

func NewVisionTool() *VisionTool {
	return &VisionTool{
		BaseTool: BaseTool{
			name:        "vision_analyze",
			description: "分析图片内容并回答问题",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"image_url": {
						Type:        "string",
						Description: "图片 URL 或本地路径",
					},
					"question": {
						Type:        "string",
						Description: "要询问的问题",
					},
				},
				Required: []string{"image_url", "question"},
			},
		},
	}
}

func (t *VisionTool) Execute(args map[string]interface{}) (string, error) {
	imageURL, _ := args["image_url"].(string)
	question, _ := args["question"].(string)
	if imageURL == "" || question == "" {
		return "", fmt.Errorf("image_url and question are required")
	}

	var rawBase64 string
	var mimeType string

	if strings.HasPrefix(imageURL, "file://") || !strings.Contains(imageURL, "://") {
		path := strings.TrimPrefix(imageURL, "file://")
		if !strings.Contains(imageURL, "://") {
			path = imageURL
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read image file: %w", err)
		}
		ext := strings.ToLower(filepath.Ext(path))
		mimeType = "image/jpeg"
		switch ext {
		case ".png":
			mimeType = "image/png"
		case ".gif":
			mimeType = "image/gif"
		case ".webp":
			mimeType = "image/webp"
		case ".bmp":
			mimeType = "image/bmp"
		}
		rawBase64 = base64.StdEncoding.EncodeToString(data)
	} else {
		if err := checkSSRF(imageURL); err != nil {
			return "", fmt.Errorf("SSRF blocked: %w", err)
		}

		data, contentType, err := fetchURL(imageURL, 50*1024*1024) // 50MB limit
		if err != nil {
			return "", fmt.Errorf("fetch image: %w", err)
		}
		mimeType = contentType
		rawBase64 = base64.StdEncoding.EncodeToString(data)
	}

	if t.llmProvider == nil {
		return "", fmt.Errorf("vision LLM provider not registered")
	}

	analysis, err := t.llmProvider.Analyze(rawBase64, mimeType, question)
	if err != nil {
		return "", fmt.Errorf("vision analysis failed: %w", err)
	}

	return analysis, nil
}

func fetchURL(rawURL string, maxBytes int) ([]byte, string, error) {
	if err := checkSSRF(rawURL); err != nil {
		return nil, "", err
	}

	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if len(via) > 0 {
			if err := checkSSRF(req.URL.String()); err != nil {
				return err
			}
		}
		return nil
	}}

	if err := checkSSRF(rawURL); err != nil {
		return nil, "", err
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoBot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return nil, "", err
	}

	return body, contentType, nil
}
