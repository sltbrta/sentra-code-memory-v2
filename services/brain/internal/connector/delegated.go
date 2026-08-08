// Delegated-permission retrieval for ACL-opaque connectors (issue #309).
//
// Some connector sources cannot expose a resolvable per-object ACL at
// ingest time (the provider's permission model is opaque to the projection).
// For those sources, admitted evidence must not be served on projection
// membership alone: each query needs an explicit, bounded, revocable
// delegated-permission grant plus a live per-object permission probe.
//
// Contract, aligned with docs/specs/security (capability grants, fail-closed
// authorization, sanitized audit):
//
//   - Grants are explicit: issued ahead of time, exact tenant/principal/
//     source-scope binding, no wildcards, bounded lifetime, revocable with an
//     epoch bump. A query names its grant; nothing is inferred.
//   - Network is bounded: at most MaxProbesPerQuery live probes per query,
//     each under ProbeTimeout. Objects beyond the budget are denied, never
//     silently allowed.
//   - Freshness is bounded: probe verdicts are cached at most VerdictTTL and
//     never past grant expiry; revocation invalidates them immediately.
//   - Cache is scoped: a verdict is keyed by tenant, principal, source scope,
//     grant, revocation epoch, and object. It is never reused across any of
//     those dimensions.
//   - Every security phase appends a sanitized, hash-linked audit receipt
//     (digests, counters, reason codes — never raw identities, object IDs or
//     bodies, queries, or tokens).
//   - Missing, expired, revoked, mismatched, or unverifiable authority fails
//     closed: the query abstains with a stable reason code instead of serving
//     opaque evidence.
package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// ErrGrantConflict marks a delegated grant ID that was already issued (or
// revoked); grant IDs are never reusable, so a revoked grant cannot be
// resurrected by re-issuing it.
var ErrGrantConflict = errors.New("connector: delegated grant conflict")

// ErrPromotionEvidenceGateRequired is returned when delegated retrieval is
// composed without the independent component/promotion evidence gate required
// by issue #307. The connector has no implicit development or production allow.
var ErrPromotionEvidenceGateRequired = errors.New("connector: delegated promotion evidence gate required")

// ErrIssuerAuthenticationRequired is returned when grant administration has no
// separately authenticated issuer context. Grant payload identity is never an
// authentication source.
var ErrIssuerAuthenticationRequired = errors.New("connector: delegated issuer authentication required")

// DelegatedComponentEvidenceContractV1 is the only component evidence shape
// emitted by this implementation. A gate that does not recognize it must deny.
const DelegatedComponentEvidenceContractV1 = "ouroboros.connector-component-evidence.v1"

// Delegated gate outcome codes (stable, receipt- and answer-safe).
const (
	DelegatedOutcomeAllowed      = "allowed"
	DelegatedOutcomePartial      = "partial"
	DelegatedOutcomeDenied       = "denied"
	DelegatedOutcomeInvalid      = "invalid_request"
	DelegatedOutcomeGrantMissing = "grant_missing"
	DelegatedOutcomeGrantExpired = "grant_expired"
	DelegatedOutcomeGrantRevoked = "grant_revoked"
	DelegatedOutcomeGrantScope   = "grant_scope_mismatch"
	DelegatedOutcomeGrantIssued  = "grant_issued"
	DelegatedOutcomeAuditFailed  = "audit_unavailable"
	DelegatedOutcomeGateBlocked  = "promotion_evidence_unavailable"
)

// Delegated per-object denial reason codes.
const (
	delegatedDenyProbe   = "probe_denied"
	delegatedDenyError   = "probe_error"
	delegatedDenyBudget  = "probe_budget_exhausted"
	delegatedDenyContent = "content_unavailable"
	delegatedDenyChanged = "content_changed"
)

// delegatedReasonObjectChanged is returned when a matched object is replaced
// under the same ID during the unlocked permission-probe window. The new
// revision was not probed, so it must never be served on ID equality alone.
const delegatedReasonObjectChanged = "delegated_object_changed"

// Hard ceilings so a misconfigured gate can never become unbounded.
const (
	maxDelegatedProbesCeiling   = 64
	maxDelegatedProbeTimeout    = 10 * time.Second
	maxDelegatedVerdictTTL      = time.Hour
	maxDelegatedVerdictsCeiling = 65536
	maxDelegatedGrants          = 4096
	maxDelegatedReceipts        = 512
	maxDelegatedFieldLength     = 128
	maxDelegatedAuditTimeout    = 2 * time.Second
)

