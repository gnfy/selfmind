package router

import "strings"

// ShouldStreamToClient returns true only for interactive clients that can render
// incremental model output without creating a noisy notification stream.
func ShouldStreamToClient(channel string) bool {
	switch normalizeChannel(channel) {
	case "cli", "terminal", "tui":
		return true
	default:
		return false
	}
}

// WorkingNotice is the one-shot acknowledgement used by message-based channels.
func WorkingNotice(channel string) string {
	if ShouldStreamToClient(channel) {
		return ""
	}
	return "已收到，AI 正在处理，完成后我会把结果发到这里。"
}

func normalizeChannel(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	channel = strings.TrimPrefix(channel, "#")
	return channel
}
