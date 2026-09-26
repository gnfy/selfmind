package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// gatedProvider streams one reply in chunks and opens gate once the loop has
// taken the last chunk, so a consumer that waits on gate misses the earlier
// chunks' live deltas.
type gatedProvider struct {
	chunks []string
	gate   chan struct{}
	once   sync.Once
}

func (p *gatedProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return strings.Join(p.chunks, ""), nil
}

func (p *gatedProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: strings.Join(p.chunks, ""), FinishReason: "stop"}, nil
}

func (p *gatedProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		for _, chunk := range p.chunks {
			ch <- llm.StreamEvent{Content: chunk}
		}
		p.once.Do(func() { close(p.gate) })
		ch <- llm.StreamEvent{FinishReason: "stop"}
	}()
	return ch, nil
}

type noToolBackend struct{}

func (noToolBackend) Dispatch(string, map[string]interface{}) (string, error) { return "", nil }
func (noToolBackend) GetToolDefinitions() []map[string]interface{}            { return nil }

// A consumer that falls behind loses live answer deltas: the event channel
// drops them rather than stall the run. The run's own answer still arrives
// whole on the result channel, and it says the live copy is incomplete, as
// does the turn itself, so the consumer knows which copy to keep.
func TestLostAnswerDeltasAreReportedWithTheWholeAnswer(t *testing.T) {
	chunks := []string{strings.Repeat("a", 120), strings.Repeat("b", 120), strings.Repeat("c", 120)}
	whole := strings.Join(chunks, "")
	for _, stalled := range []bool{true, false} {
		t.Run(fmt.Sprintf("stalled=%v", stalled), func(t *testing.T) {
			provider := &gatedProvider{chunks: chunks, gate: make(chan struct{})}
			events := make(chan string, 256)
			if stalled {
				events = make(chan string, 2)
				events <- "backlog"
				events <- "backlog"
			}
			var mu sync.Mutex
			var live strings.Builder
			var turn map[string]interface{}
			collect := func(raw string) {
				event, ok := kernel.DecodeAgentEvent(raw)
				if !ok {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch event.Type {
				case "stream":
					live.WriteString(event.Content)
				case "turn.completed":
					turn = event.Payload
				}
			}
			done := make(chan struct{})
			var reader sync.WaitGroup
			reader.Add(1)
			go func() {
				defer reader.Done()
				if stalled {
					<-provider.gate
				}
				for {
					select {
					case raw := <-events:
						collect(raw)
					case <-done:
						return
					}
				}
			}()

			agent := kernel.NewAgent(memory.NewMemoryManager(nil), noToolBackend{}, provider, "helpful", 3, 1, nil)
			resp, err := NewGateway(agent, nil).RunAgent(kernel.WithEventChannel(context.Background(), events), "user", "cli", "write the report")
			if err != nil {
				t.Fatal(err)
			}
			var final llm.StreamEvent
			for event := range resp.Stream {
				if event.Err != nil {
					t.Fatal(event.Err)
				}
				if event.Content != "" {
					final = event
				}
			}
			close(done)
			reader.Wait()
			for len(events) > 0 {
				collect(<-events)
			}

			flagged, _ := final.Payload["stream_incomplete"].(bool)
			turnFlagged, _ := turn["stream_incomplete"].(bool)
			if final.Content != whole {
				t.Fatalf("the run's answer is not whole: %d of %d bytes", len(final.Content), len(whole))
			}
			if stalled {
				if live.String() == whole || !flagged || !turnFlagged {
					t.Fatalf("lost deltas went unreported: live=%d bytes, result flag=%v, turn flag=%v", live.Len(), flagged, turnFlagged)
				}
				return
			}
			if live.String() != whole || flagged || turnFlagged || turn == nil {
				t.Fatalf("a delivered stream was reported as lost: live=%d bytes, result flag=%v, turn=%v", live.Len(), flagged, turn)
			}
		})
	}
}
