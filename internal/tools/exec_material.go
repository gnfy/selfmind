package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/kernel"

	"selfmind/internal/executionenv"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools/envprofiles"
)

// Request-derived execution material.
//
// The writable view used to be derived from the command's cwd, which had three
// consequences: only that one directory was writable even when the workspace
// declared several allowed roots, a command running in a subdirectory could not
// write to its own workspace root, and nothing outside the workspace was
// writable at all — including the state directories tools need in order to run.
// It is now derived from the request's ExecutionScope plus the run's scratch
// space, which is the only place that knows the full boundary.

// execMaterialForArgs builds the writable view, scratch space, and child
// environment for one tool call.
func execMaterialForArgs(args map[string]interface{}, cwd string) execMaterial {
	material := execMaterial{Env: leaseProcessEnv(args)}
	scope, hasScope := currentExecutionScopeAny(args)
	if snapshot := resolvedSnapshotForArgs(args); snapshot != nil {
		material.SnapshotID = snapshot.ID
		material.Generation = snapshot.Generation
	}

	roots := make([]string, 0, 4)
	if hasScope {
		roots = append(roots, scope.AllowedRoots...)
		if len(scope.AllowedRoots) == 0 {
			roots = append(roots, scope.WorkspaceRoot)
		}
	}
	// cwd stays in the writable set: a caller may legitimately run in a
	// directory the scope allows but does not list as a root (an attachment
	// partition, for instance), and dropping it would be a regression.
	roots = append(roots, cwd)
	material.WritableRoots = roots

	if !hasScope || strings.TrimSpace(scope.LeaseID) == "" {
		return material
	}
	scratch, err := executionenv.EnsureLeaseScratch(scope.LeaseID)
	if err != nil {
		// Scratch is an improvement, not a precondition: without it the sandbox
		// falls back to a private tmpfs exactly as before.
		log.Debug("exec material: run scratch unavailable", "error", err)
		return material
	}
	material.ScratchTmp = scratch.TmpDir
	material.ScratchHandle = scope.LeaseID
	material.WritableRoots = append(material.WritableRoots, scratch.Root)
	material.Env = mergeEnv(material.Env, scratch.EnvOverrides())
	// Quota is checked BEFORE the next command, never during one: killing a
	// running command for a disk-hygiene reason would abort real work, while
	// letting a run keep growing silently fills the disk.
	if bytes, sizeErr := executionenv.ScratchBytesCached(scope.LeaseID); sizeErr == nil {
		material.ScratchBytes = bytes
		if bytes >= ScratchQuotaSoftLimitBytes {
			material.ProfileError = fmt.Errorf(
				"this run's scratch directory holds %d bytes, over the %d-byte soft limit; "+
					"remove what is no longer needed under $SELFMIND_RUN_TMP (or finish the run, which releases it after its retention window) before running another command",
				bytes, int64(ScratchQuotaSoftLimitBytes))
			return material
		}
	}

	applied, profileErr := applyEnvProfiles(args, scope, scratch, &material)
	if profileErr != nil {
		// A profile failure is diagnosable and must not be swallowed: the
		// command would otherwise run with a half-prepared state overlay and
		// fail later with a misleading error.
		material.ProfileError = profileErr
		return material
	}
	material.Profiles = applied
	return material
}

