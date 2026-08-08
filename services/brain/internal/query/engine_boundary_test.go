package query

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// TestAnswerValidatesRequestShape rejects malformed and oversized requests
// before any authorization, retrieval, or synthesis work happens.
func TestAnswerValidatesRequestShape(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	_, currentID := generationIDs(t, corpus)
	valid := fixtureQuery("shape", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort")
	for _, mutation := range []struct {
		name   string
		mutate func(*Query)
	}{
		{"empty query text", func(q *Query) { q.Text = "" }},
		{"whitespace query text", func(q *Query) { q.Text = "   " }},
		{"oversized query text", func(q *Query) { q.Text = strings.Repeat("a", 8193) }},
		{"empty source", func(q *Query) { q.SourceID = "" }},
		{"empty generation", func(q *Query) { q.GenerationID = "" }},
		{"empty query identity", func(q *Query) { q.QueryID = "" }},
		{"empty freshness", func(q *Query) { q.Freshness = "" }},
		{"unknown freshness", func(q *Query) { q.Freshness = "eventually" }},
		{"empty idempotency key", func(q *Query) { q.IdempotencyKey = "" }},
		{"oversized idempotency key", func(q *Query) { q.IdempotencyKey = strings.Repeat("k", 513) }},
		{"empty tenant", func(q *Query) { q.Principal.Tenant = "" }},
		{"empty principal", func(q *Query) { q.Principal.Principal = "" }},
		{"empty session", func(q *Query) { q.Principal.Session = "" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			ask := valid
			mutation.mutate(&ask)
			if _, err := engine.Answer(context.Background(), ask); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Answer error = %v, want ErrInvalidInput", err)
			}
		})
	}
	t.Run("exactly maximal query text", func(t *testing.T) {
		ask := valid
		ask.Text = strings.Repeat("z", 8192)
		if _, err := engine.Answer(context.Background(), ask); err != nil {
			t.Fatalf("maximal query must be admitted: %v", err)
		}
	})
}

// TestAnswerHonorsProjectionAbsence proves an absent projection surfaces
// retrieval_unavailable, never false absence, while canonical coverage facts
// remain disclosed.
func TestAnswerHonorsProjectionAbsence(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	for _, state := range []ProjectionState{ProjectionAbsent, ProjectionRebuilding} {
		t.Run(string(state), func(t *testing.T) {
			original := corpus.snapshots[currentID]
			defer func() { corpus.snapshots[currentID] = original }()
			snapshot := original
			snapshot.Projection = ProjectionView{State: state}
			corpus.snapshots[currentID] = snapshot
			engine := newTestEngine(corpus, &stubAuthorizer{epoch: 3}, NewDeterministicSynthesizer())
			result, err := engine.Answer(context.Background(), fixtureQuery(
				"projection", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
			))
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if result.Answer.Status != StatusAbstained ||
				len(result.Answer.DegradedReasons) != 1 ||
				result.Answer.DegradedReasons[0] != ReasonRetrievalUnavailable {
				t.Fatalf("absent projection must abstain retrieval_unavailable, got %#v", result.Answer)
			}
			if result.Projection != state {
				t.Fatalf("projection state = %q, want %q", result.Projection, state)
			}
			if result.Coverage.CanonicalRevisionCount == 0 || result.Coverage.IndexedRevisionCount != 0 {
				t.Fatalf("coverage must retain canonical facts without the projection: %#v", result.Coverage)
			}
		})
	}
}

// TestAnswerCollapsesHydrationAndEmissionDenials proves denial at any
// authorization point is byte-identical to genuinely absent support.
func TestAnswerCollapsesHydrationAndEmissionDenials(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	for _, action := range []Action{ActionQuery, ActionHydrate, ActionEmit} {
		t.Run(string(action), func(t *testing.T) {
			engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1, deny: map[Action]bool{action: true}}, NewDeterministicSynthesizer())
			result, err := engine.Answer(context.Background(), fixtureQuery(
				"deny-"+string(action), currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
			))
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if result.Answer.Status != StatusAbstained ||
				len(result.Answer.DegradedReasons) != 1 ||
				result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
				t.Fatalf("denial at %s must collapse to absent_support, got %#v", action, result.Answer)
			}
		})
	}
}

