package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

const testScope = "github.com/ouroboros-dogfood/sample-repo"

type testIssuerContextKey struct{}

type testIssuerPort struct{}

func (testIssuerPort) AuthenticatedDelegatedIssuer(ctx context.Context) (Identity, error) {
	identity, ok := ctx.Value(testIssuerContextKey{}).(Identity)
	if !ok || !validIdentity(identity) {
		return Identity{}, ErrIssuerAuthenticationRequired
	}
	return identity, nil
}

type allowTestPromotionGate struct{}

func (allowTestPromotionGate) AuthorizeDelegatedComponent(
	_ context.Context, evidence DelegatedComponentEvidence,
) error {
	if evidence.ContractVersion != DelegatedComponentEvidenceContractV1 ||
		!isLowerHexSHA256(evidence.ConnectorDigest) || evidence.SourceRevision == "" ||
		evidence.SourceCursor == "" || evidence.ObservedAt == 0 {
		return ErrPromotionEvidenceGateRequired
	}
	return nil
}

type denyTestPromotionGate struct{}

func (denyTestPromotionGate) AuthorizeDelegatedComponent(
	context.Context, DelegatedComponentEvidence,
) error {
	return ErrPromotionEvidenceGateRequired
}

func authenticatedTestContext(identity Identity) context.Context {
	return context.WithValue(context.Background(), testIssuerContextKey{}, identity)
}

func issueGrant(gate *DelegatedGate, grant DelegatedGrant) error {
	return gate.IssueAuthenticatedGrant(
		authenticatedTestContext(Identity{
			Tenant: grant.Tenant, Principal: grant.Principal, Session: testSession,
		}),
		grant,
	)
}

func revokeGrant(gate *DelegatedGate, identity Identity, grantID string) error {
	return gate.RevokeAuthenticatedGrant(authenticatedTestContext(identity), grantID)
}

func testFreshness(clock *fakeClock) DelegatedSourceFreshness {
	return DelegatedSourceFreshness{
		ConnectorDigest: fakeConnectorDigest(), SourceRevision: "snapshot-v1",
		SourceCursor: "cursor-v1", ObservedAt: clock.Now(),
	}
}

type fakeProbe struct {
	mu      sync.Mutex
	calls   int
	allow   map[string]bool
	failErr error
}

type permissionOnlyProbe struct{}

func (permissionOnlyProbe) CheckObjectPermission(context.Context, DelegatedGrant, string) (bool, error) {
	return true, nil
}

func (p *fakeProbe) CheckObjectPermission(ctx context.Context, grant DelegatedGrant, objectID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.failErr != nil {
		return false, p.failErr
	}
	if p.allow == nil {
		return true, nil
	}
	return p.allow[objectID], nil
}

func (p *fakeProbe) HydrateObject(
	ctx context.Context, _ DelegatedGrant, projected Object,
) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	return projected, nil
}

