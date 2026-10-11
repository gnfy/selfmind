package envprofiles

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// In-place tool state, for a backend that cannot mount.
//
// SynthesizeDir and MapRWAt exist because some tools write state at a path they
// do not let us move: the AWS CLI derives its SSO token cache from HOME. A
// mounting backend shadows that path with a private, seeded directory, so the
// host is never modified. A backend with no mount namespace (macOS seatbelt)
// cannot do that. It refused such plans, which left every credential CLI whose
// authentication refreshes a cache — and every opaque script that might call
// one — unable to run sandboxed at all.
//
// The in-place form grants a write allowance on exactly each declared subpath,
// on the real host path, and nothing else: never the declared state root
// itself, never the read-only configuration and credential files, and never
// through a symbolic link below the root, wherever it points. A subpath that
// already holds anything but regular files and real directories is refused, so
// a link a sandboxed process left there is not handed back to it or followed by
// host tools. It is weaker than a private overlay — the operator's real cache
// is written and its other contents are visible — and only operator
// credentials the policy hands this call get it at all. A requirement subpath
// writes cannot express is refused with its specific reason rather than
// widened.

// HostStateWrite is one declared state subpath granted as an in-place write.
type HostStateWrite struct {
	// Profile is the profile that declared the requirement.
	Profile string
	// Path is the canonical directory that becomes writable.
	Path string
	// Root is the canonical declared state root that bounds Path.
	Root string
	// Missing reports that Path does not exist yet; it is created inside Root,
	// never outside it, before the command runs.
	Missing bool
	// Flat reports that the requirement holds only files directly in Path.
	Flat bool
}

// HostStatePlan is the in-place form of a profile set's mount-backed state.
type HostStatePlan struct {
	Writes []HostStateWrite
	// Notes are non-secret explanations, such as credentials withheld.
	Notes []string
}

// Paths lists the writable subpaths in plan order.
func (p HostStatePlan) Paths() []string {
	paths := make([]string, 0, len(p.Writes))
	for _, write := range p.Writes {
		paths = append(paths, write.Path)
	}
	return paths
}

// UnexpressibleStateError reports mount-backed state that in-place subpath
// writes cannot express. The command must not run: dropping the requirement
// would leave the tool unable to write its state, and widening it would make
// more of the host writable than the profile declares.
type UnexpressibleStateError struct {
	Profile string
	Path    string
	Reason  string
}

func (e *UnexpressibleStateError) Error() string {
	return fmt.Sprintf("%s: %s %s; an in-place sandbox can make only a declared subpath inside its state root writable",
		e.Profile, e.Path, e.Reason)
}

// mountStateSpecs returns the mount-backed specs Apply materializes: all of
// them for a mounting backend, none when state is written in place.
func mountStateSpecs[T any](specs []T, inPlace bool) []T {
	if inPlace {
		return nil
	}
	return specs
}

// stateRoot is one declared state root as the host has it.
type stateRoot struct {
	lexical   string
	canonical string
	present   bool
}

// PlanHostStateWrites translates the profiles' mount-backed write state into
// in-place write allowances. It reads the host (stat, readlink, the selected
// configuration) and changes nothing; MaterializeHostStateWrites creates what
// is missing.
func PlanHostStateWrites(profiles []*EnvProfile, ctx ApplyContext) (HostStatePlan, error) {
	var plan HostStatePlan
	if ctx.Lookup == nil {
		ctx.Lookup = func(string) (string, bool) { return "", false }
	}
	seen := map[string]bool{}
	for _, profile := range Resolve(profiles, ctx) {
		if profile == nil || len(profile.MapRWAt) == 0 {
			continue
		}
		if profile.CredentialAccess == CredentialAccessOperator && !credentialAccessAllowed(ctx) {
			// A private empty cache is what a mounting backend gives withheld
			// credentials. In place there is no private copy to give, and the
			// real cache is not this call's to write: it stays read-only.
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"%s: operator credentials withheld, so its credential cache stays read-only; this sandbox cannot hide or redirect host state", profile.ID))
			continue
		}
		roots := ctx.declaredStateRoots(profile)
		readOnly := ctx.readOnlyStatePaths(profile)
		granted := make([]string, 0, len(profile.MapRWAt))
		for _, mapping := range profile.MapRWAt {
			target := ctx.resolveSource(mapping.At)
			if target == "" {
				continue
			}
			write, present, err := planInPlaceWrite(profile.ID, target, mapping.Flat, roots, readOnly)
			if err != nil {
				return HostStatePlan{}, err
			}
			if !present {
				plan.Notes = append(plan.Notes, fmt.Sprintf(
					"%s: %s has no state root on this host, so its writable state was not prepared; the tool may report missing state",
					profile.ID, target))
				continue
			}
			if seen[write.Path] {
				continue
			}
			seen[write.Path] = true
			plan.Writes = append(plan.Writes, write)
			granted = append(granted, write.Path)
		}
		if len(granted) > 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"%s: credential state is written in place at %s, because this sandbox cannot mount a private copy",
				profile.ID, strings.Join(granted, ", ")))
		}
	}
	return plan, nil
}

