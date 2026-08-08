package orgscope_test

// Regression tests for the issue #311 PR review findings:
//  1. offboarding/revocation must win over concurrent (in-flight) reads;
//  2. Restore must checksum backup contents against the canonical manifest;
//  3. audit entries must be deep copies (immutable receipts);
//  4. an in-flight Erase must fail closed in Query and History (no stale
//     snippet after Resolve).
//
// The trap fixture arms a one-shot mutation on the injected clock so a policy
// or erasure mutation lands deterministically in the middle of a read: after
// the read's top-of-call activity check and mid-Resolve, exactly the window a
// concurrent request would race through.

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/orgscope"
)

type trapFixture struct {
	t     *testing.T
	dir   *orgscope.Directory
	auth  *orgscope.Authority
	store *orgscope.Store
	trap  func()
}

// newTrapFixture wires a fixture whose clock fires f.trap exactly once on the
// next clock read. Resolve reads the clock after its activity check, so the
// trap lands inside the authorization window of an in-flight read.
func newTrapFixture(t *testing.T) *trapFixture {
	t.Helper()
	f := &trapFixture{t: t}
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	dir, err := orgscope.NewDirectory("acme", func() time.Time {
		if f.trap != nil {
			fire := f.trap
			f.trap = nil
			fire()
		}
		return base
	})
	if err != nil {
		t.Fatal(err)
	}
	f.dir = dir
	f.auth = orgscope.NewAuthority(dir)
	f.store = orgscope.NewStore(f.auth)
	return f
}

func (f *trapFixture) provision(users ...string) {
	f.t.Helper()
	for _, u := range users {
		if _, err := f.dir.Provision(u); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *trapFixture) put(items ...orgscope.Item) {
	f.t.Helper()
	for _, item := range items {
		if err := f.store.Put(item); err != nil {
			f.t.Fatalf("put %s: %v", item.ID, err)
		}
	}
}

func ownItem(id, user, text string) orgscope.Item {
	return orgscope.Item{
		ID:    id,
		Scope: orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: user},
		Owner: user,
		Text:  text,
	}
}

// Finding 1: offboarding that lands after the top-of-call activity check but
// before data is served must still deny; the revocation epoch wins the race.
func TestOffboardingWinsInFlightReads(t *testing.T) {
	f := newTrapFixture(t)
	f.provision("alice")
	f.put(ownItem("i-alice", "alice", "private secret note"))
	// Warm cache and session history while alice is active.
	if _, err := f.store.Query(principal("alice"), "secret"); err != nil {
		t.Fatal(err)
	}

	// Offboarding lands mid-Query, inside the authorization pass.
	f.trap = func() {
		if _, err := f.dir.Deprovision("alice"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Query(principal("alice"), "secret"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatalf("in-flight offboarding query = %v, want ErrDenied", err)
	}

	// Reinstate, then offboard mid-History replay.
	f.provision("alice")
	if _, err := f.store.Query(principal("alice"), "secret"); err != nil {
		t.Fatal(err)
	}
	f.trap = func() {
		if _, err := f.dir.Deprovision("alice"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.History(principal("alice")); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatalf("in-flight offboarding history = %v, want ErrDenied", err)
	}
}

// Finding 2: Restore must recompute the checksum over canonical backup
// contents and reject modifications whose checksum was not recomputed. This
// is an integrity check, not an authenticity or provenance proof.
func TestRestoreRejectsModifiedBackupWithoutMatchingChecksum(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(
		ownItem("e-1", "alice", "erasable payroll figure"),
		ownItem("k-1", "alice", "kept payroll note"),
		ownItem("k-2", "alice", "kept roadmap note"),
	)
	if _, err := f.store.Erase("gdpr_request", "e-1"); err != nil {
		t.Fatal(err)
	}
	backup, err := f.store.CreateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.Items) != 2 || len(backup.Tombstones) != 1 {
		t.Fatalf("backup shape = %d items, %d tombstones", len(backup.Items), len(backup.Tombstones))
	}

	// The untampered backup restores cleanly.
	fresh := orgscope.NewStore(f.auth)
	if err := fresh.Restore(backup); err != nil {
		t.Fatalf("clean restore = %v", err)
	}
	// Canonical verification: element order does not matter, contents do.
	reordered := backup
	reordered.Items = []orgscope.Item{backup.Items[1], backup.Items[0]}
	if err := fresh.Restore(reordered); err != nil {
		t.Fatalf("reordered restore = %v", err)
	}

	// Tampered item content is rejected.
	tampered := backup
	tampered.Items = append([]orgscope.Item(nil), backup.Items...)
	tampered.Items[0].Text = "tampered payload"
	if err := fresh.Restore(tampered); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatalf("tampered-content restore = %v, want ErrRejected", err)
	}
	// Stripping tombstones (a resurrection attempt) is rejected.
	resurrect := backup
	resurrect.Tombstones = nil
	if err := fresh.Restore(resurrect); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatalf("tombstone-stripped restore = %v, want ErrRejected", err)
	}
	// Injecting an extra item is rejected.
	injected := backup
	injected.Items = append(append([]orgscope.Item(nil), backup.Items...), ownItem("evil", "alice", "smuggled item"))
	if err := fresh.Restore(injected); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatalf("injected-item restore = %v, want ErrRejected", err)
	}
	// A non-empty but unverified digest is rejected.
	forged := backup
	forged.Digest = strings.Repeat("ab", 32)
	if err := fresh.Restore(forged); !errors.Is(err, orgscope.ErrRejected) {
		t.Fatalf("forged-digest restore = %v, want ErrRejected", err)
	}
	// The rejected restores left no tampered state behind.
	if leaks := fresh.VerifyErasure("e-1", "evil"); len(leaks.Leaks) != 0 {
		t.Fatalf("rejected restores leaked: %v", leaks.Leaks)
	}
}

