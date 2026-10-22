package delivery

import (
	"context"
	"errors"
	"selfmind/internal/tools"
	"time"
)

// A successful platform call must not become retryable when saving its
// receipt fails. A completely unwritable store leaves sending unresolved;
// stale-claim recovery also refuses to blindly resend that unknown effect.
func (s *Service) recordAcceptedDelivery(ctx context.Context, id string, confirmed bool, nativeID string) error {
	ctx = context.WithoutCancel(ctx)
	if !confirmed {
		return s.store.MarkDeliverySentUnconfirmed(ctx, id)
	}
	var err error
	if nativeID != "" {
		err = s.store.MarkDeliverySentWithNativeID(ctx, id, nativeID)
	} else {
		err = s.store.MarkDeliveryAttempt(ctx, id, true, "", time.Time{})
	}
	if err == nil {
		return nil
	}
	return errors.Join(err, s.store.MarkDeliverySentUnconfirmed(ctx, id,
		"platform accepted the send; receipt save failed: "+tools.RedactSensitive(err.Error())))
}
