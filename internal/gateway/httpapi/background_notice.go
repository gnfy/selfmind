package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
)

// PublishBackgroundNotice emits one concise person-scoped live notice. Details
// stay in model status/doctor; this surface only announces a state transition.
func (d *Server) PublishBackgroundNotice(content string, recovered bool) {
	if d == nil || d.Control == nil || strings.TrimSpace(content) == "" {
		return
	}
	partitions, err := d.Control.ListPersonPartitions(context.Background())
	if err != nil {
		log.Warn("gateway: list people for background notice failed", "error", err)
		return
	}
	kind := "warning"
	if recovered {
		kind = "success"
	}
	payload, _ := json.Marshal(map[string]string{"message": strings.TrimSpace(content), "kind": kind})
	seen := make(map[string]struct{}, len(partitions))
	for _, partition := range partitions {
		key := partition.TenantID + "\x00" + partition.PersonID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		d.events().publish(api.RunEvent{
			TenantID: partition.TenantID, PersonID: partition.PersonID,
			Type: "background.notice", Durability: api.EventEphemeral,
			CreatedAt: time.Now(), Payload: payload,
		})
	}
}
