package llm

import (
	"bytes"
	"strings"
)

func sseDataBytes(line []byte) ([]byte, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(line[len("data:"):]), true
}

func sseDataString(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(line[len("data:"):]), true
}
