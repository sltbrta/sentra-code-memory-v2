package companymode

import (
	"context"
	"sync"
)

// StaticPolicy is a fail-closed company authorization map that mirrors the
// OpenFGA-compatible company fixtures without importing the broker-internal
// evaluator (Go internal package boundary). Fixture identity is proven by
// //services/broker/internal/authz:authz_test (in-process + hermetic HTTP dual-run)
// against fixtures_company.json. Default company composition stays in-process;
// durable live OpenFGA store conformance remains DEF-015 / issue #72 residual.
type StaticPolicy struct {
	mu      sync.RWMutex
	allow   map[string]struct{}
	revoked map[string]struct{}
}

// NewAuthzPolicy seeds the two-principal company relationship decisions.
func NewAuthzPolicy() (*StaticPolicy, error) {
	p := &StaticPolicy{
		allow:   make(map[string]struct{}),
		revoked: make(map[string]struct{}),
	}
	// alice owner: full company actions on company-acme.
	for _, action := range []string{
		"source.add", "source.reconcile", "source.revoke",
		"query", "hydrate", "emit", "source.status", "source.search",
		"artifact.read", "artifact.admit", "artifact.delete",
		"operator.status",
	} {
		p.allow[key("alice", "company-acme", action)] = struct{}{}
	}
	// bob viewer: read/query only.
	for _, action := range []string{
		"query", "hydrate", "emit", "source.status", "source.search", "artifact.read",
	} {
		p.allow[key("bob", "company-acme", action)] = struct{}{}
	}
	// member-only and cross-tenant have no allow entries (default deny).
	return p, nil
}

func key(principal, tenant, action string) string {
	return principal + "|" + tenant + "|" + action
}

// Allow evaluates company actions through the seeded fixture map.
func (p *StaticPolicy) Allow(_ context.Context, principal Principal, action, _ string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, revoked := p.revoked[key(principal.ID, principal.TenantID, action)]; revoked {
		return false
	}
	_, ok := p.allow[key(principal.ID, principal.TenantID, action)]
	return ok
}

// Revoke removes an allow entry (used by tests for epoch-style denial).
func (p *StaticPolicy) Revoke(principal Principal, action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked[key(principal.ID, principal.TenantID, action)] = struct{}{}
}
