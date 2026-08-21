package tools

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The `selfmind` metadata block of a tool definition is untyped wire data: it
// crosses into internal/kernel (which must not import this package) and into
// provider adapters that marshal it as JSON. A named string type stored there
// looks correct in Go but makes every consumer's `.(string)` assertion fail
// silently, which is how the hidden/deferred exposure filter was inert while
// its unit tests passed on hand-written literals. These tests pin the wire
// shape against the REAL registry so the same class of defect cannot return.

type exposureWireTestTool struct{ BaseTool }

func newExposureWireTestTool(name string, exposure ToolExposure) *exposureWireTestTool {
	return &exposureWireTestTool{BaseTool: BaseTool{
		name: name, description: "exposure wire probe",
		schema: ToolSchema{Type: "object", Properties: map[string]PropertyDef{
			"target": {Type: "string"},
		}},
		metadata: ToolMetadata{Exposure: exposure, Category: "test", RiskLevel: ToolRiskLow},
		handler:  func(map[string]interface{}) (string, error) { return "ok", nil },
	}}
}

// TestToolDefinitionMetadataIsJSONPrimitive pins that every value in the
// definition's `selfmind` block already has a JSON-primitive dynamic type, so a
// consumer asserting `.(string)` sees the real value. Comparing against a JSON
// round trip catches any future named-type field, not just `exposure`.
func TestToolDefinitionMetadataIsJSONPrimitive(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newExposureWireTestTool("wire_direct", ToolExposureDirect))
	reg.Register(newExposureWireTestTool("wire_deferred", ToolExposureDeferred))
	reg.Register(newExposureWireTestTool("wire_hidden", ToolExposureHidden))

	defs := reg.ToolDefinitions()
	if len(defs) != 3 {
		t.Fatalf("definitions = %d, want 3", len(defs))
	}
	for _, def := range defs {
		metadata, ok := def["selfmind"].(map[string]interface{})
		if !ok {
			t.Fatalf("definition %v has no selfmind metadata block", def["name"])
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		var roundTripped map[string]interface{}
		if err := json.Unmarshal(raw, &roundTripped); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		for key, value := range metadata {
			// A JSON round trip normalizes numbers to float64 and slices to
			// []interface{}; those are expected. What must NOT differ is a
			// named string type collapsing to a plain string.
			if _, isString := value.(string); !isString {
				continue
			}
			if reflect.TypeOf(value) != reflect.TypeOf(roundTripped[key]) {
				t.Errorf("metadata[%q] dynamic type %T is not the wire type %T; convert it to string at the producer",
					key, value, roundTripped[key])
			}
		}
	}
}

// TestToolDefinitionExposureIsAssertableString is the direct regression: the
// exposure a consumer reads out of the definition map must equal the registry's
// effective exposure AND survive a plain string assertion.
func TestToolDefinitionExposureIsAssertableString(t *testing.T) {
	reg := NewRegistry()
	want := map[string]ToolExposure{
		"wire_direct":   ToolExposureDirect,
		"wire_deferred": ToolExposureDeferred,
		"wire_hidden":   ToolExposureHidden,
	}
	for name, exposure := range want {
		reg.Register(newExposureWireTestTool(name, exposure))
	}

	seen := map[string]string{}
	for _, def := range reg.ToolDefinitions() {
		metadata, _ := def["selfmind"].(map[string]interface{})
		exposure, ok := metadata["exposure"].(string)
		if !ok {
			t.Fatalf("exposure for %v is %T, not string — consumers assert .(string)",
				def["name"], metadata["exposure"])
		}
		function, _ := def["function"].(map[string]interface{})
		toolName, _ := function["name"].(string)
		seen[toolName] = exposure
	}
	for name, exposure := range want {
		if seen[name] != string(exposure) {
			t.Errorf("definition exposure for %s = %q, want %q", name, seen[name], exposure)
		}
		if got := reg.EffectiveToolExposure(name); got != exposure {
			t.Errorf("EffectiveToolExposure(%s) = %q, want %q", name, got, exposure)
		}
	}
}

// TestLookupToolExposureMatchesDefinitionScan pins that the single-lookup path
// used by the dispatch-time availability guard agrees with the catalogue it
// replaced. The guard previously rebuilt every definition (a JSON round trip
// per registered tool, inside the streaming loop) just to answer one name.
func TestLookupToolExposureMatchesDefinitionScan(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newExposureWireTestTool("lookup_direct", ToolExposureDirect))
	reg.Register(newExposureWireTestTool("lookup_deferred", ToolExposureDeferred))
	reg.Register(newExposureWireTestTool("lookup_hidden", ToolExposureHidden))

	fromDefinitions := map[string]string{}
	for _, def := range reg.ToolDefinitions() {
		function, _ := def["function"].(map[string]interface{})
		name, _ := function["name"].(string)
		metadata, _ := def["selfmind"].(map[string]interface{})
		exposure, _ := metadata["exposure"].(string)
		fromDefinitions[name] = exposure
	}
	if len(fromDefinitions) != 3 {
		t.Fatalf("definitions = %v", fromDefinitions)
	}
	for name, want := range fromDefinitions {
		got, known := reg.LookupToolExposure(name)
		if !known {
			t.Errorf("LookupToolExposure(%s) reported unknown but the tool is in the catalogue", name)
			continue
		}
		if string(got) != want {
			t.Errorf("LookupToolExposure(%s) = %q, definition says %q", name, got, want)
		}
	}

	if _, known := reg.LookupToolExposure("never_registered"); known {
		t.Error("an unregistered name must report unknown so callers keep their fallback")
	}

	disp := &Dispatcher{registry: reg}
	exposure, known := disp.ResolveToolExposure("lookup_hidden")
	if !known || exposure != string(ToolExposureHidden) {
		t.Fatalf("ResolveToolExposure(lookup_hidden) = %q known=%v", exposure, known)
	}
	if _, known := disp.ResolveToolExposure("never_registered"); known {
		t.Error("Dispatcher.ResolveToolExposure must report unknown for an unregistered name")
	}
}
