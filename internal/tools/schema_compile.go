package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ToolSchemaStatus is the registration-time result of compiling a tool's
// provider-neutral JSON schema. Quarantined tools remain visible to
// diagnostics, but are never advertised to a model or dispatched.
type ToolSchemaStatus string

const (
	ToolSchemaActive      ToolSchemaStatus = "active"
	ToolSchemaRepaired    ToolSchemaStatus = "repaired"
	ToolSchemaQuarantined ToolSchemaStatus = "quarantined"
)

type ToolSchemaOrigin string

const (
	ToolSchemaOriginBuiltin  ToolSchemaOrigin = "builtin"
	ToolSchemaOriginExternal ToolSchemaOrigin = "external"
)

type ToolSchemaSeverity string

const (
	ToolSchemaInfo    ToolSchemaSeverity = "info"
	ToolSchemaWarning ToolSchemaSeverity = "warning"
	ToolSchemaError   ToolSchemaSeverity = "error"
)

// ToolSchemaIssue is deliberately content-free: diagnostics expose paths and
// error classes, never defaults, enum values, or other potentially sensitive
// schema content supplied by an external tool server.
type ToolSchemaIssue struct {
	Severity  ToolSchemaSeverity `json:"severity"`
	Code      string             `json:"code"`
	Path      string             `json:"path"`
	Message   string             `json:"message"`
	AutoFixed bool               `json:"auto_fixed,omitempty"`
}

// ToolSchemaReport is the public, redacted catalogue entry used by /diag.
type ToolSchemaReport struct {
	Name     string            `json:"name"`
	Origin   ToolSchemaOrigin  `json:"origin"`
	Status   ToolSchemaStatus  `json:"status"`
	Exposure ToolExposure      `json:"exposure"`
	Hash     string            `json:"hash"`
	Issues   []ToolSchemaIssue `json:"issues,omitempty"`
}

type compiledToolSchema struct {
	Report     ToolSchemaReport
	Parameters map[string]interface{}
}

// RawToolSchemaProvider lets MCP and future plugin tools preserve their full
// JSON Schema instead of being lossy-converted through ToolSchema.
type RawToolSchemaProvider interface {
	RawToolSchema() map[string]interface{}
}

// ToolSchemaOriginProvider marks schemas that must fail locally rather than
// fail daemon startup. Built-in tools intentionally default to strict.
type ToolSchemaOriginProvider interface {
	SchemaOrigin() ToolSchemaOrigin
}

func compileToolSchema(t Tool) compiledToolSchema {
	report := ToolSchemaReport{Status: ToolSchemaActive, Origin: ToolSchemaOriginBuiltin}
	if t == nil {
		report.Status = ToolSchemaQuarantined
		report.Issues = []ToolSchemaIssue{{Severity: ToolSchemaError, Code: "nil_tool", Path: "$", Message: "tool is nil"}}
		return compiledToolSchema{Report: report, Parameters: emptyObjectSchema()}
	}
	report.Name = strings.TrimSpace(t.Name())
	if provider, ok := t.(ToolSchemaOriginProvider); ok {
		if origin := provider.SchemaOrigin(); origin != "" {
			report.Origin = origin
		}
	}

	var source map[string]interface{}
	if provider, ok := t.(RawToolSchemaProvider); ok {
		source = provider.RawToolSchema()
	} else {
		source = parametersFromToolSchema(t.Schema())
	}
	parameters, err := detachedSchemaMap(source)
	if err != nil {
		report.Status = ToolSchemaQuarantined
		report.Issues = append(report.Issues, ToolSchemaIssue{
			Severity: ToolSchemaError, Code: "not_json_serializable", Path: "$", Message: "schema is not JSON serializable",
		})
		parameters = emptyObjectSchema()
	} else {
		compiler := schemaCompiler{}
		compiler.compileRoot(parameters)
		if report.Origin == ToolSchemaOriginExternal {
			compiler.rejectReservedRootProperties(parameters)
		}
		report.Issues = compiler.issues
		report.Status = compiler.status()
	}

	bytes, _ := json.Marshal(parameters)
	digest := sha256.Sum256(bytes)
	report.Hash = hex.EncodeToString(digest[:8])
	return compiledToolSchema{Report: report, Parameters: parameters}
}

