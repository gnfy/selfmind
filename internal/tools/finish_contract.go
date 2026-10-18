package tools

// Only semantic inputs belong on the normal completion surface. Runtime facts
// (files, checks, completed steps) already have durable projections. Remaining
// work and risks are optional Main judgments, not copies of the final reply.
func finishModelSchema(legacy ToolSchema) ToolSchema {
	properties := make(map[string]PropertyDef)
	for _, name := range []string{"status", "final_response", "plan_patch", "assessments", "next_steps", "risks"} {
		properties[name] = legacy.Properties[name]
	}
	properties["status"] = PropertyDef{Type: "string", Enum: []string{"done", "blocked", "failed", "waiting_external", "waiting_user"}, Description: "Main's stopping outcome for this Run. Necessary unfinished work prevents done; waiting requires an honest handoff."}
	properties["final_response"] = PropertyDef{Type: "string", Description: "Complete final reply. Written once, durably saved and shown after acceptance. Prose alone proves neither completion nor authority."}
	properties["next_steps"] = PropertyDef{Type: "array", Description: "Necessary remaining work or a handoff, when needed. Do not repeat the reply.", Items: &PropertyDef{Type: "string"}}
	properties["risks"] = PropertyDef{Type: "array", Description: "Unresolved risks needed for continuation, when any. Do not repeat the reply.", Items: &PropertyDef{Type: "string"}}
	return ToolSchema{Type: "object", AdditionalProperties: rejectAdditionalProperties(), Required: []string{"status", "final_response"}, Properties: properties}
}

func (t *FinishRunTool) argumentSchema() ToolSchema { return t.legacySchema }
