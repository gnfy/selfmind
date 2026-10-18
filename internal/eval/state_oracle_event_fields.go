package eval

import (
	"encoding/json"
	"strings"
)

// Exact event fields avoid dependence on provider whitespace or JSON key order.
// Encoded objects (notably tool args) are data only; malformed input fails closed.
func eventPayloadFieldsMatch(raw []byte, fields map[string]string) bool {
	if len(fields) == 0 {
		return true
	}
	var root interface{}
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	for path, want := range fields {
		value := root
		for _, key := range strings.Split(path, ".") {
			if key == "" {
				return false
			}
			if text, encoded := value.(string); encoded {
				if json.Unmarshal([]byte(text), &value) != nil {
					return false
				}
			}
			object, ok := value.(map[string]interface{})
			if !ok {
				return false
			}
			value, ok = object[key]
			if !ok {
				return false
			}
		}
		actual, ok := value.(string)
		if !ok || actual != want {
			return false
		}
	}
	return true
}
