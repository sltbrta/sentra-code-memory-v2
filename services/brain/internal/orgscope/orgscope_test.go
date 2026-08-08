package orgscope_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/orgscope"
)

type fixture struct {
	t     *testing.T
	dir   *orgscope.Directory
	auth  *orgscope.Authority
	store *orgscope.Store
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	dir, err := orgscope.NewDirectory("acme", func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	f.dir = dir
	f.auth = orgscope.NewAuthority(dir)
	f.store = orgscope.NewStore(f.auth)
	return f
}

func (f *fixture) provision(users ...string) {
	f.t.Helper()
	for _, u := range users {
		if _, err := f.dir.Provision(u); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) put(items ...orgscope.Item) {
	f.t.Helper()
	for _, item := range items {
		if err := f.store.Put(item); err != nil {
			f.t.Fatalf("put %s: %v", item.ID, err)
		}
	}
}

func principal(user string) orgscope.Principal {
	return orgscope.Principal{UserID: user, TenantID: "acme"}
}

func ids(citations []orgscope.Citation) map[string]bool {
	out := make(map[string]bool)
	for _, c := range citations {
		out[c.ItemID] = true
	}
	return out
}

func TestDefaultDenyScopeIsolation(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	f.put(
		orgscope.Item{ID: "i-alice", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "alice secret sapphire"},
		orgscope.Item{ID: "i-bob", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "bob"}, Owner: "bob", Text: "bob secret ruby"},
		orgscope.Item{ID: "c-1", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}, Owner: "alice", Text: "company handbook secret"},
	)

	// Unprovisioned user: hard deny, non-disclosing.
	if _, err := f.store.Query(principal("mallory"), "secret"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatalf("mallory query = %v, want ErrDenied", err)
	}
	// Cross-tenant principal: hard deny.
	if _, err := f.store.Query(orgscope.Principal{UserID: "alice", TenantID: "other"}, "secret"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("cross-tenant principal must be denied")
	}
	// Alice sees only her own individual scope; company needs a grant (default deny).
	res, err := f.store.Query(principal("alice"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Citations)
	if !got["i-alice"] || got["i-bob"] || got["c-1"] {
		t.Fatalf("alice citations = %v", got)
	}
	// Grant company to alice: now visible; bob still cannot see alice's scope.
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:alice", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}}); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.Query(principal("alice"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Citations); !got["c-1"] || got["i-bob"] {
		t.Fatalf("alice post-grant citations = %v", got)
	}
	res, err = f.store.Query(principal("bob"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Citations); got["i-alice"] || got["c-1"] || !got["i-bob"] {
		t.Fatalf("bob citations = %v", got)
	}
	// Denial message never discloses ids or existence.
	if msg := orgscope.ErrDenied.Error(); strings.Contains(msg, "alice") || !strings.Contains(msg, "not_found_or_denied") {
		t.Fatalf("denial discloses: %q", msg)
	}
}

func TestGroupJoinRoleChangeOffboardingAndReceipts(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	if _, err := f.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	f.put(orgscope.Item{ID: "t-1", Scope: orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}, Owner: "alice", Text: "team roadmap secret"})
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "group:eng", Scope: orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}}); err != nil {
		t.Fatal(err)
	}

	// Before join: deny.
	res, err := f.store.Query(principal("bob"), "roadmap")
	if err != nil || len(res.Citations) != 0 {
		t.Fatalf("pre-join = %v, %v", res.Citations, err)
	}
	// Join: allowed.
	if _, err := f.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.Query(principal("bob"), "roadmap")
	if err != nil || !ids(res.Citations)["t-1"] {
		t.Fatalf("post-join = %v, %v", res.Citations, err)
	}
	// Role change: removal revokes immediately even through the warm cache.
	removeReceipt, err := f.dir.RemoveMember("eng", "bob")
	if err != nil {
		t.Fatal(err)
	}
	res, err = f.store.Query(principal("bob"), "roadmap")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("stale grant leak through cache: %v", res.Citations)
	}
	if !res.FromCache {
		t.Fatal("expected warm-cache probe (cache hit re-filtered)")
	}
	// Session-history replay must not leak either.
	hist, err := f.store.History(principal("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 0 {
		t.Fatalf("history replay leak: %v", hist)
	}
	// Offboarding: everything denied, including own individual scope.
	f.put(orgscope.Item{ID: "i-bob", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "bob"}, Owner: "bob", Text: "bob private note"})
	offReceipt, err := f.dir.Deprovision("bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Query(principal("bob"), "private"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("offboarded user must be denied")
	}
	if _, err := f.store.History(principal("bob")); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("offboarded history must be denied")
	}
	// Receipts record revocation ordering via monotonically increasing epochs.
	if !(offReceipt.Epoch > removeReceipt.Epoch) || removeReceipt.Kind != orgscope.ReceiptMemberRemove || offReceipt.Kind != orgscope.ReceiptUserDeprovision {
		t.Fatalf("receipts = %+v then %+v", removeReceipt, offReceipt)
	}
	kinds := make(map[string]int)
	for _, r := range f.dir.Receipts() {
		kinds[r.Kind]++
	}
	for _, want := range []string{
		orgscope.ReceiptUserProvision, orgscope.ReceiptGroupCreate, orgscope.ReceiptGrantCreate,
		orgscope.ReceiptMemberAdd, orgscope.ReceiptMemberRemove, orgscope.ReceiptUserDeprovision,
	} {
		if kinds[want] == 0 {
			t.Fatalf("missing receipt kind %s in %v", want, kinds)
		}
	}
}

