// Package capability validates attenuation and current use of bounded local grants.
// A grant narrows authority; it never replaces a current authorization check.
package capability

import (
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var ErrDenied = errors.New("capability: denied")

// ValidateGrant rejects malformed, wildcarded, or expired grant authority.
// Trusted grant registries use it before making an issued grant resolvable.
func ValidateGrant(grant Grant, now time.Time) error { return validate(grant, now) }

// Grant is the minimum executable Stage 02 capability state.
type Grant struct {
	ID              string
	IDNamespace     string
	Principal       contracts.Identifier
	Tenant          contracts.Identifier
	PolicyDigest    contracts.Digest
	Actions         []string
	Resources       []contracts.Identifier
	AllowedPaths    []string
	Limits          map[string]uint64
	Fence           uint64
	RevocationEpoch uint64
	ExpiresAt       time.Time
	Nonce           string
}

// Request describes a proposed child grant.
type Request Grant

// UseRequest binds one invocation to the grant nonce, path, and metered usage.
// Empty Path is valid only for a non-filesystem use. Empty Usage means the use
// consumes no metered dimension; non-empty usage must be bounded by Limits.
type UseRequest struct {
	Action          string
	Resource        contracts.Identifier
	Fence           uint64
	RevocationEpoch uint64
	Nonce           string
	Path            string
	Usage           map[string]uint64
	Now             time.Time
}

// Attenuate returns a child grant only when every dimension is a subset of parent authority.
// Wildcards, redelegation actions, expired parents, later expiry, and widened limits deny.
func Attenuate(parent Grant, request Request, now time.Time) (Grant, error) {
	child := Grant(request)
	if validate(parent, now) != nil || validate(child, now) != nil || child.ID == parent.ID ||
		child.Principal != parent.Principal || child.Tenant != parent.Tenant || child.Fence != parent.Fence ||
		child.RevocationEpoch != parent.RevocationEpoch || child.ExpiresAt.After(parent.ExpiresAt) ||
		!stringsSubset(child.Actions, parent.Actions) || !resourcesSubset(child.Resources, parent.Resources) ||
		!constrainedPathsSubset(child.AllowedPaths, parent.AllowedPaths) ||
		!constrainedLimitsSubset(child.Limits, parent.Limits) {
		return Grant{}, ErrDenied
	}
	return cloneGrant(child), nil
}

// AuthorizeUse checks exact identity, action, resource, fence, epoch, nonce, and expiry.
// Callers must still run current policy authorization after this local validation.
func AuthorizeUse(grant Grant, identity contracts.MappedIdentityFact, request UseRequest) error {
	if validate(grant, request.Now) != nil || identity.Principal != grant.Principal || identity.Tenant != grant.Tenant ||
		request.Action == "" || !contains(grant.Actions, request.Action) || !containsResource(grant.Resources, request.Resource) ||
		request.Fence == 0 || request.Fence != grant.Fence || request.RevocationEpoch != grant.RevocationEpoch ||
		request.Nonce == "" || request.Nonce != grant.Nonce || !pathUseAllowed(request.Path, grant.AllowedPaths) ||
		!usageAllowed(request.Usage, grant.Limits) {
		return ErrDenied
	}
	return nil
}

func validate(grant Grant, now time.Time) error {
	if grant.ID == "" || grant.IDNamespace != "grant" || !validPolicyDigest(grant.PolicyDigest) ||
		grant.Nonce == "" || grant.Principal.Namespace != "principal" || grant.Principal.Value == "" ||
		grant.Tenant.Namespace != "tenant" || grant.Tenant.Value == "" || len(grant.Actions) == 0 || len(grant.Resources) == 0 ||
		grant.Fence == 0 || grant.ExpiresAt.IsZero() || !now.Before(grant.ExpiresAt) {
		return ErrDenied
	}
	for _, action := range grant.Actions {
		if action == "" || strings.Contains(action, "*") || strings.Contains(action, "delegate") {
			return ErrDenied
		}
	}
	for _, resource := range grant.Resources {
		if resource.Namespace == "" || resource.Value == "" || strings.Contains(resource.Value, "*") {
			return ErrDenied
		}
	}
	for _, prefix := range grant.AllowedPaths {
		if prefix == "" || prefix == "." || path.IsAbs(prefix) || strings.HasPrefix(prefix, "../") || path.Clean(prefix) != prefix {
			return ErrDenied
		}
	}
	for name, limit := range grant.Limits {
		if name == "" || limit == 0 {
			return ErrDenied
		}
	}
	return nil
}

func validPolicyDigest(digest contracts.Digest) bool {
	if digest.Algorithm != "sha256" || len(digest.Hex) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(digest.Hex)
	return err == nil && hex.EncodeToString(decoded) == digest.Hex
}

func stringsSubset(children, parents []string) bool {
	for _, child := range children {
		if !contains(parents, child) {
			return false
		}
	}
	return true
}

func resourcesSubset(children, parents []contracts.Identifier) bool {
	for _, child := range children {
		if !containsResource(parents, child) {
			return false
		}
	}
	return true
}

func constrainedPathsSubset(children, parents []string) bool {
	if len(parents) == 0 {
		return len(children) == 0
	}
	if len(children) == 0 {
		return false
	}
	for _, child := range children {
		allowed := false
		for _, parent := range parents {
			if child == parent || strings.HasPrefix(child, parent+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func constrainedLimitsSubset(children, parents map[string]uint64) bool {
	if len(parents) == 0 {
		return len(children) == 0
	}
	if len(children) == 0 {
		return false
	}
	for name, child := range children {
		parent, ok := parents[name]
		if !ok || child > parent {
			return false
		}
	}
	return true
}

func pathUseAllowed(requested string, allowed []string) bool {
	if len(allowed) == 0 {
		return requested == ""
	}
	if requested == "" {
		return false
	}
	if path.IsAbs(requested) || strings.HasPrefix(requested, "../") || path.Clean(requested) != requested {
		return false
	}
	for _, prefix := range allowed {
		if requested == prefix || strings.HasPrefix(requested, prefix+"/") {
			return true
		}
	}
	return false
}

func usageAllowed(usage, limits map[string]uint64) bool {
	if len(limits) == 0 {
		return len(usage) == 0
	}
	if len(usage) != len(limits) {
		return false
	}
	for name, used := range usage {
		maximum, ok := limits[name]
		if name == "" || !ok || used > maximum {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsResource(values []contracts.Identifier, want contracts.Identifier) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cloneGrant(grant Grant) Grant {
	limits := grant.Limits
	grant.Actions = append([]string(nil), grant.Actions...)
	grant.Resources = append([]contracts.Identifier(nil), grant.Resources...)
	grant.AllowedPaths = append([]string(nil), grant.AllowedPaths...)
	grant.Limits = make(map[string]uint64, len(limits))
	for name, limit := range limits {
		grant.Limits[name] = limit
	}
	return grant
}
