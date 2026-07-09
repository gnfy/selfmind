package control

import (
	"context"
	"strings"
	"time"
)

// inboundDedupRetention bounds how long inbound message ids are remembered.
// IM platforms redeliver within minutes; 48h comfortably covers every retry
// schedule plus the weixin sync-buffer replay window after a restart.
const inboundDedupRetention = 48 * time.Hour

// MarkInboundSeen records an inbound platform message id and reports whether
// this is the FIRST time it was seen. IM platforms re-POST an event on any
// non-2xx or slow response, and the weixin sync buffer replays recent messages
// after a reconnect; this durable first-seen check is what keeps a redelivered
// message from running the agent twice (an in-memory map dies with the
// process). An empty id reports first-seen: there is nothing safe to dedup on.
func (s *Store) MarkInboundSeen(ctx context.Context, platform, messageID string) (bool, error) {
	platform = strings.TrimSpace(platform)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return true, nil
	}
	now := time.Now().Unix()
	// Opportunistic aging: the table stays bounded to the retention window. A
	// prune failure must never drop a real message, so it is non-fatal.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM inbound_dedup WHERE created_at < ?`,
		now-int64(inboundDedupRetention/time.Second))
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO inbound_dedup(platform, message_id, created_at) VALUES(?,?,?)`,
		platform, messageID, now)
	if err != nil {
		// Fail open: a dedup-store hiccup must not silently drop inbound work.
		return true, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return true, err
	}
	return n > 0, nil
}
