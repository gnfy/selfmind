package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

const defaultConfigTemplate = `
model:
  provider: ""
  default: ""

providers:
  openai:
    api_key: ""
    base_url: "https://api.openai.com/v1"
    protocol: "openai_chat"
  anthropic:
    api_key: ""
    base_url: "https://api.anthropic.com"
    protocol: "anthropic_messages"
  google:
    api_key: ""
    base_url: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
    protocol: "openai_compatible"
  custom: []

provider_profiles: {}

auth:
  credentials_file: "~/.selfmind/auth.json"

storage:
  type: "sqlite"
  data_dir: "~/.selfmind/data"

gateway:
  addr: "127.0.0.1:8765"
  token: ""
  outbound_webhook_url: ""
  outbound_webhook_token: ""
  telegram_token: ""
  delivery_max_message_chars: 3500
  delivery_retry_attempts: 3
  weixin:
    enabled: false
    owner_person_id: ""
    account_id: ""
    token: ""
    base_url: "https://ilinkai.weixin.qq.com"
    cdn_base_url: "https://novac2c.cdn.weixin.qq.com/c2c"
    dm_policy: "open"
    group_policy: "disabled"
    allow_from: []
    group_allow_from: []
    split_multiline_messages: false
    send_chunk_delay_seconds: 1.5
    send_chunk_retries: 4

evolution:
  enabled: true
  min_complexity_threshold: 5
  auto_archive_confidence: 0.8
  nudge_interval: 10

delegation:
  provider: ""
  model: ""
  api_key: ""
  max_retries: 3
  max_iterations: 50

models:
  source: "local"
  roles: {}

intent:
  mode: "hybrid"
  rules:
    continue: []
    task: []
    casual: []
  thresholds:
    direct: 0.8
    ask: 0.55

cron:
  enabled: true

editor:
  large_paste_chars: 1000
  large_paste_lines: 10
`

type Options struct {
	Path            string
	CreateIfMissing bool
}

type Config struct {
	Path             string                      `mapstructure:"-" yaml:"-"`
	Model            ModelConfig                 `mapstructure:"model" yaml:"model,omitempty"`
	Agent            AgentConfig                 `mapstructure:"agent" yaml:"agent,omitempty"`
	Storage          StorageConfig               `mapstructure:"storage" yaml:"storage,omitempty"`
	Gateway          GatewayConfig               `mapstructure:"gateway" yaml:"gateway,omitempty"`
	Providers        ProvidersConfig             `mapstructure:"providers" yaml:"providers,omitempty"`
	ProviderProfiles map[string]ProviderEndpoint `mapstructure:"provider_profiles" yaml:"provider_profiles,omitempty"`
	Auth             AuthConfig                  `mapstructure:"auth" yaml:"auth,omitempty"`
	Evolution        EvolutionConfig             `mapstructure:"evolution" yaml:"evolution,omitempty"`
	MCP              MCPConfig                   `mapstructure:"mcp" yaml:"mcp,omitempty"`
	Delegation       DelegationConfig            `mapstructure:"delegation" yaml:"delegation,omitempty"`
	Cron             CronConfig                  `mapstructure:"cron" yaml:"cron,omitempty"`
	Editor           EditorConfig                `mapstructure:"editor" yaml:"editor,omitempty"`
	Memory           MemoryConfig                `mapstructure:"memory" yaml:"memory,omitempty"`
	Models           ModelsConfig                `mapstructure:"models" yaml:"models,omitempty"`
	Intent           IntentConfig                `mapstructure:"intent" yaml:"intent,omitempty"`
}

type ModelConfig struct {
	Provider      string `mapstructure:"provider" yaml:"provider,omitempty"`
	Default       string `mapstructure:"default" yaml:"default,omitempty"`
	ContextLength int    `mapstructure:"context_length" yaml:"context_length,omitempty"`
}

type IntentConfig struct {
	Mode       string                 `mapstructure:"mode" yaml:"mode,omitempty"`
	Rules      map[string][]string    `mapstructure:"rules" yaml:"rules,omitempty"`
	Thresholds IntentThresholdsConfig `mapstructure:"thresholds" yaml:"thresholds,omitempty"`
}

type IntentThresholdsConfig struct {
	Direct float64 `mapstructure:"direct" yaml:"direct,omitempty"`
	Ask    float64 `mapstructure:"ask" yaml:"ask,omitempty"`
}

