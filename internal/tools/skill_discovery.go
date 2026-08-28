package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxSkillScanDepth bounds recursive package discovery below one Skill root. A
// package directory at this depth is discovered; a deeper one is not. External
// packages use a <category>/<name>/SKILL.md layout and at most one level more,
// so the bound covers observed layouts while keeping a root that points at a
// large tree from triggering an unbounded walk. It is a constant rather than
// configuration so every discovery site agrees.
const maxSkillScanDepth = 4

// excludedSkillScanDirs are never traversed. A vendored dependency or cache
// tree can contain a SKILL.md that belongs to another package, not to this
// root. Dot-prefixed directories such as .git and .archive are already skipped
// by the scan itself, so only the visible names need listing here.
var excludedSkillScanDirs = map[string]bool{
	"node_modules":  true,
	"site-packages": true,
	"__pycache__":   true,
	"venv":          true,
}

const skillPackageManifestRelPath = ".claude-plugin/plugin.json"

// discoveredSkillPath is one candidate main location under a root.
type discoveredSkillPath struct {
	Path   string
	Format string
}

// skillRootScan is the result of discovering one root. PackageName is the
// manifest-declared source name when a manifest governs the root and is empty
// otherwise; qualified names fall back to the root scope in that case.
type skillRootScan struct {
	PackageName string
	Paths       []discoveredSkillPath
}

// discoverSkillPaths lists the Skill packages under one root.
//
// A read-only root governed by a package manifest yields exactly the packages
// that manifest lists: the manifest is the only signal available about which
// packages its author published, and an unpublished draft should neither reach
// the model catalog nor spend its bounded budget. Manifest gating is limited to
// read-only roots because a writable root is where this person installs, and an
// install whose result then failed to appear would be unexplainable.
//
// Any other root is scanned recursively within maxSkillScanDepth. A directory
// holding SKILL.md is a package and its subtree is not scanned further, so a
// SKILL.md preserved under references/, templates/, assets/, or scripts/ stays
// a resource instead of becoming a second Skill, while a category directory
// that merely shares one of those names stays discoverable.
func discoverSkillPaths(root SkillRoot) (skillRootScan, error) {
	if !root.Writable {
		if manifest, ok := readSkillPackageManifest(root.Path); ok {
			return skillRootScan{
				PackageName: manifest.Name,
				Paths:       manifest.packagesUnder(root.Path),
			}, nil
		}
	}
	paths, err := scanSkillRoot(root.Path)
	if err != nil {
		return skillRootScan{}, err
	}
	return skillRootScan{Paths: paths}, nil
}

func scanSkillRoot(rootPath string) ([]discoveredSkillPath, error) {
	var out []discoveredSkillPath
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || excludedSkillScanDirs[name] {
				continue
			}
			path := filepath.Join(dir, name)
			if !entry.IsDir() {
				// A bare Markdown Skill is recognized only at the top level of
				// a root. Deeper Markdown files are package resources or
				// ordinary documents.
				if depth == 1 && strings.HasSuffix(name, ".md") {
					out = append(out, discoveredSkillPath{Path: path, Format: "file"})
				}
				continue
			}
			if isSkillPackageDir(path) {
				out = append(out, discoveredSkillPath{Path: path, Format: "dir"})
				continue
			}
			if depth < maxSkillScanDepth {
				if err := walk(path, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(rootPath, 1); err != nil {
		return nil, err
	}
	return out, nil
}

func isSkillPackageDir(path string) bool {
	st, err := os.Stat(filepath.Join(path, "SKILL.md"))
	return err == nil && !st.IsDir()
}

// skillPackageManifest is the published-package declaration of one external
// source. Root is the directory holding the manifest, which is what the
// declared package paths are relative to.
type skillPackageManifest struct {
	Name  string
	Root  string
	Paths []string
}

// readSkillPackageManifest looks for a manifest at the root itself and at its
// immediate parent. External packages are commonly rooted one level above their
// skills directory, so a root pointed at <package>/skills must still find
// <package>/.claude-plugin/plugin.json. The search stops after one level and
// never yields a package outside the root.
func readSkillPackageManifest(rootPath string) (skillPackageManifest, bool) {
	clean := filepath.Clean(rootPath)
	bases := []string{clean}
	if parent := filepath.Dir(clean); parent != clean {
		bases = append(bases, parent)
	}
	for _, base := range bases {
		data, err := os.ReadFile(filepath.Join(base, skillPackageManifestRelPath))
		if err != nil {
			continue
		}
		var doc struct {
			Name   string   `json:"name"`
			Skills []string `json:"skills"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			continue
		}
		if len(doc.Skills) == 0 {
			// A manifest that declares no Skills does not govern discovery;
			// treating it as an empty allow-list would blank out the root.
			continue
		}
		return skillPackageManifest{
			Name:  strings.TrimSpace(doc.Name),
			Root:  base,
			Paths: doc.Skills,
		}, true
	}
	return skillPackageManifest{}, false
}

// packagesUnder resolves the declared package paths and keeps those that live
// under rootPath and actually hold a SKILL.md.
func (m skillPackageManifest) packagesUnder(rootPath string) []discoveredSkillPath {
	root := filepath.Clean(rootPath)
	var out []discoveredSkillPath
	seen := map[string]bool{}
	for _, declared := range m.Paths {
		declared = strings.TrimSpace(declared)
		if declared == "" {
			continue
		}
		path := declared
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.Root, path)
		}
		path = filepath.Clean(path)
		if !isPathWithin(root, path) || !isSkillPackageDir(path) {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, discoveredSkillPath{Path: path, Format: "dir"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func isPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
