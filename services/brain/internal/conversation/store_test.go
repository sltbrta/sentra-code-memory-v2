package conversation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type testClock struct {
	mu  sync.Mutex
	now int64
}

func (c *testClock) NowUnixMilli() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now++
	return c.now
}

type fakePayloads struct {
	mu       sync.Mutex
	objects  map[string][]byte
	putCount int
	putErr   error
	getErr   error
	tamper   bool
}

func newFakePayloads() *fakePayloads {
	return &fakePayloads{objects: make(map[string][]byte)}
}

func (f *fakePayloads) Put(_ context.Context, _ string, payload []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return "", f.putErr
	}
	f.putCount++
	artifactID := "artifact-" + payloadDigest(payload)
	f.objects[artifactID] = append([]byte(nil), payload...)
	return artifactID, nil
}

func (f *fakePayloads) Get(_ context.Context, _, artifactID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	payload, found := f.objects[artifactID]
	if !found {
		return nil, errors.New("missing artifact")
	}
	if f.tamper {
		mutated := append([]byte(nil), payload...)
		mutated[0] ^= 0xff
		return mutated, nil
	}
	return append([]byte(nil), payload...), nil
}

func (f *fakePayloads) Purge(_ context.Context, _, artifactID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.objects[artifactID]; !found {
		return nil
	}
	delete(f.objects, artifactID)
	return nil
}

func (f *fakePayloads) puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCount
}

func testIdentity(tenant, principal, session string) contracts.MappedIdentityFact {
	return contracts.MappedIdentityFact{
		Principal:   contracts.Identifier{Namespace: "principal", Value: principal},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: tenant},
		Session:     contracts.Identifier{Namespace: "session", Value: session},
		Credentials: contracts.PeerCredentials{UID: 501, PID: 4242},
	}
}

