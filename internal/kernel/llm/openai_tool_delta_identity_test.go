package llm

import (
	"encoding/json"
	"testing"
)

func TestOpenAIToolDeltaIdentityRejectsAmbiguousOrConflictingFragments(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"interleaved stable IDs", `[{"id":"a","function":{"name":"read_file","arguments":"{"}},{"id":"b","function":{"name":"verify","arguments":"{}"}},{"id":"a","function":{"arguments":"}"}}]`, false},
		{"fragment without identity", `[{"id":"a","function":{"name":"read_file"}},{"id":"b","function":{"name":"verify"}},{"function":{"arguments":"{}"}}]`, true},
		{"one index two identities", `[{"index":0,"id":"a","function":{"name":"read_file"}},{"index":0,"id":"b","function":{"name":"verify"}}]`, true},
		{"one identity two indexes", `[{"index":0,"id":"a","function":{"name":"read_file"}},{"index":1,"id":"a","function":{"arguments":"{}"}}]`, true},
		{"ordinary indexed fragments", `[{"index":0,"id":"a","function":{"name":"read_","arguments":"{"}},{"index":0,"function":{"name":"file","arguments":"}"}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deltas []openAIToolCallDelta
			if err := json.Unmarshal([]byte(tc.raw), &deltas); err != nil {
				t.Fatal(err)
			}
			acc := map[int]*OpenAIToolCall{}
			err := accumulateOpenAIToolDeltas(acc, deltas)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v calls=%+v", err, orderedOpenAIToolCalls(acc))
			}
			if !tc.wantError && (acc[0].Function.Arguments != "{}" || acc[0].Function.Name != "read_file") {
				t.Fatalf("call identity lost: %+v", acc[0])
			}
		})
	}
}
