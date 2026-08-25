package tools

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
)

type SkillResourceManifestEntry struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Bytes       int    `json:"bytes"`
}

// SkillPackageSnapshot freezes the source identity used for candidate refs,
// activation receipts, and resource drift checks. MainSource includes front
// matter because VersionHash remains the stored-source compatibility hash.
type SkillPackageSnapshot struct {
	Info             SkillInfo
	MainSource       string
	VersionHash      string
	PackageHash      string
	DescriptionHash  string
	ResourceManifest []SkillResourceManifestEntry
	ResourceBodies   map[string]string
}

func ReadSkillPackageForTenant(tenantID, name string, invocation ...map[string]interface{}) (SkillPackageSnapshot, error) {
	info, content, files, err := ReadSkillPayloadForTenant(tenantID, name, "", invocation...)
	if err != nil {
		return SkillPackageSnapshot{}, err
	}
	snapshot := SkillPackageSnapshot{
		Info: info, MainSource: content, ResourceBodies: make(map[string]string, len(files)),
	}
	descriptionDigest := sha256.Sum256([]byte(strings.TrimSpace(info.Description)))
	snapshot.DescriptionHash = fmt.Sprintf("%x", descriptionDigest[:])
	sort.Strings(files)
	for _, file := range files {
		target, err := safeSupportPath(info.Path, file)
		if err != nil {
			return SkillPackageSnapshot{}, err
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return SkillPackageSnapshot{}, err
		}
		snapshot.ResourceBodies[file] = string(data)
	}
	snapshot.VersionHash, snapshot.PackageHash, snapshot.ResourceManifest = BuildSkillPackageIdentity(content, snapshot.ResourceBodies)
	return snapshot, nil
}

// BuildSkillPackageIdentity deterministically hashes one source package. It is
// shared by filesystem reads and curator candidate creation so automatic
// packages cannot acquire a different identity merely because they have not
// been published to disk yet.
func BuildSkillPackageIdentity(mainSource string, resources map[string]string) (versionHash, packageHash string, manifest []SkillResourceManifestEntry) {
	mainDigest := sha256.Sum256([]byte(mainSource))
	versionHash = fmt.Sprintf("%x", mainDigest[:])
	paths := make([]string, 0, len(resources))
	for path := range resources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	packageDigest := sha256.New()
	writeSkillPackageDigestPart(packageDigest, "SKILL.md", []byte(mainSource))
	for _, path := range paths {
		content := resources[path]
		digest := sha256.Sum256([]byte(content))
		manifest = append(manifest, SkillResourceManifestEntry{
			Path: path, ContentHash: fmt.Sprintf("%x", digest[:]), Bytes: len(content),
		})
		writeSkillPackageDigestPart(packageDigest, path, []byte(content))
	}
	packageHash = fmt.Sprintf("%x", packageDigest.Sum(nil))
	return versionHash, packageHash, manifest
}

func writeSkillPackageDigestPart(hasher interface{ Write([]byte) (int, error) }, path string, content []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(path)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(path))
	binary.BigEndian.PutUint64(size[:], uint64(len(content)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write(content)
}
