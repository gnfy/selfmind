package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

const defaultConfigTemplate = `
agent:
  # 当前使用的主供应商名称 (可选: anthropic, gemini, openai, minimax, openrouter 或自定义名称)
  provider: "gemini"
  
  # 对话最大迭代次数
  max_iterations: 90
  
  # 请求重试次数
  max_retries: 3
  
  # 运行日志级别 (DEBUG, INFO, WARN, ERROR)
  log_level: "INFO"
  
  # Agent 的“灵魂”或人格设定
  soul: "helpful"

providers:
  # 各大主流供应商的 API 密钥
  anthropic_api_key: ""
  openai_api_key: ""
  gemini_api_key: ""
  minimax_api_key: ""
  openrouter_api_key: ""

  # 自定义供应商列表
  custom:
    - name: "deepseek"
      base_url: "https://api.deepseek.com/v1"
      api_key: ""
      model: "deepseek-chat"

storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

evolution:
  enabled: true
  min_complexity_threshold: 5
  auto_archive_confidence: 0.8

delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

cron:
  enabled: true

editor:
  # 粘贴内容超过此字符数时显示占位符（0=禁用）
  large_paste_chars: 1000
  # 粘贴内容超过此行数时显示占位符（0=禁用）
  large_paste_lines: 10
`

type Config struct {
	Agent      AgentConfig       `mapstructure:"agent"`
	Storage    StorageConfig     `mapstructure:"storage"`
	Providers  ProvidersConfig   `mapstructure:"providers"`
	Evolution  EvolutionConfig   `mapstructure:"evolution"`
	MCP        MCPConfig         `mapstructure:"mcp"`
	Delegation DelegationConfig  `mapstructure:"delegation"`
	Cron       CronConfig        `mapstructure:"cron"`
	Editor     EditorConfig      `mapstructure:"editor"`
	Memory     MemoryConfig      `mapstructure:"memory"`
}

type EditorConfig struct {
	LargePasteChars int `mapstructure:"large_paste_chars"`
	LargePasteLines int `mapstructure:"large_paste_lines"`
}

type MemoryConfig struct {
	AutoExtractInterval int  `mapstructure:"auto_extract_interval"` // 每 N 轮提取一次
	AutoExtractMinChars int  `mapstructure:"auto_extract_min_chars"` // 最小内容长度
	SemanticRecall      bool `mapstructure:"semantic_recall"`        // 是否启用语义召回
	UseMemoryFence      bool `mapstructure:"use_memory_fence"`       // 是否用 fence 格式注入记忆
}

// MCPConfig MCP 服务器配置
type MCPConfig struct {
	Servers []MCP_SERVER `mapstructure:"servers"`
}

// MCP_SERVER MCP 服务器定义
type MCP_SERVER struct {
	Name      string            `mapstructure:"name"`
	Transport string            `mapstructure:"transport"` // "stdio" or "http"
	Command   string            `mapstructure:"command,omitempty"`
	Args      []string          `mapstructure:"args,omitempty"`
	URL       string            `mapstructure:"url,omitempty"`
	Headers   map[string]string `mapstructure:"headers,omitempty"`
	Auth      map[string]string `mapstructure:"auth,omitempty"`
	EnvFilter []string          `mapstructure:"env_filter,omitempty"`
}

type EvolutionConfig struct {
	Enabled                bool    `mapstructure:"enabled"`
	Mode                   string  `mapstructure:"mode"`
	MinComplexityThreshold int     `mapstructure:"min_complexity_threshold"`
	AutoArchiveConfidence  float64 `mapstructure:"auto_archive_confidence"`
	NudgeInterval         int     `mapstructure:"nudge_interval"`
	SkillsDir              string  `mapstructure:"skills_dir"`
}

type AgentConfig struct {
	Soul          string `mapstructure:"soul"`
	Provider      string `mapstructure:"provider"`
	Model         string `mapstructure:"model"`
	MaxIterations int    `mapstructure:"max_iterations"`
	MaxRetries    int    `mapstructure:"max_retries"`
	LogLevel      string `mapstructure:"log_level"`
}

type StorageConfig struct {
	Type    string `mapstructure:"type"`
	DataDir string `mapstructure:"data_dir"`
}

type ProvidersConfig struct {
	AnthropicAPIKey  string `mapstructure:"anthropic_api_key"`
	OpenAIAPIKey     string `mapstructure:"openai_api_key"`
	OpenRouterAPIKey string `mapstructure:"openrouter_api_key"`
	GeminiAPIKey     string `mapstructure:"gemini_api_key"`
	MiniMaxAPIKey    string `mapstructure:"minimax_api_key"`
	Custom           []CustomProvider `mapstructure:"custom"`
}

type CustomProvider struct {
	Name    string `mapstructure:"name"`
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
	Model   string `mapstructure:"model"`
}

// DelegationConfig 子 Agent 委托配置
type DelegationConfig struct {
	Provider      string `mapstructure:"provider"` // "anthropic", "openai", "openrouter"
	Model         string `mapstructure:"model"`
	APIKey        string `mapstructure:"api_key"`
	MaxRetries    int    `mapstructure:"max_retries"`
	MaxIterations int    `mapstructure:"max_iterations"`
}

// CronConfig cron 调度器配置
type CronConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

func LoadConfig() (*Config, error) {
	// 0. 确保配置文件存在
	ensureConfigExists()

	v := viper.New()

	// 1. 设置默认值
	v.SetDefault("agent.max_iterations", 90)
	v.SetDefault("agent.max_retries", 3)
	v.SetDefault("delegation.max_iterations", 50)
	v.SetDefault("agent.log_level", "INFO")
	v.SetDefault("storage.type", "sqlite")
	v.SetDefault("storage.data_dir", "~/.selfmind/data")
	v.SetDefault("editor.large_paste_chars", 1000)
	v.SetDefault("editor.large_paste_lines", 10)
	v.SetDefault("memory.auto_extract_interval", 5)
	v.SetDefault("memory.auto_extract_min_chars", 80)
	v.SetDefault("memory.semantic_recall", true)
	v.SetDefault("memory.use_memory_fence", true)

	// 2. 加载配置文件
	home, _ := os.UserHomeDir()
	v.AddConfigPath(filepath.Join(home, ".selfmind"))
	v.SetConfigName("config")
	v.SetConfigType("yaml")

	// 3. 环境变量支持 (e.g., SELF_AGENT_MAX_ITERATIONS)
	v.SetEnvPrefix("SELF")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("error reading config file: %w", err)
		}
	} else {

	}

	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unable to decode into struct: %w", err)
	}

	return &config, nil
}

func ensureConfigExists() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir := filepath.Join(home, ".selfmind")
	configPath := filepath.Join(configDir, "config.yaml")

	// 检查目录是否存在
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return fmt.Errorf("failed to create config directory: %w", err)
		}
	}

	// 检查文件是否存在
	if _, err := os.Stat(configPath); os.IsNotExist(err) {

		if err := os.WriteFile(configPath, []byte(defaultConfigTemplate), 0644); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	return nil
}