// applyEnvProfiles prepares the state overlays the command's programs need. The
// program set comes from the shell AST, not from the first token: a command like
// `set -euo pipefail; gcloud ... | jq ...` needs gcloud's overlay, and deriving
// the set from the leading word would miss it entirely.
func applyEnvProfiles(
	args map[string]interface{},
	scope ExecutionScope,
	scratch executionenv.LeaseScratch,
	material *execMaterial,
) ([]string, error) {
	toolName := stringArg(args, "_tool_name")
	programs := execCommandPrograms(toolName, args)
	profiles := envprofiles.Match(programs)
	if len(profiles) == 0 {
		return nil, nil
	}
	toolchainRoot := ""
	if root, err := executionenv.ToolchainDir(scope.PersonID, "."); err == nil {
		toolchainRoot = root
	}
	snapshotEnv := material.Env
	result, err := envprofiles.Apply(profiles, envprofiles.ApplyContext{
		Home:              envValue(snapshotEnv, "HOME"),
		StateRoot:         scratch.StateDir,
		ScratchTmp:        scratch.TmpDir,
		ToolchainRoot:     toolchainRoot,
		Lookup:            func(name string) (string, bool) { return lookupEnv(snapshotEnv, name) },
		Trust:             scope.TrustLevel,
		HasCredentialRead: credentialReadAllowed(scope, args),
	})
	if err != nil {
		return nil, err
	}
	material.WritableRoots = append(material.WritableRoots, result.WritableRoots...)
	material.ReadOnlyPaths = append(material.ReadOnlyPaths, result.ReadOnlyPaths...)
	for _, overlay := range result.OverlayMounts {
		material.OverlayMounts = append(material.OverlayMounts,
			SandboxOverlayMount{Source: overlay.Source, Target: overlay.Target})
	}
	material.Env = mergeEnv(material.Env, result.EnvOverrides)
	material.ProfileNotes = append(material.ProfileNotes, result.Notes...)
	material.CopiedStateFiles = result.CopiedFiles
	material.CopiedStateBytes = result.CopiedBytes
	return result.Applied, nil
}

// execCommandPrograms returns every real program a command will run, skipping
// shell builtins and control keywords. It reuses the same segmentation the
// safety floor uses, so profile matching and approval classification can never
// disagree about what a command actually invokes.
func execCommandPrograms(toolName string, args map[string]interface{}) []string {
	if strings.EqualFold(strings.TrimSpace(toolName), "execute_code") {
		return []string{"python3"}
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		// The tool name is normally stamped by the dispatcher; fall back to the
		// command argument so a direct call still prepares the right state.
		payload = strings.TrimSpace(stringArg(args, "command"))
	}
	if payload == "" {
		return nil
	}
	segments, _ := expandCommandSegments(payload, 0)
	seen := map[string]bool{}
	programs := make([]string, 0, len(segments))
	for _, fields := range segments {
		base := segmentRealProgram(fields)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		programs = append(programs, base)
	}
	return programs
}

// segmentRealProgram finds the program a segment actually runs.
//
// Two kinds of leading word are treated differently, because what follows them
// is different:
//
//   - a CONTROL keyword introduces a command, so scanning continues. Shell
//     splitting yields segments like `do kubectl get ns` from a loop body, and a
//     segment-level skip would silently miss the one program whose state needs
//     preparing — precisely the class of command that failed in the sandbox.
//   - a BUILTIN takes arguments, not a command, so scanning stops. Continuing
//     would read `pipefail` out of `set -euo pipefail` as a program name.
func segmentRealProgram(fields []string) string {
	progIdx, ok := segmentProgram(fields)
	if !ok {
		return ""
	}
	for i := progIdx; i < len(fields); i++ {
		token := strings.TrimSpace(fields[i])
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		base := strings.ToLower(filepath.Base(token))
		if base == "" {
			continue
		}
		if _, introduces := shellCommandIntroducers[base]; introduces {
			continue
		}
		if _, control := shellControlKeywords[base]; control {
			// A loop or case HEADER (`for t in a b`, `case $x in`) contains a
			// variable and a word list, not a command — the body lives in the
			// `do`/pattern segment. Reading on would take the loop variable for
			// a program name.
			return ""
		}
		if _, neutral := shellNeutralWords[base]; neutral {
			return ""
		}
		return base
	}
	return ""
}

// shellCommandIntroducers are the control keywords a COMMAND follows. Every
// other control keyword introduces a header (a variable, a word list, a
// pattern) and therefore ends the search for a program in that segment.
var shellCommandIntroducers = map[string]struct{}{
	"if": {}, "then": {}, "else": {}, "elif": {}, "while": {}, "until": {}, "do": {},
}

func scopeHasCapability(scope ExecutionScope, capability string) bool {
	for _, granted := range scope.Capabilities {
		if strings.EqualFold(strings.TrimSpace(granted), capability) {
			return true
		}
	}
	return false
}

func lookupEnv(env []string, name string) (string, bool) {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			return value, true
		}
	}
	return "", false
}

