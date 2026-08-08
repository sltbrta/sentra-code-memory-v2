// Package localauthority exposes the minimum local identity, policy, and
// capability broker used by the Stage 2 runtime. It keeps the internal policy
// engines replaceable while making every use perform a current-policy check.
package localauthority

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/authz"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/capability"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/identity"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrDenied is the single non-disclosing result for invalid identity,
	// capability, stale epoch, missing relationship, or current-policy denial.
	ErrDenied = errors.New("local authority broker: denied")
	// ErrInvalidConfig reports an incomplete fixed local identity mapping.
	ErrInvalidConfig = errors.New("local authority broker: invalid configuration")
)

// Identifier is a namespaced authority identifier.
type Identifier = shared.Identifier

// Digest binds trusted issued-grant policy facts.
type Digest = shared.Digest

// PeerCredentials are authenticated operating-system peer facts.
type PeerCredentials = shared.PeerCredentials

// Identity is the trusted result of mapping operating-system peer facts.
type Identity = shared.MappedIdentityFact

// Grant is the bounded, expiring capability evaluated for one use.
type Grant = capability.Grant

// UseRequest binds an invocation to the grant's exact fence, epoch, nonce,
// resource, path, and metered limits.
type UseRequest = capability.UseRequest

// Decision is a current, non-sensitive authorization disposition.
type Decision struct {
	Allowed         bool
	ReasonCode      string
	RevocationEpoch uint64
}

// Config fixes the only operating-system user and authority identity accepted
// by a broker instance. Session is configured, never inferred from a body.
type Config struct {
	UID       uint32
	Principal Identifier
	Tenant    Identifier
	Session   Identifier
}

// Broker combines peer identity, local relationship policy, and attenuated
// capability checks. Mutation methods are intended for trusted configuration
// code; request handling should use MapPeer and Authorize.
//
// The default policy store is the in-process OpenFGA-compatible evaluator.
// A remote OpenFGA RelationshipStore may be injected only through trusted
// composition; missing configuration must keep the fail-closed in-process path.
type Broker struct {
	mapper  *identity.Mapper
	policy  authz.RelationshipStore
	session Identifier
	mu      sync.RWMutex
	grants  map[string]Grant
}

// New constructs a default-deny broker for one fixed local identity.
//
// It returns ErrInvalidConfig for missing or wrongly namespaced identity. The
// returned broker has no relationships and therefore authorizes nothing until
// trusted configuration writes explicit tuples. Policy always starts on the
// in-process adapter (fail closed); live OpenFGA is never selected implicitly.
func New(config Config) (*Broker, error) {
	return NewWithStore(config, authz.NewInProcessAdapter())
}

// NewWithStore constructs a broker with an explicit RelationshipStore.
// A nil store is rejected so callers cannot accidentally disable policy.
func NewWithStore(config Config, store authz.RelationshipStore) (*Broker, error) {
	if config.Session.Namespace != "session" || config.Session.Value == "" {
		return nil, ErrInvalidConfig
	}
	if store == nil {
		return nil, ErrInvalidConfig
	}
	mapper, err := identity.NewMapper(config.UID, config.Principal, config.Tenant)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	return &Broker{
		mapper: mapper, policy: store, session: config.Session,
		grants: make(map[string]Grant),
	}, nil
}

// RegisterGrant installs one exact trusted issued grant for later request-body
// resolution. Registration is explicit and in-memory in Stage 2; restart
// composition must reinstall every grant. Existing IDs cannot be overwritten
// with different authority facts.
func (b *Broker) RegisterGrant(grant Grant, now time.Time) error {
	if b == nil || capability.ValidateGrant(grant, now) != nil {
		return ErrDenied
	}
	cloned := cloneGrant(grant)
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, found := b.grants[grant.ID]; found && !reflect.DeepEqual(existing, cloned) {
		return ErrDenied
	}
	b.grants[grant.ID] = cloned
	return nil
}

// MapPeer maps an authenticated peer before any request body is decoded.
// Missing PID, wrong UID, and invalid configured identity all return ErrDenied.
func (b *Broker) MapPeer(credentials PeerCredentials) (Identity, error) {
	if b == nil || b.mapper == nil {
		return Identity{}, ErrDenied
	}
	mapped, err := b.mapper.Map(credentials, b.session)
	if err != nil {
		return Identity{}, ErrDenied
	}
	return mapped, nil
}

// AddRelationship installs one checked `object#relation@user` relationship.
// It returns ErrDenied for malformed input and never treats tenant membership
// as evidence access by itself.
func (b *Broker) AddRelationship(value string) error {
	if b == nil || b.policy == nil {
		return ErrDenied
	}
	tuple, err := authz.ParseTuple(value)
	if err != nil || b.policy.Write(tuple) != nil {
		return ErrDenied
	}
	return nil
}