func TestDenyOverlayBeatsEveryAllow(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(
		orgscope.Item{ID: "i-alice", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "secret note"},
		orgscope.Item{ID: "c-1", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}, Owner: "alice", Text: "secret handbook"},
	)
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:alice", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}}); err != nil {
		t.Fatal(err)
	}
	// Deny overlays beat the explicit company grant and the implicit own-scope allow.
	if _, err := f.auth.Deny("alice", orgscope.Scope{Kind: orgscope.ScopeCompany}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Deny("alice", orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Query(principal("alice"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("deny overlay leak: %v", res.Citations)
	}
}

func TestDelegatedGrantExpiryAndDelegatorOffboarding(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob", "carol")
	f.put(orgscope.Item{ID: "i-alice", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "delegated secret"})

	// Time-boxed delegated grant from alice to bob.
	if _, err := f.auth.Grant(orgscope.Grant{
		Subject: "user:bob", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		ExpiresAt: f.now.Add(time.Hour), DelegatedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	// Durable delegated grant from alice to carol.
	if _, err := f.auth.Grant(orgscope.Grant{
		Subject: "user:carol", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		DelegatedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Query(principal("bob"), "delegated")
	if err != nil || !ids(res.Citations)["i-alice"] {
		t.Fatalf("live delegated grant = %v, %v", res.Citations, err)
	}
	// Expiry: advance clock past the deadline; warm cache must not serve.
	f.now = f.now.Add(2 * time.Hour)
	res, err = f.store.Query(principal("bob"), "delegated")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("expired delegated grant leak: %v", res.Citations)
	}
	// Delegator offboarding kills carol's durable delegated grant fail-closed.
	if _, err := f.dir.Deprovision("alice"); err != nil {
		t.Fatal(err)
	}
	res, err = f.store.Query(principal("carol"), "delegated")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("delegator-offboarded grant leak: %v", res.Citations)
	}
	// Explicit revocation emits a receipt.
	if _, err := f.auth.Revoke("user:carol", orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	receipts := f.dir.Receipts()
	if receipts[len(receipts)-1].Kind != orgscope.ReceiptGrantRevoke {
		t.Fatalf("last receipt = %+v", receipts[len(receipts)-1])
	}
}

func TestErasureAcrossProjectionsRestoreAndRebuild(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(
		orgscope.Item{ID: "e-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "erasable payroll figure"},
		orgscope.Item{ID: "k-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "kept payroll note"},
	)
	// Warm cache + session history so every projection holds e-1.
	if _, err := f.store.Query(principal("alice"), "payroll"); err != nil {
		t.Fatal(err)
	}
	// Pre-erasure backup: the classic resurrection vector.
	preBackup, err := f.store.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := f.store.Erase("gdpr_request", "e-1")
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Complete {
		t.Fatalf("erasure incomplete: %+v", receipt)
	}
	if receipt.Projections["primary"] != 1 || receipt.Projections["cache"] == 0 || receipt.Projections["session"] == 0 || receipt.Projections["index"] == 0 {
		t.Fatalf("projection purge counts = %v", receipt.Projections)
	}
	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("leaks after erase: %v", leaks.Leaks)
	}
	// Query, cache and history no longer see it; kept item still visible.
	res, err := f.store.Query(principal("alice"), "payroll")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Citations); got["e-1"] || !got["k-1"] {
		t.Fatalf("post-erase citations = %v", got)
	}
	hist, err := f.store.History(principal("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hist); got["e-1"] {
		t.Fatal("history resurrected erased item")
	}
	// Re-ingest of a tombstoned id is rejected.
	err = f.store.Put(orgscope.Item{ID: "e-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "sneaky payroll again"})
	if !errors.Is(err, orgscope.ErrRejected) {
		t.Fatalf("tombstoned re-ingest = %v", err)
	}
	// Rebuild cannot resurrect.
	f.store.RebuildProjections()
	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("leaks after rebuild: %v", leaks.Leaks)
	}
	// Restoring the pre-erasure backup cannot resurrect: live tombstones win.
	if err := f.store.Restore(preBackup); err != nil {
		t.Fatal(err)
	}
	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("leaks after pre-erasure restore: %v", leaks.Leaks)
	}
	res, err = f.store.Query(principal("alice"), "payroll")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Citations); got["e-1"] || !got["k-1"] {
		t.Fatalf("post-restore citations = %v", got)
	}
	// A post-erasure backup carries tombstones into a fresh store.
	postBackup, err := f.store.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	fresh := orgscope.NewStore(f.auth)
	if err := fresh.Restore(postBackup); err != nil {
		t.Fatal(err)
	}
	if leaks := fresh.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("fresh restore leaks: %v", leaks.Leaks)
	}
	// Audit survives erasure but never holds content.
	foundErasure := false
	for _, entry := range f.store.Audit() {
		if entry.Kind == "erasure" {
			foundErasure = true
		}
		if strings.Contains(strings.Join(entry.ItemIDs, " "), "payroll") || strings.Contains(entry.Digest, "payroll") {
			t.Fatalf("audit stores content: %+v", entry)
		}
	}
	if !foundErasure {
		t.Fatal("missing erasure audit entry")
	}
}

func TestRedTeamZeroUnauthorizedCitationsReport(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob", "eve")
	if _, err := f.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	f.put(
		orgscope.Item{ID: "i-alice", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "alpha secret"},
		orgscope.Item{ID: "t-eng", Scope: orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}, Owner: "bob", Text: "beta secret"},
		orgscope.Item{ID: "c-1", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}, Owner: "alice", Text: "gamma secret"},
	)
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "group:eng", Scope: orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}}); err != nil {
		t.Fatal(err)
	}

	card := orgscope.NewReportCard("acme")
	forbiddenForEve := map[string]bool{"i-alice": true, "t-eng": true, "c-1": true}

	// Red-team retrieval bank: every probe must yield zero unauthorized citations.
	probes := []struct {
		p     orgscope.Principal
		query string
	}{
		{principal("eve"), "secret"},
		{principal("eve"), "alpha beta gamma"},
		{orgscope.Principal{UserID: "eve", TenantID: ""}, "secret"},
		{orgscope.Principal{UserID: "", TenantID: "acme"}, "secret"},
		{orgscope.Principal{UserID: "eve", TenantID: "acme|other"}, "secret"},
	}
	for _, probe := range probes {
		res, err := f.store.Query(probe.p, probe.query)
		if err != nil {
			card.RecordProbe(nil, forbiddenForEve) // denied outright: zero citations
			continue
		}
		card.RecordProbe(res.Citations, forbiddenForEve)
	}
	// Stale-grant probes: revoke bob's team access, then retry (warm cache).
	if _, err := f.store.Query(principal("bob"), "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.RemoveMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.Query(principal("bob"), "beta")
	if err != nil {
		t.Fatal(err)
	}
	card.RecordStaleGrantProbe(len(res.Citations) != 0)
	hist, err := f.store.History(principal("bob"))
	if err != nil {
		t.Fatal(err)
	}
	card.RecordStaleGrantProbe(len(hist) != 0)

	// Erasure + restore probes.
	preBackup, err := f.store.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.store.EraseOwner("offboarding_deletion", "alice")
	if err != nil {
		t.Fatal(err)
	}
	card.RecordErasure(receipt, f.store.VerifyErasure(receipt.ItemIDs...))
	if err := f.store.Restore(preBackup); err != nil {
		t.Fatal(err)
	}
	restoreLeaks := f.store.VerifyErasure(receipt.ItemIDs...)
	f.store.RebuildProjections()
	rebuildLeaks := f.store.VerifyErasure(receipt.ItemIDs...)
	card.RecordRestore(len(restoreLeaks.Leaks) == 0 && len(rebuildLeaks.Leaks) == 0)

	report := card.Build()
	if report.LeakRate != 0 || report.UnauthorizedCitations != 0 {
		t.Fatalf("leak rate = %+v", report)
	}
	if report.StaleGrantRate != 0 || report.StaleGrantHits != 0 {
		t.Fatalf("stale grant rate = %+v", report)
	}
	if report.ErasureCompletionRate != 1 {
		t.Fatalf("erasure completion = %+v", report)
	}
	if !report.RestoreCorrect {
		t.Fatalf("restore incorrect = %+v", report)
	}
	if len(report.NonClaims) == 0 {
		t.Fatal("report must carry substrate non-claims")
	}
}

