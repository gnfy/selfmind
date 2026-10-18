package tools

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// One walker owns coercion and validation for every tool, independent of the
// model or wire protocol. Refusals never dispatch a partially corrected call.
func checkToolArguments(schema ToolSchema, args map[string]interface{}, coerce, validate bool) (map[string]interface{}, error) {
	check := argumentCheck{coerce: coerce, validate: validate}
	out := check.object("", args, schema.Properties, schema.Required, schema.AdditionalProperties, true)
	if len(check.issues) != 0 {
		return nil, errors.Join(check.issues...)
	}
	return out, nil
}

type argumentCheck struct {
	coerce, validate bool
	issues           []error
}

// Eight independent errors are enough for one correction without flooding the
// model with an unbounded list from a malformed array. Later fields stay refused.
func (c *argumentCheck) full() bool { return len(c.issues) >= 8 }
func (c *argumentCheck) add(err error) {
	if err != nil && !c.full() {
		c.issues = append(c.issues, err)
	}
}

func (c *argumentCheck) object(path string, value map[string]interface{}, properties map[string]PropertyDef, required []string, additional *bool, root bool) map[string]interface{} {
	out := make(map[string]interface{}, len(value))
	if c.validate {
		for _, name := range required {
			if _, ok := value[name]; !ok {
				c.add(fmt.Errorf("missing required parameter: %s — expected %s", joinParameterPath(path, name), expectedParameter(properties[name])))
			}
		}
	}
	names := make([]string, 0, len(value))
	for name := range value {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c.full() {
			break
		}
		item := value[name]
		if root && strings.HasPrefix(name, "_") {
			out[name] = item
			continue
		}
		def, ok := properties[name]
		if !ok {
			if additional != nil && !*additional {
				c.add(unknownParameterError(path, name, properties))
			} else {
				out[name] = item
			}
			continue
		}
		if c.coerce && item == nil && def.Type != "" && def.Type != "null" {
			if stringInList(name, required) {
				c.add(fmt.Errorf("required parameter %s cannot be null", joinParameterPath(path, name)))
			}
			continue
		}
		out[name] = c.property(joinParameterPath(path, name), item, def)
	}
	return out
}

func (c *argumentCheck) property(path string, value interface{}, def PropertyDef) interface{} {
	if c.coerce {
		var err error
		value, err = coerceValue(path, value, def.Type)
		if err != nil {
			c.add(err)
			return nil
		}
	}
	if c.validate {
		if err := validateType(path, value, def.Type); err != nil {
			c.add(err)
			return nil
		}
		if len(def.Enum) > 0 {
			text, ok := value.(string)
			if !ok || !stringInList(text, def.Enum) {
				c.add(fmt.Errorf("parameter %s must be one of %s, got %q", path, strings.Join(def.Enum, ", "), text))
				return nil
			}
		}
	}
	switch def.Type {
	case "object":
		if object, ok := value.(map[string]interface{}); ok {
			return c.object(path, object, def.Properties, def.Required, def.AdditionalProperties, false)
		}
	case "array":
		if items, ok := value.([]interface{}); ok && def.Items != nil {
			out := make([]interface{}, len(items))
			for i, item := range items {
				if c.full() {
					break
				}
				out[i] = c.property(fmt.Sprintf("%s[%d]", path, i), item, *def.Items)
			}
			return out
		}
	}
	return value
}