// TestAnswerDropsClaimsWithUnverifiableCitations drives a hostile synthesizer
// that fabricates citation anchors; every claim must be discarded and the
// result must abstain with citation_verification_failed.
func TestAnswerDropsClaimsWithUnverifiableCitations(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	for name, synthesizer := range map[string]Synthesizer{
		"unknown evidence index": hostileCitationSynthesizer{citation: ProposedCitation{
			EvidenceIndex: 99, StartLine: 3, StartColumn: 1, EndLine: 3, EndColumn: 53,
		}},
		"range outside hydrated block": hostileCitationSynthesizer{citation: ProposedCitation{
			EvidenceIndex: 0, StartLine: 40, StartColumn: 1, EndLine: 40, EndColumn: 9,
		}},
		"backwards range": hostileCitationSynthesizer{citation: ProposedCitation{
			EvidenceIndex: 0, StartLine: 3, StartColumn: 53, EndLine: 3, EndColumn: 1,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, synthesizer)
			result, err := engine.Answer(context.Background(), fixtureQuery(
				"hostile", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
			))
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if result.Answer.Status != StatusAbstained ||
				len(result.Answer.DegradedReasons) != 1 ||
				result.Answer.DegradedReasons[0] != ReasonCitationVerificationFailed {
				t.Fatalf("fabricated citations must abstain citation_verification_failed, got %#v", result.Answer)
			}
			if result.Answer.Prose != "" {
				t.Fatalf("failed citation verification must leak no prose: %q", result.Answer.Prose)
			}
		})
	}
}

// hostileCitationSynthesizer returns one claim with one fabricated citation.
type hostileCitationSynthesizer struct{ citation ProposedCitation }

func (h hostileCitationSynthesizer) Synthesize(_ context.Context, _ SynthesisRequest) (Synthesis, error) {
	return Synthesis{
		Prose: "Fabricated claim prose.",
		Claims: []ProposedClaim{{
			Statement:          "A fabricated claim.",
			Citations:          []ProposedCitation{h.citation},
			ConfidencePerMille: 1000,
		}},
		TokenUsage: 9,
	}, nil
}

// TestAnswerRejectsHydrationIntegrityFailures corrupts hydrated bytes so the
// canonical content digest no longer matches; the evidence must be dropped as
// a citation verification failure, never served.
func TestAnswerRejectsHydrationIntegrityFailures(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot := corpus.snapshots[currentID]
	tampered := ingestion.HydratedFile{
		Revision: snapshot.Projection.Files["src/go/modify-00.go"].Revision,
		Content:  []byte("package seed\n\nfunc Anchor() string { return \"tampered\" }\n"),
	}
	snapshot.Projection.Files["src/go/modify-00.go"] = tampered
	corpus.snapshots[currentID] = snapshot
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"integrity", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained ||
		len(result.Answer.DegradedReasons) != 1 ||
		result.Answer.DegradedReasons[0] != ReasonCitationVerificationFailed {
		t.Fatalf("digest mismatch must abstain citation_verification_failed, got %#v", result.Answer)
	}
}

// TestAnswerDisclosesPartialCoverageForUnindexedFacets pins a generation whose
// canonical manifest carries a record-without-follow symlink the projection
// never indexed: the named facet surfaces partial_coverage, never false
// absence or deletion.
func TestAnswerDisclosesPartialCoverageForUnindexedFacets(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot := corpus.snapshots[currentID]
	link := ingestion.FileRevision{
		Path:          "docs/runbook.link",
		PathDigest:    testHexDigest("path:docs/runbook.link"),
		Kind:          ingestion.EntrySymlink,
		Mode:          "120000",
		SizeBytes:     16,
		BlobOID:       testGitOID("blob-link"),
		ContentDigest: testHexDigest("link-content"),
		RevisionID:    testHexDigest("revision:current:docs/runbook.link"),
	}
	snapshot.Revisions = append(snapshot.Revisions, link)
	corpus.snapshots[currentID] = snapshot
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"unindexed", currentID, "What does docs/runbook.link point at?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained {
		t.Fatalf("unindexed facet must abstain, got %#v", result.Answer)
	}
	assertReasonSet(t, result.Answer.DegradedReasons, ReasonAbsentSupport, ReasonPartialCoverage)
	if result.Coverage.CanonicalRevisionCount != result.Coverage.IndexedRevisionCount+1 {
		t.Fatalf("coverage must count the unindexed canonical revision: %#v", result.Coverage)
	}
}

// TestAnswerDisclosesPartialCoverageWhenIndexIsIncomplete proves absence over
// a partial index is never reported as plain absence.
func TestAnswerDisclosesPartialCoverageWhenIndexIsIncomplete(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot := corpus.snapshots[currentID]
	link := ingestion.FileRevision{
		Path: "docs/runbook.link", PathDigest: testHexDigest("path:docs/runbook.link"),
		Kind: ingestion.EntrySymlink, Mode: "120000", SizeBytes: 16,
		BlobOID: testGitOID("blob-link"), ContentDigest: testHexDigest("link-content"),
		RevisionID: testHexDigest("revision:current:docs/runbook.link"),
	}
	snapshot.Revisions = append(snapshot.Revisions, link)
	corpus.snapshots[currentID] = snapshot
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"absent-partial", currentID, "What does the billing service return for an overdue invoice?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	assertReasonSet(t, result.Answer.DegradedReasons, ReasonAbsentSupport, ReasonPartialCoverage)
}