func (p *fakeProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testGate(t *testing.T, probe PermissionProbe, clock *fakeClock, mutate func(*DelegatedGateConfig)) *DelegatedGate {
	t.Helper()
	cfg := DelegatedGateConfig{
		Probe:         probe,
		Issuer:        testIssuerPort{},
		PromotionGate: allowTestPromotionGate{},
		OpaqueScopes:  []string{testScope},
		Clock:         clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	gate, err := NewDelegatedGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

func testGrant(id, principal string, clock *fakeClock) DelegatedGrant {
	return DelegatedGrant{
		ID: id, Tenant: testTenant, Principal: principal, SourceScope: testScope,
		IssuedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour),
	}
}

func identityFor(principal string) Identity {
	return Identity{Tenant: testTenant, Principal: principal, Session: testSession}
}

func TestDelegatedGateFailClosedGrantLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, nil)

	// Missing grant: fail closed, zero probes.
	allowed, receipt := gate.authorize(ctx, testIdentity(), testScope, "", []string{"obj-1"})
	if len(allowed) != 0 || receipt.Outcome != DelegatedOutcomeGrantMissing || probe.callCount() != 0 {
		t.Fatalf("missing grant: allowed=%v outcome=%q probes=%d", allowed, receipt.Outcome, probe.callCount())
	}

	if err := issueGrant(gate, testGrant("grant-1", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}

	// Unknown grant ID is the same non-disclosing missing outcome.
	_, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-unknown", []string{"obj-1"})
	if receipt.Outcome != DelegatedOutcomeGrantMissing {
		t.Fatalf("unknown grant outcome = %q", receipt.Outcome)
	}

	// Valid grant: live probe allows, receipt records the probe.
	allowed, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1"})
	if !allowed["obj-1"] || receipt.Outcome != DelegatedOutcomeAllowed ||
		receipt.ProbeCalls != 1 || receipt.CacheHits != 0 || receipt.GrantDigest == "" {
		t.Fatalf("allow: allowed=%v receipt=%+v", allowed, receipt)
	}

	// Fresh verdict is cached: no second probe within TTL.
	allowed, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1"})
	if !allowed["obj-1"] || receipt.CacheHits != 1 || receipt.ProbeCalls != 0 || probe.callCount() != 1 {
		t.Fatalf("cache hit: receipt=%+v probes=%d", receipt, probe.callCount())
	}

	// Freshness bound: past VerdictTTL the object is re-probed.
	clock.Advance(6 * time.Minute)
	_, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1"})
	if receipt.ProbeCalls != 1 || probe.callCount() != 2 {
		t.Fatalf("stale verdict not re-probed: receipt=%+v probes=%d", receipt, probe.callCount())
	}

	// Expired grant: fail closed, no probe.
	clock.Advance(2 * time.Hour)
	before := probe.callCount()
	allowed, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1"})
	if len(allowed) != 0 || receipt.Outcome != DelegatedOutcomeGrantExpired || probe.callCount() != before {
		t.Fatalf("expired: allowed=%v outcome=%q", allowed, receipt.Outcome)
	}

	// Revoked grant: fail closed; revoke is owner-only and idempotent.
	if err := issueGrant(gate, testGrant("grant-2", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	if _, r := gate.authorize(ctx, testIdentity(), testScope, "grant-2", []string{"obj-1"}); r.Outcome != DelegatedOutcomeAllowed {
		t.Fatalf("pre-revoke outcome = %q", r.Outcome)
	}
	if err := revokeGrant(gate, identityFor("principal-other"), "grant-2"); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("foreign revoke err = %v", err)
	}
	if err := revokeGrant(gate, testIdentity(), "grant-2"); err != nil {
		t.Fatal(err)
	}
	if err := revokeGrant(gate, testIdentity(), "grant-2"); err != nil {
		t.Fatalf("idempotent revoke err = %v", err)
	}
	before = probe.callCount()
	allowed, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-2", []string{"obj-1"})
	if len(allowed) != 0 || receipt.Outcome != DelegatedOutcomeGrantRevoked || probe.callCount() != before {
		t.Fatalf("revoked: allowed=%v outcome=%q", allowed, receipt.Outcome)
	}

	// Revoked grant IDs are never reusable.
	if err := issueGrant(gate, testGrant("grant-2", testPrincipal, clock)); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("reissue err = %v", err)
	}
}

func TestDelegatedGateCacheScoping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, nil)

	if err := issueGrant(gate, testGrant("grant-a", "principal-a", clock)); err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("grant-b", "principal-b", clock)); err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("grant-a2", "principal-a", clock)); err != nil {
		t.Fatal(err)
	}

	// Principal A probes once, then hits its own scoped cache.
	gate.authorize(ctx, identityFor("principal-a"), testScope, "grant-a", []string{"obj-1"})
	gate.authorize(ctx, identityFor("principal-a"), testScope, "grant-a", []string{"obj-1"})
	if probe.callCount() != 1 {
		t.Fatalf("principal-a probes = %d", probe.callCount())
	}

	// Principal B never reuses A's verdict for the same object.
	_, receipt := gate.authorize(ctx, identityFor("principal-b"), testScope, "grant-b", []string{"obj-1"})
	if receipt.CacheHits != 0 || receipt.ProbeCalls != 1 || probe.callCount() != 2 {
		t.Fatalf("cross-principal reuse: receipt=%+v probes=%d", receipt, probe.callCount())
	}

	// A different grant for the same principal is also probed separately.
	_, receipt = gate.authorize(ctx, identityFor("principal-a"), testScope, "grant-a2", []string{"obj-1"})
	if receipt.CacheHits != 0 || receipt.ProbeCalls != 1 || probe.callCount() != 3 {
		t.Fatalf("cross-grant reuse: receipt=%+v probes=%d", receipt, probe.callCount())
	}

	// Tenant mismatch is the non-disclosing missing outcome.
	foreign := Identity{Tenant: "tenant-z", Principal: "principal-a", Session: testSession}
	if _, r := gate.authorize(ctx, foreign, testScope, "grant-a", []string{"obj-1"}); r.Outcome != DelegatedOutcomeGrantMissing {
		t.Fatalf("tenant mismatch outcome = %q", r.Outcome)
	}

	// Scope mismatch fails closed even with a live grant.
	if _, r := gate.authorize(ctx, identityFor("principal-a"), "github.com/other/repo", "grant-a", []string{"obj-1"}); r.Outcome != DelegatedOutcomeGrantScope {
		t.Fatalf("scope mismatch outcome = %q", r.Outcome)
	}

	// Revocation purges cached verdicts for exactly that grant.
	if err := revokeGrant(gate, identityFor("principal-a"), "grant-a"); err != nil {
		t.Fatal(err)
	}
	_, receipt = gate.authorize(ctx, identityFor("principal-b"), testScope, "grant-b", []string{"obj-1"})
	if receipt.CacheHits != 1 || probe.callCount() != 3 {
		t.Fatalf("unrelated grant lost its cache: receipt=%+v probes=%d", receipt, probe.callCount())
	}
}

func TestDelegatedGateBoundedProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.MaxProbesPerQuery = 2
	})
	if err := issueGrant(gate, testGrant("grant-1", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}

	ids := []string{"obj-1", "obj-2", "obj-3", "obj-4", "obj-5"}
	allowed, receipt := gate.authorize(ctx, testIdentity(), testScope, "grant-1", ids)
	if receipt.ProbeCalls != 2 || probe.callCount() != 2 {
		t.Fatalf("probe budget exceeded: receipt=%+v probes=%d", receipt, probe.callCount())
	}
	if len(allowed) != 2 || receipt.Outcome != DelegatedOutcomePartial ||
		receipt.DeniedReasons[delegatedDenyBudget] != 3 {
		t.Fatalf("budget denial not fail-closed: allowed=%v receipt=%+v", allowed, receipt)
	}

	// Probe errors deny and are never cached as permission.
	probe.failErr = errors.New("provider unavailable")
	allowed, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-9"})
	if len(allowed) != 0 || receipt.Outcome != DelegatedOutcomeDenied ||
		receipt.DeniedReasons[delegatedDenyError] != 1 {
		t.Fatalf("probe error not fail-closed: allowed=%v receipt=%+v", allowed, receipt)
	}
	probe.failErr = nil
	_, receipt = gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-9"})
	if receipt.ProbeCalls != 1 || len(receipt.DeniedReasons) != 0 {
		t.Fatalf("error verdict was cached: receipt=%+v", receipt)
	}
}

func TestDelegatedVerdictCacheCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.MaxCachedVerdicts = 1
	})
	if err := issueGrant(gate, testGrant("cache-cap-grant", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	gate.authorize(ctx, testIdentity(), testScope, "cache-cap-grant", []string{"obj-1"})
	gate.authorize(ctx, testIdentity(), testScope, "cache-cap-grant", []string{"obj-2"})
	gate.authorize(ctx, testIdentity(), testScope, "cache-cap-grant", []string{"obj-1"})
	if probe.callCount() != 3 {
		t.Fatalf("bounded cache failed to evict oldest verdict: probes=%d", probe.callCount())
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.verdicts) != 1 || len(gate.verdictOrder) != 1 {
		t.Fatalf("cache exceeded bound: verdicts=%d order=%d", len(gate.verdicts), len(gate.verdictOrder))
	}
}

func TestDelegatedGrantValidation(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	gate := testGate(t, &fakeProbe{}, clock, nil)

	valid := testGrant("grant-ok", testPrincipal, clock)
	cases := map[string]DelegatedGrant{
		"empty id":         func() DelegatedGrant { g := valid; g.ID = ""; return g }(),
		"wildcard scope":   func() DelegatedGrant { g := valid; g.SourceScope = "github.com/owner/*"; return g }(),
		"non-opaque scope": func() DelegatedGrant { g := valid; g.SourceScope = "github.com/other/repo"; return g }(),
		"empty principal":  func() DelegatedGrant { g := valid; g.Principal = ""; return g }(),
		"already expired":  func() DelegatedGrant { g := valid; g.ExpiresAt = clock.Now().Add(-time.Minute); return g }(),
		"future issued_at": func() DelegatedGrant {
			g := valid
			g.IssuedAt = clock.Now().Add(time.Minute)
			g.ExpiresAt = clock.Now().Add(time.Hour)
			return g
		}(),
		"inverted window": func() DelegatedGrant { g := valid; g.ExpiresAt = g.IssuedAt.Add(-time.Hour); return g }(),
		"unbounded ttl":   func() DelegatedGrant { g := valid; g.ExpiresAt = g.IssuedAt.Add(48 * time.Hour); return g }(),
	}
	for name, grant := range cases {
		if err := issueGrant(gate, grant); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if err := issueGrant(gate, valid); err != nil {
		t.Fatal(err)
	}
	foreignIssue := testGrant("foreign-issue", testPrincipal, clock)
	if err := gate.IssueAuthenticatedGrant(
		authenticatedTestContext(identityFor("principal-other")), foreignIssue,
	); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("foreign issuer err = %v", err)
	}
	if err := issueGrant(gate, valid); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("duplicate id err = %v", err)
	}
}

