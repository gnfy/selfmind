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
// above it platform-neutral. Linux enforces with bubblewrap and macOS with
// seatbelt, from the SAME plan, environment snapshot, scratch space and tool
// profiles; the readiness gate for multi-tenant execution also records that
// bubblewrap is a single-user boundary, so a container/microVM backend will be
// a further implementation rather than a rewrite of the exec path.
//
// Backends are NOT interchangeable in strength. Seatbelt is a permission
// filter: it confines writes and egress, but provides no PID/IPC/UTS namespace
// and cannot mount, so a plan needing OverlayMounts or SynthesizedDirs is
// refused there and degrades to approval-gated host execution. Callers must
// keep reading the containment assessment rather than treating "enforced" as
// one uniform guarantee.

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

// seatbeltBackend enforces a plan with macOS sandbox-exec: an unrestricted
// read view of the host, writes confined to the plan's writable roots and the
// run's scratch directory, and no egress unless the plan shares the network.
type seatbeltBackend struct{}

func (seatbeltBackend) Name() string { return seatbeltBackendName }

func (seatbeltBackend) Available() bool {
	return runtime.GOOS == "darwin" && sandbox.SeatbeltAvailable()
}

func (seatbeltBackend) Command(ctx context.Context, argv []string, plan SandboxPlan, material ProcessMaterial) (*exec.Cmd, error) {
	policy := sandbox.Policy{
		Network:    plan.NetworkMode == "shared",
		ScratchTmp: material.ScratchTmp(),
	}
	for _, root := range plan.WritableRoots {
		if resolved := resolveWritableRoot(root); resolved != "" {
			policy.WritableRoots = append(policy.WritableRoots, resolved)
		}
	}
	// Mount-backed state redirection is carried through unchanged so the policy
	// builder can refuse it. Dropping these here instead would produce a policy
	// that looks enforceable while the tool state it is supposed to redirect
	// silently points at the host.
	for _, dir := range plan.SynthesizedDirs {
		policy.SynthesizedDirs = append(policy.SynthesizedDirs, sandbox.SynthesizedDir{
			Target:           strings.TrimSpace(dir.Target),
			ReadOnlyChildren: dir.ReadOnlyChildren,
		})
	}
	for _, overlay := range plan.OverlayMounts {
		policy.OverlayMounts = append(policy.OverlayMounts, sandbox.OverlayMount{
			Source: resolveWritableRoot(overlay.Source),
			Target: strings.TrimSpace(overlay.Target),
		})
	}
	wrapped, ok := sandbox.WrapSeatbelt(policy, argv)
	if !ok {
		return nil, errSandboxUnavailable
	}
	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	// The policy travels in argv, which is world-readable through ps. It holds
	// only paths and rule names, never secrets; secrets stay in the child
	// environment, exactly as on the bubblewrap path.
	cmd.Env = material.Env()
	return cmd, nil
}

// hostBackend runs the command directly. It is the approval-gated escape hatch
// on every platform. It still applies the
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
	backendSeatbelt   SandboxBackend = seatbeltBackend{}
	backendHost       SandboxBackend = hostBackend{}
)

// IsolationBackendForPlatform returns the backend that enforces isolation on
// goos, or nil when the platform has none. It answers "could this platform
// enforce at all", separately from whether this particular host can — the
// backend's own Available reports that.
func IsolationBackendForPlatform(goos string) SandboxBackend {
	switch goos {
	case "linux":
		return backendBubblewrap
	case "darwin":
		return backendSeatbelt
	}
	return nil
}

// SandboxBackendForMode returns the backend that enforces a mode.
func SandboxBackendForMode(mode SandboxMode) SandboxBackend {
	if mode == SandboxIsolated {
		if backend := IsolationBackendForPlatform(runtime.GOOS); backend != nil {
			return backend
		}
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
