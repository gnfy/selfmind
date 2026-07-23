package cli

import (
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
)

type eventSource uint8

const (
	eventSourceTurn eventSource = iota
	eventSourceWatch
)

// uiEventRef keeps daemon event identity intact until the reducer applies it.
// Watcher callbacks and the local turn stream share Bubble Tea's message queue,
// so source and run ownership must be checked at reduction time.
type uiEventRef struct {
	Source  eventSource
	RunID   string
	EventID string
	Cursor  int64
	LiveSeq uint64
}

func eventRefFromStream(event llm.StreamEvent, source eventSource) uiEventRef {
	return uiEventRef{
		Source:  source,
		RunID:   strings.TrimSpace(event.RunID),
		EventID: strings.TrimSpace(event.EventID),
		Cursor:  event.Cursor,
		LiveSeq: event.LiveSeq,
	}
}

func (r uiEventRef) key() string {
	if r.EventID != "" {
		return "event:" + r.EventID
	}
	if r.RunID != "" && r.LiveSeq > 0 {
		return fmt.Sprintf("live:%s:%d", r.RunID, r.LiveSeq)
	}
	return ""
}

func (m *uiModel) acceptEvent(ref uiEventRef) bool {
	if ref.Source == eventSourceWatch {
		if !m.watchingRun {
			return false
		}
		if m.watchedRunID != "" && ref.RunID != m.watchedRunID {
			return false
		}
	}
	key := ref.key()
	if key == "" {
		return true
	}
	if m.seenEventKeys == nil {
		m.seenEventKeys = make(map[string]struct{})
	}
	if _, exists := m.seenEventKeys[key]; exists {
		return false
	}
	m.seenEventKeys[key] = struct{}{}
	m.seenEventOrder = append(m.seenEventOrder, key)
	const maxSeenEvents = 2048
	if len(m.seenEventOrder) > maxSeenEvents {
		drop := len(m.seenEventOrder) - maxSeenEvents
		for _, old := range m.seenEventOrder[:drop] {
			delete(m.seenEventKeys, old)
		}
		m.seenEventOrder = append([]string(nil), m.seenEventOrder[drop:]...)
	}
	return true
}
