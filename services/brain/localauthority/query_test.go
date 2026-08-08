package localauthority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestQuerySurfaceAnswerLifecycle proves the composed Stage 04 query surface
// over a real durable runtime: cited answers, stale and absent outcomes,
// private conversation history, catalog facts, revocation collapsing to
// absent_support, and restart rebuild with idempotent replay.
func TestQuerySurfaceAnswerLifecycle(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	writeRepositoryFiles(t, repository, map[string]string{
		"main.go":   "package sample\n\nfunc Anchor() string { return \"stage-marker\" }\n",
		"notes.txt": "unindexed fixture note\n",
	})
	if err := os.Symlink("notes.txt", filepath.Join(repository, "notes-link")); err != nil {
		t.Fatal(err)
	}
	firstCommit := commitRepository(t, repository, "initial")

	root := t.TempDir()
	config, keys := durableTestConfig(root)
	// Conversation payloads are bounded at 1 MiB; the vault read ceiling must
	// cover them or history hydration fails closed.
	config.Storage.MaxReadBytes = 1 << 20
	config.Ingestion = testIngestionConfig(repository)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	requestContext := testIngestionContext(config, identity)
	added, err := runtime.AddSource(ctx, AddSourceRequest{
		IngestionContext: requestContext, ExpectedCommitOID: firstCommit, IdempotencyKey: "add-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.OpenQuerySurface(ctx, identity, allowQueryAuthorizer, NewDeterministicQuerySynthesizer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("receipt-enforced surface without provider error = %v", err)
	}
	admissionCalls := 0
	strictSurface, err := runtime.OpenQuerySurface(
		ctx, identity, allowQueryAuthorizer, NewDeterministicQuerySynthesizer(),
		QueryEvidenceAdmitterFunc(func(context.Context, QueryEvidenceAdmissionRequest) bool {
			admissionCalls++
			return true
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	strictResult, err := strictSurface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-strict", Principal: QueryPrincipal{Tenant: "t", Principal: "p", Session: "s"},
		SourceID: added.Status.SourceID, GenerationID: added.Status.GenerationID,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-strict",
	})
	if err != nil || strictResult.Answer.Status != "answered" || admissionCalls != 2 {
		t.Fatalf("receipt-enforced answer = %#v, calls = %d, err = %v", strictResult.Answer, admissionCalls, err)
	}
	if err := strictSurface.Close(); err != nil {
		t.Fatal(err)
	}
	surface, err := runtime.OpenLegacyQuerySurfaceWithoutEvidenceAdmission(ctx, identity, allowQueryAuthorizer, NewDeterministicQuerySynthesizer())
	if err != nil {
		t.Fatal(err)
	}
	if err := surface.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	principal := QueryPrincipal{Tenant: "t", Principal: "p", Session: "s"}
	sourceID := added.Status.SourceID
	generationOne := added.Status.GenerationID

	result, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-1", Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer.Status != "answered" || len(result.Answer.Claims) != 1 || len(result.Answer.DegradedReasons) != 0 {
		t.Fatalf("answer = %#v", result.Answer)
	}
	citation := result.Answer.Claims[0].Citations[0]
	if citation.Path != "main.go" || citation.StartLine != 3 || citation.EndLine != 3 ||
		len(citation.SupportingTextDigest) != 64 || citation.GitOID != firstCommit {
		t.Fatalf("citation = %#v", citation)
	}
	if result.Freshness.State != "current" || result.Freshness.GenerationID != generationOne ||
		result.Coverage.CanonicalRevisionCount != 3 || result.Coverage.IndexedRevisionCount != 1 ||
		result.Projection != "ready" {
		t.Fatalf("result disclosures = %#v", result)
	}

	status, err := surface.Engine().Status(ctx, principal, sourceID)
	if err != nil || status.Projection != "ready" || status.Freshness.State != "current" ||
		status.Coverage.IndexedRevisionCount != 1 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	catalog, err := surface.CatalogSource(ctx)
	if err != nil || catalog.State != "ready" || catalog.CurrentGenerationID != generationOne ||
		catalog.RepositoryID != "test-repository" {
		t.Fatalf("catalog source = %#v, %v", catalog, err)
	}
	facts, err := surface.CatalogGenerationFacts(ctx, generationOne)
	if err != nil || facts.Sequence != 1 || facts.SnapshotID != added.Status.SnapshotID ||
		facts.CommitOID != firstCommit || len(facts.Readiness) != 5 {
		t.Fatalf("catalog facts = %#v, %v", facts, err)
	}

	admission, err := surface.Conversations().Admit(ctx, ConversationAdmission{
		Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-1",
	})
	if err != nil || admission.Replayed {
		t.Fatalf("admission = %#v, %v", admission, err)
	}
	stored := result
	if _, err := surface.Conversations().Complete(ctx, ConversationCompletion{
		Tenant: "t", Principal: "p", IdempotencyKey: "ask-1", Result: &stored,
	}); err != nil {
		t.Fatal(err)
	}
	resolution, err := surface.Conversations().Resolve(ctx, "t", "p", "ask-1")
	if err != nil || !resolution.Completed || resolution.Status != "active" || resolution.Result == nil ||
		resolution.Result.Answer.Prose != result.Answer.Prose {
		t.Fatalf("resolution = %#v, %v", resolution, err)
	}
	page, err := surface.Conversations().History(ctx, "t", "p", "", 50)
	if err != nil || len(page.Turns) != 2 {
		t.Fatalf("history = %#v, %v", page, err)
	}

	// An admitted-but-uncompleted query is visibly failed after recovery.
	if _, err := surface.Conversations().Admit(ctx, ConversationAdmission{
		Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "interrupted?", Freshness: "best_effort", IdempotencyKey: "ask-crash",
	}); err != nil {
		t.Fatal(err)
	}
	if err := surface.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	runtime, err = OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	surface, err = runtime.OpenLegacyQuerySurfaceWithoutEvidenceAdmission(ctx, identity, allowQueryAuthorizer, NewDeterministicQuerySynthesizer())
	if err != nil {
		t.Fatal(err)
	}
	if err := surface.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	crashed, err := surface.Conversations().Resolve(ctx, "t", "p", "ask-crash")
	if err != nil || !crashed.Completed || crashed.Status != "failed" {
		t.Fatalf("recovered resolution = %#v, %v", crashed, err)
	}
	// The restart rebuilt the projection: the same ask answers identically.
	restarted, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-2", Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-2",
	})
	if err != nil || restarted.Answer.Status != "answered" ||
		restarted.Answer.Claims[0].Citations[0] != citation {
		t.Fatalf("restarted answer = %#v, %v", restarted, err)
	}

	writeRepositoryFiles(t, repository, map[string]string{
		"main.go": "package sample\n\nfunc Anchor() string { return \"next-marker\" }\n",
	})
	secondCommit := commitRepository(t, repository, "update")
	reconciled, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: generationOne,
		ExpectedCommitOID: firstCommit, TargetCommitOID: secondCommit, IdempotencyKey: "reconcile-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	generationTwo := reconciled.Status.GenerationID

	stale, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-3", Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Answer.Status != "partial" {
		t.Fatalf("stale answer = %#v", stale)
	}
	if stale.Freshness.State != "stale_disclosed" || len(stale.Answer.DegradedReasons) != 1 ||
		stale.Answer.DegradedReasons[0] != "stale_support" {
		t.Fatalf("stale disclosures = %#v", stale)
	}
	abstainedStale, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-4", Principal: principal, SourceID: sourceID, GenerationID: generationOne,
		Text: "What does main.go return?", Freshness: "abstain_if_stale", IdempotencyKey: "ask-4",
	})
	if err != nil || abstainedStale.Answer.Status != "abstained" ||
		len(abstainedStale.Answer.DegradedReasons) != 1 || abstainedStale.Answer.DegradedReasons[0] != "stale_support" {
		t.Fatalf("abstain-if-stale = %#v, %v", abstainedStale, err)
	}
	absent, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-5", Principal: principal, SourceID: sourceID, GenerationID: generationTwo,
		Text: "What does the billing service return?", Freshness: "best_effort", IdempotencyKey: "ask-5",
	})
	if err != nil || absent.Answer.Status != "abstained" ||
		!containsQueryReason(absent.Answer.DegradedReasons, "absent_support") {
		t.Fatalf("absent answer = %#v, %v", absent, err)
	}
	if _, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-6", Principal: principal, SourceID: sourceID, GenerationID: "unknown-generation",
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-6",
	}); !errors.Is(err, ErrQueryUnknownScope) {
		t.Fatalf("unknown generation error = %v", err)
	}
	supersededFacts, err := surface.CatalogGenerationFacts(ctx, generationOne)
	if err != nil || supersededFacts.Sequence != 1 || supersededFacts.CommitOID != firstCommit {
		t.Fatalf("superseded facts = %#v, %v", supersededFacts, err)
	}

	if _, err := runtime.RevokeSource(ctx, RevokeSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: generationTwo,
		RevocationEpoch: 2, IdempotencyKey: "revoke-source",
	}); err != nil {
		t.Fatal(err)
	}
	// A revoked source collapses to the same absent_support abstention as
	// genuinely absent support while freshness and coverage stay truthful.
	revoked, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-7", Principal: principal, SourceID: sourceID, GenerationID: generationTwo,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-7",
	})
	if err != nil || revoked.Answer.Status != "abstained" || len(revoked.Answer.Claims) != 0 ||
		len(revoked.Answer.DegradedReasons) != 1 || revoked.Answer.DegradedReasons[0] != "absent_support" ||
		revoked.Freshness.GenerationID != generationTwo {
		t.Fatalf("revoked answer = %#v, %v", revoked, err)
	}
	if _, err := surface.Engine().Status(ctx, principal, sourceID); !errors.Is(err, ErrQueryUnknownScope) {
		t.Fatalf("revoked status error = %v", err)
	}
	catalog, err = surface.CatalogSource(ctx)
	if err != nil || catalog.State != "revoked" || catalog.CurrentGenerationID != "" {
		t.Fatalf("revoked catalog source = %#v, %v", catalog, err)
	}
	if _, err := surface.CatalogGenerationFacts(ctx, generationTwo); err != nil {
		t.Fatalf("revoked generation facts must stay immutable: %v", err)
	}
	if err := surface.Close(); err != nil {
		t.Fatal(err)
	}

	// After a restart the revoked source restores its immutable generations, so
	// the same ask keeps the identical non-disclosing abstention.
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	surface, err = runtime.OpenLegacyQuerySurfaceWithoutEvidenceAdmission(ctx, identity, allowQueryAuthorizer, NewDeterministicQuerySynthesizer())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = surface.Close() }()
	restartedRevoked, err := surface.Engine().Answer(ctx, QueryRequest{
		QueryID: "q-8", Principal: principal, SourceID: sourceID, GenerationID: generationTwo,
		Text: "What does main.go return?", Freshness: "best_effort", IdempotencyKey: "ask-8",
	})
	if err != nil || restartedRevoked.Answer.Status != "abstained" ||
		len(restartedRevoked.Answer.DegradedReasons) != 1 ||
		restartedRevoked.Answer.DegradedReasons[0] != "absent_support" {
		t.Fatalf("restarted revoked answer = %#v, %v", restartedRevoked, err)
	}
}