// DelegatedGrant is one explicit, bounded, revocable permission delegation
// from a principal to this brain for retrieval over one ACL-opaque source
// scope. It narrows authority; it never replaces the live per-object probe.
type DelegatedGrant struct {
	ID          string
	Tenant      string
	Principal   string
	SourceScope string // exact scope, e.g. "github.com/owner/repo"; no wildcards
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// PermissionProbe answers, live against the connector provider, whether the
// delegated principal may currently read one object. It is one half of the
// required DelegatedProvider port; permission-only implementations fail gate
// construction closed.
type PermissionProbe interface {
	CheckObjectPermission(ctx context.Context, grant DelegatedGrant, objectID string) (bool, error)
}

// DelegatedGateConfig bounds one DelegatedGate.
type DelegatedGateConfig struct {
	// Provider is the live permission and content boundary (required). Both
	// operations are called with the exact grant and object; no fallback exists.
	Provider DelegatedProvider
	// Probe is the legacy permission-only field. It is accepted only when the
	// value also implements DelegatedProvider, keeping source compatibility
	// while failing closed instead of silently using projected content.
	Probe PermissionProbe
	// GrantStore owns single-use grant state. Defaults to a bounded in-memory
	// reference store; durable/database/RPC adapters implement the same port.
	GrantStore DelegatedGrantStore
	// AuditSink optionally persists sanitized receipts outside this process.
	// A sink failure makes the associated authorization fail closed.
	AuditSink DelegatedAuditSink
	// Issuer resolves an authenticated administrator from trusted request
	// context. It is required even when a composition only intends to query:
	// there is no self-authenticating grant issuance helper.
	Issuer DelegatedIssuerPort
	// PromotionGate independently validates versioned component/source evidence
	// before any opaque source is enumerated. It is required and has no default
	// allow; this package makes no live-provider production certification claim.
	PromotionGate DelegatedPromotionGate
	// OpaqueScopes lists the exact source scopes that are ACL-opaque and
	// therefore require delegated-permission retrieval (required, non-empty).
	OpaqueScopes []string
	// MaxProbesPerQuery bounds objects entering each gated provider phase
	// (default 8, cap 64). Each admitted object has at most one content call and
	// one uncached post-hydration permission call in addition to initial ACL.
	MaxProbesPerQuery int
	// ProbeTimeout bounds one live probe (default 2s, cap 10s).
	ProbeTimeout time.Duration
	// VerdictTTL bounds verdict freshness (default 5m, cap 1h). A verdict
	// additionally never outlives its grant.
	VerdictTTL time.Duration
	// MaxCachedVerdicts bounds the verdict cache (default 1024, cap 65536).
	MaxCachedVerdicts int
	// MaxGrantTTL bounds one grant's lifetime at issue (default 24h).
	MaxGrantTTL time.Duration
	// AuditTimeout bounds one external audit append (default/cap 2s).
	AuditTimeout time.Duration
	// StoreTimeout bounds one grant-store operation (default 2s, cap 10s).
	StoreTimeout time.Duration
	// Clock is injectable for tests; defaults to time.Now.
	Clock func() time.Time
}

// DelegatedReceipt is one sanitized, hash-linked audit record of a gated
// query. It carries stable IDs, digests, counters, and reason codes only.
type DelegatedReceipt struct {
	Sequence                 uint64
	AtUnixNano               int64
	TenantDigest             string
	PrincipalDigest          string
	SourceScopeDigest        string
	ConnectorDigest          string
	SourceRevisionDigest     string
	SourceCursorDigest       string
	SourceObservedAtUnixNano int64
	GrantDigest              string
	Outcome                  string
	RequestedObjects         int
	AllowedObjects           int
	ProbeCalls               int
	ContentCalls             int
	CacheHits                int
	DeniedReasons            map[string]int
	PrevDigest               string
	Digest                   string
}

// DelegatedSourceFreshness pins the connector component and exact provider
// view used by one opaque query. Revision and cursor remain raw only inside the
// authorization call; receipts retain domain-separated digests.
type DelegatedSourceFreshness struct {
	ConnectorDigest string
	SourceRevision  string
	SourceCursor    string
	ObservedAt      time.Time
}

type delegatedVerdict struct {
	allowed bool
	expires time.Time
}

// DelegatedGate serializes delegated-permission decisions for ACL-opaque
// source scopes. Safe for concurrent use. Probes run outside the gate lock.
type DelegatedGate struct {
	cfg DelegatedGateConfig

	mu           sync.Mutex
	scopes       map[string]struct{}
	verdicts     map[string]delegatedVerdict
	verdictOrder []string // FIFO eviction order
	auditMu      sync.Mutex
	receipts     []DelegatedReceipt
	sequence     uint64
	lastDigest   string
}

// NewDelegatedGate validates bounds and constructs a gate.
func NewDelegatedGate(cfg DelegatedGateConfig) (*DelegatedGate, error) {
	if cfg.Provider == nil && cfg.Probe != nil {
		cfg.Provider, _ = cfg.Probe.(DelegatedProvider)
	}
	if cfg.Provider == nil || len(cfg.OpaqueScopes) == 0 {
		return nil, ErrInvalidInput
	}
	if cfg.Issuer == nil {
		return nil, ErrIssuerAuthenticationRequired
	}
	if cfg.PromotionGate == nil {
		return nil, ErrPromotionEvidenceGateRequired
	}
	if cfg.GrantStore == nil {
		cfg.GrantStore = NewMemoryDelegatedGrantStore(maxDelegatedGrants)
	}
	scopes := make(map[string]struct{}, len(cfg.OpaqueScopes))
	for _, scope := range cfg.OpaqueScopes {
		if !validDelegatedField(scope) {
			return nil, ErrInvalidInput
		}
		scopes[strings.ToLower(scope)] = struct{}{}
	}
	if cfg.MaxProbesPerQuery <= 0 {
		cfg.MaxProbesPerQuery = 8
	}
	if cfg.MaxProbesPerQuery > maxDelegatedProbesCeiling {
		cfg.MaxProbesPerQuery = maxDelegatedProbesCeiling
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 2 * time.Second
	}
	if cfg.ProbeTimeout > maxDelegatedProbeTimeout {
		cfg.ProbeTimeout = maxDelegatedProbeTimeout
	}
	if cfg.VerdictTTL <= 0 {
		cfg.VerdictTTL = 5 * time.Minute
	}
	if cfg.VerdictTTL > maxDelegatedVerdictTTL {
		cfg.VerdictTTL = maxDelegatedVerdictTTL
	}
	if cfg.MaxCachedVerdicts <= 0 {
		cfg.MaxCachedVerdicts = 1024
	}
	if cfg.MaxCachedVerdicts > maxDelegatedVerdictsCeiling {
		cfg.MaxCachedVerdicts = maxDelegatedVerdictsCeiling
	}
	if cfg.MaxGrantTTL <= 0 {
		cfg.MaxGrantTTL = 24 * time.Hour
	}
	if cfg.AuditTimeout <= 0 || cfg.AuditTimeout > maxDelegatedAuditTimeout {
		cfg.AuditTimeout = maxDelegatedAuditTimeout
	}
	if cfg.StoreTimeout <= 0 {
		cfg.StoreTimeout = 2 * time.Second
	}
	if cfg.StoreTimeout > maxDelegatedProbeTimeout {
		cfg.StoreTimeout = maxDelegatedProbeTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &DelegatedGate{
		cfg:      cfg,
		scopes:   scopes,
		verdicts: make(map[string]delegatedVerdict),
	}, nil
}

// OpaqueScope reports whether a source scope requires delegated retrieval.
func (g *DelegatedGate) OpaqueScope(scope string) bool {
	if g == nil {
		return false
	}
	_, ok := g.scopes[strings.ToLower(scope)]
	return ok
}

// IssueAuthenticatedGrant authenticates its issuer through the separately
// injected DelegatedIssuerPort. Grant IDs are single-use forever:
// a duplicate or previously revoked ID is rejected with ErrGrantConflict.
// A grant whose IssuedAt lies in the future is invalid: authority never
// predates its own issuance, so a pre-dated or clock-skewed window fails
// closed at issue time instead of becoming silently valid later.
//
// Capacity: every issued row is retained forever (including after revoke —
// single-use-forever IDs require remembering them), and every retained row
// counts toward maxDelegatedGrants. The cap is therefore a lifetime issuance
// bound for this in-memory gate, not a live-grant count.
func (g *DelegatedGate) IssueAuthenticatedGrant(ctx context.Context, grant DelegatedGrant) error {
	if g == nil {
		return ErrInvalidInput
	}
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalidInput
	}
	now := g.cfg.Clock()
	if !validDelegatedField(grant.ID) || !validDelegatedField(grant.Tenant) ||
		!validDelegatedField(grant.Principal) || !validDelegatedField(grant.SourceScope) ||
		grant.IssuedAt.IsZero() || grant.ExpiresAt.IsZero() ||
		grant.IssuedAt.After(now) ||
		!grant.IssuedAt.Before(grant.ExpiresAt) ||
		grant.ExpiresAt.Sub(grant.IssuedAt) > g.cfg.MaxGrantTTL ||
		!now.Before(grant.ExpiresAt) {
		return ErrInvalidInput
	}
	if !g.OpaqueScope(grant.SourceScope) {
		return ErrInvalidInput
	}
	issuer, err := g.cfg.Issuer.AuthenticatedDelegatedIssuer(ctx)
	if err != nil || !validIdentity(issuer) {
		return ErrIssuerAuthenticationRequired
	}
	if issuer.Tenant != grant.Tenant || issuer.Principal != grant.Principal {
		return ErrNotFoundOrDenied
	}
	storeCtx, cancel := context.WithTimeout(ctx, g.cfg.StoreTimeout)
	err = g.cfg.GrantStore.Prepare(storeCtx, DelegatedGrantRecord{Grant: grant, Epoch: 1})
	cancel()
	if err != nil {
		return err
	}
	receipt := newDelegatedReceipt(now, issuer, grant.SourceScope, grant, DelegatedSourceFreshness{})
	receipt.Outcome = DelegatedOutcomeGrantIssued
	if _, ok := g.appendReceipt(ctx, receipt); !ok {
		abortCtx, abortCancel := context.WithTimeout(context.Background(), g.cfg.StoreTimeout)
		_ = g.cfg.GrantStore.Abort(abortCtx, grant.ID)
		abortCancel()
		return ErrNotFoundOrDenied
	}
	storeCtx, cancel = context.WithTimeout(ctx, g.cfg.StoreTimeout)
	_, err = g.cfg.GrantStore.Activate(storeCtx, grant.ID)
	cancel()
	return err
}

// RevokeAuthenticatedGrant revokes the authenticated caller's own grant: the
// epoch bumps and every cached verdict for the grant is purged, so no cached
// permission survives revocation. Idempotent for the owner; unknown or foreign
// grants return the single non-disclosing denial.
func (g *DelegatedGate) RevokeAuthenticatedGrant(ctx context.Context, grantID string) error {
	if g == nil || grantID == "" {
		return ErrInvalidInput
	}
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalidInput
	}
	identity, authErr := g.cfg.Issuer.AuthenticatedDelegatedIssuer(ctx)
	if authErr != nil || !validIdentity(identity) {
		return ErrIssuerAuthenticationRequired
	}
	storeCtx, cancel := context.WithTimeout(ctx, g.cfg.StoreTimeout)
	record, err := g.cfg.GrantStore.Revoke(storeCtx, identity, grantID)
	cancel()
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.purgeVerdictsLocked(grantID)
	g.mu.Unlock()
	receipt := newDelegatedReceipt(g.cfg.Clock(), identity, record.Grant.SourceScope, record.Grant, DelegatedSourceFreshness{})
	receipt.Outcome = DelegatedOutcomeGrantRevoked
	if _, ok := g.appendReceipt(ctx, receipt); !ok {
		return ErrNotFoundOrDenied
	}
	return nil
}