func TestFailClosedValidationEdges(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	// Malformed scopes, subjects, and ids are rejected or denied, never allowed.
	if err := f.auth.Resolve(principal("alice"), orgscope.Scope{Kind: "everything"}); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("unknown scope kind must deny")
	}
	if err := f.auth.Resolve(principal("alice"), orgscope.Scope{Kind: orgscope.ScopeCompany, ID: "x"}); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("company scope with id must deny")
	}
	if err := f.auth.Resolve(principal("alice"), orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "../alice"}); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("traversal scope id must deny")
	}
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "root", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}}); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("malformed subject must be rejected")
	}
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:ghost", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}}); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("grant to inactive user must deny")
	}
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "group:ghosts", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}}); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("grant to missing group must deny")
	}
	if _, err := orgscope.NewDirectory("bad|tenant", nil); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("separator tenant id must be rejected")
	}
	if err := f.store.Put(orgscope.Item{ID: "a|b", Scope: orgscope.Scope{Kind: orgscope.ScopeCompany}, Owner: "alice", Text: "x"}); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("separator item id must be rejected")
	}
	if _, err := f.store.Erase("reason"); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("empty erase must be rejected")
	}
	if _, err := f.store.EraseOwner("reason", "ghost"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("erase for unknown owner must deny non-disclosingly")
	}
	if err := f.store.Put(orgscope.Item{ID: "ghost-item", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "ghost"}, Owner: "ghost", Text: "later"}); err != nil {
		t.Fatalf("denied no-op erasure must not fence future owner writes: %v", err)
	}
	if _, err := f.store.EraseOwner("", "ghost"); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("empty erasure reason must reject before fencing")
	}
	if err := f.store.Put(orgscope.Item{ID: "ghost-item-2", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "ghost"}, Owner: "ghost", Text: "later"}); err != nil {
		t.Fatalf("rejected erasure must not fence future owner writes: %v", err)
	}
	fenceScope := orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}
	if err := f.store.Put(orgscope.Item{ID: "fence-1", Scope: fenceScope, Owner: "alice", Text: "erase me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EraseScope("reason", fenceScope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EraseScope("reason", fenceScope); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("repeated empty scope erasure must deny")
	}
	if err := f.store.Put(orgscope.Item{ID: "fence-2", Scope: fenceScope, Owner: "alice", Text: "must stay erased"}); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("successful scope erasure fence must reject new writes")
	}
	// Restore of a foreign-tenant or unmanifested backup is rejected.
	if err := f.store.Restore(orgscope.Backup{TenantID: "acme"}); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("digestless backup must be rejected")
	}
	if err := f.store.Restore(orgscope.Backup{TenantID: "other", Digest: "d"}); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatal("cross-tenant backup must be rejected")
	}
}
