package envprofiles

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TrustTrusted and TrustUntrusted mirror the workspace trust levels. They are
// duplicated as plain strings so this package stays dependency-free and can move
// with the execution side.
const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"
)

// ApplyContext is everything the engine supplies for one command. Paths are
// passed in rather than derived here, so this package needs no knowledge of the
// runtime layout and is fully testable with temporary directories.
type ApplyContext struct {
	// Home is the operator's home directory, used for StateSource fallbacks.
	Home string
	// StateRoot is the run's state overlay root (<lease>/state).
	StateRoot string
	// ScratchTmp is the run's shared temp directory.
	ScratchTmp string
	// ToolchainRoot is the person-level persistent cache root.
	ToolchainRoot string
	// Lookup resolves an environment variable from the run's snapshot. It must
	// NOT read the process environment: the snapshot is the run's binding.
	Lookup func(name string) (string, bool)
	// Trust is the active workspace's trust level.
	Trust string
	// HasCredentialRead reports whether the workspace holds the credential:read
	// capability, which is what lets an untrusted workspace use operator state.
	HasCredentialRead bool
}

// Result is the material a set of profiles contributes to one command.
type Result struct {
	// Applied lists the profile ids that contributed.
	Applied []string
	// EnvOverrides are NAME=value entries to merge over the run environment.
	EnvOverrides []string
	// WritableRoots must be writable inside the sandbox.
	WritableRoots []string
	// ReadOnlyPaths are approved host paths to map read-only.
	ReadOnlyPaths []string
	// OverlayMounts bind a writable state directory over a host path whose
	// location the tool does not let us change.
	OverlayMounts []OverlayMount
	// Notes are non-secret, user-visible explanations — most importantly when
	// operator credentials were withheld, so a resulting "not logged in" is
	// diagnosable instead of mysterious.
	Notes []string
	// CopiedFiles and CopiedBytes report what the state overlay materialized.
	CopiedFiles int
	CopiedBytes int64
}

// OverlayMount is a writable state directory bound over a host path.
type OverlayMount struct {
	Source string
	Target string
}

// Apply materializes the profiles' state and returns the resulting material.
//
// Operator credential access is decided HERE, not in the catalog: a profile
// declares what it needs, policy decides whether this workspace may have it.
// An untrusted workspace without credential:read gets the redirects and empty
// directories but no credential copy, so the tool reports its own "not logged
// in" rather than failing with a read-only filesystem error.
func Apply(profiles []*EnvProfile, ctx ApplyContext) (Result, error) {
	var result Result
	if len(profiles) == 0 {
		return result, nil
	}
	if ctx.Lookup == nil {
		ctx.Lookup = func(string) (string, bool) { return "", false }
	}
	// A conditional dependency is resolved from the tool's OWN configuration,
	// not assumed. This is what keeps an EKS or local-cluster kubectl from
	// dragging in Google credentials it will never use.
	profiles = ctx.expandConditionalRequires(profiles)
	redirects := map[string]string{}
	redirectOwner := map[string]string{}

	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		allowOperator := true
		if profile.CredentialAccess == CredentialAccessOperator && !credentialAccessAllowed(ctx) {
			allowOperator = false
			result.Notes = append(result.Notes, fmt.Sprintf(
				"%s: operator credentials withheld because this workspace is untrusted and has no credential:read capability; the tool will report that it is not logged in",
				profile.ID))
		}

		for _, spec := range profile.CopyIn {
			dest, err := ctx.leaseStatePath(profile.ID)
			if err != nil {
				return result, err
			}
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return result, err
			}
			if !allowOperator {
				continue
			}
			source := ctx.resolveSource(spec.From)
			copied, materialized, err := runCopyIn(spec, source, filepath.Join(dest, "state"))
			if err != nil {
				return result, fmt.Errorf("%s state overlay: %w", profile.ID, err)
			}
			if materialized {
				result.CopiedFiles += copied.Files
				result.CopiedBytes += copied.Bytes
			}
			// The overlay is published one level down so the staging sibling
			// never collides with a redirect target; expose the published dir.
			if err := linkOverlay(filepath.Join(dest, "state"), dest); err != nil {
				return result, fmt.Errorf("%s state overlay: %w", profile.ID, err)
			}
		}

		for _, mapping := range profile.MapRO {
			if !allowOperator {
				continue
			}
			for _, source := range ctx.resolveSources(mapping.From) {
				if source == "" {
					continue
				}
				if _, err := os.Stat(source); err != nil {
					continue
				}
				result.ReadOnlyPaths = append(result.ReadOnlyPaths, source)
			}
		}

		for _, mapping := range profile.MapRWAt {
			target := ctx.resolveSource(mapping.At)
			if target == "" {
				continue
			}
			source, err := ctx.leaseStatePath(mapping.Key)
			if err != nil {
				return result, err
			}
			if err := os.MkdirAll(source, 0o700); err != nil {
				return result, err
			}
			if mapping.Seed != nil && allowOperator {
				copied, materialized, err := runCopyIn(*mapping.Seed, ctx.resolveSource(mapping.Seed.From),
					filepath.Join(source, ".seed"))
				if err != nil {
					return result, fmt.Errorf("%s state overlay: %w", profile.ID, err)
				}
				if materialized {
					result.CopiedFiles += copied.Files
					result.CopiedBytes += copied.Bytes
				}
				if err := linkOverlay(filepath.Join(source, ".seed"), source); err != nil {
					return result, fmt.Errorf("%s state overlay: %w", profile.ID, err)
				}
			}
			result.WritableRoots = append(result.WritableRoots, source)
			result.OverlayMounts = append(result.OverlayMounts, OverlayMount{Source: source, Target: target})
		}

		for _, mapping := range profile.MapRW {
			dir, err := ctx.writableDir(mapping)
			if err != nil {
				return result, err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return result, err
			}
			result.WritableRoots = append(result.WritableRoots, dir)
		}

		for _, redirect := range profile.EnvRedirect {
			target, err := ctx.redirectTarget(profile, redirect)
			if err != nil {
				return result, err
			}
			if target == "" {
				continue
			}
			if existing, seen := redirects[redirect.Name]; seen && existing != target {
				return result, &conflictError{
					Variable: redirect.Name,
					First:    redirectOwner[redirect.Name],
					Second:   profile.ID,
				}
			}
			redirects[redirect.Name] = target
			redirectOwner[redirect.Name] = profile.ID
		}

		result.Applied = append(result.Applied, profile.ID)
	}

	names := make([]string, 0, len(redirects))
	for name := range redirects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result.EnvOverrides = append(result.EnvOverrides, name+"="+redirects[name])
	}
	if ctx.StateRoot != "" {
		result.WritableRoots = append(result.WritableRoots, ctx.StateRoot)
	}
	return result, nil
}