// InvalidateObjects drops cached verdicts for object IDs whose admitted
// revisions changed. It is bounded by the reconciled page; over-purging only
// costs a fresh probe and cannot weaken authorization.
func (g *DelegatedGate) InvalidateObjects(objectIDs []string) {
	if g == nil || len(objectIDs) == 0 {
		return
	}
	drop := make(map[string]struct{}, len(objectIDs))
	for _, id := range objectIDs {
		if id == "" {
			continue
		}
		if strings.Contains(id, "\x00") {
			g.mu.Lock()
			g.verdicts = make(map[string]delegatedVerdict)
			g.verdictOrder = nil
			g.mu.Unlock()
			return
		}
		drop[id] = struct{}{}
	}
	if len(drop) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.verdictOrder[:0]
	for _, key := range g.verdictOrder {
		if idx := strings.LastIndex(key, "\x00"); idx >= 0 {
			if _, hit := drop[key[idx+1:]]; hit {
				delete(g.verdicts, key)
				continue
			}
		}
		kept = append(kept, key)
	}
	g.verdictOrder = kept
}

// Authorize gates one query's matched object IDs under one named grant.
// It never returns an error: every failure is encoded fail-closed as an
// empty allowed set plus a receipt outcome, so callers cannot accidentally
// treat a failure as permission. The receipt is appended to the audit chain.
func (g *DelegatedGate) authorize(
	ctx context.Context, identity Identity, sourceScope, grantID string, objectIDs []string,
	freshnessValues ...DelegatedSourceFreshness,
) (map[string]bool, DelegatedReceipt) {
	if g == nil {
		return nil, DelegatedReceipt{Outcome: DelegatedOutcomeInvalid}
	}
	now := g.cfg.Clock()
	var freshness DelegatedSourceFreshness
	if len(freshnessValues) > 0 {
		freshness = freshnessValues[0]
	}
	ids := uniqueSortedIDs(objectIDs)
	receipt := newDelegatedReceipt(now, identity, sourceScope, DelegatedGrant{ID: grantID}, freshness)
	receipt.RequestedObjects = len(ids)
	if ctx == nil || !validIdentity(identity) || sourceScope == "" {
		receipt.Outcome = DelegatedOutcomeInvalid
		return nil, g.mustAppendReceipt(ctx, receipt)
	}

	record, ok, storeErr := g.getGrant(ctx, grantID)
	if storeErr != nil {
		receipt.Outcome = DelegatedOutcomeGrantMissing
		return nil, g.mustAppendReceipt(ctx, receipt)
	}
	g.mu.Lock()
	switch {
	case grantID == "" || !ok || !g.validStoredGrant(grantID, record, now) ||
		record.Grant.Tenant != identity.Tenant || record.Grant.Principal != identity.Principal:
		g.mu.Unlock()
		receipt.Outcome = DelegatedOutcomeGrantMissing
		return nil, g.mustAppendReceipt(ctx, receipt)
	case record.Revoked:
		g.mu.Unlock()
		receipt.Outcome = DelegatedOutcomeGrantRevoked
		return nil, g.mustAppendReceipt(ctx, receipt)
	case !record.Active:
		g.mu.Unlock()
		receipt.Outcome = DelegatedOutcomeGrantMissing
		return nil, g.mustAppendReceipt(ctx, receipt)
	case !now.Before(record.Grant.ExpiresAt):
		g.mu.Unlock()
		receipt.Outcome = DelegatedOutcomeGrantExpired
		return nil, g.mustAppendReceipt(ctx, receipt)
	case !strings.EqualFold(record.Grant.SourceScope, sourceScope):
		g.mu.Unlock()
		receipt.Outcome = DelegatedOutcomeGrantScope
		return nil, g.mustAppendReceipt(ctx, receipt)
	}
	grant := record.Grant
	epoch := record.Epoch
	receipt.GrantDigest = delegatedGrantDigest(grant)

	allowed := make(map[string]bool)
	var misses []string
	for _, id := range ids {
		key := delegatedVerdictKey(grant, epoch, id)
		if v, hit := g.verdicts[key]; hit && now.Before(v.expires) {
			receipt.CacheHits++
			if v.allowed {
				allowed[id] = true
			} else {
				receipt.DeniedReasons[delegatedDenyProbe]++
			}
			continue
		}
		misses = append(misses, id)
	}
	g.mu.Unlock()

	// Live probes: outside every lock, bounded in count and per-call time.
	type probeResult struct {
		id      string
		allowed bool
	}
	var results []probeResult
	for _, id := range misses {
		if receipt.ProbeCalls >= g.cfg.MaxProbesPerQuery {
			receipt.DeniedReasons[delegatedDenyBudget]++
			continue
		}
		if ctx.Err() != nil {
			receipt.DeniedReasons[delegatedDenyError]++
			continue
		}
		receipt.ProbeCalls++
		probeCtx, cancel := context.WithTimeout(ctx, g.cfg.ProbeTimeout)
		ok, err := g.cfg.Provider.CheckObjectPermission(probeCtx, grant, id)
		cancel()
		if err != nil {
			// Errors deny this object and are never cached: unverifiable is
			// not a permission verdict in either direction.
			receipt.DeniedReasons[delegatedDenyError]++
			continue
		}
		results = append(results, probeResult{id: id, allowed: ok})
		if ok {
			allowed[id] = true
		} else {
			receipt.DeniedReasons[delegatedDenyProbe]++
		}
	}

	// Revocation/expiry race re-check: if authority changed while probing,
	// nothing probed under the old epoch may be served or cached.
	record, ok, storeErr = g.getGrant(ctx, grantID)
	g.mu.Lock()
	if storeErr != nil || !ok || !g.validStoredGrant(grantID, record, g.cfg.Clock()) ||
		!record.Active || record.Revoked || record.Epoch != epoch {
		receipt.Outcome = DelegatedOutcomeGrantRevoked
		receipt.AllowedObjects = 0
		g.mu.Unlock()
		return nil, g.mustAppendReceipt(ctx, receipt)
	}
	if !g.cfg.Clock().Before(record.Grant.ExpiresAt) {
		receipt.Outcome = DelegatedOutcomeGrantExpired
		receipt.AllowedObjects = 0
		g.mu.Unlock()
		return nil, g.mustAppendReceipt(ctx, receipt)
	}
	expiry := now.Add(g.cfg.VerdictTTL)
	if grant.ExpiresAt.Before(expiry) {
		expiry = grant.ExpiresAt
	}
	for _, r := range results {
		g.storeVerdictLocked(delegatedVerdictKey(grant, epoch, r.id), delegatedVerdict{
			allowed: r.allowed, expires: expiry,
		})
	}
	receipt.AllowedObjects = len(allowed)
	switch {
	case len(ids) == 0 || len(allowed) == len(ids):
		receipt.Outcome = DelegatedOutcomeAllowed
	case len(allowed) > 0:
		receipt.Outcome = DelegatedOutcomePartial
	default:
		receipt.Outcome = DelegatedOutcomeDenied
	}
	g.mu.Unlock()
	final, auditOK := g.appendReceipt(ctx, receipt)
	if !auditOK {
		return nil, final
	}
	return allowed, final
}

