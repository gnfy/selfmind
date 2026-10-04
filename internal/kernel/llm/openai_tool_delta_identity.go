package llm

import "fmt"

// A missing stream index is not index zero. Some compatible protocols send
// complete calls identified only by ID. An unidentifiable fragment must never
// be joined to one of several calls, nor may an index overwrite another ID.
func openAIToolDeltaIndex(acc map[int]*OpenAIToolCall, delta openAIToolCallDelta) (int, error) {
	if delta.Index != nil {
		index := *delta.Index
		if index < 0 {
			return 0, fmt.Errorf("openai stream: negative tool call index")
		}
		if call := acc[index]; call != nil && call.ID != "" && delta.ID != nil && *delta.ID != "" && call.ID != *delta.ID {
			return 0, fmt.Errorf("openai stream: tool call index changed identity")
		}
		if delta.ID != nil && *delta.ID != "" {
			for slot, call := range acc {
				if call.ID == *delta.ID && slot != index {
					return 0, fmt.Errorf("openai stream: tool call ID changed index")
				}
			}
		}
		return index, nil
	}
	if delta.ID != nil && *delta.ID != "" {
		for index, call := range acc {
			if call.ID == *delta.ID {
				return index, nil
			}
		}
		if len(acc) == 1 {
			for index, call := range acc {
				if call.ID == "" {
					return index, nil
				}
			}
		}
		index := 0
		for slot := range acc {
			if slot >= index {
				index = slot + 1
			}
		}
		return index, nil
	}
	if len(acc) > 1 {
		return 0, fmt.Errorf("openai stream: tool fragment has no unambiguous ID or index")
	}
	for index := range acc {
		return index, nil
	}
	return 0, nil
}
