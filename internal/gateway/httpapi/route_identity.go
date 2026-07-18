package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// routeIdentityForPerson reconstructs endpoint routing for daemon-originated
// work without creating an account. System queue recovery must preserve the
// durable person_id: resolving a blank platform_user_id through
// ResolveOrCreateAccount would normalize it to cli:local and could silently
// move the work onto a different person.
func (d *Server) routeIdentityForPerson(ctx context.Context, tenantID, personID, channel, preferredPlatform string, fallback *control.IdentityContext) *control.IdentityContext {
	identity := &control.IdentityContext{
		TenantID: tenantID,
		PersonID: personID,
		Platform: strings.TrimSpace(preferredPlatform),
	}
	if identity.Platform == "" {
		identity.Platform = "cli"
	}
	if d == nil || d.Control == nil || strings.TrimSpace(personID) == "" {
		return identity
	}
	if account, err := d.Control.AccountForChannel(ctx, tenantID, personID, channel); err != nil {
		log.Warn("route identity channel lookup failed", "person_id", personID, "error", err)
	} else if account != nil {
		return identityFromAccount(*account)
	}

	accounts, err := d.Control.ListAccountsByPerson(ctx, tenantID, personID)
	if err != nil {
		log.Warn("route identity account lookup failed", "person_id", personID, "error", err)
		return samePersonFallback(identity, fallback)
	}
	var best *control.Account
	bestScore := -1
	for i := range accounts {
		account := accounts[i]
		score := routeAccountScore(account, identity.Platform)
		if best == nil || score > bestScore || (score == bestScore && account.LastSeenAt > best.LastSeenAt) {
			copy := account
			best = &copy
			bestScore = score
		}
	}
	if best != nil {
		return identityFromAccount(*best)
	}
	return samePersonFallback(identity, fallback)
}

func routeAccountScore(account control.Account, preferredPlatform string) int {
	preferred := strings.EqualFold(strings.TrimSpace(account.Platform), strings.TrimSpace(preferredPlatform))
	nonLocal := strings.TrimSpace(account.PlatformUserID) != "" && !strings.EqualFold(strings.TrimSpace(account.PlatformUserID), "local")
	switch {
	case preferred && nonLocal:
		return 4
	case preferred:
		return 3
	case nonLocal:
		return 2
	default:
		return 1
	}
}

func identityFromAccount(account control.Account) *control.IdentityContext {
	return &control.IdentityContext{
		TenantID:       account.TenantID,
		PersonID:       account.PersonID,
		AccountID:      account.ID,
		Platform:       account.Platform,
		PlatformUserID: account.PlatformUserID,
		DisplayName:    account.DisplayName,
	}
}

func samePersonFallback(base, fallback *control.IdentityContext) *control.IdentityContext {
	if fallback == nil || fallback.TenantID != base.TenantID || fallback.PersonID != base.PersonID {
		return base
	}
	copy := *fallback
	if strings.TrimSpace(copy.Platform) == "" {
		copy.Platform = base.Platform
	}
	return &copy
}
