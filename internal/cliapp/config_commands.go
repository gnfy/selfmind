package cliapp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	uicommon "selfmind/internal/ui/common"

	"github.com/charmbracelet/lipgloss"
	"go.yaml.in/yaml/v3"
)

type configMigration struct {
	From       []string
	To         []string
	Label      string
	Deprecated bool
}

type configUpgradeReport struct {
	ConfigPath string
	Missing    []string
	Legacy     []string
	Deprecated []string
}

type configDiagnostics struct {
	Path       string
	Missing    bool
	ReadError  string
	ParseError string
	LoadError  string
	Upgrade    configUpgradeReport
	ModelLine  string
	ModelError string
}

var configMigrations = []configMigration{
	{From: []string{"providers", "openai_api_key"}, To: []string{"providers", "openai", "api_key"}, Label: "providers.openai_api_key -> providers.openai.api_key"},
	{From: []string{"providers", "anthropic_api_key"}, To: []string{"providers", "anthropic", "api_key"}, Label: "providers.anthropic_api_key -> providers.anthropic.api_key"},
	{From: []string{"providers", "gemini_api_key"}, To: []string{"providers", "google", "api_key"}, Label: "providers.gemini_api_key -> providers.google.api_key"},
	{From: []string{"providers", "openrouter_api_key"}, To: []string{"provider_profiles", "openrouter", "api_key"}, Label: "providers.openrouter_api_key -> provider_profiles.openrouter.api_key"},
	{From: []string{"providers", "minimax_api_key"}, To: []string{"provider_profiles", "minimax", "api_key"}, Label: "providers.minimax_api_key -> provider_profiles.minimax.api_key"},
	{From: []string{"agent", "provider"}, To: []string{"model", "provider"}, Label: "agent.provider -> model.provider"},
	{From: []string{"agent", "model"}, To: []string{"model", "default"}, Label: "agent.model -> model.default"},
	{From: []string{"intent", "continue_window"}, Label: "intent.continue_window is deprecated", Deprecated: true},
}

var configReportDefaultPaths = [][]string{
	{"agent", "max_iterations"},
	{"agent", "max_retries"},
	{"agent", "llm_max_retries"},
	{"agent", "llm_retry_base"},
	{"agent", "llm_retry_cap"},
	{"agent", "llm_stream_idle_timeout"},
	{"agent", "log_level"},
	{"auth", "credentials_file"},
	{"delegation", "max_iterations"},
	{"editor", "large_paste_chars"},
	{"editor", "large_paste_lines"},
	{"evolution", "enabled"},
	{"evolution", "nudge_interval"},
	{"gateway", "addr"},
	{"gateway", "delivery_max_message_chars"},
	{"gateway", "delivery_retry_attempts"},
	{"gateway", "pending_notify_after"},
	{"gateway", "weixin", "base_url"},
	{"gateway", "weixin", "cdn_base_url"},
	{"gateway", "weixin", "dm_policy"},
	{"gateway", "weixin", "group_policy"},
	{"gateway", "weixin", "send_chunk_delay_seconds"},
	{"gateway", "weixin", "send_chunk_retries"},
	{"intent", "mode"},
	{"intent", "thresholds", "direct"},
	{"intent", "thresholds", "ask"},
	{"memory", "auto_extract_interval"},
	{"memory", "auto_extract_min_chars"},
	{"memory", "semantic_recall"},
	{"memory", "use_memory_fence"},
	{"models", "source"},
	{"storage", "type"},
	{"storage", "data_dir"},
}

func (a *App) runConfigCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "config" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind config [doctor|upgrade]")
		return true, 2
	}
	switch args[0] {
	case "doctor":
		return true, a.runConfigDoctor()
	case "upgrade":
		return true, a.runConfigUpgrade()
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind config [doctor|upgrade]")
		return true, 2
	}
}

func (a *App) runConfigDoctor() int {
	cfg, original, originalDoc, err := a.loadConfigCommandInputs()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	canonicalDoc, err := canonicalConfigYAML(cfg)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	report := inspectConfigUpgrade(cfg.Path, originalDoc, canonicalDoc)
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
	fmt.Fprintf(a.stdout, "Size: %d bytes\n", len(original))
	if report.hasChanges() {
		fmt.Fprintln(a.stdout, "Status: upgrade available")
	} else {
		fmt.Fprintln(a.stdout, "Status: up to date")
	}
	printConfigReportList(a.stdout, "Missing defaults", report.Missing)
	printConfigReportList(a.stdout, "Migratable legacy keys", report.Legacy)
	printConfigReportList(a.stdout, "Deprecated keys", report.Deprecated)
	if report.hasChanges() {
		fmt.Fprintln(a.stdout)
		fmt.Fprintln(a.stdout, "Run: selfmind config upgrade")
		fmt.Fprintln(a.stdout, "Upgrade creates a timestamped .bak file before writing.")
	}
	return 0
}

