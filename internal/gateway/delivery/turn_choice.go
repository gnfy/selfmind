package delivery

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
)

// Choice notice row IDs carry their typed parent reference. The choice IDs
// minted by control contain no double underscores, so this preserves the
// outbox schema without borrowing a human-input reference or reading prose.
// Cross-endpoint recovery notices retain this reference for actionability;
// their kind keeps bare replies local to the choice's original endpoint.
func choiceIDForDelivery(d *control.Delivery) string {
	if d == nil || (d.Kind != KindTurnChoice && d.Kind != KindRecovery) {
		return ""
	}
	suffix := "__notice_" + strconv.Itoa(d.PartIndex)
	if !strings.HasSuffix(d.ID, suffix) {
		return ""
	}
	id := strings.TrimSuffix(d.ID, suffix)
	if !strings.HasPrefix(id, "choice_") || strings.Contains(id, "__") {
		return ""
	}
	return id
}

func (s *Service) enqueueTurnChoice(ctx context.Context, msg Message, parts []string, try bool) (bool, bool, error) {
	choice, err := s.store.PeekPendingTurnChoice(ctx, msg.TenantID, msg.PersonID, msg.ChoiceID, time.Now())
	if err != nil {
		return false, false, err
	}
	if choice.Channel != msg.Channel {
		return false, false, fmt.Errorf("choice notice channel does not match its saved request")
	}
	rows := s.prepareChoiceNotice(msg, parts)
	saved, err := s.store.EnqueueDeliveries(ctx, rows)
	if err != nil {
		return false, false, err
	}
	if !try {
		return true, false, nil
	}
	confirmed := true
	var lastErr error
	for i := range saved {
		if err := s.tryDelivery(ctx, &saved[i]); err != nil {
			lastErr = err
		}
		status, err := s.store.DeliveryStatus(ctx, saved[i].ID)
		if err != nil {
			lastErr = err
		}
		confirmed = confirmed && err == nil && status == "sent"
	}
	return true, confirmed, lastErr
}

// PrepareChoiceNotice stages bounded notification parts for the transaction
// that creates an asynchronous choice. It neither writes nor sends.
func (s *Service) PrepareChoiceNotice(msg Message) []control.Delivery {
	return s.prepareChoiceNotice(msg, splitMessage(msg.Content, s.opts.MaxMessageChars))
}

func (s *Service) prepareChoiceNotice(msg Message, parts []string) []control.Delivery {
	rows := make([]control.Delivery, 0, len(parts))
	for i, part := range parts {
		partMsg := msg
		partMsg.Content, partMsg.PartIndex, partMsg.PartTotal = part, i+1, len(parts)
		rows = append(rows, control.Delivery{
			ID:       msg.ChoiceID + "__notice_" + strconv.Itoa(i+1),
			TenantID: msg.TenantID, PersonID: msg.PersonID, Platform: msg.Platform,
			PlatformUserID: msg.PlatformUserID, Channel: msg.Channel, Content: part,
			Kind: msg.Kind, MaxAttempts: s.opts.RetryAttempts,
			PartIndex: i + 1, PartTotal: len(parts), IdempotencyKey: idempotencyKey(partMsg),
		})
	}
	return rows
}
