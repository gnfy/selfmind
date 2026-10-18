package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
)

// RecordingProvenance is advisory evidence identity. Legacy recordings remain
// readable with unknown provenance; hashes never confer execution authority.
type RecordingProvenance struct {
	RecordedAt string `json:"recorded_at"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Role       string `json:"role,omitempty"`
	Build      string `json:"build"`
	SchemaHash string `json:"schema_hash,omitempty"`
	PrefixHash string `json:"prefix_hash,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

func (v *vcrProvider) recordingProvenance(ctx context.Context, req ChatRequest, stream bool) *RecordingProvenance {
	route := DescribeProviderRoute(v.inner)
	if req.Model != "" {
		route.Model = req.Model
	}
	p := &RecordingProvenance{RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Provider: route.Provider, Model: route.Model, Role: string(ModelContextFrom(ctx).Role), Build: buildinfo.Fingerprint()}
	if len(req.Tools) > 0 {
		data, _ := json.Marshal(req.Tools)
		digest := sha256.Sum256(data)
		p.SchemaHash = hex.EncodeToString(digest[:])
	}
	if fp, ok := FingerprintProviderRequest(ctx, v.inner, req, stream); ok {
		p.PrefixHash, p.Protocol = fp.PrefixHash, fp.Protocol
	}
	return p
}

func (v *vcrProvider) saveRequest(ctx context.Context, path string, c cassette, req ChatRequest) {
	c.RecordedWith = v.recordingProvenance(ctx, req, c.Method == "stream")
	v.save(ctx, path, c, req.Messages)
}

// CassetteRecordingSources reports recording provenance, independently of the
// route configured for playback. Missing legacy metadata is explicitly unknown.
func CassetteRecordingSources(session string) string {
	dir := filepath.Join(vcrDir(), sanitizeVCR(session))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "unknown"
	}
	sources := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json")); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		var c cassette
		if err != nil || json.Unmarshal(data, &c) != nil || c.RecordedWith == nil || c.RecordedWith.Model == "" {
			sources["unknown"] = true
			continue
		}
		p := c.RecordedWith
		sources[p.Role+":"+p.Provider+"/"+p.Model] = true
	}
	if len(sources) == 0 {
		return "unknown"
	}
	var result []string
	for source := range sources {
		result = append(result, source)
	}
	sort.Strings(result)
	return strings.Join(result, ", ")
}