func (a *App) runConfigUpgrade() int {
	cfg, original, originalDoc, err := a.loadConfigCommandInputs()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	canonicalDoc, err := canonicalConfigYAML(cfg)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	report := inspectConfigUpgrade(cfg.Path, originalDoc, canonicalDoc)
	if !report.hasChanges() {
		fmt.Fprintf(a.stdout, "Config already up to date: %s\n", cfg.Path)
		return 0
	}

	upgraded := cloneYAMLNode(originalDoc)
	addMissingConfigDefaults(upgraded, canonicalDoc)
	applyConfigMigrations(upgraded)
	out, err := encodeConfigYAML(upgraded)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(out)) {
		fmt.Fprintf(a.stdout, "Config already up to date: %s\n", cfg.Path)
		return 0
	}

	backup, err := backupConfigFile(cfg.Path, original)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if err := writeConfigBytesAtomic(cfg.Path, out); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Config upgraded: %s\n", cfg.Path)
	fmt.Fprintf(a.stdout, "Backup: %s\n", backup)
	printConfigReportList(a.stdout, "Added defaults", report.Missing)
	printConfigReportList(a.stdout, "Migrated legacy keys", report.Legacy)
	printConfigReportList(a.stdout, "Removed deprecated keys", report.Deprecated)
	return 0
}

func (a *App) collectConfigDiagnostics() configDiagnostics {
	path, _ := config.ResolveConfigPath(a.configPath)
	diag := configDiagnostics{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			diag.Missing = true
		} else {
			diag.ReadError = err.Error()
		}
		return diag
	}
	doc, err := parseConfigYAML(raw)
	if err != nil {
		diag.ParseError = err.Error()
		return diag
	}
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		diag.LoadError = err.Error()
		return diag
	}
	canonicalDoc, err := canonicalConfigYAML(cfg)
	if err != nil {
		diag.LoadError = err.Error()
		return diag
	}
	diag.Upgrade = inspectConfigUpgrade(path, doc, canonicalDoc)

	rt, err := modelruntime.NewResolver(cfg).Resolve(a.ctx, modelruntime.Selection{})
	if err != nil {
		diag.ModelError = err.Error()
		return diag
	}
	credential := strings.TrimSpace(rt.CredentialSource)
	if credential == "" {
		credential = "none"
	}
	diag.ModelLine = fmt.Sprintf("provider=%s model=%s protocol=%s credential=%s", blankAsDash(rt.Provider), blankAsDash(rt.Model), blankAsDash(rt.Protocol), credential)
	return diag
}

func (d configDiagnostics) section() string {
	var sb strings.Builder
	sb.WriteString("== Config ==\n")
	fmt.Fprintf(&sb, "path: %s\n", d.Path)
	switch {
	case d.Missing:
		sb.WriteString("status: missing (SelfMind will create a default config on normal launch)\n")
	case d.ReadError != "":
		fmt.Fprintf(&sb, "status: unreadable (%s)\n", oneLine(d.ReadError, 160))
	case d.ParseError != "":
		fmt.Fprintf(&sb, "status: invalid YAML (%s)\n", oneLine(d.ParseError, 160))
	case d.LoadError != "":
		fmt.Fprintf(&sb, "status: load error (%s)\n", oneLine(d.LoadError, 160))
	default:
		if d.Upgrade.hasChanges() {
			fmt.Fprintf(&sb, "status: upgrade available (%d missing defaults, %d legacy, %d deprecated)\n", len(d.Upgrade.Missing), len(d.Upgrade.Legacy), len(d.Upgrade.Deprecated))
			sb.WriteString("upgrade: selfmind config upgrade\n")
		} else {
			sb.WriteString("status: ok\n")
		}
		if d.ModelError != "" {
			fmt.Fprintf(&sb, "model: not ready (%s)\n", oneLine(d.ModelError, 160))
			sb.WriteString("model_check: selfmind model check\n")
		} else if d.ModelLine != "" {
			fmt.Fprintf(&sb, "model: %s\n", d.ModelLine)
		}
	}
	if len(d.Upgrade.Legacy) > 0 {
		sb.WriteString("legacy keys:\n")
		for _, item := range d.Upgrade.Legacy {
			fmt.Fprintf(&sb, "  - %s\n", item)
		}
	}
	if len(d.Upgrade.Deprecated) > 0 {
		sb.WriteString("deprecated keys:\n")
		for _, item := range d.Upgrade.Deprecated {
			fmt.Fprintf(&sb, "  - %s\n", item)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (d configDiagnostics) startupWarnings() []string {
	var warnings []string
	switch {
	case d.ReadError != "":
		warnings = append(warnings, "config file is unreadable")
	case d.ParseError != "":
		warnings = append(warnings, "config file has invalid YAML")
	case d.LoadError != "":
		warnings = append(warnings, "config file could not be loaded")
	}
	if d.Upgrade.hasChanges() {
		warnings = append(warnings, "config can be upgraded")
	}
	if d.ModelError != "" {
		warnings = append(warnings, "AI model is not ready")
	}
	return warnings
}

func (a *App) printStartupHealthWarnings() {
	warnings := a.collectConfigDiagnostics().startupWarnings()
	if len(warnings) == 0 {
		return
	}
	warningStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(uicommon.PaletteAmber))
	for _, warning := range warnings {
		fmt.Fprintln(a.stderr, warningStyle.Render(fmt.Sprintf("\u26a0 SelfMind notice: %s. Run `selfmind doctor` for details.", warning)))
	}
}

func (a *App) loadConfigCommandInputs() (*config.Config, []byte, *yaml.Node, error) {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		return nil, nil, nil, err
	}
	doc, err := parseConfigYAML(raw)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse config %s: %w", cfg.Path, err)
	}
	return cfg, raw, doc, nil
}

