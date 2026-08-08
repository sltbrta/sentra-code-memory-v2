package query

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestAnswerDeniedIsByteIdenticalToGenuineAbsenceAcrossCorpusStates is the
// adversarial non-disclosure proof for admission denial: whatever the corpus
// or projection state, a denied principal's answer is byte-identical to an
// authorized principal's genuinely-absent answer, while freshness and
// coverage disclosures stay truthful.
func TestAnswerDeniedIsByteIdenticalToGenuineAbsenceAcrossCorpusStates(t *testing.T) {
	ask := func(corpus *fixtureCorpus, authorizer Authorizer, generationID, text string) Answer {
		engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
		result, err := engine.Answer(context.Background(), Query{
			QueryID:        "query-equivalence",
			Principal:      Principal{Tenant: testTenant, Principal: testPrincipal, Session: testSession},
			SourceID:       testSourceID,
			GenerationID:   generationID,
			Text:           text,
			Freshness:      FreshnessBestEffort,
			IdempotencyKey: "idempotency-equivalence",
		})
		if err != nil {
			t.Fatalf("Answer: %v", err)
		}
		return result.Answer
	}
	const answerable = "Which Go function in src/go/modify-00.go returns the stage marker?"
	denyAll := func() *stubAuthorizer {
		return &stubAuthorizer{epoch: 4, deny: map[Action]bool{ActionQuery: true, ActionHydrate: true, ActionEmit: true}}
	}
	baselineCorpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, baselineCorpus)
	baseline := ask(baselineCorpus, &stubAuthorizer{epoch: 4},
		currentID, "What does the billing service return for an overdue invoice?")
	if baseline.Status != StatusAbstained || len(baseline.DegradedReasons) != 1 ||
		baseline.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("baseline is not the genuine-absence abstention: %#v", baseline)
	}
	for name, arrange := range map[string]func(corpus *fixtureCorpus) string{
		"healthy corpus": func(corpus *fixtureCorpus) string {
			_, currentID := generationIDs(t, corpus)
			return currentID
		},
		"stale generation": func(corpus *fixtureCorpus) string {
			staleID, _ := generationIDs(t, corpus)
			return staleID
		},
		"absent projection": func(corpus *fixtureCorpus) string {
			_, currentID := generationIDs(t, corpus)
			setProjectionState(corpus, currentID, ProjectionAbsent)
			return currentID
		},
		"rebuilding projection": func(corpus *fixtureCorpus) string {
			_, currentID := generationIDs(t, corpus)
			setProjectionState(corpus, currentID, ProjectionRebuilding)
			return currentID
		},
	} {
		t.Run(name, func(t *testing.T) {
			corpus := buildFixtureCorpus(t)
			generationID := arrange(corpus)
			denied := ask(corpus, denyAll(), generationID, answerable)
			if !reflect.DeepEqual(denied, baseline) {
				t.Fatalf("denied answer = %#v, want byte-identical to genuine absence %#v", denied, baseline)
			}
		})
	}
}

func setProjectionState(corpus *fixtureCorpus, generationID string, state ProjectionState) {
	snapshot := corpus.snapshots[generationID]
	snapshot.Projection = ProjectionView{State: state}
	corpus.snapshots[generationID] = snapshot
}

// TestAnswerDeniedKeepsTruthfulDisclosures proves the denial collapse alters
// only the answer: freshness, coverage, and projection disclosures remain the
// truthful corpus facts the frozen AskSuccess shape requires.
func TestAnswerDeniedKeepsTruthfulDisclosures(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	staleID, _ := generationIDs(t, corpus)
	engine := newTestEngine(corpus, &stubAuthorizer{
		epoch: 6, deny: map[Action]bool{ActionQuery: true, ActionHydrate: true, ActionEmit: true},
	}, NewDeterministicSynthesizer())
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"denied-stale", staleID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Freshness.State != FreshnessStaleDisclosed || result.Freshness.GenerationID != staleID ||
		result.Freshness.ACLEpoch != 6 || result.Coverage.CanonicalRevisionCount == 0 {
		t.Fatalf("disclosures must stay truthful under denial: %#v", result)
	}
	if len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("denial must not echo staleness in reasons: %v", result.Answer.DegradedReasons)
	}
}

