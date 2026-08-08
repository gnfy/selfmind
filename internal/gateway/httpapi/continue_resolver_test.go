package httpapi

import "testing"

func TestAffirmativeExecutionPhrases(t *testing.T) {
	for _, input := range []string{"开始执行", "执行吧", "请执行"} {
		if !looksLikeAffirmativeContinuation(input) {
			t.Fatalf("%q should be a deterministic continuation acknowledgement", input)
		}
	}
}
