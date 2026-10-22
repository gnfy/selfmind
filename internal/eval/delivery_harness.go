package eval

import (
	"context"
	"strings"
	"sync"
	"time"

	"selfmind/internal/gateway/delivery"
)

// evalDeliverySender stands in for the external IM transport, the way a cassette
// stands in for a provider. Everything it fronts — coalescing, the recovery
// dedup key, and closing the old rows — is production code running against a
// real control database.
//
// Backlog-only cases keep HasPlatform false to preserve their notification
// paths. delivery_enabled opts into ordinary gateway notices against a real
// control store and an in-memory transport.
type evalDeliverySender struct {
	mu      sync.Mutex
	sent    []delivery.Message
	enabled bool
}

func (s *evalDeliverySender) HasPlatform(string) bool { return s.enabled }

func (s *evalDeliverySender) Send(ctx context.Context, msg delivery.Message) error {
	_, err := s.SendWithReceipt(ctx, msg)
	return err
}

// SendWithReceipt confirms delivery. The person is issuing the recovery command
// from the affected chat, so that session is fresh by construction — the same
// condition the daemon requires before it re-pushes anything.
func (s *evalDeliverySender) SendWithReceipt(_ context.Context, msg delivery.Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return true, nil
}

// Sent returns the recorded outbound content, newest last.
func (s *evalDeliverySender) Sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, msg := range s.sent {
		out = append(out, strings.TrimSpace(msg.Content))
	}
	return out
}

// newEvalDeliveryService returns a delivery service for cases that seed
// deliveries, or nil for every other case so their notification paths stay
// exactly as they are without one. delivery_enabled opts into notifications.
func newEvalDeliveryService(c *Case, harness *runtimeHarness) *delivery.Service {
	if c == nil || c.Setup == nil || (len(c.Setup.Deliveries) == 0 && !c.Setup.DeliveryEnabled) || harness == nil || harness.controlStore == nil {
		return nil
	}
	sender := &evalDeliverySender{enabled: c.Setup.DeliveryEnabled}
	harness.deliverySender = sender
	// A long poll interval keeps the background worker out of the way; the case
	// drives recovery explicitly. Seeds carry their own age, so the default
	// catch-up window already treats them as outside automatic recovery.
	return delivery.NewService(harness.controlStore, sender, delivery.Options{
		PollInterval: time.Hour,
	})
}