func TestQuerySurfaceFailsClosedWithoutIngestionOrPorts(t *testing.T) {
	ctx := context.Background()
	config, keys := durableTestConfig(t.TempDir())
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.OpenQuerySurface(ctx, testIdentity(), allowQueryAuthorizer, NewDeterministicQuerySynthesizer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("surface without ingestion error = %v", err)
	}
	config.Ingestion = testIngestionConfig(t.TempDir())
	if _, err := runtime.OpenQuerySurface(ctx, testIdentity(), nil, NewDeterministicQuerySynthesizer(), QueryEvidenceAdmitterFunc(func(context.Context, QueryEvidenceAdmissionRequest) bool { return true })); !errors.Is(err, ErrInvalid) {
		t.Fatalf("surface without authorizer error = %v", err)
	}
	if _, err := runtime.OpenQuerySurface(ctx, testIdentity(), allowQueryAuthorizer, nil, QueryEvidenceAdmitterFunc(func(context.Context, QueryEvidenceAdmissionRequest) bool { return true })); !errors.Is(err, ErrInvalid) {
		t.Fatalf("surface without synthesizer error = %v", err)
	}
}

func allowQueryAuthorizer(_ context.Context, _ Identity, _ string, _ string) (bool, uint64, error) {
	return true, 1, nil
}

func containsQueryReason(reasons []QueryReason, want string) bool {
	for _, reason := range reasons {
		if string(reason) == want {
			return true
		}
	}
	return false
}