func TestDelegatedGateRejectsPermissionOnlyProvider(t *testing.T) {
	t.Parallel()
	if _, err := NewDelegatedGate(DelegatedGateConfig{
		Probe: permissionOnlyProbe{}, OpaqueScopes: []string{testScope},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("permission-only provider err = %v", err)
	}
}

func TestDelegatedGateRequiresIndependentIssuerAndPromotionPorts(t *testing.T) {
	t.Parallel()
	base := DelegatedGateConfig{
		Provider: &fakeProbe{}, OpaqueScopes: []string{testScope},
	}
	if _, err := NewDelegatedGate(base); !errors.Is(err, ErrIssuerAuthenticationRequired) {
		t.Fatalf("missing issuer err = %v", err)
	}
	base.Issuer = testIssuerPort{}
	if _, err := NewDelegatedGate(base); !errors.Is(err, ErrPromotionEvidenceGateRequired) {
		t.Fatalf("missing promotion gate err = %v", err)
	}
}

func TestDelegatedGrantCannotSelfAuthenticateFromPayload(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	gate := testGate(t, &fakeProbe{}, clock, nil)
	grant := testGrant("no-self-auth", testPrincipal, clock)
	if err := gate.IssueAuthenticatedGrant(context.Background(), grant); !errors.Is(err, ErrIssuerAuthenticationRequired) {
		t.Fatalf("untrusted payload authenticated itself: %v", err)
	}
	if _, ok, err := gate.cfg.GrantStore.Get(context.Background(), grant.ID); err != nil || ok {
		t.Fatalf("unauthenticated grant persisted: ok=%v err=%v", ok, err)
	}
}

type failingAuditSink struct{}

func (failingAuditSink) AppendDelegatedReceipt(context.Context, DelegatedReceipt) error {
	return errors.New("audit unavailable")
}

func TestDelegatedGrantAuditFailureLeavesPermanentInactiveTombstone(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	store := NewMemoryDelegatedGrantStore(4)
	gate := testGate(t, &fakeProbe{}, clock, func(cfg *DelegatedGateConfig) {
		cfg.GrantStore = store
		cfg.AuditSink = failingAuditSink{}
	})
	grant := testGrant("audit-failed-issue", testPrincipal, clock)
	if err := issueGrant(gate, grant); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("audit failure err = %v", err)
	}
	record, ok, err := store.Get(context.Background(), grant.ID)
	if err != nil || !ok {
		t.Fatalf("tombstone lookup: ok=%v err=%v", ok, err)
	}
	if record.Active || !record.Revoked {
		t.Fatalf("audit failure left usable grant: %+v", record)
	}
	if _, err := store.Activate(context.Background(), grant.ID); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("audit-failed tombstone activated: %v", err)
	}
	if err := issueGrant(gate, grant); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("audit-failed ID reused: %v", err)
	}
}

func TestDelegatedReceiptChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	gate := testGate(t, &fakeProbe{}, clock, nil)
	if err := issueGrant(gate, testGrant("grant-1", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}

	gate.authorize(ctx, testIdentity(), testScope, "", []string{"obj-1"})
	gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1", "obj-2"})
	gate.authorize(ctx, testIdentity(), testScope, "grant-1", []string{"obj-1"})

	receipts := gate.Receipts()
	if len(receipts) != 4 {
		t.Fatalf("receipts = %d", len(receipts))
	}
	if !gate.VerifyReceiptChain() {
		t.Fatal("valid chain failed verification")
	}
	for i, r := range receipts {
		if r.Sequence != uint64(i+1) || r.Digest == "" {
			t.Fatalf("receipt %d = %+v", i, r)
		}
		if i > 0 && r.PrevDigest != receipts[i-1].Digest {
			t.Fatalf("chain link broken at %d", i)
		}
		// Sanitized: receipts never carry object bodies or query text.
		if strings.Contains(r.GrantDigest, "obj-") {
			t.Fatalf("receipt leaks object data: %+v", r)
		}
	}

	// Tampering with retained history is a visible integrity failure.
	gate.auditMu.Lock()
	gate.receipts[1].AllowedObjects++
	gate.auditMu.Unlock()
	if gate.VerifyReceiptChain() {
		t.Fatal("tampered chain passed verification")
	}
}

func TestQueryConnectorEvidenceDelegatedGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{allow: map[string]bool{
		"file:README.md": true,
		"issue:1":        true,
	}}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, nil)
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-delegated"),
	})
	if err != nil {
		t.Fatal(err)
	}
	connectionID := connected.ConnectionId.Value

	query := func(key, grantID string) *contractsv1.QueryConnectorEvidenceSuccess {
		t.Helper()
		answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
			Identity: testIdentity(),
			Request: &contractsv1.QueryConnectorEvidenceRequest{
				ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
				Query:          "billing",
				IdempotencyKey: key,
			},
			DelegatedGrantID: grantID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return answer
	}

	// No grant: the opaque scope abstains instead of serving admitted evidence.
	got := query("dq-1", "")
	if got.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		len(got.Answer.DegradedReasons) != 1 || got.Answer.DegradedReasons[0] != "delegated_grant_missing" {
		t.Fatalf("no-grant answer = %+v", got.Answer)
	}
	if probe.callCount() != 0 {
		t.Fatalf("probes without grant = %d", probe.callCount())
	}
	freshReceipt := gate.Receipts()[0]
	if freshReceipt.ConnectorDigest != fakeConnectorDigest() ||
		freshReceipt.SourceRevisionDigest != delegatedFieldDigest("source-revision", "snapshot-v1") ||
		freshReceipt.SourceCursorDigest != delegatedFieldDigest("source-cursor", "cursor-v1") ||
		freshReceipt.SourceObservedAtUnixNano != fakeConnectorObservedAt.UnixNano() {
		t.Fatalf("query receipt omitted source freshness: %+v", freshReceipt)
	}

	// Explicit grant: probed objects answer with native citations.
	if err := issueGrant(gate, testGrant("grant-q", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	got = query("dq-2", "grant-q")
	if got.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED ||
		len(got.Answer.Claims) == 0 || len(got.Answer.Claims[0].Citations) == 0 {
		t.Fatalf("granted answer = %+v", got.Answer)
	}
	if probe.callCount() == 0 {
		t.Fatal("granted query skipped live probes")
	}

	// Partial provider denial demotes the answer and flags it.
	probe.mu.Lock()
	probe.allow["issue:1"] = false
	probe.mu.Unlock()
	clock.Advance(6 * time.Minute) // expire cached verdicts
	got = query("dq-3", "grant-q")
	if got.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_PARTIAL ||
		!containsReason(got.Answer.DegradedReasons, "delegated_partial") {
		t.Fatalf("partial answer = %+v", got.Answer)
	}

	// Full provider denial abstains.
	probe.mu.Lock()
	probe.allow["file:README.md"] = false
	probe.mu.Unlock()
	clock.Advance(6 * time.Minute)
	got = query("dq-4", "grant-q")
	if got.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(got.Answer.DegradedReasons, "delegated_denied") {
		t.Fatalf("denied answer = %+v", got.Answer)
	}

	// Revocation is immediate for subsequent queries.
	if err := revokeGrant(gate, testIdentity(), "grant-q"); err != nil {
		t.Fatal(err)
	}
	got = query("dq-5", "grant-q")
	if got.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(got.Answer.DegradedReasons, "delegated_grant_revoked") {
		t.Fatalf("revoked answer = %+v", got.Answer)
	}

	// Grant lifecycle plus each gated security phase left sanitized receipts.
	if len(gate.Receipts()) != 9 || !gate.VerifyReceiptChain() {
		t.Fatalf("receipts = %d chainOK = %v", len(gate.Receipts()), gate.VerifyReceiptChain())
	}
}

func TestDelegatedQueryPromotionEvidenceFailsClosedBeforeFanout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	probe := &fakeProbe{}
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.PromotionGate = denyTestPromotionGate{}
	})
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("promotion-gated-connect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant("promotion-gated-grant", testPrincipal, clock)
	if err := issueGrant(gate, grant); err != nil {
		t.Fatal(err)
	}
	result, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId: connected.ConnectionId, Query: "billing",
			IdempotencyKey: "promotion-gated-query",
		},
		DelegatedGrantID: grant.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(result.Answer.DegradedReasons, "delegated_"+DelegatedOutcomeGateBlocked) {
		t.Fatalf("promotion gate answer = %+v", result.Answer)
	}
	if probe.callCount() != 0 {
		t.Fatalf("promotion denial reached provider: %d calls", probe.callCount())
	}
}

type missingFreshnessSource struct{}

func (missingFreshnessSource) Snapshot(context.Context, string, string) (SnapshotPage, error) {
	return SnapshotPage{
		Cursor: "cursor-unsafe", Complete: true,
		Objects: []Object{{
			ID: "issue:unsafe", Kind: ObjectKindIssue, Title: "billing",
			Body: "must not admit", Version: "unsafe-v1",
		}},
	}, nil
}

func (missingFreshnessSource) Delta(context.Context, string, string, string) (SnapshotPage, error) {
	return SnapshotPage{}, nil
}

