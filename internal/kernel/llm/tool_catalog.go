package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxProviderToolNameBytes       = 64
	ProviderToolCatalogBudgetBytes = 48 * 1024
)

type ToolCatalogIssue struct {
	Index      int    `json:"index"`
	SourceName string `json:"source_name,omitempty"`
	Name       string `json:"name,omitempty"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// ToolCatalogEntry preserves the registry identity beside the exact provider
// wire name. Provider adapters may normalize or alias names, so diagnostics
// must not try to reconstruct source identity from the serialized spelling.
type ToolCatalogEntry struct {
	Index      int    `json:"index"`
	SourceName string `json:"source_name"`
	WireName   string `json:"wire_name"`
}

// ToolCatalogPreview is the redacted, provider-wire view shared by request
// preflight, gateway status, and doctor. Schemas are hashed/countable but never
// retained in the diagnostic result.
type ToolCatalogPreview struct {
	Protocol    string             `json:"protocol"`
	Count       int                `json:"count"`
	WireBytes   int                `json:"wire_bytes"`
	BudgetBytes int                `json:"budget_bytes"`
	OverBudget  bool               `json:"over_budget"`
	Hash        string             `json:"hash,omitempty"`
	Names       []string           `json:"names,omitempty"`
	Entries     []ToolCatalogEntry `json:"entries,omitempty"`
	Issues      []ToolCatalogIssue `json:"issues,omitempty"`
}

func (p ToolCatalogPreview) Valid() bool { return len(p.Issues) == 0 }

func (p ToolCatalogPreview) WithinBudget() bool { return !p.OverBudget }

type toolCatalogPreviewer interface {
	PreviewToolCatalog(context.Context, []ToolDefinition) ToolCatalogPreview
}

// PreviewProviderToolCatalog asks the actual adapter to render the same tool
// wire shape used by Chat/StreamChat. Transparent wrappers are traversed; a
// provider without the optional capability gets the conservative common
// function-name contract instead of an invented protocol claim.
func PreviewProviderToolCatalog(ctx context.Context, provider Provider, tools []ToolDefinition) ToolCatalogPreview {
	if ctx == nil {
		ctx = context.Background()
	}
	if provider != nil {
		if previewer, ok := provider.(toolCatalogPreviewer); ok {
			return previewer.PreviewToolCatalog(ctx, tools)
		}
		if inner, ok := unwrapProvider(provider); ok {
			return PreviewProviderToolCatalog(ctx, inner, tools)
		}
	}
	return buildToolCatalogPreview("native_unknown", tools, tools, tools)
}

// EnsureProviderToolCatalog prevents a deterministic provider 400 from being
// sent over the network. Doctor consumes the same preview, so diagnostics and
// transport cannot drift into separate implementations of the wire contract.
func EnsureProviderToolCatalog(ctx context.Context, provider Provider, tools []ToolDefinition) error {
	preview := PreviewProviderToolCatalog(ctx, provider, tools)
	if preview.Valid() {
		return nil
	}
	return ToolCatalogError{Preview: preview}
}

type ToolCatalogError struct{ Preview ToolCatalogPreview }

func (e ToolCatalogError) Error() string {
	if len(e.Preview.Issues) == 0 {
		return "provider tool catalog is invalid"
	}
	issue := e.Preview.Issues[0]
	return fmt.Sprintf("provider tool catalog is invalid for %s: tools[%d] name %q: %s",
		emptyCatalogProtocol(e.Preview.Protocol), issue.Index, issue.Name, issue.Message)
}

func emptyCatalogProtocol(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown protocol"
	}
	return value
}

func buildToolCatalogPreview(protocol string, source, effective []ToolDefinition, wire interface{}) ToolCatalogPreview {
	preview := ToolCatalogPreview{
		Protocol: protocol, Count: len(effective), BudgetBytes: ProviderToolCatalogBudgetBytes,
		Names: make([]string, 0, len(effective)), Entries: make([]ToolCatalogEntry, 0, len(effective)),
	}
	seen := make(map[string]int, len(effective))
	for index, tool := range effective {
		name := strings.TrimSpace(tool.Name)
		sourceName := name
		if index < len(source) {
			sourceName = strings.TrimSpace(source[index].Name)
		}
		preview.Names = append(preview.Names, name)
		preview.Entries = append(preview.Entries, ToolCatalogEntry{Index: index, SourceName: sourceName, WireName: name})
		switch {
		case name == "":
			preview.Issues = append(preview.Issues, ToolCatalogIssue{Index: index, SourceName: sourceName, Name: name, Code: "empty_name", Message: "function name is empty"})
		case len(name) > maxProviderToolNameBytes:
			preview.Issues = append(preview.Issues, ToolCatalogIssue{Index: index, SourceName: sourceName, Name: name, Code: "name_too_long", Message: "function name exceeds 64 bytes"})
		case !providerToolNameValid(name):
			preview.Issues = append(preview.Issues, ToolCatalogIssue{Index: index, SourceName: sourceName, Name: name, Code: "invalid_name", Message: "expected ^[a-zA-Z0-9_-]+$"})
		}
		if prior, ok := seen[name]; ok && name != "" {
			preview.Issues = append(preview.Issues, ToolCatalogIssue{Index: index, SourceName: sourceName, Name: name, Code: "duplicate_name", Message: fmt.Sprintf("duplicates tools[%d]", prior)})
		} else {
			seen[name] = index
		}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		preview.Issues = append(preview.Issues, ToolCatalogIssue{Index: -1, Code: "wire_marshal", Message: err.Error()})
		return preview
	}
	preview.WireBytes = len(raw)
	preview.OverBudget = preview.WireBytes > preview.BudgetBytes
	digest := sha256.Sum256(raw)
	preview.Hash = hex.EncodeToString(digest[:8])
	return preview
}

func providerToolNameValid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !isResponsesToolNameRune(r) {
			return false
		}
	}
	return true
}

func previewOpenAIToolCatalog(quirks ProviderQuirks, tools []ToolDefinition) ToolCatalogPreview {
	normalized := normalizeToolDefinitions(tools)
	if strings.EqualFold(strings.TrimSpace(quirks.ToolSchema), "moonshot") {
		normalized = sanitizeMoonshotToolDefinitions(normalized)
	}
	return buildToolCatalogPreview("openai_chat", tools, normalized, openAIToolDefinitions(normalized))
}

func previewAnthropicToolCatalog(quirks ProviderQuirks, tools []ToolDefinition) ToolCatalogPreview {
	normalized := normalizeToolDefinitions(tools)
	if strings.EqualFold(strings.TrimSpace(quirks.ToolSchema), "moonshot") {
		normalized = sanitizeMoonshotToolDefinitions(normalized)
	}
	return buildToolCatalogPreview("anthropic_messages", tools, normalized, anthropicTools(normalized))
}