// TestAnswerBoundsEvidencePacking shrinks the pack bound so one named facet is
// excluded; the surviving claim must disclose partial coverage.
func TestAnswerBoundsEvidencePacking(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	limits := DefaultLimits()
	limits.MaxEvidenceEntries = 1
	engine, err := NewEngine(Config{
		Corpus: corpus, Authorizer: &stubAuthorizer{epoch: 1},
		Synthesizer: NewDeterministicSynthesizer(), Clock: stubClock{now: testNow}, Limits: limits,
		AllowLegacyUnadmittedEvidence: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"packing", currentID,
		"Compare the anchor returned by src/go/modify-00.go with the function in src/python/modify-00.py.",
		"best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusPartial {
		t.Fatalf("bounded pack must downgrade to partial, got %#v", result.Answer)
	}
	assertReasonSet(t, result.Answer.DegradedReasons, ReasonPartialCoverage)
	if len(result.Answer.Claims) != 1 {
		t.Fatalf("exactly one packed claim must survive: %#v", result.Answer.Claims)
	}
}

// TestAnswerAbstainsWhenSynthesizerFindsNoClaim proves an empty synthesis over
// packed evidence is absent support, not a provider failure.
func TestAnswerAbstainsWhenSynthesizerFindsNoClaim(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, silentSynthesizer{})
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"silent", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained ||
		len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("silent synthesis must abstain absent_support, got %#v", result.Answer)
	}
}

type silentSynthesizer struct{}

func (silentSynthesizer) Synthesize(_ context.Context, _ SynthesisRequest) (Synthesis, error) {
	return Synthesis{}, nil
}

// TestEngineStatusComposesGetStatusFacts proves the authorized status view
// pins the current generation with coverage and projection truth.
func TestEngineStatusComposesGetStatusFacts(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 11}, NewDeterministicSynthesizer())
	status, err := engine.Status(context.Background(), Principal{
		Tenant: testTenant, Principal: testPrincipal, Session: testSession,
	}, testSourceID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.SourceID != testSourceID || status.Freshness.GenerationID != currentID ||
		status.Freshness.State != FreshnessDegraded || status.Projection != ProjectionReady {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.Freshness.ACLEpoch != 11 {
		t.Fatalf("acl epoch = %d, want 11", status.Freshness.ACLEpoch)
	}
	if status.Coverage.CanonicalRevisionCount == 0 ||
		status.Coverage.CanonicalRevisionCount != status.Coverage.IndexedRevisionCount {
		t.Fatalf("fixture corpus is fully indexed: %#v", status.Coverage)
	}
}

// TestEngineStatusCollapsesDeniedAndUnknown proves denied and unknown sources
// share one typed non-disclosing failure for the gateway error outcome.
func TestEngineStatusCollapsesDeniedAndUnknown(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1, deny: map[Action]bool{ActionQuery: true}}, NewDeterministicSynthesizer())
	_, err := engine.Status(context.Background(), Principal{
		Tenant: testTenant, Principal: testPrincipal, Session: testSession,
	}, testSourceID)
	if !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("denied Status error = %v, want ErrUnknownScope", err)
	}
	engine = newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	_, err = engine.Status(context.Background(), Principal{
		Tenant: testTenant, Principal: testPrincipal, Session: testSession,
	}, "source:absent")
	if !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("unknown Status error = %v, want ErrUnknownScope", err)
	}
}

// TestAnswerAuthorizesBeforeRetrieval proves an admission-denied query stops
// before any later funnel stage: the authorizer stub records exactly one
// checkpoint call, and the synthesizer panics if it is ever invoked.
func TestAnswerAuthorizesBeforeRetrieval(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	authorizer := &stubAuthorizer{epoch: 1, deny: map[Action]bool{ActionQuery: true, ActionHydrate: true, ActionEmit: true}}
	engine := newTestEngine(corpus, authorizer, hostileIfCalledSynthesizer{})
	if _, err := engine.Answer(context.Background(), fixtureQuery(
		"acl-first", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(authorizer.calls) != 1 || authorizer.calls[0] != ActionQuery {
		t.Fatalf("denied query must stop after admission authorization: %v", authorizer.calls)
	}
}

type hostileIfCalledSynthesizer struct{}

func (hostileIfCalledSynthesizer) Synthesize(_ context.Context, _ SynthesisRequest) (Synthesis, error) {
	panic("synthesizer must never run for a denied principal")
}

func assertReasonSet(t *testing.T, got []Reason, want ...Reason) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("reasons = %v, want %v", got, want)
	}
	remaining := make(map[Reason]int, len(want))
	for _, reason := range want {
		remaining[reason]++
	}
	for _, reason := range got {
		if remaining[reason] == 0 {
			t.Fatalf("reasons = %v, want %v", got, want)
		}
		remaining[reason]--
	}
}