type EditorConfig struct {
	LargePasteChars int `mapstructure:"large_paste_chars" yaml:"large_paste_chars,omitempty"`
	LargePasteLines int `mapstructure:"large_paste_lines" yaml:"large_paste_lines,omitempty"`
}

type AuthConfig struct {
	CredentialsFile string `mapstructure:"credentials_file" yaml:"credentials_file,omitempty"`
}

type MemoryConfig struct {
	AutoExtractInterval int  `mapstructure:"auto_extract_interval" yaml:"auto_extract_interval,omitempty"`
	AutoExtractMinChars int  `mapstructure:"auto_extract_min_chars" yaml:"auto_extract_min_chars,omitempty"`
	SemanticRecall      bool `mapstructure:"semantic_recall" yaml:"semantic_recall,omitempty"`
	UseMemoryFence      bool `mapstructure:"use_memory_fence" yaml:"use_memory_fence,omitempty"`
}

type MCPConfig struct {
	Servers []MCP_SERVER `mapstructure:"servers" yaml:"servers,omitempty"`
}

type MCP_SERVER struct {
	Name      string            `mapstructure:"name" yaml:"name,omitempty"`
	Transport string            `mapstructure:"transport" yaml:"transport,omitempty"`
	Command   string            `mapstructure:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `mapstructure:"args,omitempty" yaml:"args,omitempty"`
	URL       string            `mapstructure:"url,omitempty" yaml:"url,omitempty"`
	Headers   map[string]string `mapstructure:"headers,omitempty" yaml:"headers,omitempty"`
	Auth      map[string]string `mapstructure:"auth,omitempty" yaml:"auth,omitempty"`
	EnvFilter []string          `mapstructure:"env_filter,omitempty" yaml:"env_filter,omitempty"`
}

type EvolutionConfig struct {
	Enabled                bool    `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Mode                   string  `mapstructure:"mode" yaml:"mode,omitempty"`
	MinComplexityThreshold int     `mapstructure:"min_complexity_threshold" yaml:"min_complexity_threshold,omitempty"`
	AutoArchiveConfidence  float64 `mapstructure:"auto_archive_confidence" yaml:"auto_archive_confidence,omitempty"`
	NudgeInterval          int     `mapstructure:"nudge_interval" yaml:"nudge_interval,omitempty"`
	SkillsDir              string  `mapstructure:"skills_dir" yaml:"skills_dir,omitempty"`
}

type AgentConfig struct {
	Soul          string `mapstructure:"soul" yaml:"soul,omitempty"`
	Provider      string `mapstructure:"provider" yaml:"-"`
	Model         string `mapstructure:"model" yaml:"-"`
	MaxIterations int    `mapstructure:"max_iterations" yaml:"max_iterations,omitempty"`
	MaxRetries    int    `mapstructure:"max_retries" yaml:"max_retries,omitempty"`
	LogLevel      string `mapstructure:"log_level" yaml:"log_level,omitempty"`
}

type StorageConfig struct {
	Type    string `mapstructure:"type" yaml:"type,omitempty"`
	DataDir string `mapstructure:"data_dir" yaml:"data_dir,omitempty"`
}

type GatewayConfig struct {
	Addr                    string       `mapstructure:"addr" yaml:"addr,omitempty"`
	URL                     string       `mapstructure:"url" yaml:"url,omitempty"`
	Token                   string       `mapstructure:"token" yaml:"token,omitempty"`
	DrainTimeout            string       `mapstructure:"drain_timeout" yaml:"drain_timeout,omitempty"`
	OutboundWebhookURL      string       `mapstructure:"outbound_webhook_url" yaml:"outbound_webhook_url,omitempty"`
	OutboundWebhookToken    string       `mapstructure:"outbound_webhook_token" yaml:"outbound_webhook_token,omitempty"`
	TelegramToken           string       `mapstructure:"telegram_token" yaml:"telegram_token,omitempty"`
	DeliveryMaxMessageChars int          `mapstructure:"delivery_max_message_chars" yaml:"delivery_max_message_chars,omitempty"`
	DeliveryRetryAttempts   int          `mapstructure:"delivery_retry_attempts" yaml:"delivery_retry_attempts,omitempty"`
	Weixin                  WeixinConfig `mapstructure:"weixin" yaml:"weixin,omitempty"`
}

