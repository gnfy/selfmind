package doccheck

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	ManifestPath   = "docs/manifest.yaml"
	IndexPath      = "docs/README.md"
	maxAgentsBytes = 20 * 1024
	maxStatusLines = 300
)

var markdownLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

type Manifest struct {
	Version           int                `yaml:"version"`
	Documents         []Document         `yaml:"documents"`
	ExcludedDocuments []ExcludedDocument `yaml:"excluded_documents,omitempty"`
}

type ExcludedDocument struct {
	Path   string `yaml:"path"`
	Reason string `yaml:"reason"`
}

type Document struct {
	Path          string `yaml:"path"`
	Title         string `yaml:"title"`
	Class         string `yaml:"class"`
	Owner         string `yaml:"owner"`
	State         string `yaml:"state"`
	Language      string `yaml:"language"`
	TranslationOf string `yaml:"translation_of,omitempty"`
	SourceHash    string `yaml:"source_hash,omitempty"`
	ApprovedBy    string `yaml:"approved_by,omitempty"`
	ReviewBy      string `yaml:"review_by,omitempty"`
	Verdict       string `yaml:"verdict,omitempty"`
}

type Report struct {
	Documents   int
	ActivePlans int
	Errors      []string
}

func (r Report) OK() bool { return len(r.Errors) == 0 }

func Check(root string, now time.Time) Report {
	report := Report{}
	manifest, err := LoadManifest(root)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	report.Documents = len(manifest.Documents)

	checkSizeLimits(root, &report)
	checkPublicEntrypoints(root, &report)
	checkManifest(root, manifest, now, &report)
	checkMarkdown(root, manifest, &report)

	want := []byte(RenderIndex(manifest))
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(IndexPath)))
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", IndexPath, err))
	} else if string(got) != string(want) {
		report.Errors = append(report.Errors, fmt.Sprintf("%s is stale; run `selfmind docs index`", IndexPath))
	}

	sort.Strings(report.Errors)
	return report
}

func checkPublicEntrypoints(root string, report *Report) {
	contracts := []struct {
		path      string
		maxLines  int
		required  []string
		forbidden []string
	}{
		{
			path:     "README.md",
			maxLines: 300,
			required: []string{"Model Manager", "Main", "Background", "providers.custom", "private auth", "Ctrl+J", "Ctrl+V"},
			forbidden: []string{
				"provider_profiles:",
				"provider_profiles.<id>",
				"custom:ollama",
				"| `Shift+Enter` |",
			},
		},
		{
			path:     "README.zh-CN.md",
			maxLines: 300,
			required: []string{"Model Manager", "Main", "Background", "providers.custom", "私有 auth store", "Ctrl+J", "Ctrl+V"},
			forbidden: []string{
				"provider_profiles:",
				"provider_profiles.<id>",
				"custom:ollama",
				"| `Shift+Enter` |",
			},
		},
		{
			path:     "npm/selfmind/README.md",
			maxLines: 120,
			required: []string{"Manager to configure", "Main", "Background", "Ctrl+J", "Ctrl+V", "@selfmind/cli@next"},
			forbidden: []string{
				"primary model and background model",
				"Use `selfmind@next`",
				"| `Shift+Enter` |",
			},
		},
	}

	for _, contract := range contracts {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(contract.path)))
		if err != nil {
			report.Errors = append(report.Errors, contract.path+": "+err.Error())
			continue
		}
		if !utf8.Valid(data) {
			report.Errors = append(report.Errors, contract.path+" is not valid UTF-8")
			continue
		}
		if lines := lineCount(data); lines > contract.maxLines {
			report.Errors = append(report.Errors, fmt.Sprintf("%s is %d lines; public entrypoint limit is %d", contract.path, lines, contract.maxLines))
		}
		text := string(data)
		for _, phrase := range contract.required {
			if !strings.Contains(text, phrase) {
				report.Errors = append(report.Errors, fmt.Sprintf("%s is missing current public contract %q", contract.path, phrase))
			}
		}
		for _, phrase := range contract.forbidden {
			if strings.Contains(text, phrase) {
				report.Errors = append(report.Errors, fmt.Sprintf("%s contains stale public contract %q", contract.path, phrase))
			}
		}
	}
}

