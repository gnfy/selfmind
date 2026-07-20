package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
  # How long the TUI may sit without keystrokes before it stops counting as
  # "attached" (pushes then resume on your preferred IM). "0" = never idle.
  presence_idle_timeout: "5m"
  # An unanswered approval/question is re-pushed to your preferred IM this long
  # after it was raised, if you have since left the CLI (presence detached) and
  # no IM notification was sent yet. The escrow sweep runs every 60s, so the
  # effective latency is this value + up to 60s. "0" = disable escrow.
  pending_notify_after: "2m"
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
  wechat:
    enabled: false
    app_id: ""
    app_secret: ""
    token: ""
    base_url: "https://api.weixin.qq.com"
  feishu:
    enabled: false
    app_id: ""
    app_secret: ""
    base_url: "https://open.feishu.cn"
    verification_token: ""
    encrypt_key: ""
  qq:
    enabled: false
    app_id: ""
    secret: ""
    token: ""
    base_url: "https://api.sgroup.qq.com"
    sandbox: false

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

flight_recorder:
  enabled: false
  keep: 20

memory:
  auto_extract_interval: 5
  auto_extract_min_chars: 80
  semantic_recall: true
  use_memory_fence: true

tasks:
  inbox_enabled: true
  default_list_limit: 10
  auto_archive_done_after: "720h"
  auto_archive_cancelled_after: "168h"
  maintenance_model_role: "memory_extract"
  maintenance_fallback_roles: ["background_review", "fast_classifier"]
  maintenance_debounce: "5m"
  maintenance_max_wait: "15m"
  maintenance_batch_max_runs: 10
  maintenance_quota_probe_initial: "15m"
  maintenance_quota_probe_max: "4h"

editor:
  large_paste_chars: 1000
  large_paste_lines: 10

# Persistent CLI input history (up/down-arrow recall across sessions),
# stored as ~/.selfmind/input_history.jsonl. persistence: "none" disables
# disk writes; in-session recall still works.
history:
  persistence: "save-all"   # save-all | none
  max_bytes: 524288
  load_entries: 200

# Web search backend. Local DuckDuckGo scraping is unreliable (anti-bot
# blocks, GFW), so a hosted search API is strongly recommended: pick one
# backend and paste ITS key. Leave both empty for best-effort DuckDuckGo.
#   tavily : https://tavily.com          (recommended; AI-native, free tier)
#   brave  : https://brave.com/search/api (independent index, free tier)
#   serper : https://serper.dev           (Google results, free credits)
# (searxng: set search_backend: searxng and put your instance URL in api_key)
web:
  search_backend: ""   # tavily | brave | serper | firecrawl | searxng | duckduckgo
  api_key: ""

# Shell/Python execution prefers an isolated, no-network Linux sandbox. Calls
# that intentionally need host credentials or networking can request
# sandbox=host and go through the normal approval flow.
exec_sandbox:
  enabled: true
  required: false
  allow_network: false
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
	FlightRecorder   FlightRecorderConfig        `mapstructure:"flight_recorder" yaml:"flight_recorder,omitempty"`
	Editor           EditorConfig                `mapstructure:"editor" yaml:"editor,omitempty"`
	Memory           MemoryConfig                `mapstructure:"memory" yaml:"memory,omitempty"`
	Tasks            TaskConfig                  `mapstructure:"tasks" yaml:"tasks,omitempty"`
	Models           ModelsConfig                `mapstructure:"models" yaml:"models,omitempty"`
	Intent           IntentConfig                `mapstructure:"intent" yaml:"intent,omitempty"`
	Web              WebConfig                   `mapstructure:"web" yaml:"web,omitempty"`
	ExecSandbox      ExecSandboxConfig           `mapstructure:"exec_sandbox" yaml:"exec_sandbox,omitempty"`
	History          HistoryConfig               `mapstructure:"history" yaml:"history,omitempty"`
}

