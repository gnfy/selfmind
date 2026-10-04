package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

const externalEffectUsage = "Usage: /effects [resolve <claim_id> <watch_id>]"

// effectsCommandReply exposes the conservative unknown-target recovery path.
// The watcher is a daemon-recorded read-only observation; the person, rather
// than the model, explicitly judges that it resolves this exact effect.
func (d *Server) effectsCommandReply(ctx context.Context, identity *control.IdentityContext, args []string) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "External effects unavailable.", nil
	}
	if len(args) == 3 && strings.EqualFold(args[0], "resolve") {
		watch, userErr, err := d.resolveWatcherReference(ctx, identity, args[2])
		if err != nil || userErr != "" {
			return userErr, err
		}
		claim, err := d.Control.ObserveExternalEffectWithWatch(ctx, identity.TenantID, identity.PersonID, args[1], watch.ID)
		if err != nil {
			return "The effect remains occupied: " + err.Error(), nil
		}
		// The durable wait remains authoritative. An immediate pass improves
		// latency; the daemon's next pass retries if queueing fails here.
		d.runExternalResourceWaitPass(ctx)
		return fmt.Sprintf("Released external effect %s using finalized watcher %s. Waiting work will resume when admitted.", claim.ID, shortExternalWatchID(watch.ID)), nil
	}
	if len(args) != 0 {
		return externalEffectUsage, nil
	}
	claims, err := d.Control.ListUnresolvedExternalEffects(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		return "", err
	}
	if len(claims) == 0 {
		return "No unresolved external effects.", nil
	}
	var sb strings.Builder
	sb.WriteString("Unresolved external effects:\n")
	for _, claim := range claims {
		fmt.Fprintf(&sb, "- %s | run %s | target %s | %s\n", claim.ID, claim.RunID, claim.TargetKey, claim.State)
	}
	sb.WriteString("After a watcher for the same run succeeds and finalizes, confirm the relation with /effects resolve <claim_id> <watch_id>.")
	return sb.String(), nil
}