type WeixinConfig struct {
	Enabled                bool     `mapstructure:"enabled" yaml:"enabled,omitempty"`
	OwnerPersonID          string   `mapstructure:"owner_person_id" yaml:"owner_person_id,omitempty"`
	AccountID              string   `mapstructure:"account_id" yaml:"account_id,omitempty"`
	Token                  string   `mapstructure:"token" yaml:"token,omitempty"`
	BaseURL                string   `mapstructure:"base_url" yaml:"base_url,omitempty"`
	CDNBaseURL             string   `mapstructure:"cdn_base_url" yaml:"cdn_base_url,omitempty"`
	DMPolicy               string   `mapstructure:"dm_policy" yaml:"dm_policy,omitempty"`
	GroupPolicy            string   `mapstructure:"group_policy" yaml:"group_policy,omitempty"`
	AllowFrom              []string `mapstructure:"allow_from" yaml:"allow_from,omitempty"`
	GroupAllowFrom         []string `mapstructure:"group_allow_from" yaml:"group_allow_from,omitempty"`
	SplitMultilineMessages bool     `mapstructure:"split_multiline_messages" yaml:"split_multiline_messages,omitempty"`
	SendChunkDelaySeconds  float64  `mapstructure:"send_chunk_delay_seconds" yaml:"send_chunk_delay_seconds,omitempty"`
	SendChunkRetries       int      `mapstructure:"send_chunk_retries" yaml:"send_chunk_retries,omitempty"`
}

type ProvidersConfig struct {
	OpenAI    ProviderEndpoint `mapstructure:"openai" yaml:"openai,omitempty"`
	Anthropic ProviderEndpoint `mapstructure:"anthropic" yaml:"anthropic,omitempty"`
	Google    ProviderEndpoint `mapstructure:"google" yaml:"google,omitempty"`

	AnthropicAPIKey  string `mapstructure:"anthropic_api_key" yaml:"-"`
	OpenAIAPIKey     string `mapstructure:"openai_api_key" yaml:"-"`
	OpenRouterAPIKey string `mapstructure:"openrouter_api_key" yaml:"-"`
	GeminiAPIKey     string `mapstructure:"gemini_api_key" yaml:"-"`
	MiniMaxAPIKey    string `mapstructure:"minimax_api_key" yaml:"-"`

	Custom []CustomProvider `mapstructure:"custom" yaml:"custom,omitempty"`
}

type ProviderEndpoint struct {
	APIKey          string                 `mapstructure:"api_key" yaml:"api_key,omitempty"`
	BaseURL         string                 `mapstructure:"base_url" yaml:"base_url,omitempty"`
	Protocol        string                 `mapstructure:"protocol" yaml:"protocol,omitempty"`
	Model           string                 `mapstructure:"model" yaml:"model,omitempty"`
	ContextLength   int                    `mapstructure:"context_length" yaml:"context_length,omitempty"`
	Headers         map[string]string      `mapstructure:"headers" yaml:"headers,omitempty"`
	MaxTokens       int                    `mapstructure:"max_tokens" yaml:"max_tokens,omitempty"`
	ReasoningEffort string                 `mapstructure:"reasoning_effort" yaml:"reasoning_effort,omitempty"`
	Thinking        map[string]interface{} `mapstructure:"thinking" yaml:"thinking,omitempty"`
	ServiceTier     string                 `mapstructure:"service_tier" yaml:"service_tier,omitempty"`
	Quirks          ProviderQuirks         `mapstructure:"quirks" yaml:"quirks,omitempty"`
}

type ProviderQuirks struct {
	AuthHeader        string `mapstructure:"auth_header" yaml:"auth_header,omitempty"`
	ToolSchema        string `mapstructure:"tool_schema" yaml:"tool_schema,omitempty"`
	SystemMessageMode string `mapstructure:"system_message_mode" yaml:"system_message_mode,omitempty"`
	ThinkingMode      string `mapstructure:"thinking_mode" yaml:"thinking_mode,omitempty"`
	UserAgent         string `mapstructure:"user_agent" yaml:"user_agent,omitempty"`
}

type ModelsConfig struct {
	Source string                     `mapstructure:"source" yaml:"source,omitempty"`
	Roles  map[string]ModelRoleConfig `mapstructure:"roles" yaml:"roles,omitempty"`
}

