package httpapi

import (
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

func formatResourceWaitBacklog(waits []control.ExternalResourceWaitProjection, now time.Time) string {
	var pending, ready, observation int
	var oldest time.Time
	for _, wait := range waits {
		if oldest.IsZero() || wait.CreatedAt.Before(oldest) {
			oldest = wait.CreatedAt
		}
		if wait.Ready {
			ready++
		} else if wait.RunStatus == "blocked" || wait.NeedsObservation {
			observation++
		} else {
			pending++
		}
	}
	age := "none"
	if !oldest.IsZero() {
		age = now.Sub(oldest).Round(time.Second).String()
	}
	return fmt.Sprintf("External resource waits now: waiting for live observation %d, ready %d, needs observation %d, oldest %s (bounded to 100 Runs).\n", pending, ready, observation, age)
}

func formatResourceWaitDetail(wait control.ExternalResourceWaitProjection) string {
	if wait.RunStatus == "blocked" && wait.Ready {
		return "The held effect has been observed. Use /resume " + wait.RunID + " to assess and finish the remaining work."
	}
	if wait.RunStatus == "blocked" || wait.NeedsObservation {
		var ids []string
		for _, claim := range wait.BlockingClaims {
			ids = append(ids, claim.ID)
		}
		return "External effect needs observation; automatic continuation is unavailable. Claims: " + strings.Join(ids, ", ") + ". Use /effects, then /resume " + wait.RunID + " for read-only inspection. The blocked call was not dispatched."
	}
	if wait.Ready {
		return "External target is available; the exact continuation is waiting for dispatch."
	}
	return "External target is occupied; a live run or bound watcher is providing observation. The blocked call was not dispatched."
}
