package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// =============================================================================
// TTS Tool
// =============================================================================

// TTSTool 文本转语音
type TTSTool struct {
	BaseTool
}

func NewTTSTool() *TTSTool {
	return &TTSTool{
		BaseTool: BaseTool{
			name:        "text_to_speech",
			description: "将文本转换为语音音频",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"text": {
						Type:        "string",
						Description: "要转换的文本（最多 4000 字符）",
					},
					"output_path": {
						Type:        "string",
						Description: "输出文件路径，默认 ~/.selfmind/audio_cache/<timestamp>.mp3",
					},
					"voice": {
						Type:        "string",
						Description: "声音名称（provider 特定）",
					},
				},
				Required: []string{"text"},
			},
		},
	}
}

func (t *TTSTool) Execute(args map[string]interface{}) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("text is required")
	}
	if len(text) > 4000 {
		return "", fmt.Errorf("text exceeds 4000 character limit")
	}

	outputPath, _ := args["output_path"].(string)
	if outputPath == "" {
		os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".selfmind", "audio_cache"), 0755)
		outputPath = filepath.Join(os.Getenv("HOME"), ".selfmind", "audio_cache",
			fmt.Sprintf("%d.mp3", time.Now().UnixNano()))
	}

	provider := os.Getenv("TTS_PROVIDER")
	if provider == "" {
		provider = "edge"
	}

	var audioData []byte
	var err error

	switch provider {
	case "openai":
		audioData, err = t.ttsOpenAI(text)
	case "elevenlabs":
		audioData, err = t.ttsElevenLabs(text, args["voice"].(string))
	default: // edge
		audioData, err = t.ttsEdge(text)
	}

	if err != nil {
		return "", fmt.Errorf("TTS failed: %w", err)
	}

	if err := os.WriteFile(outputPath, audioData, 0644); err != nil {
		return "", fmt.Errorf("write audio file: %w", err)
	}

	return fmt.Sprintf("MEDIA:%s", outputPath), nil
}

func (t *TTSTool) ttsEdge(text string) ([]byte, error) {
	// Microsoft Edge TTS (免费，无需 API key)
	// 使用 edge-tts 库的 REST API 直接调用
	voice := os.Getenv("EDGE_TTS_VOICE")
	if voice == "" {
		voice = "en-US-AriaNeural"
	}

	// edge-tts 使用 WebSocket，这里简化处理
	// 实际应该用成熟的 edge-tts Go 绑定或直接调 WebSocket
	// 暂时返回错误提示用户配置其他 provider
	return nil, fmt.Errorf("edge TTS requires edge-tts Go binding; set TTS_PROVIDER=openai or TTS_PROVIDER=elevenlabs")
}

func (t *TTSTool) ttsOpenAI(text string) ([]byte, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	payload := map[string]interface{}{
		"model": "tts-1",
		"input": text,
		"voice": "alloy",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (t *TTSTool) ttsElevenLabs(text, voiceID string) ([]byte, error) {
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ELEVENLABS_API_KEY not set")
	}
	if voiceID == "" {
		voiceID = "EXAVITQu4vr4xnSDxMaL" // default
	}

	endpoint := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", voiceID)
	payload := map[string]interface{}{
		"text":           text,
		"model_id":       "eleven_monolingual_v1",
		"voice_settings":  map[string]float64{"stability": 0.5, "similarity_boost": 0.75},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "xi-api-key "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}