func TestOpaqueSnapshotWithoutSourceFreshnessIsNotAdmitted(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	gate := testGate(t, &fakeProbe{}, clock, nil)
	kernel, err := New(Config{Source: missingFreshnessSource{}, Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(context.Background(), ConnectCommand{
		Identity: testIdentity(), Request: validConnect("missing-freshness-connect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if connected.State != contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_DEGRADED ||
		connected.RepositoryObjectCount != 0 || connected.IssueObjectCount != 0 {
		t.Fatalf("freshness-free page admitted: %+v", connected)
	}
	status, err := kernel.GetConnectorStatus(context.Background(), StatusCommand{
		Identity: testIdentity(), ConnectionID: connected.ConnectionId.Value,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.LastErrorCode != "source_freshness_unavailable" {
		t.Fatalf("last error = %q", status.LastErrorCode)
	}
}

// blockingProbe parks every CheckObjectPermission call until released,
// closing started when the first probe begins. It stands in for a slow or
// hung provider so tests can prove probes never hold kernel-wide locks.
type blockingProbe struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProbe) CheckObjectPermission(ctx context.Context, _ DelegatedGrant, _ string) (bool, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (p *blockingProbe) HydrateObject(
	ctx context.Context, _ DelegatedGrant, projected Object,
) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	return projected, nil
}

// TestRevokeConnectorNotBlockedBySlowProbe is the issue #309 follow-up
// regression: delegated permission probes run outside the kernel-wide mutex,
// so RevokeConnector completes immediately while a probe is in flight, and
// the in-flight query then fails closed (its pre-probe ACL epoch no longer
// matches) instead of serving evidence revoked mid-probe.
func TestRevokeConnectorNotBlockedBySlowProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &blockingProbe{started: make(chan struct{}), release: make(chan struct{})}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.ProbeTimeout = 10 * time.Second // keep the probe parked, not timed out
	})
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-slow-probe"),
	})
	if err != nil {
		t.Fatal(err)
	}
	connectionID := connected.ConnectionId.Value
	if err := issueGrant(gate, testGrant("grant-slow", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}

	type queryResult struct {
		answer *contractsv1.QueryConnectorEvidenceSuccess
		err    error
	}
	queryDone := make(chan queryResult, 1)
	go func() {
		answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
			Identity: testIdentity(),
			Request: &contractsv1.QueryConnectorEvidenceRequest{
				ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
				Query:          "billing",
				IdempotencyKey: "slow-query",
			},
			DelegatedGrantID: "grant-slow",
		})
		queryDone <- queryResult{answer: answer, err: err}
	}()

	<-probe.started // the query is now parked inside a live probe

	revokeDone := make(chan error, 1)
	go func() {
		_, err := kernel.RevokeConnector(ctx, RevokeCommand{
			Identity:       testIdentity(),
			ConnectionID:   connectionID,
			IdempotencyKey: "revoke-during-probe",
		})
		revokeDone <- err
	}()
	select {
	case err := <-revokeDone:
		if err != nil {
			t.Fatalf("revoke during probe err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RevokeConnector blocked behind an in-flight delegated probe")
	}

	// Release the probe: the parked query must fail closed with the
	// non-disclosing denial, never an answer gated under the stale epoch.
	close(probe.release)
	select {
	case result := <-queryDone:
		if !errors.Is(result.err, ErrNotFoundOrDenied) {
			t.Fatalf("mid-probe revoke not fail-closed: answer=%+v err=%v", result.answer, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("query did not finish after probe release")
	}
}

// TestReconcileDeleteDuringParkedProbeNotServed is the fresh-eyes follow-up
// to the issue #309 revoke race: ReconcileConnector deletes admitted objects
// WITHOUT bumping the ACL epoch (a reconcile is not a revoke), so the
// post-probe epoch re-check alone cannot catch it. The captured matches must
// be re-intersected with the live object map after the kernel lock is
// reacquired: evidence deleted by a reconcile that ran while the delegated
// probe was parked must never be served, even when the probe allows it.
func TestReconcileDeleteDuringParkedProbeNotServed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &blockingProbe{started: make(chan struct{}), release: make(chan struct{})}
	clock := newFakeClock()
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.ProbeTimeout = 10 * time.Second // keep the probe parked, not timed out
	})
	fake := NewFakeSourceAPI()
	kernel, err := New(Config{Source: fake, Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-reconcile-race"),
	})
	if err != nil {
		t.Fatal(err)
	}
	connectionID := connected.ConnectionId.Value
	if err := issueGrant(gate, testGrant("grant-reconcile-race", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}

	// The next complete delta from cursor-v1 deletes every object the query
	// "billing" matches.
	fake.SeedDeletion("ouroboros-dogfood", "sample-repo", "cursor-v1", "file:README.md")
	fake.SeedDeletion("ouroboros-dogfood", "sample-repo", "cursor-v1", "issue:1")

	type queryResult struct {
		answer *contractsv1.QueryConnectorEvidenceSuccess
		err    error
	}
	queryDone := make(chan queryResult, 1)
	go func() {
		answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
			Identity: testIdentity(),
			Request: &contractsv1.QueryConnectorEvidenceRequest{
				ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
				Query:          "billing",
				IdempotencyKey: "reconcile-race-query",
			},
			DelegatedGrantID: "grant-reconcile-race",
		})
		queryDone <- queryResult{answer: answer, err: err}
	}()

	<-probe.started // the query captured its matches and is parked in a probe

	// The reconcile must not block behind the parked probe, and its complete
	// page deletes both matched objects.
	type reconcileResult struct {
		success *contractsv1.ReconcileConnectorSuccess
		err     error
	}
	reconcileDone := make(chan reconcileResult, 1)
	go func() {
		success, err := kernel.ReconcileConnector(ctx, ReconcileCommand{
			Identity: testIdentity(),
			Request: &contractsv1.ReconcileConnectorRequest{
				ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
				KnownCursor:    "cursor-v1",
				Reason:         "delete-during-probe",
				IdempotencyKey: "reconcile-during-probe",
			},
		})
		reconcileDone <- reconcileResult{success: success, err: err}
	}()
	select {
	case result := <-reconcileDone:
		if result.err != nil || !result.success.PageComplete || result.success.DeletedObjectCount != 2 {
			t.Fatalf("reconcile during probe = %+v err=%v", result.success, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReconcileConnector blocked behind an in-flight delegated probe")
	}

	// Release the probe. It allows every object, the connection is still
	// active, and the ACL epoch never changed — yet the deleted evidence must
	// not be served: the query abstains because its support no longer exists.
	close(probe.release)
	select {
	case result := <-queryDone:
		if result.err != nil {
			t.Fatalf("query after reconcile-delete err = %v", result.err)
		}
		answer := result.answer.Answer
		if answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
			!containsReason(answer.DegradedReasons, "absent_support") {
			t.Fatalf("reconcile-deleted evidence served: %+v", answer)
		}
		if len(answer.Claims) != 0 {
			t.Fatalf("reconcile-deleted evidence cited: %+v", answer.Claims)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("query did not finish after probe release")
	}
}

func TestQueryConnectorEvidenceNonOpaqueScopeUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	probe := &fakeProbe{}
	clock := newFakeClock()
	// The gate covers a different scope, so the dogfood repo is non-opaque.
	gate := testGate(t, probe, clock, func(cfg *DelegatedGateConfig) {
		cfg.OpaqueScopes = []string{"github.com/other/opaque-repo"}
	})
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-plain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId:   connected.ConnectionId,
			Query:          "billing",
			IdempotencyKey: "plain-query",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED {
		t.Fatalf("non-opaque answer = %+v", answer.Answer)
	}
	if probe.callCount() != 0 || len(gate.Receipts()) != 0 {
		t.Fatalf("non-opaque scope was gated: probes=%d receipts=%d", probe.callCount(), len(gate.Receipts()))
	}
}

func TestDelegatedIndividualTeamCompanyIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	probe := &fakeProbe{}
	individual := "provider.example/company-a/team-red/user-alice"
	team := "provider.example/company-a/team-red"
	company := "provider.example/company-a"
	gate, err := NewDelegatedGate(DelegatedGateConfig{
		Provider:      probe,
		Issuer:        testIssuerPort{},
		PromotionGate: allowTestPromotionGate{},
		OpaqueScopes:  []string{individual, team, company},
		Clock:         clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(id, tenant, principal, scope string) DelegatedGrant {
		return DelegatedGrant{
			ID: id, Tenant: tenant, Principal: principal, SourceScope: scope,
			IssuedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour),
		}
	}
	for _, candidate := range []DelegatedGrant{
		grant("individual-a", "company-a", "alice", individual),
		grant("team-alice", "company-a", "alice", team),
		grant("company-alice", "company-a", "alice", company),
	} {
		if err := issueGrant(gate, candidate); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		identity Identity
		scope    string
		grantID  string
		outcome  string
	}{
		{"individual cannot widen to team", Identity{"company-a", "alice", testSession}, team, "individual-a", DelegatedOutcomeGrantScope},
		{"teammate cannot borrow team grant", Identity{"company-a", "bob", testSession}, team, "team-alice", DelegatedOutcomeGrantMissing},
		{"team cannot widen to company", Identity{"company-a", "alice", testSession}, company, "team-alice", DelegatedOutcomeGrantScope},
		{"other company cannot borrow company grant", Identity{"company-b", "alice", testSession}, company, "company-alice", DelegatedOutcomeGrantMissing},
	}
	before := probe.callCount()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, outcome := gate.preauthorizeQuery(
				ctx, tc.identity, tc.scope, tc.grantID, testFreshness(clock),
			)
			if outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", outcome, tc.outcome)
			}
		})
	}
	if probe.callCount() != before {
		t.Fatalf("cross-boundary denial reached provider: before=%d after=%d", before, probe.callCount())
	}
}

