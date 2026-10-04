package httpapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

const maxClientAdditionalRoots = 8

type localFilesystemAuthorityKey struct{}

func withLocalFilesystemAuthority(ctx context.Context) context.Context {
	return context.WithValue(ctx, localFilesystemAuthorityKey{}, true)
}

func hasLocalFilesystemAuthority(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	allowed, _ := ctx.Value(localFilesystemAuthorityKey{}).(bool)
	return allowed
}

// prepareRequestExecutionRoots converts a local CLI path request into the
// durable binding used by queue, run, scope, context discovery, and scheduling.
// Workspace-derived roots are rebuilt from the selected workspace while
// already-frozen CLI roots survive queue drain and approval continuation.
func (c *RunCoordinator) prepareRequestExecutionRoots(ctx context.Context, workspace *control.Workspace, req *api.MessageRequest) error {
	if req == nil {
		return nil
	}
	if len(req.ClientAdditionalRoots) > 0 && !hasLocalFilesystemAuthority(ctx) {
		return fmt.Errorf("--add-dir is available only to an authenticated local CLI connected to a loopback gateway")
	}
	if len(req.ClientAdditionalRoots) > maxClientAdditionalRoots {
		return fmt.Errorf("--add-dir may be specified at most %d times", maxClientAdditionalRoots)
	}
	if len(req.ClientAdditionalRoots) > 0 {
		req.AdditionalRootsRequested = true
	}

	bindings := workspaceRootBindings(workspace)
	// A recovered isolated Run keeps its exact physical view. Rebuilding the
	// logical workspace root here would quietly reintroduce write authority
	// over the original checkout.
	for _, frozen := range req.ExecutionRoots {
		if frozen.Source != executionenv.RootSourceExecutionView {
			continue
		}
		if len(req.ExecutionRoots) != 1 || workspace == nil || frozen.Role != executionenv.RootRolePrimary || frozen.GitBaseline == nil ||
			frozen.GitBaseline.Root != canonicalStoredDirectory(workspace.LocalPath) || !frozen.Writable() ||
			len(req.ClientAdditionalRoots) != 0 || c == nil || c.srv == nil || c.srv.Control == nil {
			return fmt.Errorf("isolated execution view no longer matches its workspace")
		}
		// Availability is checked after Run creation so a missing or changed
		// view becomes a durable waiting_user outcome with its exact lineage.
		// Rejecting it here would strand a claimed queue item before it had a
		// Run to explain why it could not proceed.
		req.ExecutionRoots = executionenv.CloneRootBindings(req.ExecutionRoots)
		return nil
	}
	// The first admitted direct writer freezes a clean repository baseline.
	// A later request accepted while it is changing files inherits that same
	// commit; re-probing the now-dirty checkout would lose the admission fact.
	// Multiple roots need composite topology and are never advertised as a
	// single-repository candidate.
	if len(bindings) == 1 && c != nil && c.activeCapacity() > 1 {
		primary := &bindings[0]
		for _, frozen := range req.ExecutionRoots {
			if frozen.Source == executionenv.RootSourceWorkspace && frozen.Role == executionenv.RootRolePrimary && frozen.Path == primary.Path && frozen.GitBaseline != nil {
				baseline := *frozen.GitBaseline
				primary.GitBaseline = &baseline
				break
			}
		}
		if primary.GitBaseline == nil {
			var activeRoot bool
			for _, active := range c.activeRunsForPerson(workspace.OwnerPersonID) {
				for _, root := range active.ExecutionRoots {
					if root.Path != primary.Path || !root.Writable() {
						continue
					}
					activeRoot = true
					if root.GitBaseline != nil {
						baseline := *root.GitBaseline
						primary.GitBaseline = &baseline
					}
				}
			}
			if !activeRoot {
				if baseline, err := executionenv.InspectCleanGitBaseline(ctx, primary.Path); err == nil {
					primary.GitBaseline = &baseline
				}
			}
		}
	}
	// Preserve non-workspace roots already frozen into a queued or recovered
	// request. Historical tasks never populate this field themselves.
	for _, binding := range req.ExecutionRoots {
		if binding.Source == executionenv.RootSourceWorkspace || binding.Source == "" {
			continue
		}
		bindings = appendRootBinding(bindings, normalizeFrozenRootBinding(binding))
	}
	for _, raw := range req.ClientAdditionalRoots {
		canonical, err := canonicalRequestedDirectory(raw)
		if err != nil {
			return err
		}
		bindings = appendRootBinding(bindings, executionenv.RootBinding{
			Path: canonical, Role: executionenv.RootRoleAdditional,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir,
			ContextRoot: true,
		})
	}
	if len(bindings) != 1 {
		for i := range bindings {
			bindings[i].GitBaseline = nil
		}
	}
	req.ExecutionRoots = executionenv.CloneRootBindings(bindings)
	// Paths have crossed the authenticated boundary and are now represented by
	// the typed internal snapshot. Clearing the wire field prevents detached
	// async contexts from needing to retain request-authentication metadata.
	req.ClientAdditionalRoots = nil
	return nil
}

// WorkspaceRootBindings exposes the exact root bindings a request bound to this
// workspace freezes into its run, so harnesses that seed historical runs can
// place them in the same execution domain a live turn would use.
func WorkspaceRootBindings(workspace *control.Workspace) []executionenv.RootBinding {
	return workspaceRootBindings(workspace)
}

