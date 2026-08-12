package llm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
)

// marshalWithExtraBody mirrors the OpenAI SDK escape hatch: the typed request
// establishes the normal protocol payload, then operator-supplied extra_body
// values are merged on top. Nested objects are merged recursively so adding a
// vendor field does not discard sibling protocol fields.
func marshalWithExtraBody(value interface{}, extra map[string]interface{}) ([]byte, error) {
	if len(extra) == 0 {
		return json.Marshal(value)
	}
	baseBytes, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var base map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(baseBytes)))
	decoder.UseNumber()
	if err := decoder.Decode(&base); err != nil {
		return nil, fmt.Errorf("request payload is not a JSON object: %w", err)
	}
	return json.Marshal(mergeRequestMaps(base, extra))
}

func mergeRequestMaps(base, override map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base)+len(override))
	for key, value := range base {
		out[key] = cloneRequestValue(value)
	}
	for key, value := range override {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if current, ok := out[key].(map[string]interface{}); ok {
			if incoming, ok := value.(map[string]interface{}); ok {
				out[key] = mergeRequestMaps(current, incoming)
				continue
			}
		}
		out[key] = cloneRequestValue(value)
	}
	return out
}

func cloneRequestValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return mergeRequestMaps(nil, typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = cloneRequestValue(item)
		}
		return out
	default:
		return value
	}
}

func urlWithExtraQuery(rawURL string, extra map[string]interface{}) (string, error) {
	if len(extra) == 0 {
		return rawURL, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse request URL: %w", err)
	}
	query := parsed.Query()
	for key, value := range extra {
		key = strings.TrimSpace(key)
		if key == "" || value == nil {
			continue
		}
		query.Del(key)
		for _, encoded := range queryValues(value) {
			query.Add(key, encoded)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func queryValues(value interface{}) []string {
	rv := reflect.ValueOf(value)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		out := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out = append(out, queryValue(rv.Index(i).Interface()))
		}
		return out
	}
	return []string{queryValue(value)}
}

func queryValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case map[string]interface{}:
		data, err := json.Marshal(typed)
		if err == nil {
			return string(data)
		}
	}
	return fmt.Sprint(value)
}
