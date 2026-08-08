package query

import (
	"context"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// TestAnswerHydratesOnlyAfterHydrationAuthorization proves the documented
// funnel ordering: hydration authorization runs before any canonical byte or
// digest work. The hydrated bytes are tampered, so any hydration or digest
// verification would surface citation_verification_failed; a hydration-denied
// principal must instead see exactly absent_support, and the authorizer must
// record exactly the admission and hydration checkpoints.
func TestAnswerHydratesOnlyAfterHydrationAuthorization(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot := corpus.snapshots[currentID]
	tampered := ingestion.HydratedFile{
		Revision: snapshot.Projection.Files["src/go/modify-00.go"].Revision,
		Content:  []byte("package seed\n\nfunc Anchor() string { return \"tampered\" }\n"),
	}
	snapshot.Projection.Files["src/go/modify-00.go"] = tampered
	corpus.snapshots[currentID] = snapshot
	authorizer := &stubAuthorizer{epoch: 5, deny: map[Action]bool{ActionHydrate: true}}
	engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"hydrate-order", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained ||
		len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("hydration denial must collapse to absent_support without digest work: %#v", result.Answer)
	}
	if len(authorizer.calls) != 2 || authorizer.calls[0] != ActionQuery || authorizer.calls[1] != ActionHydrate {
		t.Fatalf("checkpoints = %v, want [query hydrate]", authorizer.calls)
	}
}

// TestAnswerKeepsAdmissionEpochWhenHydrationAuthorizationErrors proves an
// authorizer error at the hydration checkpoint never clobbers the disclosed
// ACL epoch with the zero value.
func TestAnswerKeepsAdmissionEpochWhenHydrationAuthorizationErrors(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	authorizer := &stubAuthorizer{epoch: 9, errOn: map[Action]bool{ActionHydrate: true}}
	engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"epoch", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained ||
		len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("authorization error must collapse to absent_support: %#v", result.Answer)
	}
	if result.Freshness.ACLEpoch != 9 {
		t.Fatalf("acl epoch = %d, want the admission epoch 9", result.Freshness.ACLEpoch)
	}
}

// TestAnswerDisclosesHydrationEpochOnSuccess proves the hydration checkpoint
// epoch replaces the admission epoch when hydration authorization succeeds.
func TestAnswerDisclosesHydrationEpochOnSuccess(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	authorizer := &epochPerActionAuthorizer{epochs: map[Action]uint64{ActionQuery: 3, ActionHydrate: 8, ActionEmit: 12}}
	engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"epoch-success", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered: %#v", result.Answer.Status, result.Answer)
	}
	if result.Freshness.ACLEpoch != 8 {
		t.Fatalf("acl epoch = %d, want the hydration epoch 8", result.Freshness.ACLEpoch)
	}
}

// epochPerActionAuthorizer returns a distinct epoch per checkpoint.
type epochPerActionAuthorizer struct{ epochs map[Action]uint64 }

func (s *epochPerActionAuthorizer) Authorize(_ context.Context, _ Principal, action Action, _ string) (Decision, error) {
	return Decision{Allowed: true, Epoch: s.epochs[action]}, nil
}

// TestAnswerDiscardsOnlyFailingClaims proves the frozen per-claim rule: one
// fabricated citation removes only its own claim, the surviving claim is
// emitted as a disclosed partial, and prose is regenerated from the surviving
// statement so no material span lacks a supported claim.
func TestAnswerDiscardsOnlyFailingClaims(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, mixedCitationSynthesizer{})
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"mixed", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusPartial {
		t.Fatalf("status = %q, want partial: %#v", result.Answer.Status, result.Answer)
	}
	if len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonCitationVerificationFailed {
		t.Fatalf("reasons = %v, want exactly [citation_verification_failed]", result.Answer.DegradedReasons)
	}
	if len(result.Answer.Claims) != 1 || result.Answer.Claims[0].Statement != "The Go function returns the stage marker." {
		t.Fatalf("claims = %#v", result.Answer.Claims)
	}
	if result.Answer.Prose != "The Go function returns the stage marker." {
		t.Fatalf("prose = %q, want only the surviving statement", result.Answer.Prose)
	}
	assertCitationDigests(t, corpus, currentID, result)
}