func inspectConfigUpgrade(path string, originalDoc, canonicalDoc *yaml.Node) configUpgradeReport {
	report := configUpgradeReport{ConfigPath: path}
	originalRoot := yamlDocumentRoot(originalDoc)
	canonicalRoot := yamlDocumentRoot(canonicalDoc)
	for _, p := range configReportDefaultPaths {
		if yamlPathExists(canonicalRoot, p) && !yamlPathExists(originalRoot, p) {
			report.Missing = append(report.Missing, strings.Join(p, "."))
		}
	}
	for _, m := range configMigrations {
		if !yamlPathExists(originalRoot, m.From) {
			continue
		}
		if m.Deprecated {
			report.Deprecated = append(report.Deprecated, m.Label)
		} else {
			report.Legacy = append(report.Legacy, m.Label)
		}
	}
	report.Missing = append(report.Missing, missingKimiCheapRoleDefaults(originalRoot)...)
	sort.Strings(report.Missing)
	sort.Strings(report.Legacy)
	sort.Strings(report.Deprecated)
	return report
}

func (r configUpgradeReport) hasChanges() bool {
	return len(r.Missing) > 0 || len(r.Legacy) > 0 || len(r.Deprecated) > 0
}

func printConfigReportList(w interface{ Write([]byte) (int, error) }, title string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(w, "%s: none\n", title)
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, item := range items {
		fmt.Fprintf(w, "  - %s\n", item)
	}
}

func canonicalConfigYAML(cfg *config.Config) (*yaml.Node, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return parseConfigYAML(data)
}

func parseConfigYAML(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if strings.TrimSpace(string(data)) == "" {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		return &doc, nil
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0] == nil {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top-level config must be a YAML mapping")
	}
	return &doc, nil
}

func encodeConfigYAML(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlDocumentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func cloneYAMLNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	cp := *n
	cp.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		cp.Content[i] = cloneYAMLNode(child)
	}
	return &cp
}

func yamlPathExists(root *yaml.Node, path []string) bool {
	return yamlPathNode(root, path) != nil
}

func yamlPathNode(root *yaml.Node, path []string) *yaml.Node {
	node := root
	for _, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		node = yamlMappingValue(node, key)
	}
	return node
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func yamlSetMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping.Kind == 0 {
		mapping.Kind = yaml.MappingNode
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func yamlDeleteMappingValue(mapping *yaml.Node, key string) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return true
		}
	}
	return false
}

func yamlEnsureMappingPath(root *yaml.Node, path []string) *yaml.Node {
	node := root
	for _, key := range path {
		next := yamlMappingValue(node, key)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode}
			yamlSetMappingValue(node, key, next)
		}
		node = next
	}
	return node
}

func addMissingConfigDefaults(targetDoc, canonicalDoc *yaml.Node) {
	root := yamlDocumentRoot(targetDoc)
	addMissingYAMLMapping(root, yamlDocumentRoot(canonicalDoc), nil)
	addMissingKimiCheapRoleDefaults(root)
}

var kimiCheapRoleDefaults = []string{
	"fast_classifier",
	"memory_extract",
	"background_review",
	"skill_curator",
	"semantic_recall",
}

