package app

import (
	"context"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// ReviewApprovalGrants withdraws stored approval classes that the current
// eligibility floor rejects. It runs once at daemon boot, before any traffic.
//
// It exists because the floor is newer than the ledger: grant keys minted by
// the earlier first-token derivation named a shell prologue rather than a
// program (`command:set`, `command:for`), and older host grants carried no
// workspace/command resource at all. Both shapes authorise far more than the
// human who approved them intended, and neither can be corrected by the
// mint-time floor alone. The sweep is idempotent — a revoked grant is skipped
// on the next boot — and only ever REMOVES authority.
//
// Grants that remain eligible are returned so the caller can log a reviewable
// summary; the sweep never re-grants or widens anything.
func ReviewApprovalGrants(ctx context.Context, store *control.Store) (revoked int, kept []control.ApprovalGrant, err error) {
	if store == nil {
		return 0, nil, nil
	}
	grants, err := store.ListAllApprovalGrants(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, grant := range grants {
		family, keep := tools.ReviewPersistedGrantKey(grant.PatternKey)
		if keep {
			kept = append(kept, grant)
			continue
		}
		withdrawn, revokeErr := store.RevokeApprovalGrant(ctx, grant.TenantID, grant.PersonID, grant.ID)
		if revokeErr != nil {
			log.Warn("approval grant review: revoke failed", "grant", grant.ID, "error", revokeErr)
			continue
		}
		if withdrawn {
			revoked++
			// The key itself is non-secret (a class, never a command), so it is
			// safe and necessary to name what was withdrawn.
			log.Warn("approval grant review: withdrew an over-broad remembered class",
				"grant", grant.ID, "scope", grant.ScopeKind, "family", family, "pattern", grant.PatternKey)
		}
	}
	return revoked, kept, nil
}
