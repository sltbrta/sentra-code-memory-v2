package orgscope_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/orgscope"
)

func TestOffboardingPermanentlyInvalidatesUserAndDelegatedGrants(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob", "carol")
	company := orgscope.Scope{Kind: orgscope.ScopeCompany}
	f.put(orgscope.Item{ID: "company-1", Scope: company, Owner: "alice", Text: "company launch plan"})

	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:bob", Scope: company}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:carol", Scope: company, DelegatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"bob", "carol"} {
		got, err := f.store.Query(principal(user), "launch")
		if err != nil || len(got.Citations) != 1 {
			t.Fatalf("%s pre-offboarding = %+v, %v", user, got, err)
		}
	}

	if _, err := f.dir.Deprovision("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Deprovision("alice"); err != nil {
		t.Fatal(err)
	}
	// Re-provisioning creates a new lifecycle incarnation. Grants issued to or
	// delegated by an older incarnation must never resurrect.
	f.provision("alice", "bob")
	for _, user := range []string{"bob", "carol"} {
		got, err := f.store.Query(principal(user), "launch")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Citations) != 0 {
			t.Fatalf("stale grant resurrected for %s: %+v", user, got.Citations)
		}
	}
}

func TestPrincipalArtifactsAreBoundToLifecycleIncarnation(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(orgscope.Item{
		ID: "alice-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "sapphire private note",
	})
	first, err := f.store.Query(principal("alice"), "sapphire")
	if err != nil || first.FromCache || len(first.Citations) != 1 {
		t.Fatalf("first query = %+v, %v", first, err)
	}
	if history, err := f.store.History(principal("alice")); err != nil || len(history) != 1 {
		t.Fatalf("first-incarnation history = %+v, %v", history, err)
	}
	if replay, err := f.store.Replay(principal("alice")); err != nil || len(replay) != 1 {
		t.Fatalf("first-incarnation replay = %+v, %v", replay, err)
	}

	if _, err := f.dir.Deprovision("alice"); err != nil {
		t.Fatal(err)
	}
	f.provision("alice")
	if history, err := f.store.History(principal("alice")); err != nil || len(history) != 0 {
		t.Fatalf("new incarnation inherited history = %+v, %v", history, err)
	}
	if replay, err := f.store.Replay(principal("alice")); err != nil || len(replay) != 0 {
		t.Fatalf("new incarnation inherited replay = %+v, %v", replay, err)
	}
	second, err := f.store.Query(principal("alice"), "sapphire")
	if err != nil || second.FromCache || len(second.Citations) != 1 {
		t.Fatalf("new incarnation inherited cache = %+v, %v", second, err)
	}
}

func TestItemReplacementInvalidatesCandidateCacheAndPrincipalArtifacts(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	item := orgscope.Item{
		ID: "stable-id", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "old sapphire content",
	}
	f.put(item)
	if _, err := f.store.Query(principal("alice"), "sapphire"); err != nil {
		t.Fatal(err)
	}
	item.Text = "new ruby content"
	f.put(item)
	if history, err := f.store.History(principal("alice")); err != nil || len(history) != 0 {
		t.Fatalf("history aliased replacement = %+v, %v", history, err)
	}
	if replay, err := f.store.Replay(principal("alice")); err != nil || len(replay) != 0 {
		t.Fatalf("replay aliased replacement = %+v, %v", replay, err)
	}
	if got, err := f.store.Query(principal("alice"), "sapphire"); err != nil || got.FromCache || len(got.Citations) != 0 {
		t.Fatalf("old candidate cache survived replacement = %+v, %v", got, err)
	}
	if got, err := f.store.Query(principal("alice"), "ruby"); err != nil || len(got.Citations) != 1 {
		t.Fatalf("replacement not indexed = %+v, %v", got, err)
	}
}

