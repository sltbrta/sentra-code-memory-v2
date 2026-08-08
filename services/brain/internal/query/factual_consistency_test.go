package query

import (
	"context"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

func TestAnswerScoresOnlyVerifiedClaimsAndPreservesCitations(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	scorer := &capturingConsistencyScorer{}
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 7}, NewDeterministicSynthesizer())
	engine.scorer = scorer

	result, err := engine.Answer(context.Background(), fixtureQuery(
		"factual-consistency", currentID,
		"Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort",
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer.Status != StatusAnswered || len(result.Answer.Claims) != 1 ||
		len(result.Answer.Claims[0].Citations) != 1 {
		t.Fatalf("grounded answer changed: %#v", result.Answer)
	}
	consistency := result.Answer.FactualConsistency
	if consistency.Status != factualconsistency.StatusScored || consistency.ScorePerMille != 875 ||
		consistency.Provenance == nil || consistency.Provenance.CalibrationID != "query-fixture-v1" {
		t.Fatalf("consistency = %#v", consistency)
	}
	if len(scorer.request.Claims) != 1 || len(scorer.request.Claims[0].Supports) != 1 ||
		!strings.Contains(scorer.request.Claims[0].Supports[0], "return") {
		t.Fatalf("scorer did not receive exact verified support: %#v", scorer.request)
	}
}

type capturingConsistencyScorer struct {
	request factualconsistency.Request
}

func (s *capturingConsistencyScorer) Score(_ context.Context, request factualconsistency.Request) (factualconsistency.Result, error) {
	s.request = request
	count := uint32(len(request.Claims))
	return factualconsistency.Result{
		Status: factualconsistency.StatusScored, ScorePerMille: 875,
		Provenance: &factualconsistency.Provenance{
			ScorerID: "test-scorer", ScorerVersion: "v1", CalibrationID: "query-fixture-v1",
			CalibrationDigest: strings.Repeat("a", 64),
		},
		EvaluatedClaimCount: count, TotalClaimCount: count,
	}, nil
}