type sequencedProvider struct {
	mu          sync.Mutex
	permissions []bool
	mutate      func(Object) Object
	contentCall int
	probeCall   int
}

type contentTimeoutProvider struct{}

func (contentTimeoutProvider) CheckObjectPermission(context.Context, DelegatedGrant, string) (bool, error) {
	return true, nil
}

func (contentTimeoutProvider) HydrateObject(
	ctx context.Context, _ DelegatedGrant, _ Object,
) (Object, error) {
	<-ctx.Done()
	return Object{}, ctx.Err()
}

func TestDelegatedContentProbeTimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	gate := testGate(t, contentTimeoutProvider{}, clock, func(cfg *DelegatedGateConfig) {
		cfg.ProbeTimeout = 20 * time.Millisecond
	})
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("content-timeout-connect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("content-timeout-grant", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId: connected.ConnectionId, Query: "next sprint", IdempotencyKey: "content-timeout-query",
		},
		DelegatedGrantID: "content-timeout-grant",
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("content timeout was not bounded: %s", elapsed)
	}
	if answer.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(answer.Answer.DegradedReasons, "delegated_denied") {
		t.Fatalf("timed-out content served: %+v", answer.Answer)
	}
}

func (p *sequencedProvider) CheckObjectPermission(
	ctx context.Context, _ DelegatedGrant, _ string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeCall++
	if len(p.permissions) == 0 {
		return false, nil
	}
	allowed := p.permissions[0]
	p.permissions = p.permissions[1:]
	return allowed, nil
}

func (p *sequencedProvider) HydrateObject(
	ctx context.Context, _ DelegatedGrant, projected Object,
) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contentCall++
	if p.mutate != nil {
		return p.mutate(projected), nil
	}
	return projected, nil
}

func TestDelegatedPostHydrationPermissionRecheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	provider := &sequencedProvider{permissions: []bool{true, false}}
	gate := testGate(t, provider, clock, nil)
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("post-hydration-connect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("post-hydration-grant", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId: connected.ConnectionId, Query: "next sprint", IdempotencyKey: "post-hydration-query",
		},
		DelegatedGrantID: "post-hydration-grant",
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(answer.Answer.DegradedReasons, "delegated_denied") {
		t.Fatalf("post-hydration denial served content: %+v", answer.Answer)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.probeCall != 2 || provider.contentCall != 1 {
		t.Fatalf("provider calls: permission=%d content=%d", provider.probeCall, provider.contentCall)
	}
}

func TestDelegatedContentRevisionMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	provider := &sequencedProvider{
		permissions: []bool{true},
		mutate: func(object Object) Object {
			object.Version += "-provider-changed"
			object.Body = "foreign content"
			return object
		},
	}
	gate := testGate(t, provider, clock, nil)
	kernel, err := New(Config{Source: NewFakeSourceAPI(), Delegated: gate})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("content-mismatch-connect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("content-mismatch-grant", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId: connected.ConnectionId, Query: "next sprint", IdempotencyKey: "content-mismatch-query",
		},
		DelegatedGrantID: "content-mismatch-grant",
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED ||
		!containsReason(answer.Answer.DegradedReasons, delegatedReasonObjectChanged) {
		t.Fatalf("changed provider content served: %+v", answer.Answer)
	}
}

type recordingAuditSink struct {
	mu       sync.Mutex
	receipts []DelegatedReceipt
}

type blockingAuditSink struct {
	mu         sync.Mutex
	calls      int
	blockAfter int
}

func (s *blockingAuditSink) AppendDelegatedReceipt(ctx context.Context, _ DelegatedReceipt) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call < s.blockAfter {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestDelegatedAuditTimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	sink := &blockingAuditSink{blockAfter: 2}
	gate, err := NewDelegatedGate(DelegatedGateConfig{
		Provider: &fakeProbe{}, Issuer: testIssuerPort{}, PromotionGate: allowTestPromotionGate{},
		OpaqueScopes: []string{testScope}, Clock: clock.Now,
		AuditSink: sink, AuditTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := issueGrant(gate, testGrant("audit-timeout-grant", testPrincipal, clock)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	allowed, receipt := gate.authorize(
		ctx, testIdentity(), testScope, "audit-timeout-grant", []string{"obj-1"},
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("audit timeout was not bounded: %s", elapsed)
	}
	if len(allowed) != 0 || receipt.Outcome != DelegatedOutcomeAuditFailed {
		t.Fatalf("audit failure did not fail closed: allowed=%v receipt=%+v", allowed, receipt)
	}
}

func (s *recordingAuditSink) AppendDelegatedReceipt(_ context.Context, receipt DelegatedReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
	return nil
}

func TestDelegatedStoreRestartAndSanitizedAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newFakeClock()
	store := NewMemoryDelegatedGrantStore(16)
	sink := &recordingAuditSink{}
	newGate := func() *DelegatedGate {
		gate, err := NewDelegatedGate(DelegatedGateConfig{
			Provider: &fakeProbe{}, Issuer: testIssuerPort{}, PromotionGate: allowTestPromotionGate{},
			OpaqueScopes: []string{testScope}, Clock: clock.Now,
			GrantStore: store, AuditSink: sink,
		})
		if err != nil {
			t.Fatal(err)
		}
		return gate
	}
	first := newGate()
	grant := testGrant("sensitive-grant", "alice@example.test", clock)
	grant.Tenant = "sensitive-company-name"
	identity := Identity{Tenant: grant.Tenant, Principal: grant.Principal, Session: testSession}
	if err := first.IssueAuthenticatedGrant(authenticatedTestContext(identity), grant); err != nil {
		t.Fatal(err)
	}
	second := newGate()
	if _, outcome := second.preauthorizeQuery(ctx, identity, testScope, grant.ID, testFreshness(clock)); outcome != "" {
		t.Fatalf("persisted grant outcome = %q", outcome)
	}
	if err := second.RevokeAuthenticatedGrant(authenticatedTestContext(identity), grant.ID); err != nil {
		t.Fatal(err)
	}
	third := newGate()
	if _, outcome := third.preauthorizeQuery(ctx, identity, testScope, grant.ID, testFreshness(clock)); outcome != DelegatedOutcomeGrantRevoked {
		t.Fatalf("persisted revoke outcome = %q", outcome)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.receipts) < 3 {
		t.Fatalf("audit receipts = %d", len(sink.receipts))
	}
	for _, receipt := range sink.receipts {
		rendered := fmt.Sprintf("%+v", receipt)
		for _, secret := range []string{grant.ID, grant.Tenant, grant.Principal, grant.SourceScope} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("receipt leaks %q: %s", secret, rendered)
			}
		}
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