func LoadManifest(root string) (Manifest, error) {
	path := filepath.Join(root, filepath.FromSlash(ManifestPath))
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	if !utf8.Valid(data) {
		return Manifest{}, fmt.Errorf("%s is not valid UTF-8", ManifestPath)
	}
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	if manifest.Version != 1 {
		return Manifest{}, fmt.Errorf("%s: unsupported version %d", ManifestPath, manifest.Version)
	}
	return manifest, nil
}

func WriteIndex(root string) error {
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(IndexPath))
	return os.WriteFile(path, []byte(RenderIndex(manifest)), 0o644)
}

func RenderIndex(manifest Manifest) string {
	docs := append([]Document(nil), manifest.Documents...)
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].Class != docs[j].Class {
			return docs[i].Class < docs[j].Class
		}
		return docs[i].Path < docs[j].Path
	})
	var b strings.Builder
	b.WriteString("# Documentation Index\n\n")
	b.WriteString("Generated from `docs/manifest.yaml` by `selfmind docs index`. Do not edit this file by hand.\n")
	lastClass := ""
	for _, doc := range docs {
		if doc.Class != lastClass {
			lastClass = doc.Class
			b.WriteString("\n## " + classTitle(lastClass) + "\n\n")
		}
		path := strings.TrimPrefix(doc.Path, "docs/")
		meta := doc.State
		if doc.Language != "" {
			meta += ", " + doc.Language
		}
		b.WriteString(fmt.Sprintf("- [%s](%s) - %s\n", doc.Title, path, meta))
	}
	return b.String()
}

func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:8]), nil
}

func checkSizeLimits(root string, report *Report) {
	if data, err := os.ReadFile(filepath.Join(root, "AGENTS.md")); err != nil {
		report.Errors = append(report.Errors, "AGENTS.md: "+err.Error())
	} else {
		if !utf8.Valid(data) {
			report.Errors = append(report.Errors, "AGENTS.md is not valid UTF-8")
		}
		if len(data) > maxAgentsBytes {
			report.Errors = append(report.Errors, fmt.Sprintf("AGENTS.md is %d bytes; limit is %d", len(data), maxAgentsBytes))
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("docs/STATUS.md"))); err != nil {
		report.Errors = append(report.Errors, "docs/STATUS.md: "+err.Error())
	} else if lines := lineCount(data); lines > maxStatusLines {
		report.Errors = append(report.Errors, fmt.Sprintf("docs/STATUS.md is %d lines; limit is %d", lines, maxStatusLines))
	}
}

func checkManifest(root string, manifest Manifest, now time.Time, report *Report) {
	allowedClass := map[string]bool{"archive": true, "contract": true, "decision": true, "guide": true, "plan": true, "reference": true, "status": true}
	allowedState := map[string]bool{"active": true, "archived": true, "current": true, "paused": true}
	seen := make(map[string]bool, len(manifest.Documents))
	excluded := make(map[string]bool, len(manifest.ExcludedDocuments))
	for _, doc := range manifest.ExcludedDocuments {
		path := filepath.ToSlash(filepath.Clean(doc.Path))
		if path != doc.Path || !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
			report.Errors = append(report.Errors, fmt.Sprintf("manifest has invalid excluded document path %q", doc.Path))
			continue
		}
		if excluded[path] {
			report.Errors = append(report.Errors, fmt.Sprintf("manifest excludes %s more than once", path))
		}
		excluded[path] = true
		if strings.TrimSpace(doc.Reason) == "" {
			report.Errors = append(report.Errors, fmt.Sprintf("excluded document %s must declare a reason", path))
		}
	}
	for _, doc := range manifest.Documents {
		path := filepath.ToSlash(filepath.Clean(doc.Path))
		if path != doc.Path || !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
			report.Errors = append(report.Errors, fmt.Sprintf("manifest has invalid document path %q", doc.Path))
			continue
		}
		if seen[path] {
			report.Errors = append(report.Errors, fmt.Sprintf("manifest lists %s more than once", path))
		}
		seen[path] = true
		if excluded[path] {
			report.Errors = append(report.Errors, fmt.Sprintf("%s cannot be both public and excluded", path))
		}
		if !allowedClass[doc.Class] {
			report.Errors = append(report.Errors, fmt.Sprintf("%s has invalid class %q", path, doc.Class))
		}
		if !allowedState[doc.State] {
			report.Errors = append(report.Errors, fmt.Sprintf("%s has invalid state %q", path, doc.State))
		}
		if strings.TrimSpace(doc.Title) == "" || strings.TrimSpace(doc.Owner) == "" {
			report.Errors = append(report.Errors, fmt.Sprintf("%s must declare title and owner", path))
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", path, err))
		}
		if doc.Class == "plan" && doc.State == "active" {
			report.ActivePlans++
		}
		if doc.Class == "plan" && (doc.State == "active" || doc.State == "paused") {
			checkPlanReview(doc, now, report)
		}
		if doc.TranslationOf != "" {
			if !seenOrDeclared(manifest.Documents, doc.TranslationOf) {
				report.Errors = append(report.Errors, fmt.Sprintf("%s translation source %s is not registered", path, doc.TranslationOf))
			} else if hash, err := HashFile(filepath.Join(root, filepath.FromSlash(doc.TranslationOf))); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s source hash: %v", path, err))
			} else if hash != doc.SourceHash {
				report.Errors = append(report.Errors, fmt.Sprintf("%s translation is stale: source %s hash is %s, manifest has %s", path, doc.TranslationOf, hash, doc.SourceHash))
			}
		}
	}
	if report.ActivePlans > 1 {
		report.Errors = append(report.Errors, fmt.Sprintf("%d active plans registered; at most one is allowed", report.ActivePlans))
	}

	paths, err := markdownFiles(filepath.Join(root, "docs"))
	if err != nil {
		report.Errors = append(report.Errors, "docs inventory: "+err.Error())
		return
	}
	for _, path := range paths {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if !seen[rel] && !excluded[rel] {
			report.Errors = append(report.Errors, fmt.Sprintf("%s is missing from %s", rel, ManifestPath))
		}
	}
	for path := range seen {
		if !containsPath(paths, filepath.Join(root, filepath.FromSlash(path))) {
			report.Errors = append(report.Errors, fmt.Sprintf("%s is registered but not present", path))
		}
	}
}