// newAuthorityDB builds a real migrated authority database and seeds the given
// sessions through the canonical Stage 02 session ledger, exactly as the
// composed runtime would before serving the query surface.
func newAuthorityDB(t *testing.T, sessions ...contracts.MappedIdentityFact) string {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir() + "/authority.db"
	authority, err := localstate.OpenWithMigrations(ctx, path, schema.Migrations(), localstate.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range sessions {
		if err := authority.OpenSession(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type storeFixture struct {
	store    *Store
	payloads *fakePayloads
	clock    *testClock
	path     string
}

func newStoreFixture(t *testing.T, path string) *storeFixture {
	t.Helper()
	payloads := newFakePayloads()
	clock := &testClock{now: 1000}
	store, err := Open(context.Background(), path, payloads, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &storeFixture{store: store, payloads: payloads, clock: clock, path: path}
}

func principal(tenant, id, session string) query.Principal {
	return query.Principal{Tenant: tenant, Principal: id, Session: session}
}

func admission(p query.Principal, key, text string) Admission {
	return Admission{
		Principal: p, SourceID: "source-1", GenerationID: "generation-1", Text: text,
		Freshness: query.FreshnessBestEffort, IdempotencyKey: key,
	}
}

func answeredResult(queryID string) *query.Result {
	return &query.Result{
		Answer: query.Answer{
			QueryID: queryID,
			Status:  query.StatusAnswered,
			Prose:   "ConfigPath returns the parsed path.",
			Claims: []query.Claim{{
				ClaimID:   "claim-0001",
				Statement: "ConfigPath returns the parsed path.",
				Citations: []query.Citation{{
					EvidenceID: "evidence-1", SourceRevisionID: "revision-1",
					GitOID: strings.Repeat("a", 40), Path: "internal/config/config.go",
					StartLine: 12, StartColumn: 1, EndLine: 14, EndColumn: 2,
					SupportingTextDigest: strings.Repeat("b", 64),
				}},
				ConfidencePerMille: 900,
			}},
			TokenUsage: 120,
			FactualConsistency: factualconsistency.Result{
				Status: factualconsistency.StatusScored, ScorePerMille: 850,
				Provenance: &factualconsistency.Provenance{
					ScorerID: "fixture-scorer", ScorerVersion: "v1", CalibrationID: "fixture-calibration-v1",
					CalibrationDigest: strings.Repeat("e", 64),
				},
				EvaluatedClaimCount: 1, TotalClaimCount: 1,
			},
		},
		Freshness: query.Freshness{
			GenerationID: "generation-1", Sequence: 1,
			CommitOID: strings.Repeat("c", 40), TreeOID: strings.Repeat("d", 40),
			GenerationState: query.GenerationReady, State: query.FreshnessCurrent,
			ACLEpoch: 7, ObservedAt: time.UnixMilli(4242).UTC(),
		},
		Coverage:   query.Coverage{CanonicalRevisionCount: 5, IndexedRevisionCount: 5},
		Projection: query.ProjectionReady,
	}
}

func completion(tenant, principalID, key string, result *query.Result) Completion {
	return Completion{Tenant: tenant, Principal: principalID, IdempotencyKey: key, Result: result}
}

func TestAdmitCommitsUserTurnAndIdempotencyAtomically(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	result, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "what does ConfigPath return?"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || result.QueryID == "" || result.UserTurnID == "" {
		t.Fatalf("unexpected admission: %+v", result)
	}
	if again := queryID("t1", "p1", "key-1"); again != result.QueryID {
		t.Fatalf("query identity is not deterministic: %q vs %q", again, result.QueryID)
	}
	if fixture.payloads.puts() != 1 {
		t.Fatalf("payload puts=%d, want 1", fixture.payloads.puts())
	}
	var turnCount, idempotencyCount int
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM conversation_turns`).Scan(&turnCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM conversation_query_idempotency`).Scan(&idempotencyCount); err != nil {
		t.Fatal(err)
	}
	if turnCount != 1 || idempotencyCount != 1 {
		t.Fatalf("admission is not atomic: turns=%d idempotency=%d", turnCount, idempotencyCount)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.NextCursor != "" {
		t.Fatalf("history=%+v", page)
	}
	turn := page.Turns[0]
	if turn.Role != RoleUser || turn.Status != StatusActive || turn.Sequence != 1 ||
		turn.Text != "what does ConfigPath return?" || turn.Answer != nil || turn.IdempotencyKey != "" {
		t.Fatalf("user turn shape: %+v", turn)
	}
	resolution, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Completed || resolution.UserTurnID != result.UserTurnID || resolution.QueryID != result.QueryID {
		t.Fatalf("resolution=%+v", resolution)
	}
}

func TestAdmitReplaysExactRetryAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()
	p := principal("t1", "p1", "sess1")

	first, err := fixture.store.Admit(ctx, admission(p, "key-1", "question one"))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.store.Admit(ctx, admission(p, "key-1", "question one"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.QueryID != first.QueryID || replayed.UserTurnID != first.UserTurnID {
		t.Fatalf("replay mismatch: %+v vs %+v", replayed, first)
	}
	if fixture.payloads.puts() != 1 {
		t.Fatalf("replay staged a payload: puts=%d", fixture.payloads.puts())
	}
	if _, err := fixture.store.Admit(ctx, admission(p, "key-1", "question CHANGED")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if _, err := fixture.store.Admit(ctx, admission(p, "key-2", "question one")); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 || page.Turns[0].Sequence != 1 || page.Turns[1].Sequence != 2 {
		t.Fatalf("conflict or replay mutated history: %+v", page.Turns)
	}
}

func TestCompleteAppendsAssistantTurnExactlyOnce(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()
	p := principal("t1", "p1", "sess1")

	admitted, err := fixture.store.Admit(ctx, admission(p, "key-1", "question"))
	if err != nil {
		t.Fatal(err)
	}
	want := answeredResult(admitted.QueryID)
	completed, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", want))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Replayed || completed.Sequence != 2 || completed.AssistantTurnID == "" {
		t.Fatalf("completion=%+v", completed)
	}
	again, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", answeredResult(admitted.QueryID)))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.AssistantTurnID != completed.AssistantTurnID {
		t.Fatalf("completion replay=%+v", again)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 {
		t.Fatalf("turns=%d, want exactly one completion", len(page.Turns))
	}
	assistant := page.Turns[1]
	if assistant.Role != RoleAssistant || assistant.Status != StatusActive ||
		assistant.IdempotencyKey != "key-1" || assistant.Text != "" || assistant.Answer == nil {
		t.Fatalf("assistant turn shape: %+v", assistant)
	}
	if !reflect.DeepEqual(*assistant.Answer, want.Answer) {
		t.Fatalf("answer round trip mismatch:\n got %+v\nwant %+v", *assistant.Answer, want.Answer)
	}
	resolution, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Completed || resolution.Status != StatusActive || resolution.Result == nil {
		t.Fatalf("resolution=%+v", resolution)
	}
	if !reflect.DeepEqual(*resolution.Result, *want) {
		t.Fatalf("result round trip mismatch:\n got %+v\nwant %+v", *resolution.Result, *want)
	}
}

func TestCompleteRejectsDifferingSecondOutcome(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()
	p := principal("t1", "p1", "sess1")

	admitted, err := fixture.store.Admit(ctx, admission(p, "key-1", "question"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", answeredResult(admitted.QueryID))); err != nil {
		t.Fatal(err)
	}
	changed := answeredResult(admitted.QueryID)
	changed.Answer.Prose = "a different answer entirely"
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", changed)); !errors.Is(err, ErrCompletionConflict) {
		t.Fatalf("differing completion err=%v", err)
	}
	if _, err := fixture.store.Complete(ctx, Completion{
		Tenant: "t1", Principal: "p1", IdempotencyKey: "key-1", Failed: true,
	}); !errors.Is(err, ErrCompletionConflict) {
		t.Fatalf("active-then-failed err=%v", err)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 || page.Turns[1].Status != StatusActive {
		t.Fatalf("conflicting completion mutated history: %+v", page.Turns)
	}
}

func TestCompleteRequiresAdmission(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	if _, err := fixture.store.Complete(context.Background(),
		completion("t1", "p1", "never-admitted", answeredResult("query-x"))); !errors.Is(err, ErrUnknownAdmission) {
		t.Fatalf("err=%v", err)
	}
	if _, err := fixture.store.Resolve(context.Background(), "t1", "p1", "never-admitted"); !errors.Is(err, ErrUnknownAdmission) {
		t.Fatalf("resolve err=%v", err)
	}
}

func TestReplayPreservesProjectionAndCoverageByteExact(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	admitted, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "question"))
	if err != nil {
		t.Fatal(err)
	}
	// An abstention composed while the projection was rebuilding: coverage and
	// projection state are the whole outcome and must survive replay exactly.
	want := &query.Result{
		Answer: query.Answer{
			QueryID:            admitted.QueryID,
			Status:             query.StatusAbstained,
			DegradedReasons:    []query.Reason{query.ReasonRetrievalUnavailable},
			TokenUsage:         0,
			FactualConsistency: factualconsistency.Abstained(),
		},
		Freshness: query.Freshness{
			GenerationID: "generation-1", Sequence: 2,
			CommitOID: strings.Repeat("c", 40), TreeOID: strings.Repeat("d", 40),
			GenerationState: query.GenerationDegraded, State: query.FreshnessStaleDisclosed,
			ACLEpoch: 9, ObservedAt: time.UnixMilli(5151).UTC(),
		},
		Coverage:   query.Coverage{CanonicalRevisionCount: 7, IndexedRevisionCount: 3},
		Projection: query.ProjectionRebuilding,
	}
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", want)); err != nil {
		t.Fatal(err)
	}
	resolution, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Completed || resolution.Result == nil {
		t.Fatalf("resolution=%+v", resolution)
	}
	if !reflect.DeepEqual(*resolution.Result, *want) {
		t.Fatalf("projection or coverage lost on replay:\n got %+v\nwant %+v", *resolution.Result, *want)
	}
	if resolution.Result.Projection != query.ProjectionRebuilding {
		t.Fatalf("projection replayed as %q, want rebuilding", resolution.Result.Projection)
	}
}

func TestLegacyPayloadWithoutProjectionStillReplays(t *testing.T) {
	t.Parallel()
	// Payloads written before projection persistence carry no projection key;
	// they must still decode, replaying the projection as honestly unknown
	// rather than failing hydration.
	legacy := []byte(`{"version":1,"result":{"answer":{"query_id":"query-1","status":"abstained","degraded_reasons":["absent_support"],"token_usage":0},"freshness":{"generation_id":"gen-1","sequence":1,"commit_oid":"` +
		strings.Repeat("a", 40) + `","tree_oid":"` + strings.Repeat("b", 40) +
		`","generation_state":"ready","state":"current","acl_epoch":1,"observed_at_ms":42},"coverage":{"canonical_revision_count":2,"indexed_revision_count":2}}}`)
	payload, err := unmarshalPayload(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Result == nil {
		t.Fatal("legacy payload lost its result")
	}
	result := domainResult(payload.Result)
	if result.Projection != "" {
		t.Fatalf("legacy projection must replay as unknown, got %q", result.Projection)
	}
	if result.Coverage.CanonicalRevisionCount != 2 || result.Answer.Status != query.StatusAbstained {
		t.Fatalf("legacy result corrupted: %+v", result)
	}
}

func TestFailedCompletionIsVisibleAndNeverReadAsFact(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "question")); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.Complete(ctx, Completion{
		Tenant: "t1", Principal: "p1", IdempotencyKey: "key-1", Failed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Replayed || completed.Sequence != 2 {
		t.Fatalf("failed completion=%+v", completed)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 {
		t.Fatalf("turns=%d", len(page.Turns))
	}
	failed := page.Turns[1]
	if failed.Role != RoleAssistant || failed.Status != StatusFailed || failed.Answer != nil || failed.Text != "" {
		t.Fatalf("failed turn must carry no answer and no text: %+v", failed)
	}
	resolution, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Completed || resolution.Status != StatusFailed || resolution.Result != nil {
		t.Fatalf("failed resolution=%+v", resolution)
	}
}

func TestRecoverInterruptedMarksUncompletedQueriesFailed(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()
	p := principal("t1", "p1", "sess1")

	admitted, err := fixture.store.Admit(ctx, admission(p, "key-1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Admit(ctx, admission(p, "key-2", "second")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", answeredResult(admitted.QueryID))); err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.store.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	if again, err := fixture.store.RecoverInterrupted(ctx); err != nil || again != 0 {
		t.Fatalf("recovery is not idempotent: again=%d err=%v", again, err)
	}
	resolution, err := fixture.store.Resolve(ctx, "t1", "p1", "key-2")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Completed || resolution.Status != StatusFailed {
		t.Fatalf("interrupted resolution=%+v", resolution)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 4 || page.Turns[3].Status != StatusFailed || page.Turns[3].Sequence != 4 {
		t.Fatalf("recovered history: %+v", page.Turns)
	}
}

func TestCrashMidCompletionRecoveryAcrossReopen(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	path := newAuthorityDB(t, identity)
	ctx := context.Background()
	payloads := newFakePayloads()
	clock := &testClock{now: 1000}

	store, err := Open(ctx, path, payloads, clock)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "crashed question"))
	if err != nil {
		t.Fatal(err)
	}
	// Crash after admission commit but before any completion: the user turn and
	// idempotency record are durable, the assistant completion never happened.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path, payloads, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	recovered, err := reopened.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	replayed, err := reopened.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "crashed question"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.QueryID != admitted.QueryID {
		t.Fatalf("post-crash admission=%+v", replayed)
	}
	resolution, err := reopened.Resolve(ctx, "t1", "p1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Completed || resolution.Status != StatusFailed || resolution.Result != nil {
		t.Fatalf("post-crash resolution=%+v", resolution)
	}
	page, err := reopened.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 || page.Turns[1].Status != StatusFailed {
		t.Fatalf("post-crash history: %+v", page.Turns)
	}
}

func TestHistoryPaginatesInTotalOrderThroughOpaqueCursor(t *testing.T) {
	t.Parallel()
	identityOne := testIdentity("t1", "p1", "sess1")
	identityTwo := testIdentity("t1", "p1", "sess2")
	fixture := newStoreFixture(t, newAuthorityDB(t, identityOne, identityTwo))
	ctx := context.Background()

	for index, session := range []string{"sess1", "sess2", "sess1"} {
		key := "key-" + session + "-" + string(rune('a'+index))
		admitted, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", session), key, "question "+session))
		if err != nil {
			t.Fatal(err)
		}
		if index != 1 {
			if _, err := fixture.store.Complete(ctx, completion("t1", "p1", key, answeredResult(admitted.QueryID))); err != nil {
				t.Fatal(err)
			}
		}
	}
	var seen []Turn
	cursor := ""
	for page := 0; page < 4; page++ {
		result, err := fixture.store.History(ctx, "t1", "p1", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, result.Turns...)
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatalf("pagination did not terminate: cursor=%q turns=%d", cursor, len(seen))
	}
	if len(seen) != 5 {
		t.Fatalf("turns=%d, want 5", len(seen))
	}
	for index := 1; index < len(seen); index++ {
		before, after := seen[index-1], seen[index]
		if after.OccurredAtMs < before.OccurredAtMs ||
			(after.OccurredAtMs == before.OccurredAtMs && after.TurnID <= before.TurnID) {
			t.Fatalf("total order violated at %d: %+v then %+v", index, before, after)
		}
	}
	if seen[0].SessionID != "sess1" || seen[1].SessionID != "sess1" ||
		seen[2].SessionID != "sess2" || seen[4].SessionID != "sess1" {
		t.Fatalf("sessions interleaved unexpectedly: %+v", seen)
	}
	empty, err := fixture.store.History(ctx, "t1", "p1", encodeCursor(seen[4].OccurredAtMs, seen[4].TurnID), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Turns) != 0 || empty.NextCursor != "" {
		t.Fatalf("cursor at end must yield an empty page: %+v", empty)
	}
	for _, malformed := range []string{"garbage", "v1.!!!", "v2." + cursor, "v1." + encodeCursor(1, "")} {
		if _, err := fixture.store.History(ctx, "t1", "p1", malformed, 2); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("malformed cursor %q err=%v", malformed, err)
		}
	}
}

func TestHistoryIsInvisibleAcrossPrincipalsAndTenants(t *testing.T) {
	t.Parallel()
	fixture := newStoreFixture(t, newAuthorityDB(t,
		testIdentity("t1", "p1", "sess1"), testIdentity("t1", "p2", "sess2"), testIdentity("t2", "p1", "sess3")))
	ctx := context.Background()

	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "p1 question")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p2", "sess2"), "key-1", "p2 question")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Admit(ctx, admission(principal("t2", "p1", "sess3"), "key-1", "t2 question")); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].Text != "p1 question" {
		t.Fatalf("cross-principal leakage: %+v", page.Turns)
	}
	other, err := fixture.store.History(ctx, "t1", "p2", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Turns) != 1 || other.Turns[0].Text != "p2 question" {
		t.Fatalf("p2 history: %+v", other.Turns)
	}
	// Idempotency scope is per (tenant, principal): the same key under another
	// principal is an independent admission, never a replay or conflict.
	resolution, err := fixture.store.Resolve(ctx, "t1", "p2", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.UserTurnID == "" || resolution.QueryID == "" {
		t.Fatalf("p2 resolution=%+v", resolution)
	}
}

func TestHydrationRejectsTamperedPayloads(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	admitted, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "question"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", answeredResult(admitted.QueryID))); err != nil {
		t.Fatal(err)
	}
	fixture.payloads.tamper = true
	if _, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("tampered history err=%v", err)
	}
	if _, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1"); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("tampered resolve err=%v", err)
	}
}