func workspaceRootBindings(workspace *control.Workspace) []executionenv.RootBinding {
	if workspace == nil {
		return nil
	}
	primary := canonicalStoredDirectory(workspace.LocalPath)
	bindings := make([]executionenv.RootBinding, 0, len(workspace.AllowedRoots)+1)
	if primary != "" {
		bindings = appendRootBinding(bindings, executionenv.RootBinding{
			Path: primary, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace,
			ContextRoot: true,
		})
	}
	for _, raw := range workspace.AllowedRoots {
		path := canonicalStoredDirectory(raw)
		if path == "" || path == primary {
			continue
		}
		bindings = appendRootBinding(bindings, executionenv.RootBinding{
			Path: path, Role: executionenv.RootRoleAdditional,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace,
			ContextRoot: true,
		})
	}
	return bindings
}

func canonicalRequestedDirectory(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("--add-dir requires a non-empty directory")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("--add-dir path must be absolute after client resolution: %s", raw)
	}
	clean := filepath.Clean(raw)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("inspect --add-dir %s: %w", clean, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--add-dir path is not a directory: %s", clean)
	}
	canonical, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("resolve --add-dir %s: %w", clean, err)
	}
	absolute, err := filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("resolve --add-dir %s: %w", clean, err)
	}
	return filepath.Clean(absolute), nil
}

// canonicalStoredDirectory is intentionally best-effort. A durable workspace
// or queued run may be temporarily unmounted; admission preserves its exact
// path so the normal environment-unavailable lifecycle can park it visibly.
func canonicalStoredDirectory(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return ""
	}
	absolute, err := filepath.Abs(filepath.Clean(raw))
	if err != nil {
		return ""
	}
	if canonical, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		absolute = canonical
	}
	return filepath.Clean(absolute)
}

func normalizeFrozenRootBinding(binding executionenv.RootBinding) executionenv.RootBinding {
	binding.Path = canonicalStoredDirectory(binding.Path)
	if binding.Role == "" {
		binding.Role = executionenv.RootRoleAdditional
	}
	if binding.AccessCap == "" {
		binding.AccessCap = executionenv.RootAccessWrite
	}
	return binding
}

func appendRootBinding(bindings []executionenv.RootBinding, binding executionenv.RootBinding) []executionenv.RootBinding {
	binding.Path = canonicalStoredDirectory(binding.Path)
	if binding.Path == "" {
		return bindings
	}
	for i := range bindings {
		if bindings[i].Path != binding.Path {
			continue
		}
		// Exact duplicates do not need another sandbox mount, but an explicit
		// nested/project binding may add context significance or a stronger cap.
		bindings[i].ContextRoot = bindings[i].ContextRoot || binding.ContextRoot
		if binding.Writable() {
			bindings[i].AccessCap = executionenv.RootAccessWrite
		}
		return bindings
	}
	return append(bindings, binding)
}

func executionRootUnavailable(ctx context.Context, bindings []executionenv.RootBinding) (string, error) {
	for _, binding := range bindings {
		path := strings.TrimSpace(binding.Path)
		if path == "" || binding.Role == executionenv.RootRoleAttachment {
			continue
		}
		if binding.Source == executionenv.RootSourceExecutionView {
			if binding.GitBaseline == nil {
				return path, fmt.Errorf("isolated view has no admitted baseline")
			}
			view, err := executionenv.InspectGitView(ctx, filepath.Dir(path), filepath.Base(path), *binding.GitBaseline)
			if err != nil || view.Path != path {
				return path, fmt.Errorf("isolated view is missing or changed: %v", err)
			}
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return path, err
		}
		if !info.IsDir() {
			return path, fmt.Errorf("not a directory")
		}
	}
	return "", nil
}

func rootsExpandWorkspaceAuthority(bindings []executionenv.RootBinding) bool {
	workspaceRoots := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Source == executionenv.RootSourceWorkspace {
			workspaceRoots = append(workspaceRoots, binding.Path)
		}
	}
	for _, binding := range bindings {
		if binding.Source == executionenv.RootSourceExecutionView {
			return true
		}
		if binding.Source != executionenv.RootSourceCLIAddDir {
			continue
		}
		insideWorkspace := false
		for _, root := range workspaceRoots {
			if pathWithinRoot(binding.Path, root) {
				insideWorkspace = true
				break
			}
		}
		if !insideWorkspace {
			return true
		}
	}
	return false
}

func pathWithinRoot(path, root string) bool {
	path = canonicalStoredDirectory(path)
	root = canonicalStoredDirectory(root)
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameCLIAdditionalRoots(a, b []executionenv.RootBinding) bool {
	paths := func(bindings []executionenv.RootBinding) map[string]struct{} {
		out := make(map[string]struct{})
		for _, binding := range bindings {
			if binding.Source == executionenv.RootSourceCLIAddDir {
				out[binding.Path] = struct{}{}
			}
		}
		return out
	}
	aPaths, bPaths := paths(a), paths(b)
	if len(aPaths) != len(bPaths) {
		return false
	}
	for path := range aPaths {
		if _, ok := bPaths[path]; !ok {
			return false
		}
	}
	return true
}