// declaredStateRoots resolves a profile's SynthesizeDir roots on this host.
func (c ApplyContext) declaredStateRoots(profile *EnvProfile) []stateRoot {
	roots := make([]stateRoot, 0, len(profile.SynthesizeDir))
	for _, spec := range profile.SynthesizeDir {
		lexical := c.resolveSource(spec.At)
		if lexical == "" {
			continue
		}
		root := stateRoot{lexical: filepath.Clean(lexical)}
		if info, err := os.Stat(lexical); err == nil && info.IsDir() {
			if canonical, err := filepath.EvalSymlinks(lexical); err == nil {
				root.canonical, root.present = canonical, true
			}
		}
		roots = append(roots, root)
	}
	// The deepest root bounds a target most tightly.
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i].lexical) > len(roots[j].lexical) })
	return roots
}

// readOnlyStatePaths is the profile's state that must stay read-only, in both
// its spelled and its real location.
func (c ApplyContext) readOnlyStatePaths(profile *EnvProfile) []string {
	sources := make([]StateSource, 0, len(profile.MapRO)+2)
	for _, mapping := range profile.MapRO {
		sources = append(sources, mapping.From)
	}
	for _, spec := range profile.SynthesizeDir {
		sources = append(sources, spec.KeepReadOnly...)
	}
	paths := make([]string, 0, len(sources)*2)
	for _, source := range sources {
		for _, path := range c.resolveSources(source) {
			if path == "" {
				continue
			}
			paths = append(paths, filepath.Clean(path))
			if canonical, err := filepath.EvalSymlinks(path); err == nil {
				paths = append(paths, canonical)
			}
		}
	}
	return paths
}

// planInPlaceWrite validates one MapRWAt target as an in-place write. present
// is false when the host has no state root to hold it, which is the same "no
// state on this host" a mounting backend skips with a note.
func planInPlaceWrite(profileID, target string, flat bool, roots []stateRoot, readOnly []string) (HostStateWrite, bool, error) {
	target = filepath.Clean(target)
	refuse := func(reason string) (HostStateWrite, bool, error) {
		return HostStateWrite{}, false, &UnexpressibleStateError{Profile: profileID, Path: target, Reason: reason}
	}
	// Only a cache that holds files alone is written in place. Its scan is a
	// listing of one directory the sandbox cannot replace, so nothing a
	// sandboxed process swaps underneath it can redirect the repair.
	if !flat {
		return refuse("is not declared flat (files only); only a flat cache is written in place")
	}
	var root *stateRoot
	for i := range roots {
		if withinDir(roots[i].lexical, target) {
			root = &roots[i]
			break
		}
	}
	if root == nil {
		return refuse("has no declared state root to bound an in-place write")
	}
	if target == root.lexical {
		return refuse("is the declared state root itself, which stays read-only")
	}
	if !root.present {
		return HostStateWrite{}, false, nil
	}

	rel, err := filepath.Rel(root.lexical, target)
	if err != nil {
		return refuse("cannot be resolved")
	}
	// Every component below the root must be a real directory. A link — even
	// one that stays inside the root — can be wider than the declared subpath
	// (`sso/` for `sso/cache`), and whoever can repoint it steers later grants.
	// Missing components are created inside the root, one real directory at a
	// time, by MaterializeHostStateWrites.
	write := HostStateWrite{Profile: profileID, Root: root.canonical, Path: filepath.Join(root.canonical, rel), Flat: flat}
	current := root.lexical
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			write.Missing = true
			break
		}
		if err != nil {
			return refuse(fmt.Sprintf("cannot be inspected (%v)", err))
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(current); err == nil && !withinDir(root.canonical, resolved) {
				return refuse(fmt.Sprintf("resolves to %s, outside its declared state root %s", resolved, root.canonical))
			}
			return refuse(fmt.Sprintf("passes through the symbolic link %s; only real directories below the state root are written in place", current))
		}
		if !info.IsDir() {
			return refuse(fmt.Sprintf("is not a directory (%s)", current))
		}
	}
	for _, path := range readOnly {
		if withinDir(write.Path, path) {
			return refuse(fmt.Sprintf("would make the read-only %s writable", path))
		}
		if withinDir(path, write.Path) {
			return refuse(fmt.Sprintf("lies inside the read-only %s", path))
		}
	}
	// What the cache already holds is not a reason to refuse: anything a host
	// tool must not meet is moved out before the command runs
	// (RepairHostStateWrites), as it is after.
	return write, true, nil
}