type ModelRoleConfig struct {
	Provider        string                 `mapstructure:"provider" yaml:"provider,omitempty"`
	Model           string                 `mapstructure:"model" yaml:"model,omitempty"`
	BaseURL         string                 `mapstructure:"base_url" yaml:"base_url,omitempty"`
	APIKey          string                 `mapstructure:"api_key" yaml:"api_key,omitempty"`
	ContextLength   int                    `mapstructure:"context_length" yaml:"context_length,omitempty"`
	MaxTokens       int                    `mapstructure:"max_tokens" yaml:"max_tokens,omitempty"`
	Headers         map[string]string      `mapstructure:"headers" yaml:"headers,omitempty"`
	ReasoningEffort string                 `mapstructure:"reasoning_effort" yaml:"reasoning_effort,omitempty"`
	Thinking        map[string]interface{} `mapstructure:"thinking" yaml:"thinking,omitempty"`
	ServiceTier     string                 `mapstructure:"service_tier" yaml:"service_tier,omitempty"`
	Quirks          ProviderQuirks         `mapstructure:"quirks" yaml:"quirks,omitempty"`
}

type CustomProvider struct {
	Name     string                           `mapstructure:"name" yaml:"name,omitempty"`
	BaseURL  string                           `mapstructure:"base_url" yaml:"base_url,omitempty"`
	APIKey   string                           `mapstructure:"api_key" yaml:"api_key,omitempty"`
	Protocol string                           `mapstructure:"protocol" yaml:"protocol,omitempty"`
	Model    string                           `mapstructure:"model" yaml:"model,omitempty"`
	Models   map[string]CustomModelProperties `mapstructure:"models" yaml:"models,omitempty"`
}

type CustomModelProperties struct {
	ContextLength int `mapstructure:"context_length" yaml:"context_length,omitempty"`
}

type DelegationConfig struct {
	Provider      string `mapstructure:"provider" yaml:"provider,omitempty"`
	Model         string `mapstructure:"model" yaml:"model,omitempty"`
	APIKey        string `mapstructure:"api_key" yaml:"api_key,omitempty"`
	MaxRetries    int    `mapstructure:"max_retries" yaml:"max_retries,omitempty"`
	MaxIterations int    `mapstructure:"max_iterations" yaml:"max_iterations,omitempty"`
}

type CronConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
}

func LoadConfig(options ...Options) (*Config, error) {
	opts := Options{}
	if len(options) > 0 {
		opts = options[0]
	}
	path, isDefault := ResolveConfigPath(opts.Path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if isDefault || opts.CreateIfMissing {
			if err := EnsureConfigExists(path); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("config file does not exist: %s", path)
		}
	}

	v := viper.New()
	setDefaults(v)
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("SELF")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("error reading config file %s: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unable to decode config %s: %w", path, err)
	}
	cfg.Path = path
	cfg.Normalize()
	return &cfg, nil
}

func SaveConfig(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(path) == "" {
		path = cfg.Path
	}
	if strings.TrimSpace(path) == "" {
		path, _ = ResolveConfigPath("")
	}
	cfg.Path = path
	cfg.Normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ResolveConfigPath(explicit string) (string, bool) {
	if path := strings.TrimSpace(explicit); path != "" {
		return cleanPath(path), false
	}
	if path := strings.TrimSpace(os.Getenv("SELF_CONFIG")); path != "" {
		return cleanPath(path), false
	}
	return DefaultConfigPath(), true
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".selfmind", "config.yaml")
	}
	return filepath.Join(home, ".selfmind", "config.yaml")
}