func missingKimiCheapRoleDefaults(root *yaml.Node) []string {
	if !hasKimiCodingConfig(root) {
		return nil
	}
	var missing []string
	for _, role := range kimiCheapRoleDefaults {
		if !yamlPathExists(root, []string{"models", "roles", role}) {
			missing = append(missing, "models.roles."+role)
		}
	}
	return missing
}

func addMissingKimiCheapRoleDefaults(root *yaml.Node) {
	if !hasKimiCodingConfig(root) {
		return
	}
	roles := yamlEnsureMappingPath(root, []string{"models", "roles"})
	for _, role := range kimiCheapRoleDefaults {
		if yamlMappingValue(roles, role) != nil {
			continue
		}
		roleConfig := &yaml.Node{Kind: yaml.MappingNode}
		yamlSetMappingValue(roleConfig, "provider", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "kimi-coding"})
		yamlSetMappingValue(roleConfig, "model", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "kimi-for-coding"})
		yamlSetMappingValue(roles, role, roleConfig)
	}
}

func hasKimiCodingConfig(root *yaml.Node) bool {
	if root == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(yamlScalarString(yamlPathNode(root, []string{"model", "provider"}))), "kimi-coding") {
		return true
	}
	return yamlPathExists(root, []string{"provider_profiles", "kimi-coding"})
}

func yamlScalarString(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func addMissingYAMLMapping(target, canonical *yaml.Node, path []string) {
	if target == nil || canonical == nil || target.Kind != yaml.MappingNode || canonical.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(canonical.Content); i += 2 {
		key := canonical.Content[i].Value
		nextPath := append(append([]string{}, path...), key)
		if isSensitiveConfigPath(nextPath) {
			continue
		}
		canonVal := canonical.Content[i+1]
		targetVal := yamlMappingValue(target, key)
		if targetVal == nil {
			if literal, ok := configLiteralDefault(nextPath); ok {
				yamlSetMappingValue(target, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: literal})
				continue
			}
			if canonVal.Kind == yaml.MappingNode {
				targetVal = &yaml.Node{Kind: yaml.MappingNode}
				yamlSetMappingValue(target, key, targetVal)
				addMissingYAMLMapping(targetVal, canonVal, nextPath)
				continue
			}
			yamlSetMappingValue(target, key, cloneYAMLNode(canonVal))
			continue
		}
		if targetVal.Kind == yaml.MappingNode && canonVal.Kind == yaml.MappingNode {
			addMissingYAMLMapping(targetVal, canonVal, nextPath)
		}
	}
}

func configLiteralDefault(path []string) (string, bool) {
	joined := strings.Join(path, ".")
	switch joined {
	case "auth.credentials_file":
		return "~/.selfmind/auth.json", true
	default:
		return "", false
	}
}

func isSensitiveConfigPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	key := strings.ToLower(path[len(path)-1])
	switch {
	case key == "api_key", key == "token", key == "app_secret", key == "secret", key == "outbound_webhook_token", key == "telegram_token", key == "encrypt_key", key == "verification_token":
		return true
	case strings.Contains(key, "secret"), strings.Contains(key, "token"):
		return true
	default:
		return false
	}
}

func applyConfigMigrations(doc *yaml.Node) {
	root := yamlDocumentRoot(doc)
	for _, m := range configMigrations {
		if m.Deprecated {
			yamlDeletePath(root, m.From)
			continue
		}
		moveYAMLPath(root, m.From, m.To)
	}
	pruneEmptyMapping(root, []string{"agent"})
}

func moveYAMLPath(root *yaml.Node, from, to []string) {
	value := yamlPathNode(root, from)
	if value == nil {
		return
	}
	if !yamlPathExists(root, to) {
		parent := yamlEnsureMappingPath(root, to[:len(to)-1])
		yamlSetMappingValue(parent, to[len(to)-1], cloneYAMLNode(value))
	}
	yamlDeletePath(root, from)
}

func yamlDeletePath(root *yaml.Node, path []string) bool {
	if len(path) == 0 {
		return false
	}
	parent := yamlPathNode(root, path[:len(path)-1])
	if parent == nil {
		return false
	}
	return yamlDeleteMappingValue(parent, path[len(path)-1])
}

func pruneEmptyMapping(root *yaml.Node, path []string) {
	node := yamlPathNode(root, path)
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content) > 0 {
		return
	}
	yamlDeletePath(root, path)
}

func backupConfigFile(path string, data []byte) (string, error) {
	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, data, 0600); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return backup, nil
}

func writeConfigBytesAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