// PreauthorizeQuery validates the exact grant before the kernel enumerates or
// searches any objects. Successful preauthorization is intentionally not
// cached and emits no receipt yet; the object phase revalidates the grant and
// records the complete decision. Denials emit one sanitized receipt.
func (g *DelegatedGate) preauthorizeQuery(
	ctx context.Context, identity Identity, sourceScope, grantID string,
	freshness DelegatedSourceFreshness,
) (DelegatedGrantRecord, string) {
	if g == nil {
		return DelegatedGrantRecord{}, DelegatedOutcomeInvalid
	}
	now := g.cfg.Clock()
	receipt := newDelegatedReceipt(now, identity, sourceScope, DelegatedGrant{ID: grantID}, freshness)
	deny := func(outcome string) (DelegatedGrantRecord, string) {
		receipt.Outcome = outcome
		appended, ok := g.appendReceipt(ctx, receipt)
		if !ok {
			return DelegatedGrantRecord{}, DelegatedOutcomeAuditFailed
		}
		return DelegatedGrantRecord{}, appended.Outcome
	}
	if ctx == nil || ctx.Err() != nil || !validIdentity(identity) || sourceScope == "" {
		return deny(DelegatedOutcomeInvalid)
	}
	componentEvidence, valid := delegatedComponentEvidence(freshness, now)
	if !valid {
		return deny(DelegatedOutcomeGateBlocked)
	}
	gateCtx, cancel := context.WithTimeout(ctx, g.cfg.ProbeTimeout)
	err := g.cfg.PromotionGate.AuthorizeDelegatedComponent(gateCtx, componentEvidence)
	cancel()
	if err != nil {
		return deny(DelegatedOutcomeGateBlocked)
	}
	record, ok, err := g.getGrant(ctx, grantID)
	switch {
	case err != nil || grantID == "" || !ok || !g.validStoredGrant(grantID, record, now) ||
		record.Grant.Tenant != identity.Tenant || record.Grant.Principal != identity.Principal:
		return deny(DelegatedOutcomeGrantMissing)
	case record.Revoked:
		return deny(DelegatedOutcomeGrantRevoked)
	case !record.Active:
		return deny(DelegatedOutcomeGrantMissing)
	case !now.Before(record.Grant.ExpiresAt):
		return deny(DelegatedOutcomeGrantExpired)
	case !strings.EqualFold(record.Grant.SourceScope, sourceScope):
		return deny(DelegatedOutcomeGrantScope)
	default:
		return record, ""
	}
}

