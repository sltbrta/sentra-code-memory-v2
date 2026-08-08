package connector

import (
	"context"
	"sync"
)

// DelegatedGrantRecord is the persistence-neutral state of one delegated
// grant. Stores must preserve used IDs after revocation and atomically advance
// Epoch when a grant is revoked.
type DelegatedGrantRecord struct {
	Grant   DelegatedGrant
	Epoch   uint64
	Active  bool
	Revoked bool
}

// DelegatedGrantStore is the durable boundary for delegated grants. A live
// adapter may implement this over a database or RPC; connector never assumes
// either. Prepare must atomically reserve every ID as inactive and reject every
// previously used ID with ErrGrantConflict. Activate is the only transition
// that can make a prepared grant queryable. This two-phase contract guarantees
// that an audit failure after Prepare cannot leave active authority behind.
// Abort permanently tombstones a prepared ID and must never make it active;
// it is the context-independent compensation after an audit append failure.
// Revoke must be idempotent for the owning tenant/principal and return the
// non-disclosing ErrNotFoundOrDenied for an unknown or foreign grant.
type DelegatedGrantStore interface {
	Prepare(context.Context, DelegatedGrantRecord) error
	Activate(context.Context, string) (DelegatedGrantRecord, error)
	Abort(context.Context, string) error
	Get(context.Context, string) (DelegatedGrantRecord, bool, error)
	Revoke(context.Context, Identity, string) (DelegatedGrantRecord, error)
}

// DelegatedIssuerPort authenticates the grant administrator from trusted
// transport/session context. The grant payload is deliberately not an input:
// an implementation must never derive or accept issuer identity from the grant
// being issued. Gateway, worker, and RPC adapters own this authentication step.
type DelegatedIssuerPort interface {
	AuthenticatedDelegatedIssuer(context.Context) (Identity, error)
}

// DelegatedContentProbe hydrates the provider's current representation of an
// object after a live permission allow. The projected object is an integrity
// expectation and routing hint, not authority. Implementations must fetch by
// the exact grant scope and object ID and return the same ID; they must not use
// search, web fallback, or a broader credential when the object is missing.
type DelegatedContentProbe interface {
	HydrateObject(context.Context, DelegatedGrant, Object) (Object, error)
}

// DelegatedProvider is the live, RPC-neutral provider boundary required by an
// ACL-opaque connector. Permission is checked before content hydration and
// again after hydration; content never enters the verdict cache or receipts.
type DelegatedProvider interface {
	PermissionProbe
	DelegatedContentProbe
}

// DelegatedAuditSink receives sanitized, hash-linked receipts. Implementations
// may persist locally or forward over RPC, but must not enrich receipts with
// query text, object IDs/content, tokens, or raw tenant/principal/scope IDs.
type DelegatedAuditSink interface {
	AppendDelegatedReceipt(context.Context, DelegatedReceipt) error
}

// DelegatedComponentEvidence is the transport-neutral, versioned input to the
// promotion gate. It records component and source freshness only; it is not a
// claim that a live provider has been production-certified.
type DelegatedComponentEvidence struct {
	ContractVersion string
	ConnectorDigest string
	SourceRevision  string
	SourceCursor    string
	ObservedAt      int64
}

// DelegatedPromotionGate is the typed fail-closed bridge to the component and
// promotion evidence work tracked by issue #307. No default allow exists. A
// live composition must verify the exact versioned evidence independently.
type DelegatedPromotionGate interface {
	AuthorizeDelegatedComponent(context.Context, DelegatedComponentEvidence) error
}

// DelegatedGrantPort is the stable connector-facing administration boundary.
// It deliberately uses Go command values rather than protobuf messages so the
// same adapter can sit behind an RPC, CLI, database worker, or in-process
// composition without making any transport an authority source. Issuer
// identity is always resolved by DelegatedIssuerPort, never accepted from the
// command or synthesized from the grant subject.
type DelegatedGrantPort interface {
	IssueAuthenticatedGrant(context.Context, DelegatedGrant) error
	RevokeAuthenticatedGrant(context.Context, string) error
}

// MemoryDelegatedGrantStore is the bounded reference store used by tests and
// in-process composition. Production adapters can replace it without changing
// connector query semantics.
type MemoryDelegatedGrantStore struct {
	mu      sync.Mutex
	records map[string]DelegatedGrantRecord
	max     int
}

// NewMemoryDelegatedGrantStore constructs a bounded single-use grant store.
func NewMemoryDelegatedGrantStore(maxRecords int) *MemoryDelegatedGrantStore {
	if maxRecords <= 0 || maxRecords > maxDelegatedGrants {
		maxRecords = maxDelegatedGrants
	}
	return &MemoryDelegatedGrantStore{
		records: make(map[string]DelegatedGrantRecord),
		max:     maxRecords,
	}
}

func (s *MemoryDelegatedGrantStore) Prepare(ctx context.Context, record DelegatedGrantRecord) error {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return ErrInvalidInput
	}
	if _, exists := s.records[record.Grant.ID]; exists {
		return ErrGrantConflict
	}
	if len(s.records) >= s.max {
		return ErrInvalidInput
	}
	record.Active = false
	record.Revoked = false
	s.records[record.Grant.ID] = record
	return nil
}

func (s *MemoryDelegatedGrantStore) Activate(
	ctx context.Context, id string,
) (DelegatedGrantRecord, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || id == "" {
		return DelegatedGrantRecord{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return DelegatedGrantRecord{}, ErrInvalidInput
	}
	record, ok := s.records[id]
	if !ok || record.Revoked {
		return DelegatedGrantRecord{}, ErrNotFoundOrDenied
	}
	record.Active = true
	s.records[id] = record
	return record, nil
}

func (s *MemoryDelegatedGrantStore) Abort(ctx context.Context, id string) error {
	if s == nil || ctx == nil || ctx.Err() != nil || id == "" {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return ErrInvalidInput
	}
	record, ok := s.records[id]
	if !ok || record.Active {
		return ErrNotFoundOrDenied
	}
	record.Revoked = true
	record.Epoch++
	s.records[id] = record
	return nil
}

func (s *MemoryDelegatedGrantStore) Get(ctx context.Context, id string) (DelegatedGrantRecord, bool, error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return DelegatedGrantRecord{}, false, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return DelegatedGrantRecord{}, false, ErrInvalidInput
	}
	record, ok := s.records[id]
	return record, ok, nil
}

func (s *MemoryDelegatedGrantStore) Revoke(
	ctx context.Context, identity Identity, id string,
) (DelegatedGrantRecord, error) {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return DelegatedGrantRecord{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return DelegatedGrantRecord{}, ErrInvalidInput
	}
	record, ok := s.records[id]
	if !ok || record.Grant.Tenant != identity.Tenant || record.Grant.Principal != identity.Principal {
		return DelegatedGrantRecord{}, ErrNotFoundOrDenied
	}
	if !record.Revoked {
		record.Active = false
		record.Revoked = true
		record.Epoch++
		s.records[id] = record
	}
	return record, nil
}

var _ DelegatedGrantStore = (*MemoryDelegatedGrantStore)(nil)
var _ DelegatedGrantPort = (*DelegatedGate)(nil)