func EnsureConfigExists(path string) error {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(defaultConfigTemplate), 0600); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	return nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("agent.max_iterations", 90)
	v.SetDefault("agent.max_retries", 3)
	v.SetDefault("delegation.max_iterations", 50)
	v.SetDefault("agent.log_level", "INFO")
	v.SetDefault("storage.type", "sqlite")
	v.SetDefault("storage.data_dir", "~/.selfmind/data")
	v.SetDefault("gateway.addr", "127.0.0.1:8765")
	v.SetDefault("gateway.delivery_max_message_chars", 3500)
	v.SetDefault("gateway.delivery_retry_attempts", 3)
	v.SetDefault("gateway.weixin.base_url", "https://ilinkai.weixin.qq.com")
	v.SetDefault("gateway.weixin.cdn_base_url", "https://novac2c.cdn.weixin.qq.com/c2c")
	v.SetDefault("gateway.weixin.dm_policy", "open")
	v.SetDefault("gateway.weixin.group_policy", "disabled")
	v.SetDefault("gateway.weixin.send_chunk_delay_seconds", 1.5)
	v.SetDefault("gateway.weixin.send_chunk_retries", 4)
	v.SetDefault("auth.credentials_file", "~/.selfmind/auth.json")
	v.SetDefault("editor.large_paste_chars", 1000)
	v.SetDefault("editor.large_paste_lines", 10)
	v.SetDefault("memory.auto_extract_interval", 5)
	v.SetDefault("memory.auto_extract_min_chars", 80)
	v.SetDefault("memory.semantic_recall", true)
	v.SetDefault("memory.use_memory_fence", true)
	v.SetDefault("evolution.enabled", true)
	v.SetDefault("evolution.nudge_interval", 10)
	v.SetDefault("models.source", "local")
	v.SetDefault("intent.mode", "hybrid")
	v.SetDefault("intent.thresholds.direct", 0.8)
	v.SetDefault("intent.thresholds.ask", 0.55)
}

