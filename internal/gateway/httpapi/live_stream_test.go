package httpapi

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestLiveStreamHubIsPersonScopedAndNonPersistent(t *testing.T) {
	hub := newLiveStreamHub()
	a, stopA := hub.subscribe("person-a")
	defer stopA()
	b, stopB := hub.subscribe("person-b")
	defer stopB()

	hub.publish("person-a", llm.StreamEvent{EventType: "stream", Content: "hello"})
	select {
	case event := <-a:
		if event.Content != "hello" {
			t.Fatalf("content=%q", event.Content)
		}
	default:
		t.Fatal("person-a did not receive its delta")
	}
	select {
	case event := <-b:
		t.Fatalf("person-b received cross-person delta: %+v", event)
	default:
	}
}