func envValue(env []string, name string) string {
	value, _ := lookupEnv(env, name)
	return value
}

// mergeEnv applies overrides to a base environment, replacing an existing
// assignment for the same name rather than appending a duplicate. Duplicates
// would leave the effective value up to the child's own parsing order.
func mergeEnv(base []string, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	index := make(map[string]int, len(base))
	for i, entry := range base {
		if name, _, ok := strings.Cut(entry, "="); ok {
			index[envKeyForMerge(name)] = i
		}
	}
	out := append([]string{}, base...)
	for _, override := range overrides {
		name, _, ok := strings.Cut(override, "=")
		if !ok {
			continue
		}
		if position, exists := index[envKeyForMerge(name)]; exists {
			out[position] = override
			continue
		}
		index[envKeyForMerge(name)] = len(out)
		out = append(out, override)
	}
	return out
}

// envKeyForMerge normalizes a variable name for override matching. POSIX
// environments are case-sensitive, so only surrounding whitespace is removed.
func envKeyForMerge(name string) string { return strings.TrimSpace(name) }

// scratchForArgs resolves the run's scratch space without creating it, for
// callers that only need to report on it.
func scratchForArgs(args map[string]interface{}) (executionenv.LeaseScratch, bool) {
	scope, ok := currentExecutionScopeAny(args)
	if !ok || strings.TrimSpace(scope.LeaseID) == "" || executionenv.RuntimeRoot() == "" {
		return executionenv.LeaseScratch{}, false
	}
	scratch, err := executionenv.EnsureLeaseScratch(scope.LeaseID)
	if err != nil {
		return executionenv.LeaseScratch{}, false
	}
	return scratch, true
}

// absoluteCWD resolves a command's working directory for the writable view.
// A relative value would otherwise be resolved against the daemon's own
// process directory, which is whatever directory happened to start it.
func absoluteCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "." {
		return ""
	}
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return filepath.Clean(cwd)
}

// emitProfilePreparation reports the state overlays a command received. It is
// execution EVIDENCE: without it, a withheld operator credential shows up only
// as the tool's own "not logged in", with nothing linking that to the trust
// decision that caused it.
func emitProfilePreparation(ctx context.Context, toolName, toolCallID string, material execMaterial) {
	if len(material.Profiles) == 0 && len(material.ProfileNotes) == 0 {
		return
	}
	payload := map[string]interface{}{
		"tool_name":    toolName,
		"tool_call_id": toolCallID,
		"profiles":     material.Profiles,
	}
	if material.CopiedStateFiles > 0 {
		payload["state_files"] = material.CopiedStateFiles
		payload["state_bytes"] = material.CopiedStateBytes
	}
	if len(material.ProfileNotes) > 0 {
		payload["notes"] = material.ProfileNotes
	}
	message := "env profiles: " + strings.Join(material.Profiles, ", ")
	if len(material.ProfileNotes) > 0 {
		message += " — " + strings.Join(material.ProfileNotes, "; ")
	}
	emitToolProgress(kernel.EventChannelFromContext(ctx), "tool.environment", payload, message)
}

// durableExecMaterial prepares execution material for a daemon-owned check.
// It mirrors execMaterialForArgs but takes its identity from the durable record
// instead of an ExecutionScope, because no agent run is live.
func durableExecMaterial(ctx context.Context, command, cwd string, durable DurableExecutionScope) (execMaterial, error) {
	material := execMaterial{Env: leaseProcessEnv(nil)}
	if snapshot := executionenv.DefaultRegistry().Current(); snapshot != nil {
		material.SnapshotID = snapshot.ID
		material.Generation = snapshot.Generation
	}
	if root := absoluteCWD(cwd); root != "" {
		material.WritableRoots = append(material.WritableRoots, root)
	}
	key := strings.TrimSpace(durable.ScratchKey)
	if key == "" || executionenv.RuntimeRoot() == "" {
		return material, nil
	}
	scratch, err := executionenv.EnsureLeaseScratch(key)
	if err != nil {
		log.Debug("durable exec material: scratch unavailable", "error", err)
		return material, nil
	}
	material.ScratchTmp = scratch.TmpDir
	material.ScratchHandle = key
	material.WritableRoots = append(material.WritableRoots, scratch.Root)
	material.Env = mergeEnv(material.Env, scratch.EnvOverrides())

	scope := ExecutionScope{
		TenantID:     durable.TenantID,
		PersonID:     durable.PersonID,
		WorkspaceID:  durable.WorkspaceID,
		TrustLevel:   durable.TrustLevel,
		Capabilities: durable.Capabilities,
	}
	args := map[string]interface{}{"_tool_name": "terminal", "command": command}
	applied, profileErr := applyEnvProfiles(args, scope, scratch, &material)
	if profileErr != nil {
		return material, fmt.Errorf("prepare durable execution environment: %w", profileErr)
	}
	material.Profiles = applied
	emitProfilePreparation(ctx, "watch_external", "", material)
	return material, nil
}