// TestAnswerCollapsesRevocationBeforeEmission pins the Answer-side mid-query
// revocation contract: the emission checkpoint discards every claim and the
// answer collapses to absent_support, with exactly the three checkpoints.
func TestAnswerCollapsesRevocationBeforeEmission(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	authorizer := &stubAuthorizer{epoch: 2, deny: map[Action]bool{ActionEmit: true}}
	engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
	scorer := &capturingConsistencyScorer{}
	engine.scorer = scorer
	result, err := engine.Answer(context.Background(), fixtureQuery(
		"revoke-emit", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAbstained || len(result.Answer.Claims) != 0 ||
		len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("emission revocation must collapse to absent_support: %#v", result.Answer)
	}
	if result.Answer.FactualConsistency.Status != "abstained" ||
		result.Answer.FactualConsistency.Reason != "answer_abstained" ||
		result.Answer.FactualConsistency.Provenance != nil {
		t.Fatalf("emission revocation must discard score disclosure: %#v", result.Answer.FactualConsistency)
	}
	if len(scorer.request.Claims) == 0 {
		t.Fatal("fixture scorer did not produce the pre-revocation score")
	}
	if len(authorizer.calls) != 3 || authorizer.calls[0] != ActionQuery ||
		authorizer.calls[1] != ActionHydrate || authorizer.calls[2] != ActionEmit {
		t.Fatalf("checkpoints = %v, want [query hydrate emit]", authorizer.calls)
	}
}

// TestStatusCollapsesRevocationMidFlight proves a revocation landing between
// the corpus reads and emission collapses the whole status read to the
// non-disclosing scope failure: no generation, freshness, coverage, or
// projection metadata is emitted.
func TestStatusCollapsesRevocationMidFlight(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	for name, authorizer := range map[string]*stubAuthorizer{
		"denied at emission": {epoch: 1, deny: map[Action]bool{ActionEmit: true}},
		"error at emission":  {epoch: 1, errOn: map[Action]bool{ActionEmit: true}},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newTestEngine(corpus, authorizer, NewDeterministicSynthesizer())
			status, err := engine.Status(context.Background(), Principal{
				Tenant: testTenant, Principal: testPrincipal, Session: testSession,
			}, testSourceID)
			if !errors.Is(err, ErrUnknownScope) {
				t.Fatalf("Status error = %v, want ErrUnknownScope", err)
			}
			if status != (SourceStatus{}) {
				t.Fatalf("revoked status must emit no metadata: %#v", status)
			}
			if len(authorizer.calls) != 2 || authorizer.calls[0] != ActionQuery || authorizer.calls[1] != ActionEmit {
				t.Fatalf("checkpoints = %v, want [query emit]", authorizer.calls)
			}
		})
	}
}

// TestAnswerRejectsExtremeCitationCoordinates drives citations with
// math.MaxUint32 line and column coordinates; bounds are compared in the
// uint32/uint64 domain, so the result is a citation failure, never a panic.
func TestAnswerRejectsExtremeCitationCoordinates(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	for name, citation := range map[string]ProposedCitation{
		"max end column":   {EvidenceIndex: 0, StartLine: 3, StartColumn: 1, EndLine: 3, EndColumn: math.MaxUint32},
		"max start column": {EvidenceIndex: 0, StartLine: 3, StartColumn: math.MaxUint32 - 1, EndLine: 3, EndColumn: math.MaxUint32},
		"max end line":     {EvidenceIndex: 0, StartLine: 3, StartColumn: 1, EndLine: math.MaxUint32, EndColumn: 2},
		"max start line":   {EvidenceIndex: 0, StartLine: math.MaxUint32 - 1, StartColumn: 1, EndLine: math.MaxUint32, EndColumn: 2},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, hostileCitationSynthesizer{citation: citation})
			result, err := engine.Answer(context.Background(), fixtureQuery(
				"extreme", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
			))
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if result.Answer.Status != StatusAbstained ||
				len(result.Answer.DegradedReasons) != 1 ||
				result.Answer.DegradedReasons[0] != ReasonCitationVerificationFailed {
				t.Fatalf("extreme coordinates must abstain citation_verification_failed: %#v", result.Answer)
			}
		})
	}
}

// TestAnswerRejectsUnicodeWhitespaceQuery proves Unicode-whitespace-only
// queries (for example U+3000) are malformed input, never absent support.
func TestAnswerRejectsUnicodeWhitespaceQuery(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	ask := fixtureQuery("unicode", currentID, "　", "best_effort")
	if _, err := engine.Answer(context.Background(), ask); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("U+3000-only query error = %v, want ErrInvalidInput", err)
	}
	ask = fixtureQuery("unicode-padded", currentID, "　What does anchor return?　", "best_effort")
	if _, err := engine.Answer(context.Background(), ask); err != nil {
		t.Fatalf("padded but non-empty query must be admitted: %v", err)
	}
}

// TestStatusValidatesPrincipalShape proves Status enforces the same identity
// bounds as Query.validate on every principal field.
func TestStatusValidatesPrincipalShape(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	valid := Principal{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
	oversized := strings.Repeat("p", 513)
	for name, principal := range map[string]Principal{
		"oversized tenant":    {Tenant: oversized, Principal: testPrincipal, Session: testSession},
		"oversized principal": {Tenant: testTenant, Principal: oversized, Session: testSession},
		"oversized session":   {Tenant: testTenant, Principal: testPrincipal, Session: oversized},
		"empty tenant":        {Tenant: "", Principal: testPrincipal, Session: testSession},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := engine.Status(context.Background(), principal, testSourceID); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Status error = %v, want ErrInvalidInput", err)
			}
		})
	}
	if _, err := engine.Status(context.Background(), valid, testSourceID); err != nil {
		t.Fatalf("valid principal must be admitted: %v", err)
	}
}