// HydrateAuthorized performs the live content probe and a second live
// permission check after hydration. Only objects allowed by both permission
// checks may reach answer construction. Content and object IDs are never
// cached or written to receipts.
func (g *DelegatedGate) hydrateAuthorized(
	ctx context.Context,
	identity Identity,
	sourceScope string,
	record DelegatedGrantRecord,
	objects []Object,
	freshness DelegatedSourceFreshness,
) ([]Object, DelegatedReceipt) {
	now := g.cfg.Clock()
	receipt := newDelegatedReceipt(now, identity, sourceScope, record.Grant, freshness)
	receipt.RequestedObjects = len(objects)
	receipt.DeniedReasons = make(map[string]int)
	hydrated := make([]Object, 0, len(objects))

	for i, projected := range objects {
		if i >= g.cfg.MaxProbesPerQuery {
			receipt.DeniedReasons[delegatedDenyBudget]++
			continue
		}
		if ctx.Err() != nil {
			receipt.DeniedReasons[delegatedDenyError]++
			continue
		}
		receipt.ContentCalls++
		contentCtx, cancel := context.WithTimeout(ctx, g.cfg.ProbeTimeout)
		live, err := g.cfg.Provider.HydrateObject(contentCtx, record.Grant, projected)
		cancel()
		if err != nil {
			receipt.DeniedReasons[delegatedDenyContent]++
			continue
		}
		if live.ID != projected.ID || live.Deleted || live.Version == "" || live != projected {
			receipt.DeniedReasons[delegatedDenyChanged]++
			continue
		}

		// This check is deliberately live and never satisfied by the verdict
		// cache: revocation between permission allow and content hydration wins.
		receipt.ProbeCalls++
		probeCtx, cancel := context.WithTimeout(ctx, g.cfg.ProbeTimeout)
		allowed, err := g.cfg.Provider.CheckObjectPermission(probeCtx, record.Grant, live.ID)
		cancel()
		if err != nil {
			receipt.DeniedReasons[delegatedDenyError]++
			continue
		}
		if !allowed {
			receipt.DeniedReasons[delegatedDenyProbe]++
			continue
		}
		hydrated = append(hydrated, live)
	}

	current, ok, err := g.getGrant(ctx, record.Grant.ID)
	if err != nil || !ok || !g.validStoredGrant(record.Grant.ID, current, g.cfg.Clock()) ||
		!current.Active || current.Revoked || current.Epoch != record.Epoch {
		receipt.Outcome = DelegatedOutcomeGrantRevoked
		hydrated = nil
	} else if !g.cfg.Clock().Before(current.Grant.ExpiresAt) {
		receipt.Outcome = DelegatedOutcomeGrantExpired
		hydrated = nil
	} else {
		receipt.AllowedObjects = len(hydrated)
		switch {
		case len(objects) == 0 || len(hydrated) == len(objects):
			receipt.Outcome = DelegatedOutcomeAllowed
		case len(hydrated) > 0:
			receipt.Outcome = DelegatedOutcomePartial
		default:
			receipt.Outcome = DelegatedOutcomeDenied
		}
	}
	final, auditOK := g.appendReceipt(ctx, receipt)
	if !auditOK {
		final.Outcome = DelegatedOutcomeAuditFailed
		return nil, final
	}
	return hydrated, final
}

