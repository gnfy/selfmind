package tools

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"selfmind/internal/tools/sandbox"
)

// Sandbox backends.
//
// Naming the enforcement mechanism behind an interface is what keeps the layers
// above it platform-neutral. Today Linux enforces with bubblewrap and macOS
// falls back to approval-controlled host execution using the SAME plan,
// environment snapshot, scratch space and tool profiles; the readiness gate for
// multi-tenant execution also records that bubblewrap is a single-user boundary,
// so a container/microVM backend will eventually be a third implementation
// rather than a rewrite of the exec path.

// bubblewrapBackend enforces a plan with bwrap: read-only host root, writable
// roots bound over it, the run's scratch bound at both its real path and /tmp,
// and the network namespace unshared unless the plan shares it.
type bubblewrapBackend struct{}

func (bubblewrapBackend) Name() string { return bubblewrapBackendName }

func (bubblewrapBackend) Available() bool {
	return runtime.GOOS == "linux" && sandbox.Available()
}

func (b bubblewrapBackend) Command(ctx context.Context, argv []string, plan SandboxPlan, material ProcessMaterial) (*exec.Cmd, error) {
	policy := sandbox.Policy{
		Network:    plan.NetworkMode == "shared",
		ScratchTmp: material.ScratchTmp(),
	}
	for _, root := range plan.WritableRoots {
		if resolved := resolveWritableRoot(root); resolved != "" {
			policy.WritableRoots = append(policy.WritableRoots, resolved)
		}
	}
	// Synthesized roots must reach the policy before the overlays that land
	// inside them; sandbox.WrapArgv emits them in that order.
	for _, dir := range plan.SynthesizedDirs {
		target := strings.TrimSpace(dir.Target)
		if target == "" {
			continue
		}
		policy.SynthesizedDirs = append(policy.SynthesizedDirs, sandbox.SynthesizedDir{
			Target:           target,
			ReadOnlyChildren: dir.ReadOnlyChildren,
		})
	}
	for _, overlay := range plan.OverlayMounts {
		source := resolveWritableRoot(overlay.Source)
		target := strings.TrimSpace(overlay.Target)
		if source == "" || target == "" {
			continue
		}
		policy.OverlayMounts = append(policy.OverlayMounts, sandbox.OverlayMount{Source: source, Target: target})
	}
	wrapped, ok := sandbox.Wrap(policy, argv)
	if !ok {
		return nil, errSandboxUnavailable
	}
	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	// Secrets travel only through the child environment. Never encode them into
	// bwrap argv with --setenv: argv is visible through /proc/*/cmdline.
	cmd.Env = material.Env()
	return cmd, nil
}

// hostBackend runs the command directly. It is the approval-gated escape hatch
// on Linux and the first-stage implementation on macOS. It still applies the
// run's environment snapshot and scratch overrides, so $SELFMIND_RUN_TMP means
// the same thing in both modes.
type hostBackend struct{}

func (hostBackend) Name() string { return hostBackendName }

func (hostBackend) Available() bool { return true }

func (hostBackend) Command(ctx context.Context, argv []string, _ SandboxPlan, material ProcessMaterial) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = material.Env()
	return cmd, nil
}

var (
	backendBubblewrap SandboxBackend = bubblewrapBackend{}
	backendHost       SandboxBackend = hostBackend{}
)

// SandboxBackendForMode returns the backend that enforces a mode.
func SandboxBackendForMode(mode SandboxMode) SandboxBackend {
	if mode == SandboxIsolated {
		return backendBubblewrap
	}
	return backendHost
}

// SandboxBackendName reports the backend that would enforce the given mode, for
// diagnostics.
func SandboxBackendName(mode SandboxMode) string {
	return SandboxBackendForMode(mode).Name()
}

// resolveWritableRoot normalizes a writable root and resolves a symlinked path
// to its real target: bwrap binds the target, so a symlinked workspace root
// would otherwise not actually be writable.
func resolveWritableRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != "" {
		return resolved
	}
	return abs
}

// errSandboxUnavailable signals that a backend cannot enforce a plan on this
// host. The caller decides whether that is a hard failure (isolation was
// required) or an observable fallback.
var errSandboxUnavailable = errors.New("sandbox backend unavailable on this host")