// mixedCitationSynthesizer returns one well-grounded claim and one claim with
// a fabricated citation index.
type mixedCitationSynthesizer struct{}

func (mixedCitationSynthesizer) Synthesize(_ context.Context, _ SynthesisRequest) (Synthesis, error) {
	return Synthesis{
		Prose: "The Go function returns the stage marker. A fabricated claim.",
		Claims: []ProposedClaim{
			{
				Statement: "The Go function returns the stage marker.", ConfidencePerMille: 800,
				Citations: []ProposedCitation{{EvidenceIndex: 0, StartLine: 3, StartColumn: 1, EndLine: 3, EndColumn: 53}},
			},
			{
				Statement: "A fabricated claim.", ConfidencePerMille: 800,
				Citations: []ProposedCitation{{EvidenceIndex: 99, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2}},
			},
		},
		TokenUsage: 12,
	}, nil
}

// TestEngineStatusDisclosesGenerationDrift proves a reconcile landing between
// the current-generation read and the snapshot read is disclosed as stale
// rather than reported as current.
func TestEngineStatusDisclosesGenerationDrift(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	drifting := &driftingCorpus{fixtureCorpus: corpus}
	engine := newTestEngine(drifting, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	status, err := engine.Status(context.Background(), Principal{
		Tenant: testTenant, Principal: testPrincipal, Session: testSession,
	}, testSourceID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Freshness.State != FreshnessStaleDisclosed {
		t.Fatalf("freshness state = %q, want stale_disclosed after drift", status.Freshness.State)
	}
	if drifting.currentCalls < 2 {
		t.Fatalf("status must re-read the current generation after the snapshot: %d reads", drifting.currentCalls)
	}
}

// driftingCorpus reports the fixture current generation once, then a newer
// generation identity to simulate a reconcile landing mid-read.
type driftingCorpus struct {
	*fixtureCorpus
	currentCalls int
}

func (d *driftingCorpus) CurrentGeneration(ctx context.Context, sourceID string) (GenerationPin, error) {
	d.currentCalls++
	if d.currentCalls > 1 {
		return GenerationPin{SourceID: sourceID, GenerationID: testHexDigest("newer-generation")}, nil
	}
	return d.fixtureCorpus.CurrentGeneration(ctx, sourceID)
}

// TestNewEngineValidatesLimitShape proves the candidate/pack limit ordering
// guard and the positive-bound validation reject misshaped limits at
// construction.
func TestNewEngineValidatesLimitShape(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	config := Config{
		Corpus: corpus, Authorizer: &stubAuthorizer{epoch: 1},
		Synthesizer: NewDeterministicSynthesizer(), Clock: stubClock{now: testNow},
		AllowLegacyUnadmittedEvidence: true,
	}
	below := DefaultLimits()
	below.MaxCandidates = below.MaxEvidenceEntries - 1
	config.Limits = below
	if _, err := NewEngine(config); err == nil {
		t.Fatal("MaxCandidates below MaxEvidenceEntries must be rejected")
	}
	exact := DefaultLimits()
	exact.MaxCandidates = exact.MaxEvidenceEntries
	config.Limits = exact
	if _, err := NewEngine(config); err != nil {
		t.Fatalf("equal candidate and pack limits must be admitted: %v", err)
	}
	zero := DefaultLimits()
	zero.MaxEvidenceEntries = 0
	config.Limits = zero
	if _, err := NewEngine(config); err == nil {
		t.Fatal("zero evidence-entry limit must be rejected")
	}
}

func TestNewEngineRequiresEvidenceAdmissionUnlessLegacyIsExplicit(t *testing.T) {
	config := Config{
		Corpus:      buildFixtureCorpus(t),
		Authorizer:  &stubAuthorizer{epoch: 1},
		Synthesizer: NewDeterministicSynthesizer(),
		Clock:       stubClock{now: testNow},
		Limits:      DefaultLimits(),
	}
	if _, err := NewEngine(config); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing EvidenceAdmitter error = %v, want ErrInvalidInput", err)
	}
	config.AllowLegacyUnadmittedEvidence = true
	if _, err := NewEngine(config); err != nil {
		t.Fatalf("explicit legacy composition must remain constructible: %v", err)
	}
}
