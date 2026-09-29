package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

// providerRequestRouteID uses the same physical identity as the existing
// maintenance quota circuit. Model and role deliberately do not divide a
// provider endpoint/credential into independent rate-limit lanes.
func providerRequestRouteID(rt modelruntime.Runtime) string {
	credential := maintenanceCredentialIdentity(&rt)
	credentialSum := sha256.Sum256([]byte(credential))
	payload := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(rt.Provider)),
		normalizeMaintenanceQuotaEndpoint(rt.BaseURL),
		fmt.Sprintf("%x", credentialSum[:]),
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", sum[:])
}

func firstRequestGate(gates []*llm.RequestGate) *llm.RequestGate {
	if len(gates) == 0 {
		return nil
	}
	return gates[0]
}

func gateResolvedProvider(gate *llm.RequestGate, cfg *config.Config, selection modelruntime.Selection, provider llm.Provider) llm.Provider {
	if gate == nil || provider == nil || cfg == nil {
		return provider
	}
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), selection)
	if err != nil {
		return provider
	}
	return gate.Wrap(provider, providerRequestRouteID(rt))
}