func (c *Config) Normalize() {
	if c.Models.Roles == nil {
		c.Models.Roles = make(map[string]ModelRoleConfig)
	}
	if c.ProviderProfiles == nil {
		c.ProviderProfiles = make(map[string]ProviderEndpoint)
	}
	if c.Intent.Rules == nil {
		c.Intent.Rules = make(map[string][]string)
	}
	c.Intent.Mode = strings.ToLower(strings.TrimSpace(firstNonEmpty(c.Intent.Mode, "hybrid")))
	if c.Intent.Thresholds.Direct <= 0 {
		c.Intent.Thresholds.Direct = 0.8
	}
	if c.Intent.Thresholds.Ask <= 0 {
		c.Intent.Thresholds.Ask = 0.55
	}
	for name, patterns := range c.Intent.Rules {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			delete(c.Intent.Rules, name)
			continue
		}
		normalized := make([]string, 0, len(patterns))
		for _, pattern := range patterns {
			pattern = expandEnvRef(strings.TrimSpace(pattern))
			if pattern != "" {
				normalized = append(normalized, pattern)
			}
		}
		if trimmed != name {
			delete(c.Intent.Rules, name)
		}
		c.Intent.Rules[trimmed] = normalized
	}
	if strings.TrimSpace(c.Models.Source) == "" {
		c.Models.Source = "local"
	}

	c.Model.Provider = expandEnvRef(c.Model.Provider)
	c.Model.Default = expandEnvRef(c.Model.Default)
	c.Agent.Provider = expandEnvRef(c.Agent.Provider)
	c.Agent.Model = expandEnvRef(c.Agent.Model)
	c.Agent.Soul = expandEnvRef(c.Agent.Soul)
	c.Storage.DataDir = expandEnvRef(c.Storage.DataDir)
	c.Gateway.Addr = expandEnvRef(c.Gateway.Addr)
	c.Gateway.URL = expandEnvRef(c.Gateway.URL)
	c.Gateway.Token = expandEnvRef(c.Gateway.Token)
	c.Gateway.DrainTimeout = expandEnvRef(c.Gateway.DrainTimeout)
	c.Gateway.OutboundWebhookURL = expandEnvRef(c.Gateway.OutboundWebhookURL)
	c.Gateway.OutboundWebhookToken = expandEnvRef(c.Gateway.OutboundWebhookToken)
	c.Gateway.TelegramToken = expandEnvRef(c.Gateway.TelegramToken)
	c.Gateway.Weixin = normalizeWeixin(c.Gateway.Weixin)
	c.Auth.CredentialsFile = cleanPath(firstNonEmpty(c.Auth.CredentialsFile, "~/.selfmind/auth.json"))
	c.Delegation.Provider = expandEnvRef(c.Delegation.Provider)
	c.Delegation.Model = expandEnvRef(c.Delegation.Model)
	c.Delegation.APIKey = expandEnvRef(c.Delegation.APIKey)

	c.Providers.OpenAI = normalizeEndpoint(c.Providers.OpenAI, c.Providers.OpenAIAPIKey, "https://api.openai.com/v1", "openai_chat")
	c.Providers.Anthropic = normalizeEndpoint(c.Providers.Anthropic, c.Providers.AnthropicAPIKey, "https://api.anthropic.com", "anthropic_messages")
	c.Providers.Google = normalizeEndpoint(c.Providers.Google, c.Providers.GeminiAPIKey, "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "openai_compatible")

	c.Providers.OpenAIAPIKey = firstNonEmpty(c.Providers.OpenAIAPIKey, c.Providers.OpenAI.APIKey)
	c.Providers.AnthropicAPIKey = firstNonEmpty(c.Providers.AnthropicAPIKey, c.Providers.Anthropic.APIKey)
	c.Providers.GeminiAPIKey = firstNonEmpty(c.Providers.GeminiAPIKey, c.Providers.Google.APIKey)
	c.Providers.OpenRouterAPIKey = expandEnvRef(c.Providers.OpenRouterAPIKey)
	c.Providers.MiniMaxAPIKey = expandEnvRef(c.Providers.MiniMaxAPIKey)

	for i := range c.Providers.Custom {
		c.Providers.Custom[i].Name = strings.TrimSpace(c.Providers.Custom[i].Name)
		c.Providers.Custom[i].BaseURL = expandEnvRef(c.Providers.Custom[i].BaseURL)
		c.Providers.Custom[i].APIKey = expandEnvRef(c.Providers.Custom[i].APIKey)
		c.Providers.Custom[i].Protocol = firstNonEmpty(c.Providers.Custom[i].Protocol, "openai_compatible")
		c.Providers.Custom[i].Model = expandEnvRef(c.Providers.Custom[i].Model)
	}
	for name, endpoint := range c.ProviderProfiles {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			delete(c.ProviderProfiles, name)
			continue
		}
		endpoint.APIKey = expandEnvRef(endpoint.APIKey)
		endpoint.BaseURL = expandEnvRef(endpoint.BaseURL)
		endpoint.Protocol = expandEnvRef(endpoint.Protocol)
		endpoint.Model = expandEnvRef(endpoint.Model)
		endpoint.Headers = normalizeHeaders(endpoint.Headers)
		endpoint.ReasoningEffort = expandEnvRef(endpoint.ReasoningEffort)
		endpoint.ServiceTier = expandEnvRef(endpoint.ServiceTier)
		endpoint.Quirks = normalizeProviderQuirks(endpoint.Quirks)
		if trimmed != name {
			delete(c.ProviderProfiles, name)
		}
		c.ProviderProfiles[strings.ToLower(trimmed)] = endpoint
	}
	for name, role := range c.Models.Roles {
		role.Provider = expandEnvRef(role.Provider)
		role.Model = expandEnvRef(role.Model)
		role.BaseURL = expandEnvRef(role.BaseURL)
		role.APIKey = expandEnvRef(role.APIKey)
		role.Headers = normalizeHeaders(role.Headers)
		role.ReasoningEffort = expandEnvRef(role.ReasoningEffort)
		role.ServiceTier = expandEnvRef(role.ServiceTier)
		role.Quirks = normalizeProviderQuirks(role.Quirks)
		c.Models.Roles[name] = role
	}

	if strings.TrimSpace(c.Model.Provider) == "" {
		c.Model.Provider = c.Agent.Provider
	}
	if strings.TrimSpace(c.Model.Default) == "" {
		c.Model.Default = c.Agent.Model
	}
	if strings.TrimSpace(c.Agent.Provider) == "" {
		c.Agent.Provider = c.Model.Provider
	}
	if strings.TrimSpace(c.Agent.Model) == "" {
		c.Agent.Model = c.Model.Default
	}
}

