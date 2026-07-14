package control

import "sync"

// eventAppendBus reports committed task-event appends to daemon-local
// subscribers. It is an observation seam only: task_events remains the source
// of truth and a slow subscriber is allowed to miss a notification because it
// can replay from the explicit durable cursor.
type eventAppendBus struct {
	mu        sync.RWMutex
	nextID    uint64
	listeners map[uint64]func(Event)
}

func newEventAppendBus() *eventAppendBus {
	return &eventAppendBus{listeners: make(map[uint64]func(Event))}
}

func (b *eventAppendBus) subscribe(listener func(Event)) func() {
	if b == nil || listener == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.listeners[id] = listener
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.listeners, id)
			b.mu.Unlock()
		})
	}
}

func (b *eventAppendBus) publish(event Event) {
	if b == nil {
		return
	}
	b.mu.RLock()
	listeners := make([]func(Event), 0, len(b.listeners))
	for _, listener := range b.listeners {
		listeners = append(listeners, listener)
	}
	b.mu.RUnlock()
	for _, listener := range listeners {
		listener(event)
	}
}

// SubscribeEventAppends observes newly committed events. Callers must still
// use ListPersonEventsAfter for reconnect/replay correctness.
func (s *Store) SubscribeEventAppends(listener func(Event)) func() {
	if s == nil || s.events == nil {
		return func() {}
	}
	return s.events.subscribe(listener)
}