func TestInvalidInputIsRejectedBeforeAnyEffect(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()
	valid := admission(principal("t1", "p1", "sess1"), "key-1", "question")

	cases := map[string]Admission{
		"empty tenant":         func() Admission { a := valid; a.Principal.Tenant = ""; return a }(),
		"empty principal":      func() Admission { a := valid; a.Principal.Principal = ""; return a }(),
		"empty session":        func() Admission { a := valid; a.Principal.Session = ""; return a }(),
		"empty source":         func() Admission { a := valid; a.SourceID = ""; return a }(),
		"empty generation":     func() Admission { a := valid; a.GenerationID = ""; return a }(),
		"blank text":           func() Admission { a := valid; a.Text = "   "; return a }(),
		"oversized text":       func() Admission { a := valid; a.Text = strings.Repeat("x", MaxQueryLength+1); return a }(),
		"oversized key":        func() Admission { a := valid; a.IdempotencyKey = strings.Repeat("k", 513); return a }(),
		"control key":          func() Admission { a := valid; a.IdempotencyKey = "key\n1"; return a }(),
		"unknown freshness":    func() Admission { a := valid; a.Freshness = "eventually"; return a }(),
		"whitespace-padded id": func() Admission { a := valid; a.SourceID = " source"; return a }(),
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.store.Admit(ctx, candidate); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if fixture.payloads.puts() != 0 {
		t.Fatalf("invalid admission staged payloads: %d", fixture.payloads.puts())
	}
	if _, err := fixture.store.History(ctx, "t1", "p1", "", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero limit err=%v", err)
	}
	if _, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage+1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized limit err=%v", err)
	}
	if _, err := fixture.store.Complete(ctx, Completion{
		Tenant: "t1", Principal: "p1", IdempotencyKey: "k", Result: answeredResult("q"), Failed: true,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ambiguous completion err=%v", err)
	}
	if _, err := fixture.store.Complete(ctx, Completion{
		Tenant: "t1", Principal: "p1", IdempotencyKey: "k",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty completion err=%v", err)
	}
}

func TestAdmissionRequiresPersistedSession(t *testing.T) {
	t.Parallel()
	fixture := newStoreFixture(t, newAuthorityDB(t))
	if _, err := fixture.store.Admit(context.Background(),
		admission(principal("t1", "p1", "never-opened"), "key-1", "question")); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err=%v", err)
	}
}

func TestSchemaEnforcementRemainsArmedOnStoreRows(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	admitted, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "question"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Complete(ctx, completion("t1", "p1", "key-1", answeredResult(admitted.QueryID))); err != nil {
		t.Fatal(err)
	}
	assertFails := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := fixture.store.db.ExecContext(ctx, statement, arguments...); err == nil {
			t.Fatalf("schema allowed forbidden statement: %s", statement)
		}
	}
	digest := strings.Repeat("e", 64)
	// Immutability triggers reject update and delete on both tables.
	assertFails(`UPDATE conversation_turns SET status='failed' WHERE turn_id=?`, admitted.UserTurnID)
	assertFails(`DELETE FROM conversation_turns WHERE turn_id=?`, admitted.UserTurnID)
	assertFails(`UPDATE conversation_query_idempotency SET request_digest=? WHERE idempotency_key='key-1'`, digest)
	assertFails(`DELETE FROM conversation_query_idempotency WHERE idempotency_key='key-1'`)
	// The dense-append trigger rejects gaps and rewinds.
	assertFails(`INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','gap-turn',4,'user','active','artifact',?,9)`, digest)
	// The role check rejects keyed user turns and keyless assistant turns.
	assertFails(`INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','keyed-user',3,'user','active','key-2','artifact',?,9)`, digest)
	assertFails(`INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','keyless-assistant',3,'assistant','active','artifact',?,9)`, digest)
	// The partial unique index rejects a second completion per admitted query.
	assertFails(`INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES ('t1','p1','sess1','second-completion',3,'assistant','failed','key-1','artifact',?,9)`, digest)
	// The idempotency trigger rejects records referencing a non-user turn.
	assertFails(`INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES ('t1','p1','key-2',?,'sess1',?,9)`, digest, assistantTurnID("t1", "p1", "key-1"))
}