func normalizeWeixin(wx WeixinConfig) WeixinConfig {
	wx.OwnerPersonID = expandEnvRef(wx.OwnerPersonID)
	wx.AccountID = firstNonEmpty(expandEnvRef(wx.AccountID), os.Getenv("WEIXIN_ACCOUNT_ID"))
	wx.Token = firstNonEmpty(expandEnvRef(wx.Token), os.Getenv("WEIXIN_TOKEN"))
	wx.BaseURL = strings.TrimRight(configOrEnvDefault(wx.BaseURL, "WEIXIN_BASE_URL", "https://ilinkai.weixin.qq.com"), "/")
	wx.CDNBaseURL = strings.TrimRight(configOrEnvDefault(wx.CDNBaseURL, "WEIXIN_CDN_BASE_URL", "https://novac2c.cdn.weixin.qq.com/c2c"), "/")
	wx.DMPolicy = normalizePolicy(configOrEnvDefault(wx.DMPolicy, "WEIXIN_DM_POLICY", "open"), "open")
	wx.GroupPolicy = normalizePolicy(configOrEnvDefault(wx.GroupPolicy, "WEIXIN_GROUP_POLICY", "disabled"), "disabled")
	wx.AllowFrom = normalizeStringList(firstNonEmpty(strings.Join(wx.AllowFrom, ","), os.Getenv("WEIXIN_ALLOWED_USERS")))
	wx.GroupAllowFrom = normalizeStringList(firstNonEmpty(strings.Join(wx.GroupAllowFrom, ","), os.Getenv("WEIXIN_GROUP_ALLOWED_USERS")))
	if env := strings.TrimSpace(os.Getenv("WEIXIN_SPLIT_MULTILINE_MESSAGES")); env != "" {
		wx.SplitMultilineMessages = parseBool(env, wx.SplitMultilineMessages)
	}
	if wx.SendChunkDelaySeconds <= 0 {
		wx.SendChunkDelaySeconds = 1.5
	}
	if wx.SendChunkRetries <= 0 {
		wx.SendChunkRetries = 4
	}
	if wx.AccountID != "" || wx.Token != "" {
		wx.Enabled = true
	}
	return wx
}

func configOrEnvDefault(configValue, envKey, defaultValue string) string {
	configValue = expandEnvRef(configValue)
	envValue := strings.TrimSpace(os.Getenv(envKey))
	if envValue != "" && (strings.TrimSpace(configValue) == "" || strings.TrimSpace(configValue) == defaultValue) {
		return envValue
	}
	return firstNonEmpty(configValue, envValue, defaultValue)
}

func normalizePolicy(value, fallbackValue string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "open", "allowlist", "disabled", "pairing":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return fallbackValue
	}
}

func normalizeStringList(value string) []string {
	value = expandEnvRef(value)
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func parseBool(value string, fallbackValue bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallbackValue
	}
}

func (c *Config) EffectiveProvider() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(c.Model.Provider, c.Agent.Provider))
}

func (c *Config) EffectiveModel() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(c.Model.Default, c.Agent.Model))
}

func (c *Config) SetDefaultModel(provider, model string) {
	c.Model.Provider = strings.TrimSpace(provider)
	c.Model.Default = strings.TrimSpace(model)
	c.Agent.Provider = c.Model.Provider
	c.Agent.Model = c.Model.Default
}

func normalizeEndpoint(ep ProviderEndpoint, legacyKey, defaultBaseURL, defaultProtocol string) ProviderEndpoint {
	ep.APIKey = expandEnvRef(firstNonEmpty(ep.APIKey, legacyKey))
	ep.BaseURL = expandEnvRef(firstNonEmpty(ep.BaseURL, defaultBaseURL))
	ep.Protocol = firstNonEmpty(ep.Protocol, defaultProtocol)
	ep.Model = expandEnvRef(ep.Model)
	ep.Headers = normalizeHeaders(ep.Headers)
	ep.ReasoningEffort = expandEnvRef(ep.ReasoningEffort)
	ep.ServiceTier = expandEnvRef(ep.ServiceTier)
	ep.Quirks = normalizeProviderQuirks(ep.Quirks)
	return ep
}

func normalizeProviderQuirks(q ProviderQuirks) ProviderQuirks {
	q.AuthHeader = strings.ToLower(strings.TrimSpace(expandEnvRef(q.AuthHeader)))
	q.ToolSchema = strings.ToLower(strings.TrimSpace(expandEnvRef(q.ToolSchema)))
	q.SystemMessageMode = strings.ToLower(strings.TrimSpace(expandEnvRef(q.SystemMessageMode)))
	q.ThinkingMode = strings.ToLower(strings.TrimSpace(expandEnvRef(q.ThinkingMode)))
	q.UserAgent = strings.TrimSpace(expandEnvRef(q.UserAgent))
	return q
}

func normalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = expandEnvRef(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanPath(path string) string {
	path = expandEnvRef(path)
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return filepath.Clean(path)
}

func expandEnvRef(value string) string {
	if strings.Contains(value, "${") {
		return os.ExpandEnv(value)
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