// Receipts returns a copy of the retained audit chain (oldest first).
func (g *DelegatedGate) Receipts() []DelegatedReceipt {
	if g == nil {
		return nil
	}
	g.auditMu.Lock()
	defer g.auditMu.Unlock()
	out := make([]DelegatedReceipt, len(g.receipts))
	copy(out, g.receipts)
	for i := range out {
		reasons := make(map[string]int, len(g.receipts[i].DeniedReasons))
		for k, v := range g.receipts[i].DeniedReasons {
			reasons[k] = v
		}
		out[i].DeniedReasons = reasons
	}
	return out
}

// VerifyReceiptChain recomputes every retained receipt digest and link.
// A broken digest or link is a visible integrity failure.
func (g *DelegatedGate) VerifyReceiptChain() bool {
	receipts := g.Receipts()
	for i, r := range receipts {
		if delegatedReceiptDigest(r) != r.Digest {
			return false
		}
		if i > 0 && r.PrevDigest != receipts[i-1].Digest {
			return false
		}
	}
	return true
}

func (g *DelegatedGate) appendReceipt(ctx context.Context, receipt DelegatedReceipt) (DelegatedReceipt, bool) {
	g.auditMu.Lock()
	defer g.auditMu.Unlock()
	return g.appendReceiptSerialized(ctx, receipt)
}