// RemoveRelationship removes an existing relationship and advances the owning
// tenant's deny epoch atomically. The returned epoch must be used by new grants.
func (b *Broker) RemoveRelationship(value string) (uint64, error) {
	if b == nil || b.policy == nil {
		return 0, ErrDenied
	}
	tuple, err := authz.ParseTuple(value)
	if err != nil {
		return 0, ErrDenied
	}
	_, epoch, err := b.policy.Delete(tuple)
	if err != nil {
		return 0, ErrDenied
	}
	return epoch, nil
}

// SetRevocationEpoch monotonically advances a tenant's current deny epoch.
// It returns ErrDenied for empty tenants or attempts to move backward.
func (b *Broker) SetRevocationEpoch(tenant string, epoch uint64) error {
	if b == nil || b.policy == nil || b.policy.SetEpoch(tenant, epoch) != nil {
		return ErrDenied
	}
	return nil
}

// RevocationEpoch returns current trusted deny state for authenticated status
// rendering. Missing broker state or a tenant mismatch fails closed.
func (b *Broker) RevocationEpoch(tenant Identifier) (uint64, error) {
	if b == nil || b.policy == nil || tenant.Namespace != "tenant" || tenant.Value == "" {
		return 0, ErrDenied
	}
	epoch, err := b.policy.Epoch(tenant.Value)
	if err != nil {
		return 0, ErrDenied
	}
	return epoch, nil
}

// AuthorizeSource checks the current tenant-scoped brain relationship for one
// Stage 03 ingestion action. The mapped identity must originate from the
// authenticated peer; callers must never substitute request-body identity.
func (b *Broker) AuthorizeSource(ctx context.Context, mapped Identity, action string, brain Identifier) (Decision, error) {
	denied := Decision{ReasonCode: "not_found_or_denied"}
	if b == nil || b.policy == nil {
		return denied, ErrDenied
	}
	policy, err := b.policy.CheckSource(ctx, mapped, action, brain)
	denied.RevocationEpoch = policy.RevocationEpoch
	if err != nil || !policy.Allowed {
		return denied, ErrDenied
	}
	return Decision{Allowed: true, ReasonCode: "allowed", RevocationEpoch: policy.RevocationEpoch}, nil
}

// Authorize validates the exact grant use and then evaluates current policy.
// Capability success never substitutes for the current relationship check;
// stale epochs, missing tuples, and backend errors fail closed as ErrDenied.
func (b *Broker) Authorize(ctx context.Context, mapped Identity, grant Grant, use UseRequest) (Decision, error) {
	denied := Decision{ReasonCode: "not_found_or_denied", RevocationEpoch: use.RevocationEpoch}
	issued, found := b.issuedGrant(grant.ID)
	if b == nil || b.policy == nil || use.Now.IsZero() || !found || !reflect.DeepEqual(issued, grant) ||
		capability.AuthorizeUse(issued, mapped, use) != nil {
		return denied, ErrDenied
	}
	policy, err := b.policy.Check(ctx, mapped, shared.PolicyRequest{
		Action: use.Action, Resource: use.Resource, RevocationEpoch: use.RevocationEpoch,
	})
	denied.RevocationEpoch = policy.RevocationEpoch
	if err != nil || !policy.Allowed {
		return denied, ErrDenied
	}
	return Decision{Allowed: true, ReasonCode: "allowed", RevocationEpoch: policy.RevocationEpoch}, nil
}

func (b *Broker) issuedGrant(id string) (Grant, bool) {
	if b == nil || id == "" {
		return Grant{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	grant, found := b.grants[id]
	return cloneGrant(grant), found
}

func cloneGrant(grant Grant) Grant {
	grant.Actions = append([]string(nil), grant.Actions...)
	grant.Resources = append([]Identifier(nil), grant.Resources...)
	grant.AllowedPaths = append([]string(nil), grant.AllowedPaths...)
	if grant.Limits != nil {
		limits := make(map[string]uint64, len(grant.Limits))
		for name, maximum := range grant.Limits {
			limits[name] = maximum
		}
		grant.Limits = limits
	}
	return grant
}

// NewUse builds a capability-use request with the supplied trusted clock.
// Callers still must pass the result to Authorize before any effect.
func NewUse(action string, resource Identifier, fence, epoch uint64, nonce string, now time.Time) UseRequest {
	return UseRequest{
		Action: action, Resource: resource, Fence: fence,
		RevocationEpoch: epoch, Nonce: nonce, Now: now,
	}
}
