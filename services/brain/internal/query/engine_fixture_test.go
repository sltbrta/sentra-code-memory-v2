package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestEngineAnswersFrozenGroundingCases executes all twelve frozen Stage 04
// grounding cases against the engine over the reconstructed Stage 03 corpus.
// Expected statuses, citation ranges, and degraded reasons come byte-for-byte
// from tests/fixtures/stage-04/grounding/query-cases.json.
func TestEngineAnswersFrozenGroundingCases(t *testing.T) {
	manifest := loadGroundingCases(t)
	corpus := buildFixtureCorpus(t)
	staleID, currentID := generationIDs(t, corpus)
	for _, queryCase := range manifest.Cases {
		t.Run(queryCase.CaseID, func(t *testing.T) {
			generationID := currentID
			if queryCase.PinnedGeneration == "stale" {
				generationID = staleID
			}
			authorizer := &stubAuthorizer{epoch: 7}
			var synthesizer Synthesizer = NewDeterministicSynthesizer()
			switch queryCase.Interference {
			case "none":
			case "denied_relationship":
				authorizer.deny = map[Action]bool{ActionQuery: true, ActionHydrate: true, ActionEmit: true}
			case "revoked_mid_query":
				authorizer.deny = map[Action]bool{ActionEmit: true}
			case "provider_failure":
				synthesizer = failingSynthesizer{}
			default:
				t.Fatalf("unknown interference %q", queryCase.Interference)
			}
			engine := newTestEngine(corpus, authorizer, synthesizer)
			result, err := engine.Answer(context.Background(), fixtureQuery(
				queryCase.CaseID, generationID, queryCase.Query, queryCase.Freshness,
			))
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			assertFixtureOutcome(t, queryCase, result)
			assertContractInvariants(t, result)
			assertCitationDigests(t, corpus, generationID, result)
		})
	}
}

// TestEngineAnswersFrozenGroundingCasesDeterministically proves byte-for-byte
// reproducibility: a second identical run returns a deeply equal result.
func TestEngineAnswersFrozenGroundingCasesDeterministically(t *testing.T) {
	manifest := loadGroundingCases(t)
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	for _, queryCase := range manifest.Cases {
		t.Run(queryCase.CaseID+"/replay", func(t *testing.T) {
			engine := newTestEngine(corpus, &stubAuthorizer{epoch: 7}, NewDeterministicSynthesizer())
			ask := func() Result {
				result, err := engine.Answer(context.Background(), fixtureQuery(
					queryCase.CaseID, currentID, queryCase.Query, queryCase.Freshness,
				))
				if err != nil {
					t.Fatalf("Answer: %v", err)
				}
				return result
			}
			first, second := ask(), ask()
			if fmt.Sprintf("%#v", first.Answer) != fmt.Sprintf("%#v", second.Answer) {
				t.Fatalf("non-deterministic answer:\nfirst:  %#v\nsecond: %#v", first.Answer, second.Answer)
			}
		})
	}
}

func generationIDs(t *testing.T, corpus *fixtureCorpus) (staleID, currentID string) {
	t.Helper()
	current, err := corpus.CurrentGeneration(context.Background(), testSourceID)
	if err != nil {
		t.Fatalf("CurrentGeneration: %v", err)
	}
	for generationID, snapshot := range corpus.snapshots {
		if generationID != current.GenerationID {
			staleID = generationID
		}
		if snapshot.Sequence == 0 {
			t.Fatalf("snapshot %q missing sequence", generationID)
		}
	}
	if staleID == "" || current.GenerationID == "" {
		t.Fatalf("fixture corpus must hold two generations: stale=%q current=%q", staleID, current.GenerationID)
	}
	return staleID, current.GenerationID
}