func (g *DelegatedGate) appendReceiptSerialized(
	ctx context.Context, receipt DelegatedReceipt,
) (DelegatedReceipt, bool) {
	g.sequence++
	receipt.Sequence = g.sequence
	receipt.PrevDigest = g.lastDigest
	receipt.Digest = delegatedReceiptDigest(receipt)
	g.lastDigest = receipt.Digest
	g.receipts = append(g.receipts, receipt)
	if len(g.receipts) > maxDelegatedReceipts {
		g.receipts = g.receipts[len(g.receipts)-maxDelegatedReceipts:]
	}
	if g.cfg.AuditSink == nil {
		return receipt, true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auditCtx, cancel := context.WithTimeout(ctx, g.cfg.AuditTimeout)
	err := g.cfg.AuditSink.AppendDelegatedReceipt(auditCtx, receipt)
	cancel()
	if err == nil {
		return receipt, true
	}
	// Keep the local chain honest about the external append failure. Audit
	// sinks must make one Append atomic: returning an error means no receipt
	// was durably accepted.
	receipt.Outcome = DelegatedOutcomeAuditFailed
	receipt.Digest = delegatedReceiptDigest(receipt)
	g.lastDigest = receipt.Digest
	g.receipts[len(g.receipts)-1] = receipt
	return receipt, false
}

func (g *DelegatedGate) mustAppendReceipt(ctx context.Context, receipt DelegatedReceipt) DelegatedReceipt {
	appended, ok := g.appendReceipt(ctx, receipt)
	if ok {
		return appended
	}
	appended.Outcome = DelegatedOutcomeAuditFailed
	appended.Digest = delegatedReceiptDigest(appended)
	return appended
}

func (g *DelegatedGate) storeVerdictLocked(key string, v delegatedVerdict) {
	if _, exists := g.verdicts[key]; !exists {
		for len(g.verdicts) >= g.cfg.MaxCachedVerdicts && len(g.verdictOrder) > 0 {
			oldest := g.verdictOrder[0]
			g.verdictOrder = g.verdictOrder[1:]
			delete(g.verdicts, oldest)
		}
		g.verdictOrder = append(g.verdictOrder, key)
	}
	g.verdicts[key] = v
}

func (g *DelegatedGate) purgeVerdictsLocked(grantID string) {
	marker := "\x00" + grantID + "\x00"
	kept := g.verdictOrder[:0]
	for _, key := range g.verdictOrder {
		if strings.Contains(key, marker) {
			delete(g.verdicts, key)
			continue
		}
		kept = append(kept, key)
	}
	g.verdictOrder = kept
}

func delegatedVerdictKey(grant DelegatedGrant, epoch uint64, objectID string) string {
	return strings.Join([]string{
		grant.Tenant, grant.Principal, strings.ToLower(grant.SourceScope),
		grant.ID, strconv.FormatUint(epoch, 10), objectID,
	}, "\x00")
}

func delegatedGrantDigest(grant DelegatedGrant) string {
	return sha256Hex([]byte(strings.Join([]string{
		"ouroboros.delegated-grant.v1",
		grant.ID, grant.Tenant, grant.Principal, strings.ToLower(grant.SourceScope),
		strconv.FormatInt(grant.IssuedAt.UnixNano(), 10),
		strconv.FormatInt(grant.ExpiresAt.UnixNano(), 10),
	}, "\x00")))
}

func delegatedReceiptDigest(receipt DelegatedReceipt) string {
	reasons := make([]string, 0, len(receipt.DeniedReasons))
	for name, count := range receipt.DeniedReasons {
		reasons = append(reasons, name+"="+strconv.Itoa(count))
	}
	sort.Strings(reasons)
	return sha256Hex([]byte(strings.Join([]string{
		"ouroboros.delegated-receipt.v1",
		strconv.FormatUint(receipt.Sequence, 10),
		strconv.FormatInt(receipt.AtUnixNano, 10),
		receipt.TenantDigest, receipt.PrincipalDigest, receipt.SourceScopeDigest,
		receipt.ConnectorDigest, receipt.SourceRevisionDigest, receipt.SourceCursorDigest,
		strconv.FormatInt(receipt.SourceObservedAtUnixNano, 10),
		receipt.GrantDigest, receipt.Outcome,
		strconv.Itoa(receipt.RequestedObjects), strconv.Itoa(receipt.AllowedObjects),
		strconv.Itoa(receipt.ProbeCalls), strconv.Itoa(receipt.ContentCalls), strconv.Itoa(receipt.CacheHits),
		strings.Join(reasons, ","),
		receipt.PrevDigest,
	}, "\x00")))
}

func newDelegatedReceipt(
	now time.Time, identity Identity, sourceScope string, grant DelegatedGrant,
	freshness DelegatedSourceFreshness,
) DelegatedReceipt {
	receipt := DelegatedReceipt{
		AtUnixNano:               now.UnixNano(),
		TenantDigest:             delegatedFieldDigest("tenant", identity.Tenant),
		PrincipalDigest:          delegatedFieldDigest("principal", identity.Principal),
		SourceScopeDigest:        delegatedFieldDigest("scope", strings.ToLower(sourceScope)),
		ConnectorDigest:          freshness.ConnectorDigest,
		SourceRevisionDigest:     delegatedFieldDigest("source-revision", freshness.SourceRevision),
		SourceCursorDigest:       delegatedFieldDigest("source-cursor", freshness.SourceCursor),
		SourceObservedAtUnixNano: freshness.ObservedAt.UnixNano(),
		DeniedReasons:            map[string]int{},
	}
	if freshness.ObservedAt.IsZero() {
		receipt.SourceObservedAtUnixNano = 0
	}
	if grant.ID != "" && grant.Tenant != "" && grant.Principal != "" && grant.SourceScope != "" {
		receipt.GrantDigest = delegatedGrantDigest(grant)
	} else if grant.ID != "" {
		receipt.GrantDigest = delegatedFieldDigest("grant", grant.ID)
	}
	return receipt
}

func delegatedComponentEvidence(
	freshness DelegatedSourceFreshness, now time.Time,
) (DelegatedComponentEvidence, bool) {
	if !isLowerHexSHA256(freshness.ConnectorDigest) ||
		!validDelegatedField(freshness.SourceRevision) ||
		!validDelegatedCursor(freshness.SourceCursor) || freshness.ObservedAt.IsZero() ||
		freshness.ObservedAt.After(now) {
		return DelegatedComponentEvidence{}, false
	}
	return DelegatedComponentEvidence{
		ContractVersion: DelegatedComponentEvidenceContractV1,
		ConnectorDigest: freshness.ConnectorDigest,
		SourceRevision:  freshness.SourceRevision,
		SourceCursor:    freshness.SourceCursor,
		ObservedAt:      freshness.ObservedAt.UnixNano(),
	}, true
}

func validDelegatedCursor(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && trimmed == value && len(value) <= 512 && !strings.Contains(value, "\x00")
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func delegatedFieldDigest(kind, value string) string {
	if value == "" {
		return ""
	}
	return sha256Hex([]byte("ouroboros.delegated-field.v1\x00" + kind + "\x00" + value))
}

func validDelegatedField(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && trimmed == value && len(value) <= maxDelegatedFieldLength &&
		!strings.Contains(value, "*") && !strings.Contains(value, "\x00")
}

func (g *DelegatedGate) validStoredGrant(id string, record DelegatedGrantRecord, now time.Time) bool {
	grant := record.Grant
	return record.Epoch > 0 && grant.ID == id &&
		validDelegatedField(grant.ID) && validDelegatedField(grant.Tenant) &&
		validDelegatedField(grant.Principal) && validDelegatedField(grant.SourceScope) &&
		!grant.IssuedAt.IsZero() && !grant.ExpiresAt.IsZero() &&
		!grant.IssuedAt.After(now) && grant.IssuedAt.Before(grant.ExpiresAt) &&
		grant.ExpiresAt.Sub(grant.IssuedAt) <= g.cfg.MaxGrantTTL &&
		g.OpaqueScope(grant.SourceScope)
}

func (g *DelegatedGate) getGrant(
	ctx context.Context, id string,
) (DelegatedGrantRecord, bool, error) {
	if ctx == nil {
		return DelegatedGrantRecord{}, false, ErrInvalidInput
	}
	storeCtx, cancel := context.WithTimeout(ctx, g.cfg.StoreTimeout)
	record, ok, err := g.cfg.GrantStore.Get(storeCtx, id)
	cancel()
	return record, ok, err
}

func uniqueSortedIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// gateDelegatedQuery applies the delegated gate to one query's matches.
// Returns the surviving matches, extra degraded reasons for a served answer,
// and a non-empty abstain reason when the query must fail closed.
//
// It deliberately takes the captured source scope instead of the live
// *connection: it runs outside the kernel mutex (probes must not block
// lifecycle operations), so it may only touch immutable copies. The caller
// revalidates the connection lookup and ACL epoch under the lock afterwards.
func (k *Kernel) gateDelegatedQuery(
	ctx context.Context,
	identity Identity,
	sourceScope, grantID, query string,
	record DelegatedGrantRecord,
	matches []Object,
	freshness DelegatedSourceFreshness,
) ([]Object, []string, string) {
	sorted := make([]Object, len(matches))
	copy(sorted, matches)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	ids := make([]string, len(sorted))
	for i, obj := range sorted {
		ids[i] = obj.ID
	}
	allowed, receipt := k.delegated.authorize(ctx, identity, sourceScope, grantID, ids, freshness)
	switch receipt.Outcome {
	case DelegatedOutcomeAllowed, DelegatedOutcomePartial, DelegatedOutcomeDenied:
		permissionAllowed := make([]Object, 0, len(sorted))
		for _, obj := range sorted {
			if allowed[obj.ID] {
				permissionAllowed = append(permissionAllowed, obj)
			}
		}
		if len(sorted) > 0 && len(permissionAllowed) == 0 {
			return nil, nil, "delegated_denied"
		}
		hydrated, hydrationReceipt := k.delegated.hydrateAuthorized(
			ctx, identity, sourceScope, record, permissionAllowed, freshness,
		)
		if hydrationReceipt.Outcome != DelegatedOutcomeAllowed &&
			hydrationReceipt.Outcome != DelegatedOutcomePartial &&
			hydrationReceipt.Outcome != DelegatedOutcomeDenied {
			return nil, nil, fmt.Sprintf("delegated_%s", hydrationReceipt.Outcome)
		}
		if len(permissionAllowed) > 0 && len(hydrated) == 0 {
			if hydrationReceipt.DeniedReasons[delegatedDenyChanged] > 0 {
				return nil, nil, delegatedReasonObjectChanged
			}
			return nil, nil, "delegated_denied"
		}
		final := make([]Object, 0, len(hydrated))
		for _, live := range hydrated {
			if objectMatchesQuery(live, query) {
				final = append(final, live)
			}
		}
		if len(permissionAllowed) > 0 && len(final) == 0 {
			return nil, nil, delegatedReasonObjectChanged
		}
		if len(final) < len(sorted) {
			return final, []string{"delegated_partial"}, ""
		}
		return final, nil, ""
	default:
		return nil, nil, fmt.Sprintf("delegated_%s", receipt.Outcome)
	}
}

func delegatedAbstain(conn *connection, reason string) *contractsv1.QueryConnectorEvidenceSuccess {
	return &contractsv1.QueryConnectorEvidenceSuccess{
		Answer: &contractsv1.ConnectorAnswer{
			Status:             contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED,
			DegradedReasons:    []string{reason},
			FactualConsistency: factualConsistencyAbstained(),
		},
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:        lifecycleState(conn.state),
	}
}
