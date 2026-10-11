package control

import (
	"context"
	"database/sql"
)

// EnqueueDeliveries records all parts of one notification before any part can
// be sent. A failed insert leaves no partial notification behind.
func (s *Store) EnqueueDeliveries(ctx context.Context, deliveries []Delivery) ([]Delivery, error) {
	// A caller disconnect must not report a refused notification after SQLite
	// committed it. This is the same durable admission boundary as a choice.
	ctx = context.WithoutCancel(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]Delivery, 0, len(deliveries))
	for _, d := range deliveries {
		saved, err := enqueueDelivery(ctx, tx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, *saved)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

type deliveryWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