func assertFixtureOutcome(t *testing.T, queryCase groundingCase, result Result) {
	t.Helper()
	if string(result.Answer.Status) != queryCase.ExpectedStatus {
		t.Fatalf("status = %q, want %q (answer: %#v)", result.Answer.Status, queryCase.ExpectedStatus, result.Answer)
	}
	if len(result.Answer.DegradedReasons) != len(queryCase.ExpectedReasons) {
		t.Fatalf("reasons = %v, want %v", result.Answer.DegradedReasons, queryCase.ExpectedReasons)
	}
	for index, want := range queryCase.ExpectedReasons {
		if string(result.Answer.DegradedReasons[index]) != want {
			t.Fatalf("reason %d = %q, want %q (all: %v)", index, result.Answer.DegradedReasons[index], want, result.Answer.DegradedReasons)
		}
	}
	citations := flattenCitations(result.Answer)
	if len(citations) != len(queryCase.ExpectedCitations) {
		t.Fatalf("citations = %#v, want %d at %#v", citations, len(queryCase.ExpectedCitations), queryCase.ExpectedCitations)
	}
	for index, want := range queryCase.ExpectedCitations {
		got := citations[index]
		if got.Path != want.Path ||
			got.StartLine != uint32(want.StartLine) || got.StartColumn != uint32(want.StartColumn) ||
			got.EndLine != uint32(want.EndLine) || got.EndColumn != uint32(want.EndColumn) {
			t.Fatalf("citation %d = %s:[%d:%d-%d:%d], want %s:[%d:%d-%d:%d]",
				index, got.Path, got.StartLine, got.StartColumn, got.EndLine, got.EndColumn,
				want.Path, want.StartLine, want.StartColumn, want.EndLine, want.EndColumn)
		}
	}
}

// assertContractInvariants mirrors the frozen query.proto CEL rules so engine
// output can never assemble a response the boundary would reject.
func assertContractInvariants(t *testing.T, result Result) {
	t.Helper()
	answer := result.Answer
	switch answer.Status {
	case StatusAnswered:
		if answer.Prose == "" || len(answer.Claims) == 0 || len(answer.DegradedReasons) != 0 {
			t.Fatalf("answered answer violates status consistency: %#v", answer)
		}
		if answer.FactualConsistency.Status != "scored" && answer.FactualConsistency.Status != "unknown" {
			t.Fatalf("answered factual consistency is not explicit: %#v", answer.FactualConsistency)
		}
	case StatusPartial:
		if answer.Prose == "" || len(answer.Claims) == 0 || len(answer.DegradedReasons) == 0 {
			t.Fatalf("partial answer violates status consistency: %#v", answer)
		}
		if answer.FactualConsistency.Status != "scored" && answer.FactualConsistency.Status != "unknown" {
			t.Fatalf("partial factual consistency is not explicit: %#v", answer.FactualConsistency)
		}
	case StatusAbstained:
		if answer.Prose != "" || len(answer.Claims) != 0 || len(answer.DegradedReasons) == 0 {
			t.Fatalf("abstained answer violates status consistency: %#v", answer)
		}
		if answer.FactualConsistency.Status != "abstained" || answer.FactualConsistency.ScorePerMille != 0 || answer.FactualConsistency.Provenance != nil {
			t.Fatalf("abstained factual consistency is not explicit: %#v", answer.FactualConsistency)
		}
	default:
		t.Fatalf("unknown status %q", answer.Status)
	}
	if answer.Status != StatusAbstained && answer.FactualConsistency.Status == "unknown" &&
		answer.FactualConsistency.TotalClaimCount != uint32(len(answer.Claims)) {
		t.Fatalf("unknown factual consistency claim count = %d, claims = %d", answer.FactualConsistency.TotalClaimCount, len(answer.Claims))
	}
	if len(answer.Claims) > 64 || len(answer.DegradedReasons) > 8 || len(answer.Prose) > 16384 {
		t.Fatalf("answer exceeds contract bounds: %#v", answer)
	}
	for _, reason := range answer.DegradedReasons {
		if !reason.known() {
			t.Fatalf("reason %q outside the frozen vocabulary", reason)
		}
	}
	for _, claim := range answer.Claims {
		if claim.Statement == "" || len(claim.Statement) > 4096 ||
			len(claim.Citations) == 0 || len(claim.Citations) > 16 || claim.ConfidencePerMille > 1000 {
			t.Fatalf("claim violates contract bounds: %#v", claim)
		}
		for _, citation := range claim.Citations {
			if !gitOIDShape(citation.GitOID) || len(citation.SupportingTextDigest) != 64 ||
				citation.StartLine == 0 || citation.StartColumn == 0 ||
				citation.EndLine == 0 || citation.EndColumn == 0 ||
				(citation.StartLine == citation.EndLine && citation.StartColumn >= citation.EndColumn) ||
				citation.StartLine > citation.EndLine {
				t.Fatalf("citation violates anchor or digest shape: %#v", citation)
			}
		}
	}
	if result.Coverage.IndexedRevisionCount > result.Coverage.CanonicalRevisionCount {
		t.Fatalf("indexed coverage exceeds canonical: %#v", result.Coverage)
	}
	if result.Freshness.ObservedAt.IsZero() || result.Freshness.GenerationID == "" {
		t.Fatalf("freshness disclosure incomplete: %#v", result.Freshness)
	}
}

