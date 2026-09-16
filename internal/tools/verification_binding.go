package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/verification"
)

type verificationBindingValidator interface {
	ValidateVerification(context.Context, verification.Binding, string) error
}

func verificationBindingProperty() PropertyDef {
	return PropertyDef{Type: "object", Description: "Optional stable proof obligation. Keep criterion and target unchanged when correcting a check method; cite the previous verification evidence id and explain why the corrected method proves the same condition. This is not permission to weaken a failed test.", Properties: map[string]PropertyDef{
		"criterion": {Type: "string", Description: "Observable condition being verified; preserve it across retries."},
		"target":    {Type: "string", Description: "Exact artifact or operation this condition concerns."},
		"replaces":  {Type: "string", Description: "Previous verification evidence id in this work unit, only when correcting or retrying that check."},
		"reason":    {Type: "string", Description: "Why the new attempt addresses the old failure without changing the acceptance condition."},
	}, Required: []string{"criterion", "target"}}
}

func prepareVerificationBinding(args map[string]interface{}) (*verification.Binding, error) {
	raw, ok := args["check"]
	if !ok {
		return nil, nil
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
	b.Criterion = strings.TrimSpace(b.Criterion)
	b.Target = strings.TrimSpace(b.Target)
	b.Replaces = strings.TrimSpace(b.Replaces)
	b.Reason = strings.TrimSpace(b.Reason)
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