// maxStateEntries bounds verifying what a granted cache holds. A tool's cache
// holds a handful of token files; a cache past the bound cannot be verified
// and is moved aside whole rather than trusted unread.
const maxStateEntries = 4096

// irregularStateKind names why an entry may not stay in a flat cache that host
// tools read after the run, or "" when it may. A link redirects a host tool's
// next write, a special file blocks or misleads its read, a regular file with
// another name aliases a file elsewhere so a write through either changes
// both, and a directory has no place in a cache that holds files alone.
func irregularStateKind(info fs.FileInfo) string {
	switch mode := info.Mode(); {
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		if linkCount(info) > 1 {
			return "hard-linked file"
		}
		return ""
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}

// stateFinding is one entry a declared subpath may not keep.
type stateFinding struct {
	path, kind string
}

// scanStateDir lists a flat cache within the bound and returns every entry a
// host tool must not meet. An entry that vanishes while it is listed — a
// writer's temporary file renamed into place, the operator's own tool or
// another run — is gone, not irregular, and is skipped. exceeded reports a
// cache past the bound.
func scanStateDir(dir string) (findings []stateFinding, exceeded bool, err error) {
	handle, err := os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(maxStateEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > maxStateEntries {
		return nil, true, nil
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			findings = append(findings, stateFinding{path: path, kind: fmt.Sprintf("entry that cannot be inspected (%v)", err)})
		default:
			if kind := irregularStateKind(info); kind != "" {
				findings = append(findings, stateFinding{path: path, kind: kind})
			}
		}
	}
	return findings, false, nil
}

// MaterializeHostStateWrites creates every missing declared subpath inside its
// root, one directory at a time, refusing any component that is not a real
// directory. A path that left its root after planning is refused, and nothing
// is created outside the root.
func MaterializeHostStateWrites(plan HostStatePlan) error {
	for _, write := range plan.Writes {
		if err := ensureStateDirectory(write); err != nil {
			return err
		}
	}
	return nil
}

func ensureStateDirectory(write HostStateWrite) error {
	changed := &UnexpressibleStateError{Profile: write.Profile, Path: write.Path, Reason: "changed while it was being prepared"}
	rel, err := filepath.Rel(write.Root, write.Path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return changed
	}
	current := write.Root
	if !realDirectory(current) {
		return changed
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: create %s: %w", write.Profile, current, err)
		}
		if !realDirectory(current) {
			return changed
		}
	}
	return nil
}

// realDirectory reports a directory that is not a symbolic link.
func realDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// MountStateTargets lists the host paths a mounting backend shadows with
// private writable directories for these profiles, by Apply's own
// mountability rule. It reads the host and changes nothing.
func MountStateTargets(profiles []*EnvProfile, ctx ApplyContext) []string {
	if ctx.Lookup == nil {
		ctx.Lookup = func(string) (string, bool) { return "", false }
	}
	var synthesized []SynthesizedDir
	var targets []string
	for _, profile := range Resolve(profiles, ctx) {
		if profile == nil {
			continue
		}
		for _, spec := range profile.SynthesizeDir {
			if target := ctx.resolveSource(spec.At); target != "" {
				if info, err := os.Stat(target); err == nil && info.IsDir() {
					synthesized = append(synthesized, SynthesizedDir{Target: target})
				}
			}
		}
		for _, mapping := range profile.MapRWAt {
			if target := ctx.resolveSource(mapping.At); target != "" && mountable(target, synthesized) {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

// HasHostState reports whether any state location the profile declares exists
// on this host.
func HasHostState(profile *EnvProfile, ctx ApplyContext) bool {
	if profile == nil {
		return false
	}
	if ctx.Lookup == nil {
		ctx.Lookup = func(string) (string, bool) { return "", false }
	}
	return ctx.profileHasHostState(profile)
}