func TestOpenFailsClosedWithoutMigrationFour(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/empty.db"
	// An empty database file: no schema_migrations at all.
	if _, err := Open(context.Background(), path, newFakePayloads(), &testClock{}); !errors.Is(err, ErrSchemaUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), "relative/path.db", newFakePayloads(), &testClock{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("relative path err=%v", err)
	}
	if _, err := Open(context.Background(), t.TempDir()+"/x.db", nil, &testClock{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil payloads err=%v", err)
	}
	if _, err := Open(context.Background(), t.TempDir()+"/x.db", newFakePayloads(), nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil clock err=%v", err)
	}
}

func TestStoreFailsClosedAfterClose(t *testing.T) {
	t.Parallel()
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-1", "question")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-2", "again")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("admit after close err=%v", err)
	}
	if _, err := fixture.store.Complete(ctx, Completion{
		Tenant: "t1", Principal: "p1", IdempotencyKey: "key-1", Failed: true,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("complete after close err=%v", err)
	}
	if _, err := fixture.store.Resolve(ctx, "t1", "p1", "key-1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("resolve after close err=%v", err)
	}
	if _, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("history after close err=%v", err)
	}
	if _, err := fixture.store.RecoverInterrupted(ctx); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("recover after close err=%v", err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatalf("close must be idempotent: %v", err)
	}
	if fixture.payloads.puts() != 1 {
		t.Fatalf("a closed store staged payloads: %d", fixture.payloads.puts())
	}
}

