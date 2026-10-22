package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"selfmind/internal/verification"
)

type verificationBindingValidator interface {
	ValidateVerification(context.Context, verification.Binding, string) error
}

type verificationBindingResolver interface {
	ResolveVerification(context.Context, verification.Binding, string) (*verification.Binding, error)
}

// legacyVerificationStepIDArg is produced only by the compatibility
// normalizer. Native model arguments cannot forge underscore-prefixed runtime
// fields because the kernel removes them before dispatch.
const legacyVerificationStepIDArg = "_legacy_verification_step_id"

func verificationBindingProperties() map[string]PropertyDef {
	return map[string]PropertyDef{
		"step_id":            {Type: "string", Description: "Server-issued Plan step to check, including a pending step. Its stored acceptance condition is inherited. Pending steps in an open observation cycle need no transition; reopen a closed cycle as pending with update_plan mode=patch, preserving the current active step, before checking again."},
		"criterion":          {Type: "string", Description: "Condition for a standalone check (also supply target). Omit when step_id or replaces already identifies the condition. A replacement must preserve the recorded condition exactly."},
		"target":             {Type: "string", Description: "Exact artifact or operation for a standalone check. Omit to inherit from step_id or replaces. With criterion but no step_id, the check is standalone and cannot complete a Plan step."},
		"replaces":           {Type: "string", Description: "Evidence id of the check being corrected in this Run. Supply reason and keep cwd; omit criterion, target and step_id to inherit them."},
		"reason":             {Type: "string", Description: "Required with replaces: why this method checks the same unchanged condition."},
		"local_dependencies": {Type: "array", Items: &PropertyDef{Type: "string"}, Description: "Local files or directories read by this observation method whose changes can invalidate its result, including source, configuration and check scripts. Relative to cwd. Use [] only for a direct external observation independent of local files. Omission conservatively depends on all local changes; on replacement, omission inherits the prior declaration while an explicit value describes the corrected method. Declaring inputs does not prove the criterion or grant permission."},
	}
}

func normalizeVerificationArgs(args map[string]interface{}) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(args)+6)
	for key, value := range args {
		out[key] = value
	}
	raw, exists := out["check"]
	if !exists {
		return out, nil
	}
	legacy, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("parameter check must be an object, got %T", raw)
	}
	properties := verificationBindingProperties()
	for key, value := range legacy {
		destination := key
		if key == "step_id" {
			// Keep the old nested shape; the native field now exposes the same
			// server-issued identity without asking Main to move Plan state first.
			destination = legacyVerificationStepIDArg
		} else if _, known := properties[key]; !known {
			return nil, fmt.Errorf("unknown parameter: check.%s", key)
		}
		if current, present := out[destination]; present && !reflect.DeepEqual(current, value) {
			return nil, fmt.Errorf("conflicting verification parameter: %s", destination)
		}
		out[destination] = value
	}
	delete(out, "check")
	return out, nil
}