func TestGroupDeletionPermanentlyInvalidatesOldGroupGrants(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	team := orgscope.Scope{Kind: orgscope.ScopeTeam, ID: "eng"}
	if _, err := f.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "group:eng", Scope: team}); err != nil {
		t.Fatal(err)
	}
	f.put(orgscope.Item{ID: "team-1", Scope: team, Owner: "alice", Text: "engineering roadmap"})
	if got, err := f.store.Query(principal("bob"), "roadmap"); err != nil || len(got.Citations) != 1 {
		t.Fatalf("pre-delete = %+v, %v", got, err)
	}
	deleted, err := f.dir.DeleteGroup("eng")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Kind != orgscope.ReceiptGroupDelete {
		t.Fatalf("delete receipt = %+v", deleted)
	}
	if _, err := f.dir.EnsureGroup("eng"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.AddMember("eng", "bob"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Query(principal("bob"), "roadmap"); err != nil || len(got.Citations) != 0 {
		t.Fatalf("old group grant resurrected = %+v, %v", got, err)
	}
}

func TestGraphClaimsReplayErasureAndRestoreDoNotLeak(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	f.put(
		orgscope.Item{ID: "alice-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "sapphire merger evidence"},
		orgscope.Item{ID: "bob-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "bob"}, Owner: "bob", Text: "ruby hiring evidence"},
	)
	preErase, err := f.store.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Query(principal("alice"), "sapphire"); err != nil {
		t.Fatal(err)
	}

	assertOnly := func(name string, got []orgscope.Citation, want string) {
		t.Helper()
		seen := ids(got)
		if !seen[want] || len(seen) != 1 {
			t.Fatalf("%s citations = %v, want only %s", name, seen, want)
		}
	}
	claims, err := f.store.SearchClaims(principal("alice"), "evidence")
	if err != nil {
		t.Fatal(err)
	}
	assertOnly("claims", claims, "alice-1")
	graph, err := f.store.Graph(principal("alice"))
	if err != nil {
		t.Fatal(err)
	}
	assertOnly("graph", graph, "alice-1")
	replay, err := f.store.Replay(principal("alice"))
	if err != nil {
		t.Fatal(err)
	}
	assertOnly("replay", replay, "alice-1")

	receipt, err := f.store.EraseScope("deletion_request", orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := receipt.ItemIDs; len(ids) != 1 || ids[0] != "alice-1" {
		t.Fatalf("scope erasure widened or missed item: %+v", ids)
	}
	if got, err := f.store.Query(principal("bob"), "ruby"); err != nil || !ids(got.Citations)["bob-1"] {
		t.Fatalf("scope erasure widened into bob scope: %+v, %v", got, err)
	}
	for _, projection := range []string{"primary", "index", "cache", "session", "claims", "graph", "replay"} {
		if _, ok := receipt.Projections[projection]; !ok {
			t.Fatalf("receipt missing %s projection: %+v", projection, receipt.Projections)
		}
	}
	for _, rebuild := range []func(){f.store.RebuildProjections, func() {
		if err := f.store.Restore(preErase); err != nil {
			t.Fatal(err)
		}
	}} {
		rebuild()
		if leaks := f.store.VerifyErasure("alice-1"); len(leaks.Leaks) != 0 {
			t.Fatalf("erasure leaks after rebuild/restore: %v", leaks.Leaks)
		}
		claims, err = f.store.SearchClaims(principal("alice"), "evidence")
		if err != nil || len(claims) != 0 {
			t.Fatalf("claims resurrection = %+v, %v", claims, err)
		}
		graph, err = f.store.Graph(principal("alice"))
		if err != nil || len(graph) != 0 {
			t.Fatalf("graph resurrection = %+v, %v", graph, err)
		}
		replay, err = f.store.Replay(principal("alice"))
		if err != nil || len(replay) != 0 {
			t.Fatalf("replay resurrection = %+v, %v", replay, err)
		}
	}

	if got, err := f.store.SearchClaims(orgscope.Principal{UserID: "bob", TenantID: "other"}, "evidence"); !errors.Is(err, orgscope.ErrDenied) || got != nil {
		t.Fatalf("cross-tenant claims = %+v, %v", got, err)
	}
}

func TestRestoreDropsDerivedPrincipalArtifacts(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(orgscope.Item{
		ID: "shared-id", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "old sapphire content",
	})
	if _, err := f.store.Query(principal("alice"), "sapphire"); err != nil {
		t.Fatal(err)
	}

	source := orgscope.NewStore(f.auth)
	if err := source.Put(orgscope.Item{
		ID: "shared-id", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
		Owner: "alice", Text: "new ruby content",
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := source.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Restore(backup); err != nil {
		t.Fatal(err)
	}
	if history, err := f.store.History(principal("alice")); err != nil || len(history) != 0 {
		t.Fatalf("pre-restore history aliased restored content = %+v, %v", history, err)
	}
	if replay, err := f.store.Replay(principal("alice")); err != nil || len(replay) != 0 {
		t.Fatalf("pre-restore replay aliased restored content = %+v, %v", replay, err)
	}
	if got, err := f.store.Query(principal("alice"), "sapphire"); err != nil || len(got.Citations) != 0 || got.FromCache {
		t.Fatalf("pre-restore cache survived = %+v, %v", got, err)
	}
	if got, err := f.store.Query(principal("alice"), "ruby"); err != nil || len(got.Citations) != 1 {
		t.Fatalf("restored index unavailable = %+v, %v", got, err)
	}
}

func TestRevocationAndDenyRefilterGraphClaimsAndReplay(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	aliceScope := orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}
	f.put(orgscope.Item{ID: "alice-1", Scope: aliceScope, Owner: "alice", Text: "sapphire acquisition"})
	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:bob", Scope: aliceScope}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Query(principal("bob"), "sapphire"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Revoke("user:bob", aliceScope); err != nil {
		t.Fatal(err)
	}
	probes := []struct {
		name string
		run  func() ([]orgscope.Citation, error)
	}{
		{"claims", func() ([]orgscope.Citation, error) { return f.store.SearchClaims(principal("bob"), "sapphire") }},
		{"graph", func() ([]orgscope.Citation, error) { return f.store.Graph(principal("bob")) }},
		{"replay", func() ([]orgscope.Citation, error) { return f.store.Replay(principal("bob")) }},
	}
	for _, probe := range probes {
		got, err := probe.run()
		if err != nil || len(got) != 0 {
			t.Fatalf("%s stale-grant leak = %+v, %v", probe.name, got, err)
		}
	}

	if _, err := f.auth.Grant(orgscope.Grant{Subject: "user:bob", Scope: aliceScope}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Deny("bob", aliceScope); err != nil {
		t.Fatal(err)
	}
	for _, probe := range probes {
		got, err := probe.run()
		if err != nil || len(got) != 0 {
			t.Fatalf("%s deny-overlay leak = %+v, %v", probe.name, got, err)
		}
	}
}

func TestComplianceReportIncludesErasureCompletionSLO(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(orgscope.Item{ID: "alice-1", Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}, Owner: "alice", Text: "erasable"})
	receipt, err := f.store.Erase("request", "alice-1")
	if err != nil {
		t.Fatal(err)
	}
	card := orgscope.NewReportCard("acme")
	card.SetErasureSLO(time.Minute)
	card.RecordErasure(receipt, f.store.VerifyErasure("alice-1"))
	report := card.Build()
	if report.EvidenceScope != orgscope.LocalStoreErasureCoverage || report.ErasureSLO != time.Minute || !report.ErasureSLOMet || report.ErasureP95 < 0 {
		t.Fatalf("erasure SLO report = %+v", report)
	}
	invalid := receipt
	invalid.TenantID = "other"
	invalidCard := orgscope.NewReportCard("acme")
	invalidCard.RecordErasure(invalid, f.store.VerifyErasure("alice-1"))
	invalidReport := invalidCard.Build()
	if invalidReport.ErasureComplete != 0 || invalidReport.ErasureSLOMet {
		t.Fatalf("foreign-tenant receipt counted as completion: %+v", invalidReport)
	}
	incomplete := receipt
	incomplete.Complete = false
	incompleteCard := orgscope.NewReportCard("acme")
	incompleteCard.RecordErasure(incomplete, f.store.VerifyErasure("alice-1"))
	incompleteReport := incompleteCard.Build()
	if incompleteReport.ErasureComplete != 0 || incompleteReport.ErasureSLOMet {
		t.Fatalf("incomplete erasure met completion SLO: %+v", incompleteReport)
	}
	for _, nonClaim := range report.NonClaims {
		if nonClaim == "graph_claims_projections" {
			t.Fatalf("locally covered substrate remains a non-claim: %+v", report.NonClaims)
		}
	}
}