// shouldRecoverExecution decides whether one re-prepare-and-retry is warranted.
//
// The bar is deliberately high and STAGE-based, not semantic:
//
//   - the command must have run isolated (a host command has no sandbox state to
//     re-prepare);
//   - it must have produced NO output, which is the only decidable evidence that
//     nothing observable happened yet;
//   - the failure must classify as a sandbox/state write denial;
//   - a tool environment profile must have matched, so re-preparing can actually
//     change the outcome.
//
// Everything else returns false: a network denial goes through capability
// approval and an explicit re-run, and a command that printed anything is never
// replayed.
func shouldRecoverExecution(decision SandboxDecision, material execMaterial, exitCode int, err error, output string, ctx context.Context) bool {
	if err == nil || decision.Mode != SandboxIsolated {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if strings.TrimSpace(output) != "" {
		return false
	}
	if len(material.Profiles) == 0 {
		return false
	}
	class, denied := classifySandboxDenial(exitCode, err, output)
	return denied && (class == "sandbox_fs_denied" || class == "credential_state_readonly")
}

// execMaterialForRequest is the request-shaped entry to material preparation.
// The writable view comes from the REQUEST's workspace roots, so the engine no
// longer depends on re-deriving them from the argument map.
func execMaterialForRequest(req ExecutionRequest, args map[string]interface{}) execMaterial {
	if req.Durable != nil {
		material, err := durableExecMaterial(contextFromArgs(args), req.Payload, req.CWD, *req.Durable)
		if err != nil {
			material.ProfileError = err
		}
		return material
	}
	material := execMaterialForArgs(args, absoluteCWD(req.CWD))
	for _, root := range req.WorkspaceRoots {
		if strings.TrimSpace(root) != "" {
			material.WritableRoots = append(material.WritableRoots, root)
		}
	}
	return material
}

// execCommandProgramsForPayload extracts the program set from a raw payload, for
// callers that hold a request rather than an argument map.
func execCommandProgramsForPayload(toolName, payload string) []string {
	return execCommandPrograms(toolName, map[string]interface{}{
		"_tool_name": toolName,
		"command":    payload,
	})
}

// credentialReadAllowed reports whether this call may use operator credential
// state. The middleware resolves it before execution (including any human ask);
// the lease's own capabilities are the fallback for callers that bypass the
// middleware, such as a durable check.
func credentialReadAllowed(scope ExecutionScope, args map[string]interface{}) bool {
	if allowed, ok := args[credentialReadArgKey].(bool); ok {
		return allowed
	}
	return scopeHasCapability(scope, executionenv.CapabilityCredentialRead)
}

// backgroundProcessCeiling bounds a detached command. Nothing else will stop it:
// the run that started it ends, and a wedged process would otherwise hold a
// process slot, its scratch, and its copied credential state until the daemon
// restarts. An explicit timeout wins when the caller gave one.
func backgroundProcessCeiling(args map[string]interface{}) time.Duration {
	if profile, err := resolveToolProfile(args, 30); err == nil {
		if profile.RequestedTimeout > 0 {
			return profile.RequestedTimeout
		}
		if profile.Class == ToolExecutionLongRunning {
			return profile.MaxTimeout
		}
	}
	return 2 * time.Hour
}