func prepareVerificationBinding(args map[string]interface{}) (*verification.Binding, error) {
	normalized, err := normalizeVerificationArgs(args)
	if err != nil {
		return nil, err
	}
	raw := map[string]interface{}{}
	explicit := false
	for name := range verificationBindingProperties() {
		if value, ok := normalized[name]; ok {
			raw[name] = value
			explicit = true
		}
	}
	if value, ok := normalized[legacyVerificationStepIDArg]; ok {
		if current, present := raw["step_id"]; present && !reflect.DeepEqual(current, value) {
			return nil, fmt.Errorf("conflicting verification parameter: step_id")
		}
		raw["step_id"] = value
		explicit = true
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var b verification.Binding
	if err = json.Unmarshal(encoded, &b); err != nil {
		return nil, fmt.Errorf("invalid verification check: %w", err)
	}
	b.Version = 1
	b.ResolvedLocalDependencies = nil // Only runtime resolution may populate this.
	b.UnresolvedLocalDependencies = nil
	if err := resolveVerificationDependencies(normalized, &b); err != nil {
		return nil, err
	}
	b.Criterion = strings.TrimSpace(b.Criterion)
	b.Target = strings.TrimSpace(b.Target)
	b.Replaces = strings.TrimSpace(b.Replaces)
	b.Reason = strings.TrimSpace(b.Reason)
	if resolver, ok := runPlanProjectionFromArgs(args).(verificationBindingResolver); ok {
		resolved, err := resolver.ResolveVerification(ContextFromArgs(normalized), b, stringArg(normalized, "cwd"))
		if err != nil {
			var closed interface{ VerificationCycleClosed() bool }
			if errors.As(err, &closed) && closed.VerificationCycleClosed() {
				return nil, &stableToolError{cause: err, code: "verification_cycle_closed", category: "stale_precondition",
					safeMessage: RedactSensitive(err.Error()), recoveryHint: "Use update_plan with mode=patch to reopen the selected step as pending, preserving the current in_progress step; then verify once with that step_id. The accepted Plan and historical evidence have not changed.",
					failurePhase: "preparation", retryability: "after_state_change", effectState: "not_dispatched"}
			}
			return nil, err
		}
		if resolved != nil {
			// The storage resolver retains legacy adoption behavior. A tool
			// caller explicitly replacing evidence must not silently become a
			// fresh observation (for example after changing cwd).
			if b.Replaces != "" && resolved.Replaces != b.Replaces {
				return nil, fmt.Errorf("verification replacement must preserve its recorded condition, target and working directory; for a new Plan observation, pass step_id without replaces")
			}
			b = *resolved
			if err := resolveVerificationDependencies(normalized, &b); err != nil {
				return nil, err
			}
		} else if !explicit {
			return nil, nil
		}
	} else if !explicit {
		return nil, nil
	}
	if b.Criterion == "" {
		return nil, fmt.Errorf("criterion is required for a standalone check; use step_id or replaces to inherit a recorded condition")
	}
	if b.Target == "" {
		return nil, fmt.Errorf("target is required for a standalone check; use step_id or replaces to inherit a recorded condition")
	}
	if !verification.ValidBinding(&b) || len(b.Criterion) > 2000 || len(b.Target) > 1000 || len(b.Reason) > 2000 || len(b.Replaces) > 256 {
		return nil, fmt.Errorf("verification fields exceed limits or have an invalid binding: criterion <= 2000, target <= 1000, reason <= 2000, replaces <= 256 bytes")
	}
	if b.Replaces != "" {
		if b.Reason == "" {
			return nil, fmt.Errorf("replacing verification needs a reason explaining the unchanged criterion")
		}
		validator, ok := runPlanProjectionFromArgs(args).(verificationBindingValidator)
		if !ok {
			return nil, fmt.Errorf("verification replacement is unavailable without a durable run evidence resolver")
		}
		if err := validator.ValidateVerification(ContextFromArgs(normalized), b, stringArg(normalized, "cwd")); err != nil {
			return nil, err
		}
	}
	return &b, nil
}

func resolveVerificationDependencies(args map[string]interface{}, b *verification.Binding) error {
	b.ResolvedLocalDependencies = nil
	b.UnresolvedLocalDependencies = nil
	var err error
	if b.LocalDependencies != nil {
		if len(*b.LocalDependencies) > 32 {
			return fmt.Errorf("verification local_dependencies exceeds 32 paths")
		}
		dependencies := []string{}
		for _, raw := range *b.LocalDependencies {
			if strings.TrimSpace(raw) == "" || len(raw) > 4096 {
				return fmt.Errorf("verification local dependency must be a bounded nonempty path")
			}
			path := raw
			if !filepath.IsAbs(path) {
				cwd := absoluteCWD(stringArg(args, "cwd"))
				candidate := filepath.Join(cwd, path)
				if _, statErr := os.Stat(candidate); statErr != nil {
					cleanRaw := filepath.Clean(raw)
					cwdBase := filepath.Base(cwd)
					if cwdBase != "" && cwdBase != "." && cwdBase != string(filepath.Separator) && strings.HasPrefix(cleanRaw, cwdBase+string(filepath.Separator)) {
						if alt := filepath.Join(filepath.Dir(cwd), cleanRaw); alt != candidate {
							if _, altErr := os.Stat(alt); altErr == nil {
								candidate = alt
							}
						}
					}
					if _, stillErr := os.Stat(candidate); stillErr != nil {
						if scope, ok := currentExecutionScope(args); ok && strings.TrimSpace(scope.WorkspaceRoot) != "" {
							if alt := filepath.Join(scope.WorkspaceRoot, cleanRaw); alt != candidate {
								if _, altErr := os.Stat(alt); altErr == nil {
									candidate = alt
								}
							}
						}
					}
				}
				path = candidate
			}
			if scope, ok := currentExecutionScope(args); ok {
				var err error
				path, err = resolveScopedPath(scope, path)
				if err != nil {
					return err
				}
			}
			// Retain the lexical input as well as its referent. Replacing a
			// symlink can change what the check reads without changing the old target.
			dependencies = append(dependencies, filepath.Clean(path))
			if _, statErr := os.Stat(path); statErr != nil {
				b.UnresolvedLocalDependencies = append(b.UnresolvedLocalDependencies, path)
			}
			path, err = canonicalContainmentPath(path)
			if err != nil {
				return fmt.Errorf("resolve verification dependency: %w", err)
			}
			b.ResolvedLocalDependencies = append(b.ResolvedLocalDependencies, path)
		}
		sort.Strings(dependencies)
		dependencies = slices.Compact(dependencies)
		sort.Strings(b.ResolvedLocalDependencies)
		b.ResolvedLocalDependencies = slices.Compact(b.ResolvedLocalDependencies)
		b.LocalDependencies = &dependencies
		if b.Version != 3 {
			b.Version = 2
		}
	}
	return nil
}