// assertCitationDigests recomputes every supporting-text digest from the
// canonical corpus bytes, proving citations bind exact hydrated evidence.
func assertCitationDigests(t *testing.T, corpus *fixtureCorpus, generationID string, result Result) {
	t.Helper()
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, generationID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, citation := range flattenCitations(result.Answer) {
		hydrated, exists := snapshot.Projection.Files[citation.Path]
		if !exists {
			t.Fatalf("cited path %q absent from the pinned projection", citation.Path)
		}
		lines := strings.Split(string(hydrated.Content), "\n")
		if int(citation.StartLine) > len(lines) {
			t.Fatalf("citation %#v beyond hydrated content", citation)
		}
		line := lines[citation.StartLine-1]
		if int(citation.EndColumn) > len(line)+1 {
			t.Fatalf("citation %#v beyond line %q", citation, line)
		}
		want := testContentDigest([]byte(line[citation.StartColumn-1 : citation.EndColumn-1]))
		if citation.SupportingTextDigest != want {
			t.Fatalf("citation digest = %q, want %q for %q", citation.SupportingTextDigest, want, line)
		}
		if citation.GitOID != snapshot.CommitOID {
			t.Fatalf("citation Git OID = %q, want pinned commit %q", citation.GitOID, snapshot.CommitOID)
		}
		var revisionFound bool
		for _, revision := range snapshot.Revisions {
			if revision.RevisionID == citation.SourceRevisionID && revision.Path == citation.Path {
				revisionFound = true
			}
		}
		if !revisionFound {
			t.Fatalf("citation %#v names no canonical revision of the pinned generation", citation)
		}
	}
}

func flattenCitations(answer Answer) []Citation {
	var citations []Citation
	for _, claim := range answer.Claims {
		citations = append(citations, claim.Citations...)
	}
	return citations
}

func gitOIDShape(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type failingSynthesizer struct{}

func (failingSynthesizer) Synthesize(_ context.Context, _ SynthesisRequest) (Synthesis, error) {
	return Synthesis{}, fmt.Errorf("%w: provider timeout", ErrSynthesisFailed)
}

// TestAnswerRejectsUnknownScope verifies the non-disclosing typed error the
// gateway maps to not_found_or_denied for unknown or revoked scopes.
func TestAnswerRejectsUnknownScope(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	engine := newTestEngine(corpus, &stubAuthorizer{epoch: 1}, NewDeterministicSynthesizer())
	_, currentID := generationIDs(t, corpus)
	for _, mutation := range []struct {
		name   string
		mutate func(*Query)
	}{
		{"unknown source", func(q *Query) { q.SourceID = "source:other" }},
		{"unknown generation", func(q *Query) { q.GenerationID = testHexDigest("unknown") }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			ask := fixtureQuery("boundary", currentID, "Which Go function in src/go/modify-00.go returns the stage marker?", "best_effort")
			mutation.mutate(&ask)
			_, err := engine.Answer(context.Background(), ask)
			if !errors.Is(err, ErrUnknownScope) {
				t.Fatalf("Answer error = %v, want ErrUnknownScope", err)
			}
		})
	}
}