func parametersFromToolSchema(schema ToolSchema) map[string]interface{} {
	props := make(map[string]interface{}, len(schema.Properties))
	for name, property := range schema.Properties {
		props[name] = propertyDefinition(property)
	}
	out := map[string]interface{}{
		"type":       strings.TrimSpace(schema.Type),
		"properties": props,
	}
	if len(schema.Required) > 0 {
		out["required"] = append([]string(nil), schema.Required...)
	}
	return out
}

func detachedSchemaMap(source map[string]interface{}) (map[string]interface{}, error) {
	if source == nil {
		return map[string]interface{}{}, nil
	}
	bytes, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(bytes, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func emptyObjectSchema() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

type schemaCompiler struct {
	issues []ToolSchemaIssue
}

func (c *schemaCompiler) rejectReservedRootProperties(schema map[string]interface{}) {
	properties, _ := schema["properties"].(map[string]interface{})
	for name := range properties {
		if strings.HasPrefix(strings.TrimSpace(name), "_") {
			c.fail("reserved_parameter_name", "$.properties."+name, "top-level underscore parameter names are reserved by SelfMind")
		}
	}
}

func (c *schemaCompiler) status() ToolSchemaStatus {
	repaired := false
	for _, issue := range c.issues {
		if issue.Severity == ToolSchemaError {
			return ToolSchemaQuarantined
		}
		if issue.AutoFixed {
			repaired = true
		}
	}
	if repaired {
		return ToolSchemaRepaired
	}
	return ToolSchemaActive
}

func (c *schemaCompiler) compileRoot(schema map[string]interface{}) {
	if rawType, ok := schema["type"]; !ok || strings.TrimSpace(fmt.Sprint(rawType)) == "" {
		schema["type"] = "object"
		c.repair("missing_root_type", "$.type", "missing function parameter type was set to object")
	}
	if schema["type"] != "object" {
		c.fail("invalid_root_type", "$.type", "function parameters must use an object root")
	}
	c.compileNode(schema, "$", true)
}

func (c *schemaCompiler) compileNode(schema map[string]interface{}, path string, root bool) {
	types, typeKnown := schemaTypes(schema["type"])
	if schema["type"] != nil && !typeKnown {
		c.fail("invalid_type", path+".type", "type must be a supported JSON Schema type or type array")
	}
	if !typeKnown && schema["type"] == nil {
		switch {
		case schema["properties"] != nil || schema["required"] != nil:
			schema["type"] = "object"
			types = []string{"object"}
			typeKnown = true
			c.repair("inferred_object_type", path+".type", "type was inferred from object keywords")
		case schema["items"] != nil:
			schema["type"] = "array"
			types = []string{"array"}
			typeKnown = true
			c.repair("inferred_array_type", path+".type", "type was inferred from array items")
		case hasComposition(schema):
			// A composition-only schema is valid. Its branches are checked below.
		default:
			c.fail("missing_type", path+".type", "schema node has no type or composition")
		}
	}
	if root && (len(types) != 1 || types[0] != "object") {
		c.fail("invalid_root_type", path+".type", "function parameters must resolve to exactly one object type")
	}
	if !containsType(types, "object") {
		if _, exists := schema["properties"]; exists {
			c.fail("object_keyword_type_mismatch", path+".properties", "properties require an object-capable schema")
		}
		if _, exists := schema["required"]; exists {
			c.fail("object_keyword_type_mismatch", path+".required", "required requires an object-capable schema")
		}
	}
	if !containsType(types, "array") {
		if _, exists := schema["items"]; exists {
			c.fail("array_keyword_type_mismatch", path+".items", "items require an array-capable schema")
		}
	}

	if containsType(types, "object") {
		c.compileObject(schema, path)
	}
	if containsType(types, "array") {
		c.compileArray(schema, path)
	}
	c.compileEnumAndDefault(schema, path, types)
	c.compileSubschemas(schema, path)
	c.cleanMetadata(schema, path)
}

func (c *schemaCompiler) compileObject(schema map[string]interface{}, path string) {
	rawProps, exists := schema["properties"]
	if !exists || rawProps == nil {
		schema["properties"] = map[string]interface{}{}
		c.repair("missing_properties", path+".properties", "missing object properties were set to an empty object")
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		c.fail("invalid_properties", path+".properties", "properties must be an object")
		props = map[string]interface{}{}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		childPath := path + ".properties." + name
		switch child := props[name].(type) {
		case map[string]interface{}:
			c.compileNode(child, childPath, false)
		case bool:
			// Boolean schemas are valid JSON Schema and are preserved verbatim.
		default:
			c.fail("invalid_property_schema", childPath, "property schema must be an object or boolean")
		}
	}

	required, present := schema["required"]
	if !present {
		return
	}
	if required == nil {
		delete(schema, "required")
		c.repair("null_required", path+".required", "null required list was removed")
		return
	}
	items, ok := required.([]interface{})
	if !ok {
		c.fail("invalid_required", path+".required", "required must be an array of property names")
		return
	}
	seen := make(map[string]struct{}, len(items))
	normalized := make([]interface{}, 0, len(items))
	for index, item := range items {
		name, ok := item.(string)
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			c.fail("invalid_required_entry", fmt.Sprintf("%s.required[%d]", path, index), "required entries must be non-empty strings")
			continue
		}
		if _, exists := props[name]; !exists {
			c.fail("unknown_required_property", fmt.Sprintf("%s.required[%d]", path, index), "required entry does not name a declared property")
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			c.repair("duplicate_required", fmt.Sprintf("%s.required[%d]", path, index), "duplicate required entry was removed")
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		delete(schema, "required")
		c.repair("empty_required", path+".required", "empty required list was removed")
		return
	}
	schema["required"] = normalized
}

func (c *schemaCompiler) compileArray(schema map[string]interface{}, path string) {
	items, exists := schema["items"]
	if !exists || items == nil {
		schema["items"] = map[string]interface{}{}
		c.repair("missing_items", path+".items", "array items were set to an unconstrained schema")
		return
	}
	switch child := items.(type) {
	case map[string]interface{}:
		if len(child) > 0 {
			c.compileNode(child, path+".items", false)
		}
	case bool:
	default:
		c.fail("invalid_items", path+".items", "items must be an object or boolean schema")
	}
}

func (c *schemaCompiler) compileEnumAndDefault(schema map[string]interface{}, path string, types []string) {
	if rawEnum, exists := schema["enum"]; exists {
		values, ok := rawEnum.([]interface{})
		if !ok || len(values) == 0 {
			c.fail("invalid_enum", path+".enum", "enum must be a non-empty array")
		} else {
			for index, value := range values {
				if len(types) > 0 && !matchesAnyJSONType(value, types) {
					c.fail("enum_type_mismatch", fmt.Sprintf("%s.enum[%d]", path, index), "enum value does not match the declared type")
				}
			}
		}
	}
	if value, exists := schema["default"]; exists && len(types) > 0 && !matchesAnyJSONType(value, types) {
		delete(schema, "default")
		c.repair("default_type_mismatch", path+".default", "default value did not match the declared type and was removed")
	}
}

func (c *schemaCompiler) compileSubschemas(schema map[string]interface{}, path string) {
	if raw, exists := schema["$ref"]; exists {
		if ref, ok := raw.(string); !ok || strings.TrimSpace(ref) == "" {
			c.fail("invalid_ref", path+".$ref", "$ref must be a non-empty string")
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		raw, exists := schema[keyword]
		if !exists {
			continue
		}
		branches, ok := raw.([]interface{})
		if !ok || len(branches) == 0 {
			c.fail("invalid_"+keyword, path+"."+keyword, keyword+" must be a non-empty array of schemas")
			continue
		}
		for index, branch := range branches {
			branchPath := fmt.Sprintf("%s.%s[%d]", path, keyword, index)
			switch child := branch.(type) {
			case map[string]interface{}:
				if len(child) > 0 {
					c.compileNode(child, branchPath, false)
				}
			case bool:
			default:
				c.fail("invalid_composition_branch", branchPath, "composition branch must be an object or boolean schema")
			}
		}
	}
	for _, keyword := range []string{"not", "additionalProperties"} {
		raw, exists := schema[keyword]
		if !exists {
			continue
		}
		switch child := raw.(type) {
		case map[string]interface{}:
			if len(child) > 0 {
				c.compileNode(child, path+"."+keyword, false)
			}
		case bool:
		default:
			c.fail("invalid_"+keyword, path+"."+keyword, keyword+" must be an object or boolean schema")
		}
	}
	for _, keyword := range []string{"$defs", "definitions"} {
		raw, exists := schema[keyword]
		if !exists {
			continue
		}
		defs, ok := raw.(map[string]interface{})
		if !ok {
			c.fail("invalid_"+keyword, path+"."+keyword, keyword+" must be an object")
			continue
		}
		for name, rawDef := range defs {
			child, ok := rawDef.(map[string]interface{})
			if !ok {
				if _, boolean := rawDef.(bool); !boolean {
					c.fail("invalid_definition", path+"."+keyword+"."+name, "definition must be an object or boolean schema")
				}
				continue
			}
			if len(child) > 0 {
				c.compileNode(child, path+"."+keyword+"."+name, false)
			}
		}
	}
}

func (c *schemaCompiler) cleanMetadata(schema map[string]interface{}, path string) {
	for _, keyword := range []string{"title", "description"} {
		if value, exists := schema[keyword]; exists {
			if _, ok := value.(string); !ok {
				delete(schema, keyword)
				c.repair("invalid_"+keyword, path+"."+keyword, keyword+" was not a string and was removed")
			}
		}
	}
}

func (c *schemaCompiler) repair(code, path, message string) {
	c.issues = append(c.issues, ToolSchemaIssue{Severity: ToolSchemaWarning, Code: code, Path: path, Message: message, AutoFixed: true})
}

func (c *schemaCompiler) fail(code, path, message string) {
	c.issues = append(c.issues, ToolSchemaIssue{Severity: ToolSchemaError, Code: code, Path: path, Message: message})
}

func schemaTypes(raw interface{}) ([]string, bool) {
	valid := map[string]bool{"null": true, "boolean": true, "object": true, "array": true, "number": true, "string": true, "integer": true}
	switch value := raw.(type) {
	case string:
		value = strings.TrimSpace(value)
		if !valid[value] {
			return nil, false
		}
		return []string{value}, true
	case []interface{}:
		if len(value) == 0 {
			return nil, false
		}
		seen := make(map[string]struct{}, len(value))
		out := make([]string, 0, len(value))
		for _, item := range value {
			name, ok := item.(string)
			name = strings.TrimSpace(name)
			if !ok || !valid[name] {
				return nil, false
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
		return out, true
	default:
		return nil, false
	}
}

func containsType(types []string, target string) bool {
	for _, item := range types {
		if item == target {
			return true
		}
	}
	return false
}

func hasComposition(schema map[string]interface{}) bool {
	return schema["oneOf"] != nil || schema["anyOf"] != nil || schema["allOf"] != nil || schema["$ref"] != nil
}

func matchesAnyJSONType(value interface{}, types []string) bool {
	for _, expected := range types {
		if matchesJSONType(value, expected) {
			return true
		}
	}
	return false
}

func matchesJSONType(value interface{}, expected string) bool {
	switch expected {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	default:
		return false
	}
}