// ExecSandboxConfig gates bubblewrap isolation for terminal, verify, and
// execute_code. Linux defaults to best-effort isolation; Required turns an
// unavailable sandbox into a hard refusal instead of an observable host
// fallback.
type ExecSandboxConfig struct {
	Enabled      bool `mapstructure:"enabled" yaml:"enabled"`
	Required     bool `mapstructure:"required" yaml:"required"`
	AllowNetwork bool `mapstructure:"allow_network" yaml:"allow_network"`
}

// WebConfig configures the web_search backend. Local HTML scraping
// (DuckDuckGo) is unreliable on many network egresses (anti-bot 202, GFW), so
// a hosted search API is the quality path — the same choice codex (server-side
// search) and hermes (managed Firecrawl) make. Just two fields: pick ONE
// backend and give ITS credential. The key lives here, NOT in env vars: the
// daemon is started detached (setsid) and does not inherit a shell's exports,
// which is why env-only backends never worked in practice.
type WebConfig struct {
	// SearchBackend picks the backend: tavily | brave | serper | firecrawl |
	// searxng | duckduckgo. Empty = duckduckgo scraping (best-effort, no key).
	SearchBackend string `mapstructure:"search_backend" yaml:"search_backend,omitempty"`
	// APIKey is the credential for the chosen backend (the searxng backend
	// reads its instance URL from this field instead of a key). Ignored by
	// duckduckgo.
	APIKey string `mapstructure:"api_key" yaml:"api_key,omitempty"`
}

type ModelConfig struct {
	Provider      string `mapstructure:"provider" yaml:"provider,omitempty"`
	Default       string `mapstructure:"default" yaml:"default,omitempty"`
	ContextLength int    `mapstructure:"context_length" yaml:"context_length,omitempty"`
}

type IntentConfig struct {
	Mode string `mapstructure:"mode" yaml:"mode,omitempty"`
	// Deprecated: continue_window configured the implicit-continuation LLM
	// upgrade, removed with Work Timeline P3 (context is spine-based, so task
	// attachment no longer affects context). The field is kept only so
	// existing config.yaml files keep loading; its value is ignored.
	ContinueWindow string                 `mapstructure:"continue_window" yaml:"continue_window,omitempty"`
	Rules          map[string][]string    `mapstructure:"rules" yaml:"rules,omitempty"`
	Thresholds     IntentThresholdsConfig `mapstructure:"thresholds" yaml:"thresholds,omitempty"`
}

type IntentThresholdsConfig struct {
	Direct float64 `mapstructure:"direct" yaml:"direct,omitempty"`
	Ask    float64 `mapstructure:"ask" yaml:"ask,omitempty"`
}

type EditorConfig struct {
	LargePasteChars int `mapstructure:"large_paste_chars" yaml:"large_paste_chars,omitempty"`
	LargePasteLines int `mapstructure:"large_paste_lines" yaml:"large_paste_lines,omitempty"`
}

// HistoryConfig governs the CLI's persistent input history (the up/down-arrow
// composer history, ~/.selfmind/input_history.jsonl). Persistence "none"
// disables disk writes entirely; in-session history still works.
type HistoryConfig struct {
	Persistence string `mapstructure:"persistence" yaml:"persistence,omitempty"`
	MaxBytes    int64  `mapstructure:"max_bytes" yaml:"max_bytes,omitempty"`
	LoadEntries int    `mapstructure:"load_entries" yaml:"load_entries,omitempty"`
}

// PersistEnabled reports whether input history should be written to disk.
// Any value other than "none" means save (default "save-all").
func (h HistoryConfig) PersistEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(h.Persistence), "none")
}

type AuthConfig struct {
	CredentialsFile string `mapstructure:"credentials_file" yaml:"credentials_file,omitempty"`
}

type MemoryConfig struct {
	AutoExtractInterval int                    `mapstructure:"auto_extract_interval" yaml:"auto_extract_interval,omitempty"`
	AutoExtractMinChars int                    `mapstructure:"auto_extract_min_chars" yaml:"auto_extract_min_chars,omitempty"`
	SemanticRecall      bool                   `mapstructure:"semantic_recall" yaml:"semantic_recall,omitempty"`
	UseMemoryFence      bool                   `mapstructure:"use_memory_fence" yaml:"use_memory_fence,omitempty"`
	Governance          MemoryGovernanceConfig `mapstructure:"governance" yaml:"governance,omitempty"`
}

