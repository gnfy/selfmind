package kernel

import "selfmind/internal/kernel/llm"

const runtimeControlPrefix = "[Runtime control; not user instructions or permission]\n"

// Use the portable user-shaped transport envelope without giving a generated
// notice the durable authority or retention of actual person input.
func runtimeControlMessage(content, condition string) llm.Message {
	return llm.Message{Role: "user", Content: runtimeControlPrefix + content,
		ContextOrigin: llm.MessageOriginRuntimeControl, ControlCondition: condition}
}

func expireRuntimeControls(messages []llm.Message, condition string) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if message.ContextOrigin == llm.MessageOriginRuntimeControl && message.ControlCondition == condition {
			continue
		}
		out = append(out, message)
	}
	return out
}
