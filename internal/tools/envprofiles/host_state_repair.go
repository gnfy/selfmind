package envprofiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// QuarantinedStateEntry is one entry the post-command scan moved out of a
// declared in-place subpath.
type QuarantinedStateEntry struct {
	Profile string `json:"profile"`
	// Path is where the entry was found.
	Path string `json:"path"`
	// Kind says why it could not stay: a symbolic link, a named pipe, a
	// hard-linked file, a directory in a flat cache, ...
	Kind string `json:"kind"`
	// MovedTo is its quarantine path, or "" when it could not be moved.
	MovedTo string `json:"moved_to,omitempty"`
	// Error explains an entry that could not be moved, and what was done.
	Error string `json:"error,omitempty"`
}

// RepairHostStateWrites runs before and after every sandboxed command that
// writes the plan's subpaths in place, whatever its outcome, and over recorded
// command write sets when the daemon starts. A sandbox policy refuses an operation
// by the entry it names, so an exchange (RENAME_SWAP) or a directory moved or
// cloned in can still leave an entry in a cache that the operator's own tools
// would later follow or block on. Every such entry is moved out of the cache
// into quarantine — a directory the caller owns that no tool reads — and
// reported. A cache path that is no longer a real directory, or a cache that
// cannot be listed within the bound, is moved whole and the tool recreates it;
// an entry that vanishes while the cache is listed is skipped, never a reason
// to move the cache. The quarantine directory is made only when something
// moves.
func RepairHostStateWrites(plan HostStatePlan, quarantine string) []QuarantinedStateEntry {
	var repaired []QuarantinedStateEntry
	for index, write := range plan.Writes {
		info, err := os.Lstat(write.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var findings []stateFinding
		switch {
		case err != nil:
			findings = []stateFinding{{path: write.Path, kind: fmt.Sprintf("cache that cannot be inspected (%v)", err)}}
		case !info.IsDir():
			kind := irregularStateKind(info)
			if kind == "" {
				kind = "file in place of the cache directory"
			}
			findings = []stateFinding{{path: write.Path, kind: kind}}
		default:
			found, exceeded, scanErr := scanStateDir(write.Path)
			switch {
			case errors.Is(scanErr, fs.ErrNotExist):
			case scanErr != nil:
				findings = []stateFinding{{path: write.Path, kind: fmt.Sprintf("cache that cannot be listed (%v)", scanErr)}}
			case exceeded:
				findings = []stateFinding{{path: write.Path, kind: fmt.Sprintf("cache with more than %d entries to verify", maxStateEntries)}}
			default:
				findings = found
			}
		}
		for _, finding := range findings {
			if entry, present := quarantineStateEntry(write, index, finding, quarantine); present {
				repaired = append(repaired, entry)
			}
		}
	}
	return repaired
}

// quarantineStateEntry moves one entry under quarantine/<write index>/,
// keeping its place relative to the cache so a person can see what was there.
// present is false when the entry vanished before it could be moved.
func quarantineStateEntry(write HostStateWrite, index int, finding stateFinding, quarantine string) (QuarantinedStateEntry, bool) {
	entry := QuarantinedStateEntry{Profile: write.Profile, Path: finding.path, Kind: finding.kind}
	moveErr := errors.New("no quarantine directory")
	if filepath.IsAbs(quarantine) {
		rel := filepath.Base(write.Path)
		if inner, err := filepath.Rel(write.Path, finding.path); err == nil && inner != "." {
			rel = filepath.Join(rel, inner)
		}
		dest := filepath.Join(quarantine, strconv.Itoa(index), rel)
		if moveErr = os.MkdirAll(filepath.Dir(dest), 0o700); moveErr == nil {
			if moveErr = os.Rename(finding.path, dest); moveErr == nil {
				entry.MovedTo = dest
				return entry, true
			}
		}
	}
	info, err := os.Lstat(finding.path)
	if errors.Is(err, fs.ErrNotExist) {
		return entry, false
	}
	// Not movable (the quarantine is on another filesystem, say). A link or
	// special file holds no data of its own and a hard-linked file keeps its
	// other name, so those are removed; anything else is left and reported.
	if err == nil && !info.IsDir() && (!info.Mode().IsRegular() || linkCount(info) > 1) {
		if removeErr := os.Remove(finding.path); removeErr == nil {
			entry.Error = fmt.Sprintf("could not be moved to the quarantine (%v); removed", moveErr)
			return entry, true
		}
	}
	entry.Error = fmt.Sprintf("could not be moved to the quarantine (%v); left in place", moveErr)
	return entry, true
}

// CatalogHostStateWrites is every in-place cache the catalog declares that
// exists on this host, for catalog inspection. Startup recovery must instead
// use an interrupted command's persisted write set; the catalog is not evidence
// that a command modified every cache. Paths whose structure
// cannot be written in place are left out; planning refuses them anyway.
func CatalogHostStateWrites(ctx ApplyContext) HostStatePlan {
	var plan HostStatePlan
	seen := map[string]bool{}
	for i := range Catalog {
		profile := &Catalog[i]
		if len(profile.MapRWAt) == 0 {
			continue
		}
		roots := ctx.declaredStateRoots(profile)
		readOnly := ctx.readOnlyStatePaths(profile)
		for _, mapping := range profile.MapRWAt {
			target := ctx.resolveSource(mapping.At)
			if target == "" {
				continue
			}
			write, present, err := planInPlaceWrite(profile.ID, target, mapping.Flat, roots, readOnly)
			if err != nil || !present || write.Missing || seen[write.Path] {
				continue
			}
			seen[write.Path] = true
			plan.Writes = append(plan.Writes, write)
		}
	}
	return plan
}