func TestCloseDuringOperationsCommitsOrFailsClosed(t *testing.T) {
	// Not parallel: this test races Close against in-flight work on purpose.
	identity := testIdentity("t1", "p1", "sess1")
	fixture := newStoreFixture(t, newAuthorityDB(t, identity))
	ctx := context.Background()

	if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), "key-0", "seed")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := "key-race-" + strings.Repeat("k", index+1)
			if _, err := fixture.store.Admit(ctx, admission(principal("t1", "p1", "sess1"), key, "race")); err != nil {
				errs <- err
			}
			if _, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage); err != nil {
				errs <- err
			}
		}(index)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := fixture.store.Close(); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("racing operation failed with a non-closed-store error: %v", err)
		}
	}
	// Every operation either committed completely before Close acquired the
	// mutex or failed closed after it; no torn state may remain.
	if _, err := fixture.store.History(ctx, "t1", "p1", "", MaxHistoryPage); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("store must stay closed: %v", err)
	}
	reopened, err := Open(ctx, fixture.path, fixture.payloads, fixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	page, err := reopened.History(ctx, "t1", "p1", "", MaxHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) < 1 {
		t.Fatalf("committed turns were lost: %+v", page.Turns)
	}
	sequences := make([]uint64, 0, len(page.Turns))
	for _, turn := range page.Turns {
		if turn.SessionID == "sess1" {
			sequences = append(sequences, turn.Sequence)
		}
	}
	for index, sequence := range sequences {
		if sequence != uint64(index)+1 {
			t.Fatalf("dense per-session sequence broke under raced close: %v", sequences)
		}
	}
}