func checkPlanReview(doc Document, now time.Time, report *Report) {
	if doc.ApprovedBy == "" || doc.ReviewBy == "" {
		report.Errors = append(report.Errors, fmt.Sprintf("%s active/paused plan must declare approved_by and review_by", doc.Path))
		return
	}
	deadline, err := time.Parse("2006-01-02", doc.ReviewBy)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("%s has invalid review_by %q", doc.Path, doc.ReviewBy))
		return
	}
	if now.After(deadline.Add(24*time.Hour-time.Nanosecond)) && strings.TrimSpace(doc.Verdict) == "" {
		report.Errors = append(report.Errors, fmt.Sprintf("%s review expired on %s without a verdict", doc.Path, doc.ReviewBy))
	}
}

func checkMarkdown(root string, manifest Manifest, report *Report) {
	paths := []string{filepath.Join(root, "AGENTS.md")}
	docs, err := markdownFiles(filepath.Join(root, "docs"))
	if err != nil {
		report.Errors = append(report.Errors, "docs scan: "+err.Error())
		return
	}
	paths = append(paths, docs...)
	excluded := make(map[string]bool, len(manifest.ExcludedDocuments))
	for _, doc := range manifest.ExcludedDocuments {
		excluded[filepath.Clean(filepath.Join(root, filepath.FromSlash(doc.Path)))] = true
	}
	for _, path := range paths {
		if excluded[filepath.Clean(path)] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", relative(root, path), err))
			continue
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), utf8.RuneError) {
			report.Errors = append(report.Errors, fmt.Sprintf("%s is not clean UTF-8", relative(root, path)))
		}
		for _, match := range markdownLinkPattern.FindAllStringSubmatch(string(data), -1) {
			target := strings.TrimSpace(strings.Trim(match[1], "<>"))
			if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" || strings.Contains(target, " ") && !strings.HasSuffix(strings.ToLower(target), ".md") {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
			if excluded[resolved] {
				report.Errors = append(report.Errors, fmt.Sprintf("%s links to excluded document %q", relative(root, path), target))
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s has broken local link %q", relative(root, path), target))
			}
		}
	}
}

func markdownFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, filepath.Clean(path))
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func seenOrDeclared(docs []Document, path string) bool {
	for _, doc := range docs {
		if doc.Path == path {
			return true
		}
	}
	return false
}

func containsPath(paths []string, target string) bool {
	target = filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return strings.Count(string(data), "\n") + 1
}

func classTitle(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