// MemoryGovernanceConfig controls background memory self-organization
// (docs/memory-governance.zh-CN.md §4). Mode gates what may be APPLIED:
// "shadow" judges and reports only, "merge-only" additionally applies
// high-confidence MERGE, "full" adds caps/archival. The judge always uses the
// explicitly configured role and never falls back to the main coding model.
type MemoryGovernanceConfig struct {
	Enabled                bool    `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Mode                   string  `mapstructure:"mode" yaml:"mode,omitempty"` // shadow | merge-only | full
	ModelRole              string  `mapstructure:"model_role" yaml:"model_role,omitempty"`
	ConsolidationInterval  string  `mapstructure:"consolidation_interval" yaml:"consolidation_interval,omitempty"`
	ConsolidationBatchSize int     `mapstructure:"consolidation_batch_size" yaml:"consolidation_batch_size,omitempty"`
	AutoMergeConfidence    float64 `mapstructure:"auto_merge_confidence" yaml:"auto_merge_confidence,omitempty"`
	// AutoSupersedeConfidence is reserved for a future deterministic
	// supersede apply gate. It is parsed for forward compatibility but is not
	// applied by the current shadow/merge-only/full implementation.
	AutoSupersedeConfidence float64 `mapstructure:"auto_supersede_confidence" yaml:"auto_supersede_confidence,omitempty"`
	// AutoReinforceConfidence gates applying REINFORCE (fold repeats onto one
	// member's verbatim text); AutoArchiveConfidence gates applying ARCHIVE
	// (reversible). Both default to 0.90 (docs/memory-governance.zh-CN.md §4.2).
	AutoReinforceConfidence float64 `mapstructure:"auto_reinforce_confidence" yaml:"auto_reinforce_confidence,omitempty"`
	AutoArchiveConfidence   float64 `mapstructure:"auto_archive_confidence" yaml:"auto_archive_confidence,omitempty"`
	MaxActiveGlobal         int     `mapstructure:"max_active_global" yaml:"max_active_global,omitempty"`
	MaxActivePerWorkspace   int     `mapstructure:"max_active_per_workspace" yaml:"max_active_per_workspace,omitempty"`
	ArchiveAfter            string  `mapstructure:"archive_after" yaml:"archive_after,omitempty"`
	PauseWhileRunActive     *bool   `mapstructure:"pause_while_run_active" yaml:"pause_while_run_active,omitempty"`
}

type TaskConfig struct {
	InboxEnabled                 bool     `mapstructure:"inbox_enabled" yaml:"inbox_enabled,omitempty"`
	DefaultListLimit             int      `mapstructure:"default_list_limit" yaml:"default_list_limit,omitempty"`
	AutoArchiveDoneAfter         string   `mapstructure:"auto_archive_done_after" yaml:"auto_archive_done_after,omitempty"`
	AutoArchiveCancelledAfter    string   `mapstructure:"auto_archive_cancelled_after" yaml:"auto_archive_cancelled_after,omitempty"`
	MaintenanceModelRole         string   `mapstructure:"maintenance_model_role" yaml:"maintenance_model_role,omitempty"`
	MaintenanceFallbackRoles     []string `mapstructure:"maintenance_fallback_roles" yaml:"maintenance_fallback_roles,omitempty"`
	MaintenanceDebounce          string   `mapstructure:"maintenance_debounce" yaml:"maintenance_debounce,omitempty"`
	MaintenanceMaxWait           string   `mapstructure:"maintenance_max_wait" yaml:"maintenance_max_wait,omitempty"`
	MaintenanceBatchMaxRuns      int      `mapstructure:"maintenance_batch_max_runs" yaml:"maintenance_batch_max_runs,omitempty"`
	MaintenanceQuotaProbeInitial string   `mapstructure:"maintenance_quota_probe_initial" yaml:"maintenance_quota_probe_initial,omitempty"`
	MaintenanceQuotaProbeMax     string   `mapstructure:"maintenance_quota_probe_max" yaml:"maintenance_quota_probe_max,omitempty"`
	// MaintenanceLLMTimeout bounds one post-run analyzer provider call.
	// Cheap-role providers can be slow on real batches; a too-tight bound
	// converts every job into deadline-exceeded retries and skipped learning.
	MaintenanceLLMTimeout string `mapstructure:"maintenance_llm_timeout" yaml:"maintenance_llm_timeout,omitempty"`
}

const (
	DefaultTaskListLimit             = 10
	DefaultTaskAutoArchiveDone       = 30 * 24 * time.Hour
	DefaultTaskAutoArchiveCancelled  = 7 * 24 * time.Hour
	DefaultTaskMaintenanceDebounce   = 5 * time.Minute
	DefaultTaskMaintenanceMaxWait    = 15 * time.Minute
	DefaultTaskMaintenanceBatchMax   = 10
	DefaultTaskQuotaProbeInitial     = 15 * time.Minute
	DefaultTaskQuotaProbeMax         = 4 * time.Hour
	DefaultTaskMaintenanceLLMTimeout = 2 * time.Minute
)

func (t TaskConfig) ListLimit() int {
	if t.DefaultListLimit <= 0 {
		return DefaultTaskListLimit
	}
	if t.DefaultListLimit > 50 {
		return 50
	}
	return t.DefaultListLimit
}

func (t TaskConfig) AutoArchiveDurations() (time.Duration, time.Duration) {
	return parseTaskDuration(t.AutoArchiveDoneAfter, DefaultTaskAutoArchiveDone),
		parseTaskDuration(t.AutoArchiveCancelledAfter, DefaultTaskAutoArchiveCancelled)
}

func (t TaskConfig) MaintenanceBatchPolicy() (time.Duration, time.Duration, int) {
	debounce := parseTaskDuration(t.MaintenanceDebounce, DefaultTaskMaintenanceDebounce)
	maxWait := parseTaskDuration(t.MaintenanceMaxWait, DefaultTaskMaintenanceMaxWait)
	if maxWait > 0 && debounce > maxWait {
		maxWait = debounce
	}
	maxRuns := t.MaintenanceBatchMaxRuns
	if maxRuns <= 0 {
		maxRuns = DefaultTaskMaintenanceBatchMax
	}
	if maxRuns > 50 {
		maxRuns = 50
	}
	return debounce, maxWait, maxRuns
}

// MaintenanceLLMCallTimeout returns the per-call analyzer bound.
func (t TaskConfig) MaintenanceLLMCallTimeout() time.Duration {
	timeout := parseTaskDuration(t.MaintenanceLLMTimeout, DefaultTaskMaintenanceLLMTimeout)
	if timeout <= 0 {
		timeout = DefaultTaskMaintenanceLLMTimeout
	}
	return timeout
}

func (t TaskConfig) MaintenanceQuotaCircuitPolicy() (time.Duration, time.Duration) {
	initial := parseTaskDuration(t.MaintenanceQuotaProbeInitial, DefaultTaskQuotaProbeInitial)
	maximum := parseTaskDuration(t.MaintenanceQuotaProbeMax, DefaultTaskQuotaProbeMax)
	if initial <= 0 {
		initial = DefaultTaskQuotaProbeInitial
	}
	if maximum < initial {
		maximum = initial
	}
	return initial, maximum
}

func parseTaskDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	if d <= 0 {
		return 0
	}
	return d
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

	// LLM transport resilience (Package Zero). Absent/0 values fall back to
	// built-in defaults; base/cap/idle accept Go durations ("300ms", "30s").
	// LLMMaxRetries is the streaming/non-streaming call attempt count (default
	// 5); LLMRetryBase/LLMRetryCap bound the exponential backoff+jitter between
	// attempts; LLMStreamIdleTimeout bounds how long an SSE stream may stall
	// without new data before it aborts with a retryable error.
	LLMMaxRetries        int    `mapstructure:"llm_max_retries" yaml:"llm_max_retries,omitempty"`
	LLMRetryBase         string `mapstructure:"llm_retry_base" yaml:"llm_retry_base,omitempty"`
	LLMRetryCap          string `mapstructure:"llm_retry_cap" yaml:"llm_retry_cap,omitempty"`
	LLMStreamIdleTimeout string `mapstructure:"llm_stream_idle_timeout" yaml:"llm_stream_idle_timeout,omitempty"`
}

type StorageConfig struct {
	Type    string `mapstructure:"type" yaml:"type,omitempty"`
	DataDir string `mapstructure:"data_dir" yaml:"data_dir,omitempty"`
}

type GatewayConfig struct {
	Addr         string `mapstructure:"addr" yaml:"addr,omitempty"`
	URL          string `mapstructure:"url" yaml:"url,omitempty"`
	Token        string `mapstructure:"token" yaml:"token,omitempty"`
	DrainTimeout string `mapstructure:"drain_timeout" yaml:"drain_timeout,omitempty"`
	// PresenceIdleTimeout bounds how long a TUI without user INPUT keeps
	// claiming presence ("attached"). Presence means "the person is at this
	// terminal", not "a terminal is open": once the last keystroke is older
	// than this, the client's heartbeats stop claiming attachment, so pushes
	// route to the preferred IM again. Duration string; default "5m"; "0"
	// disables idle detection (an open TUI always counts as attached).
	// The decision is CLIENT-side (input age is only known there); the daemon
	// just honors the client's active=0|1 claim on presence-touching requests.
	PresenceIdleTimeout string `mapstructure:"presence_idle_timeout" yaml:"presence_idle_timeout,omitempty"`
	// PendingNotifyAfter bounds how long an unanswered approval/clarify may sit
	// before the escrow sweep re-pushes it to the preferred IM when the CLI has
	// detached and no notification was sent yet (Fix 2). Duration string;
	// default "2m"; "0" disables escrow. The sweep runs every 60s, so effective
	// latency is this value + up to 60s.
	PendingNotifyAfter      string `mapstructure:"pending_notify_after" yaml:"pending_notify_after,omitempty"`
	OutboundWebhookURL      string `mapstructure:"outbound_webhook_url" yaml:"outbound_webhook_url,omitempty"`
	OutboundWebhookToken    string `mapstructure:"outbound_webhook_token" yaml:"outbound_webhook_token,omitempty"`
	TelegramToken           string `mapstructure:"telegram_token" yaml:"telegram_token,omitempty"`
	DeliveryMaxMessageChars int    `mapstructure:"delivery_max_message_chars" yaml:"delivery_max_message_chars,omitempty"`
	DeliveryRetryAttempts   int    `mapstructure:"delivery_retry_attempts" yaml:"delivery_retry_attempts,omitempty"`
	// DeliveryCatchUpMaxAge bounds how old (Go duration, e.g. "4h") a
	// sent_unconfirmed push may be and still be re-pushed by the one-shot
	// catch-up when the peer's next inbound refreshes the platform session.
	// Empty = 4h default. "0" is invalid (use a tiny duration to disable).
	DeliveryCatchUpMaxAge string       `mapstructure:"delivery_catchup_max_age" yaml:"delivery_catchup_max_age,omitempty"`
	Weixin                WeixinConfig `mapstructure:"weixin" yaml:"weixin,omitempty"`
	Wechat                WechatConfig `mapstructure:"wechat" yaml:"wechat,omitempty"`
	Feishu                FeishuConfig `mapstructure:"feishu" yaml:"feishu,omitempty"`
	QQ                    QQConfig     `mapstructure:"qq" yaml:"qq,omitempty"`
}

// DefaultPresenceIdleTimeout is the presence_idle_timeout applied when the
// knob is absent or unparsable: 5 minutes without a keystroke marks the
// terminal idle so pushes resume on IM.
const DefaultPresenceIdleTimeout = 5 * time.Minute

// PresenceIdleTimeoutDuration parses presence_idle_timeout. Empty or invalid
// values fall back to DefaultPresenceIdleTimeout; a zero (or negative)
// duration returns 0, meaning "never idle" (the old always-attached behavior).
func (g GatewayConfig) PresenceIdleTimeoutDuration() time.Duration {
	raw := strings.TrimSpace(g.PresenceIdleTimeout)
	if raw == "" {
		return DefaultPresenceIdleTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		// Bare numbers read as seconds ("300" → 5m) so a unitless yaml value
		// is honored instead of silently ignored.
		if secs, serr := strconv.Atoi(raw); serr == nil {
			d = time.Duration(secs) * time.Second
		} else {
			return DefaultPresenceIdleTimeout
		}
	}
	if d <= 0 {
		return 0
	}
	return d
}

// DefaultPendingNotifyAfter is the gateway.pending_notify_after applied when the
// knob is absent or unparsable: 2 minutes.
const DefaultPendingNotifyAfter = 2 * time.Minute

// PendingNotifyAfterDuration parses gateway.pending_notify_after. Empty or
// invalid values fall back to DefaultPendingNotifyAfter; a zero (or negative)
// duration returns 0, meaning "disable escrow".
func (g GatewayConfig) PendingNotifyAfterDuration() time.Duration {
	raw := strings.TrimSpace(g.PendingNotifyAfter)
	if raw == "" {
		return DefaultPendingNotifyAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if secs, serr := strconv.Atoi(raw); serr == nil {
			d = time.Duration(secs) * time.Second
		} else {
			return DefaultPendingNotifyAfter
		}
	}
	if d <= 0 {
		return 0
	}
	return d
}

// WechatConfig configures a WeChat Official Account (公众号). Inbound passive
// replies only need Token (for signature verification); outbound customer
// service messages require AppID/AppSecret so the sender can fetch an
// access_token and call cgi-bin/message/custom/send.
type WechatConfig struct {
	Enabled   bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	AppID     string `mapstructure:"app_id" yaml:"app_id,omitempty"`
	AppSecret string `mapstructure:"app_secret" yaml:"app_secret,omitempty"`
	Token     string `mapstructure:"token" yaml:"token,omitempty"`
	BaseURL   string `mapstructure:"base_url" yaml:"base_url,omitempty"`
}

// FeishuConfig configures a Feishu/Lark custom app. Inbound events arrive on the
// generic /v1/im/feishu webhook (verification token / encrypt-key signature is
// validated there); outbound replies require AppID/AppSecret so the sender can
// fetch a tenant_access_token and call open-apis/im/v1/messages.
type FeishuConfig struct {
	Enabled           bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	AppID             string `mapstructure:"app_id" yaml:"app_id,omitempty"`
	AppSecret         string `mapstructure:"app_secret" yaml:"app_secret,omitempty"`
	BaseURL           string `mapstructure:"base_url" yaml:"base_url,omitempty"`
	VerificationToken string `mapstructure:"verification_token" yaml:"verification_token,omitempty"`
	EncryptKey        string `mapstructure:"encrypt_key" yaml:"encrypt_key,omitempty"`
}

// QQConfig configures a QQ official bot (QQ频道/群). Inbound events arrive on the
// generic /v1/im/qq webhook; outbound replies require AppID/Secret so the sender
// can fetch an app access token and call the QQ bot message API.
type QQConfig struct {
	Enabled bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	AppID   string `mapstructure:"app_id" yaml:"app_id,omitempty"`
	Secret  string `mapstructure:"secret" yaml:"secret,omitempty"`
	Token   string `mapstructure:"token" yaml:"token,omitempty"`
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
	Sandbox bool   `mapstructure:"sandbox" yaml:"sandbox,omitempty"`
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
	AuthHeader             string `mapstructure:"auth_header" yaml:"auth_header,omitempty"`
	ToolSchema             string `mapstructure:"tool_schema" yaml:"tool_schema,omitempty"`
	SystemMessageMode      string `mapstructure:"system_message_mode" yaml:"system_message_mode,omitempty"`
	ThinkingMode           string `mapstructure:"thinking_mode" yaml:"thinking_mode,omitempty"`
	UserAgent              string `mapstructure:"user_agent" yaml:"user_agent,omitempty"`
	ResponsesStoreFalse    bool   `mapstructure:"responses_store_false" yaml:"responses_store_false,omitempty"`
	ResponsesRequireStream bool   `mapstructure:"responses_require_stream" yaml:"responses_require_stream,omitempty"`
	// PromptCache opts an anthropic-protocol endpoint into cache_control
	// breakpoints (stable system prefix + rolling history breakpoint).
	// Off by default: providers that ignore the field are unaffected, but
	// enabling it should be a deliberate, per-provider decision.
	PromptCache bool `mapstructure:"prompt_cache" yaml:"prompt_cache,omitempty"`
}

type ModelsConfig struct {
	Source string                     `mapstructure:"source" yaml:"source,omitempty"`
	Roles  map[string]ModelRoleConfig `mapstructure:"roles" yaml:"roles,omitempty"`
}

type ModelRoleConfig struct {
	Provider        string                 `mapstructure:"provider" yaml:"provider,omitempty"`
	Model           string                 `mapstructure:"model" yaml:"model,omitempty"`
	BaseURL         string                 `mapstructure:"base_url" yaml:"base_url,omitempty"`
	Protocol        string                 `mapstructure:"protocol" yaml:"protocol,omitempty"`
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
	// MaxDepth bounds delegation nesting. Depth 1 = the top-level agent may
	// delegate once; its sub-agents are leaves that cannot delegate further.
	// This is a hard STRUCTURAL bound (sub-agents beyond the budget never
	// receive the delegate_task tool), not a runtime counter — otherwise a
	// sub-agent handed the full backend can call delegate_task again forever.
	// Default 1 (flat) when unset.
	MaxDepth int `mapstructure:"max_depth" yaml:"max_depth,omitempty"`
	// MaxConcurrent bounds parallel sub-agents in a single batch. Default 5.
	MaxConcurrent int `mapstructure:"max_concurrent" yaml:"max_concurrent,omitempty"`
	// MaxSubtasks bounds how many goals one delegate_task batch may fan out to.
	// Default 16; exceeding it fails the call with a clear error.
	MaxSubtasks int `mapstructure:"max_subtasks" yaml:"max_subtasks,omitempty"`
}

type CronConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// Timezone for cron schedules (e.g. "Asia/Shanghai"). Empty = system local.
	Timezone string `mapstructure:"timezone" yaml:"timezone,omitempty"`
	// Canary is a periodic liveness self-check: it runs a trivial agent turn and
	// alerts the configured channel ONLY on failure (silent on success), so a
	// broken deploy — e.g. an expired provider token — pings you instead of
	// silently wasting your time.
	Canary CanaryConfig `mapstructure:"canary" yaml:"canary,omitempty"`
}

// FlightRecorderConfig controls recording of real turns for `eval capture`
// (turn everyday friction into replayable regression tests). Env vars
// (SELFMIND_FLIGHT_RECORDER / _DIR / _KEEP) override these when set.
type FlightRecorderConfig struct {
	Enabled bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Dir     string `mapstructure:"dir" yaml:"dir,omitempty"`   // default ~/.selfmind/flight
	Keep    int    `mapstructure:"keep" yaml:"keep,omitempty"` // default 20 most-recent turns
}

type CanaryConfig struct {
	Enabled   bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	CronExpr  string `mapstructure:"cron" yaml:"cron,omitempty"`             // default "0 * * * *" (hourly)
	Platform  string `mapstructure:"platform" yaml:"platform,omitempty"`     // e.g. "weixin"
	DeliverTo string `mapstructure:"deliver_to" yaml:"deliver_to,omitempty"` // recipient id for the alert
	Channel   string `mapstructure:"channel" yaml:"channel,omitempty"`
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
	v.SetDefault("agent.llm_max_retries", 5)
	v.SetDefault("agent.llm_retry_base", "300ms")
	v.SetDefault("agent.llm_retry_cap", "30s")
	v.SetDefault("agent.llm_stream_idle_timeout", "180s")
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
	v.SetDefault("history.persistence", "save-all")
	v.SetDefault("history.max_bytes", 524288)
	v.SetDefault("history.load_entries", 200)
	v.SetDefault("memory.auto_extract_interval", 5)
	v.SetDefault("memory.auto_extract_min_chars", 80)
	v.SetDefault("memory.semantic_recall", true)
	v.SetDefault("memory.use_memory_fence", true)
	// Governance starts in observation-only mode. It is inert when the
	// explicitly configured maintenance role cannot be resolved and never
	// borrows the main coding model.
	v.SetDefault("memory.governance.enabled", true)
	v.SetDefault("memory.governance.mode", "shadow")
	v.SetDefault("memory.governance.model_role", "memory_extract")
	v.SetDefault("memory.governance.consolidation_interval", "24h")
	v.SetDefault("memory.governance.consolidation_batch_size", 8)
	v.SetDefault("memory.governance.auto_merge_confidence", 0.95)
	v.SetDefault("memory.governance.max_active_global", 120)
	v.SetDefault("memory.governance.max_active_per_workspace", 200)
	v.SetDefault("memory.governance.archive_after", "4320h")
	v.SetDefault("memory.governance.pause_while_run_active", true)
	v.SetDefault("tasks.inbox_enabled", true)
	v.SetDefault("tasks.default_list_limit", DefaultTaskListLimit)
	v.SetDefault("tasks.auto_archive_done_after", "720h")
	v.SetDefault("tasks.auto_archive_cancelled_after", "168h")
	v.SetDefault("tasks.maintenance_model_role", "memory_extract")
	v.SetDefault("tasks.maintenance_fallback_roles", []string{"background_review", "fast_classifier"})
	v.SetDefault("tasks.maintenance_debounce", "5m")
	v.SetDefault("tasks.maintenance_max_wait", "15m")
	v.SetDefault("tasks.maintenance_batch_max_runs", DefaultTaskMaintenanceBatchMax)
	v.SetDefault("tasks.maintenance_quota_probe_initial", "15m")
	v.SetDefault("tasks.maintenance_quota_probe_max", "4h")
	v.SetDefault("tasks.maintenance_llm_timeout", "2m")
	v.SetDefault("evolution.enabled", true)
	v.SetDefault("evolution.nudge_interval", 10)
	v.SetDefault("models.source", "local")
	v.SetDefault("intent.mode", "hybrid")
	v.SetDefault("intent.thresholds.direct", 0.8)
	v.SetDefault("intent.thresholds.ask", 0.55)
	v.SetDefault("gateway.pending_notify_after", "2m")
	v.SetDefault("exec_sandbox.enabled", true)
	v.SetDefault("exec_sandbox.required", false)
	v.SetDefault("exec_sandbox.allow_network", false)
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
	c.Intent.ContinueWindow = expandEnvRef(c.Intent.ContinueWindow)
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
	c.Gateway.PresenceIdleTimeout = expandEnvRef(c.Gateway.PresenceIdleTimeout)
	c.Gateway.PendingNotifyAfter = expandEnvRef(c.Gateway.PendingNotifyAfter)
	c.Gateway.OutboundWebhookURL = expandEnvRef(c.Gateway.OutboundWebhookURL)
	c.Gateway.OutboundWebhookToken = expandEnvRef(c.Gateway.OutboundWebhookToken)
	c.Gateway.TelegramToken = expandEnvRef(c.Gateway.TelegramToken)
	c.Gateway.Weixin = normalizeWeixin(c.Gateway.Weixin)
	c.Web = normalizeWeb(c.Web)
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
		role.Protocol = expandEnvRef(role.Protocol)
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

// normalizeWeb resolves web-search credentials from config, with an env-var
// fallback purely for convenience (config is the source of truth because the
// detached daemon does not inherit shell exports).
func normalizeWeb(w WebConfig) WebConfig {
	w.SearchBackend = strings.ToLower(strings.TrimSpace(expandEnvRef(w.SearchBackend)))
	w.APIKey = firstNonEmpty(expandEnvRef(w.APIKey), os.Getenv("SELFMIND_WEB_SEARCH_API_KEY"))
	return w
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