// credentialAccessAllowed is the trust decision. A trusted workspace may use the
// operator's credential state; an untrusted one needs an explicit capability.
func credentialAccessAllowed(ctx ApplyContext) bool {
	if strings.EqualFold(strings.TrimSpace(ctx.Trust), TrustTrusted) {
		return true
	}
	return ctx.HasCredentialRead
}

// resolveSource returns the FIRST location a source resolves to. Callers that
// must honour every entry of a list-valued variable use resolveSources.
func (c ApplyContext) resolveSource(source StateSource) string {
	all := c.resolveSources(source)
	if len(all) == 0 {
		return ""
	}
	return all[0]
}

// resolveSources returns every location a source resolves to. A list-valued
// variable (KUBECONFIG) contributes each of its entries.
func (c ApplyContext) resolveSources(source StateSource) []string {
	if name := strings.TrimSpace(source.EnvVar); name != "" {
		if value, ok := c.Lookup(name); ok && strings.TrimSpace(value) != "" {
			value = strings.TrimSpace(value)
			if !source.List {
				return []string{filepath.Clean(value)}
			}
			out := make([]string, 0, 2)
			for _, part := range filepath.SplitList(value) {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, filepath.Clean(part))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	rel := strings.TrimSpace(source.HomeRelPath)
	if rel == "" || strings.TrimSpace(c.Home) == "" {
		return nil
	}
	return []string{filepath.Join(c.Home, filepath.FromSlash(rel))}
}

func (c ApplyContext) leaseStatePath(rel string) (string, error) {
	if strings.TrimSpace(c.StateRoot) == "" {
		return "", fmt.Errorf("run state overlay is unavailable")
	}
	clean, err := safeRel(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.StateRoot, clean), nil
}

func (c ApplyContext) writableDir(mapping MapRW) (string, error) {
	clean, err := safeRel(mapping.Key)
	if err != nil {
		return "", err
	}
	if mapping.Persistent {
		if strings.TrimSpace(c.ToolchainRoot) == "" {
			return "", fmt.Errorf("toolchain cache root is unavailable")
		}
		return filepath.Join(c.ToolchainRoot, clean), nil
	}
	return c.leaseStatePath(clean)
}

func (c ApplyContext) redirectTarget(profile *EnvProfile, redirect EnvRedirect) (string, error) {
	clean, err := safeRel(redirect.RelPath)
	if err != nil {
		return "", err
	}
	switch redirect.Kind {
	case TargetLeaseState:
		return c.leaseStatePath(clean)
	case TargetToolchain:
		if strings.TrimSpace(c.ToolchainRoot) == "" {
			return "", fmt.Errorf("toolchain cache root is unavailable")
		}
		return filepath.Join(c.ToolchainRoot, clean), nil
	case TargetScratch:
		if strings.TrimSpace(c.ScratchTmp) == "" {
			return "", fmt.Errorf("run scratch is unavailable")
		}
		return filepath.Join(c.ScratchTmp, clean), nil
	case TargetHostPath:
		for _, mapping := range profile.MapRO {
			if source := c.resolveSource(mapping.From); source != "" {
				return source, nil
			}
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported redirect target for %s", redirect.Name)
	}
}

// linkOverlay exposes the published overlay directory at the path the redirect
// points to. The copy lands in <dest>/state so the ".staging" sibling used for
// the atomic rename can never collide with the directory tools will open.
func linkOverlay(published, dest string) error {
	if _, err := os.Stat(published); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(published)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(published, entry.Name())
		to := filepath.Join(dest, entry.Name())
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return os.RemoveAll(published)
}

func safeRel(value string) (string, error) {
	value = strings.TrimSpace(strings.Trim(strings.ReplaceAll(value, `\`, "/"), "/"))
	if value == "" {
		return "", fmt.Errorf("relative path is required")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe relative path %q", value)
	}
	return clean, nil
}

// expandConditionalRequires inspects each profile's declared configuration file
// and pulls in the profiles it actually names, then re-orders so a dependency is
// applied before its dependent.
//
// The read is bounded and failure-tolerant: an unreadable or oversized config
// simply resolves no extra profile, which degrades to "the helper's state was
// not prepared" rather than failing the command outright.
func (c ApplyContext) expandConditionalRequires(profiles []*EnvProfile) []*EnvProfile {
	selected := make(map[string]*EnvProfile, len(profiles))
	order := make([]string, 0, len(profiles))
	add := func(profile *EnvProfile) {
		if profile == nil {
			return
		}
		if _, exists := selected[profile.ID]; exists {
			return
		}
		selected[profile.ID] = profile
		order = append(order, profile.ID)
	}
	for _, profile := range profiles {
		add(profile)
	}
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		for _, condition := range profile.ConditionalRequires {
			required, ok := byID[strings.TrimSpace(condition.Profile)]
			if !ok {
				continue
			}
			if _, already := selected[required.ID]; already {
				continue
			}
			if !c.sourceMentions(condition) {
				continue
			}
			add(required)
			for _, transitive := range required.RequiresProfiles {
				add(byID[transitive])
			}
		}
	}
	expanded := make([]*EnvProfile, 0, len(order))
	for _, id := range order {
		expanded = append(expanded, selected[id])
	}
	sortByDependency(expanded)
	return expanded
}

// sourceMentions reports whether a profile's configuration file names a marker.
func (c ApplyContext) sourceMentions(condition ConditionalRequire) bool {
	marker := strings.TrimSpace(condition.Contains)
	if marker == "" {
		return false
	}
	limit := condition.MaxBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	// Every entry of a list-valued variable is inspected: a marker in the second
	// kubeconfig is just as decisive as one in the first.
	for _, path := range c.resolveSources(condition.From) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() > limit {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), marker) {
			return true
		}
	}
	return false
}

// sortByDependency puts a dependency before its dependent, keeping the rest
// stable. A dependent's redirects may point into directories its dependency
// creates, so the order is not cosmetic.
func sortByDependency(profiles []*EnvProfile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		return dependsOnProfile(profiles[j], profiles[i].ID) && !dependsOnProfile(profiles[i], profiles[j].ID)
	})
}

func dependsOnProfile(profile *EnvProfile, id string) bool {
	if profile == nil {
		return false
	}
	for _, required := range profile.RequiresProfiles {
		if required == id || dependsOnProfile(byID[required], id) {
			return true
		}
	}
	for _, condition := range profile.ConditionalRequires {
		if condition.Profile == id {
			return true
		}
	}
	return false
}
