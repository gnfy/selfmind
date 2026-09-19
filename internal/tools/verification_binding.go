package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func verificationBindingProperty() PropertyDef {
	return PropertyDef{Type: "object", Description: "Stable proof obligation. Omission binds only to an active verification_required plan step; otherwise provide an explicit criterion and target or step_id. For a correction supply replaces and reason; omitted identity and dependency fields are inherited from the recorded check. Use terminal for exploratory probes that do not test an acceptance condition.", Properties: map[string]PropertyDef{
		"step_id":            {Type: "string", Description: "Existing plan step whose acceptance criterion this check proves. Omit criterion to inherit its durable criterion; never invent a step id."},
		"criterion":          {Type: "string", Description: "Observable condition being verified; preserve it across retries."},
		"target":             {Type: "string", Description: "Exact artifact or operation this condition concerns."},
		"replaces":           {Type: "string", Description: "Previous verification evidence id in this Run, only when correcting or retrying that check. Checks without declared local_dependencies retain their original open-work-unit boundary."},
		"reason":             {Type: "string", Description: "Why the new attempt addresses the old failure without changing the acceptance condition."},
		"local_dependencies": {Type: "array", Items: &PropertyDef{Type: "string"}, Description: "Local files or directories whose changes can invalidate this criterion, including source, configuration and check scripts. Relative to cwd. Use [] only when this criterion is independent of local files (for example a direct external observation). Omission conservatively depends on all local changes. Preserve this declaration when replacing a check; declaring inputs does not prove the criterion or grant permission."},
	}}
}

func prepareVerificationBinding(args map[string]interface{}) (*verification.Binding, error) {
	raw, explicit := args["check"]
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
	if err := resolveVerificationDependencies(args, &b); err != nil {
		return nil, err
	}
	b.Criterion = strings.TrimSpace(b.Criterion)
	b.Target = strings.TrimSpace(b.Target)
	b.Replaces = strings.TrimSpace(b.Replaces)
	b.Reason = strings.TrimSpace(b.Reason)
	if resolver, ok := runPlanProjectionFromArgs(args).(verificationBindingResolver); ok {
		resolved, err := resolver.ResolveVerification(ContextFromArgs(args), b, stringArg(args, "cwd"))
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			b = *resolved
			if err := resolveVerificationDependencies(args, &b); err != nil {
				return nil, err
			}
		} else if !explicit {
			return nil, nil
		}
	} else if !explicit {
		return nil, nil
	}
	if !verification.ValidBinding(&b) || len(b.Criterion) > 2000 || len(b.Target) > 1000 || len(b.Reason) > 2000 || len(b.Replaces) > 256 {
		return nil, fmt.Errorf("verification check needs a bounded nonempty criterion and target")
	}
	if b.Replaces != "" {
		if b.Reason == "" {
			return nil, fmt.Errorf("replacing verification needs a reason explaining the unchanged criterion")
		}
		validator, ok := runPlanProjectionFromArgs(args).(verificationBindingValidator)
		if !ok {
			return nil, fmt.Errorf("verification replacement is unavailable without a durable run evidence resolver")
		}
		if err := validator.ValidateVerification(ContextFromArgs(args), b, stringArg(args, "cwd")); err != nil {
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
				path = filepath.Join(absoluteCWD(stringArg(args, "cwd")), path)
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
