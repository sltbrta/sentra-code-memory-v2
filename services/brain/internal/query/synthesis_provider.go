package query

import (
	"context"
	"fmt"
	"time"
)

// ProviderRequest is the single egress shape sent to a synthesis provider:
// the admitted question, the verified evidence pack, and the output bounds.
// It carries no principal, tenant, session, or credential material.
type ProviderRequest struct {
	ProviderID string
	Model      string
	Query      string
	Evidence   []EvidenceEntry
	Limits     SynthesisLimits
}

// ProviderResponse is one bounded provider result.
type ProviderResponse struct {
	Prose      string
	Claims     []ProposedClaim
	TokenUsage uint64
}

// ProviderClient is the egress port one provider integration implements. The
// v1 engine ships no live client; tests drive the shape with stubs.
type ProviderClient interface {
	Complete(ctx context.Context, request ProviderRequest) (*ProviderResponse, error)
}

// ProviderConfig binds the adapter to one policy-approved provider and model
// with a per-call context deadline. The deadline is cooperative: the client
// must honor context cancellation, and the adapter discards any response
// that arrives after it.
type ProviderConfig struct {
	ProviderID string
	Model      string
	Client     ProviderClient
	Timeout    time.Duration
}

// ProviderSynthesizer is the provider-configurable adapter shape. It fails
// closed on every provider failure mode — transport error, timeout, refusal,
// or over-bound output — surfacing a wrapped ErrSynthesisFailed with zero
// partial output, and it never retries against or falls back to another
// provider or billing identity.
type ProviderSynthesizer struct {
	config ProviderConfig
}

// NewProviderSynthesizer validates the adapter configuration; a misconfigured
// adapter fails at construction, never at request time.
func NewProviderSynthesizer(config ProviderConfig) (*ProviderSynthesizer, error) {
	if config.Client == nil || config.ProviderID == "" || config.Model == "" || config.Timeout <= 0 {
		return nil, fmt.Errorf("%w: provider configuration", ErrInvalidInput)
	}
	return &ProviderSynthesizer{config: config}, nil
}

// Synthesize executes one bounded provider call. The engine re-verifies every
// returned claim structurally; the adapter additionally guards whole-response
// bounds so an over-bound provider response fails before any claim is read.
func (p *ProviderSynthesizer) Synthesize(ctx context.Context, request SynthesisRequest) (Synthesis, error) {
	if err := ctx.Err(); err != nil {
		return Synthesis{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	response, err := p.config.Client.Complete(ctx, ProviderRequest{
		ProviderID: p.config.ProviderID,
		Model:      p.config.Model,
		Query:      request.Query,
		Evidence:   request.Evidence,
		Limits:     request.Limits,
	})
	if err != nil {
		return Synthesis{}, fmt.Errorf("%w: provider call failed", ErrSynthesisFailed)
	}
	if err := ctx.Err(); err != nil {
		return Synthesis{}, fmt.Errorf("%w: provider deadline", ErrSynthesisFailed)
	}
	if response == nil {
		return Synthesis{}, fmt.Errorf("%w: provider returned no response", ErrSynthesisFailed)
	}
	if len(response.Prose) > request.Limits.MaxProseBytes {
		return Synthesis{}, fmt.Errorf("%w: provider prose exceeds the prose bound", ErrSynthesisFailed)
	}
	if len(response.Claims) > request.Limits.MaxClaims {
		return Synthesis{}, fmt.Errorf("%w: provider claims exceed the claim bound", ErrSynthesisFailed)
	}
	synthesis := Synthesis{
		Prose:      response.Prose,
		Claims:     response.Claims,
		TokenUsage: response.TokenUsage,
	}
	if len(synthesis.Claims) == 0 {
		synthesis.Prose = ""
	}
	return synthesis, nil
}
