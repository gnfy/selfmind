package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

// SemanticExpander uses the LLM to expand a user query into synonyms and related concepts,
// enabling keyword-based FTS5 to match semantically similar but lexically different terms.
type SemanticExpander struct {
	provider llm.Provider
	enabled  bool

	// simple cache to avoid repeated expansions for the same query within a short window
	cache     map[string]cacheEntry
	cacheTTL  time.Duration
}

type cacheEntry struct {
	result    string
	cachedAt  time.Time
}

// NewSemanticExpander creates an expander. If provider is nil, expansion is disabled.
func NewSemanticExpander(provider llm.Provider, enabled bool) *SemanticExpander {
	return &SemanticExpander{
		provider: provider,
		enabled:  enabled && provider != nil,
		cache:    make(map[string]cacheEntry),
		cacheTTL: 5 * time.Minute,
	}
}

const expandPrompt = `You are a search assistant. Expand the following user query into 3-5 synonyms or closely related technical terms that might appear in past conversations. Output ONLY the expanded terms separated by spaces. Do not add explanations.

Query: %s

Expanded terms:`

// Expand takes a user query and returns an expanded query string suitable for FTS5.
// If expansion fails or is disabled, the original query is returned unchanged.
func (se *SemanticExpander) Expand(ctx context.Context, query string) string {
	if !se.enabled || se.provider == nil {
		return query
	}
	if query == "" {
		return query
	}

	// cache check
	if entry, ok := se.cache[query]; ok && time.Since(entry.cachedAt) < se.cacheTTL {
		return entry.result
	}

	prompt := fmt.Sprintf(expandPrompt, query)
	resp, err := se.provider.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return query
	}

	expanded := strings.TrimSpace(resp)
	if expanded == "" || expanded == query {
		return query
	}

	// Merge original query with expanded terms for FTS5 OR search
	// Format: query term2 term3 ...
	result := query + " " + expanded

	// clean up: remove newlines, extra spaces
	result = strings.Join(strings.Fields(result), " ")

	se.cache[query] = cacheEntry{result: result, cachedAt: time.Now()}
	return result
}