// Finding 3: audit entries are receipts; mutating a returned copy (or a
// caller-held id slice) must never alter the retained log.
func TestAuditEntriesAreDeepCopies(t *testing.T) {
	f := newFixture(t)
	f.provision("alice")
	f.put(ownItem("i-1", "alice", "secret ledger"))
	if _, err := f.store.Query(principal("alice"), "ledger"); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.store.Erase("gdpr_request", "i-1")
	if err != nil {
		t.Fatal(err)
	}

	first := f.store.Audit()
	idx := -1
	for i, entry := range first {
		if len(entry.ItemIDs) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no audit entry with item ids")
	}
	want := first[idx].ItemIDs[0]
	// Attack 1: mutate the returned audit copy.
	first[idx].ItemIDs[0] = "forged"
	// Attack 2: mutate the erasure receipt's id slice.
	receipt.ItemIDs[0] = "forged"

	second := f.store.Audit()
	if got := second[idx].ItemIDs[0]; got != want {
		t.Fatalf("retained audit mutated: got %q, want %q", got, want)
	}
	for _, entry := range second {
		for _, id := range entry.ItemIDs {
			if id == "forged" {
				t.Fatalf("forged id reached retained audit: %+v", entry)
			}
		}
	}
}

// Finding 4: an Erase that lands between Resolve and the response must win —
// Query revalidates item/tombstone state before returning snippets.
func TestInFlightEraseFailsClosedInQuery(t *testing.T) {
	f := newTrapFixture(t)
	f.provision("alice")
	f.put(
		ownItem("e-1", "alice", "ephemeral payroll figure"),
		ownItem("k-1", "alice", "kept payroll note"),
	)
	f.trap = func() {
		if _, err := f.store.Erase("gdpr_request", "e-1"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := f.store.Query(principal("alice"), "payroll")
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Citations)
	if got["e-1"] || !got["k-1"] {
		t.Fatalf("racy query citations = %v", got)
	}
	for _, c := range res.Citations {
		if strings.Contains(c.Snippet, "ephemeral") {
			t.Fatalf("stale erased snippet served: %+v", c)
		}
	}
	// The racy read must not have re-seeded any projection with the erased id.
	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("projections re-seeded by racy query: %v", leaks.Leaks)
	}
}

// Finding 4 (History): an Erase during session replay must win as well.
func TestInFlightEraseFailsClosedInHistory(t *testing.T) {
	f := newTrapFixture(t)
	f.provision("alice")
	f.put(ownItem("e-1", "alice", "ephemeral payroll figure"))
	// Warm session history with the soon-to-be-erased item.
	if _, err := f.store.Query(principal("alice"), "ephemeral"); err != nil {
		t.Fatal(err)
	}
	f.trap = func() {
		if _, err := f.store.Erase("gdpr_request", "e-1"); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := f.store.History(principal("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 0 {
		t.Fatalf("racy history served erased memory: %v", hist)
	}
	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("projections re-seeded by racy history: %v", leaks.Leaks)
	}
}

// Race coverage: reads racing revocation and erasure never serve erased ids
// after Erase returns, never serve an offboarded principal after Deprovision
// returns, and trip no data races under -race.
func TestConcurrentReadsRevocationAndErasureRace(t *testing.T) {
	f := newFixture(t)
	f.provision("alice", "bob")
	f.put(
		ownItem("e-1", "alice", "erasable payroll figure"),
		ownItem("k-1", "alice", "kept payroll note"),
	)
	if _, err := f.auth.Grant(orgscope.Grant{
		Subject: "user:bob",
		Scope:   orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"},
	}); err != nil {
		t.Fatal(err)
	}

	var erased, offboarded atomic.Bool
	var wg sync.WaitGroup
	reader := func(user string) {
		defer wg.Done()
		p := principal(user)
		for i := 0; i < 200; i++ {
			preErase := erased.Load()
			preOff := offboarded.Load()
			res, err := f.store.Query(p, "payroll")
			if err == nil {
				if user == "bob" && preOff {
					t.Errorf("offboarded bob served after deprovision returned")
				}
				if preErase && ids(res.Citations)["e-1"] {
					t.Errorf("erased e-1 served after erase returned")
				}
			}
			hist, err := f.store.History(p)
			if err == nil && preErase && ids(hist)["e-1"] {
				t.Errorf("erased e-1 replayed after erase returned")
			}
		}
	}
	wg.Add(2)
	go reader("alice")
	go reader("bob")

	if _, err := f.auth.Revoke("user:bob", orgscope.Scope{Kind: orgscope.ScopeIndividual, ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Erase("gdpr_request", "e-1"); err != nil {
		t.Fatal(err)
	}
	erased.Store(true)
	if _, err := f.dir.Deprovision("bob"); err != nil {
		t.Fatal(err)
	}
	offboarded.Store(true)
	wg.Wait()

	if leaks := f.store.VerifyErasure("e-1"); len(leaks.Leaks) != 0 {
		t.Fatalf("post-race projection leaks: %v", leaks.Leaks)
	}
	if _, err := f.store.Query(principal("bob"), "payroll"); !errors.Is(err, orgscope.ErrDenied) {
		t.Fatal("offboarded bob must stay denied")
	}
	res, err := f.store.Query(principal("alice"), "payroll")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Citations); got["e-1"] || !got["k-1"] {
		t.Fatalf("post-race citations = %v", got)
	}
}
