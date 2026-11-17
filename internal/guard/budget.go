package guard

import "context"

// Availability is implemented by the circuit-breaker provider wrappers.
type Availability interface{ Available() error }

// ProviderAvailable composes the provider circuit breakers (011) as a
// budget stage (GRD-REQ-009): it blocks while any listed provider's circuit
// is open. The ask handler maps this Block to 503 provider_unavailable.
type ProviderAvailable struct {
	Providers []Availability
}

// Name implements Guard.
func (ProviderAvailable) Name() string { return StageBudgetProvider }

// Check implements Guard.
func (p ProviderAvailable) Check(_ context.Context, text string) (Verdict, string, error) {
	for _, a := range p.Providers {
		if a != nil && a.Available() != nil {
			return Block, "", nil
		}
	}
	return Allow, text, nil
}
