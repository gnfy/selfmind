package kernel

import "context"

// CommittedTurnReader exposes only an acknowledged durable result, never a
// proposed outcome. It fences later calls even if post-commit tool-ledger or
// transport reporting fails. The gateway owns the actual commit.
type CommittedTurnReader interface {
	CommittedTurn() (status, body string, committed bool)
}

type committedTurnReaderKey struct{}

func WithCommittedTurnReader(ctx context.Context, reader CommittedTurnReader) context.Context {
	return context.WithValue(ctx, committedTurnReaderKey{}, reader)
}

func committedTurn(ctx context.Context) (string, string, bool) {
	if DelegationNamespace(ctx) != "" {
		return "", "", false
	}
	if reader, ok := ctx.Value(committedTurnReaderKey{}).(CommittedTurnReader); ok {
		return reader.CommittedTurn()
	}
	return "", "", false
}
