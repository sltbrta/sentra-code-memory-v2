package factualconsistency

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestProductShapedTradeoffEvidenceIsHeldOutAndHonest(t *testing.T) {
	fixtureBytes := readTradeoffArtifact(t, "services/brain/internal/factualconsistency/testdata/product-shaped-tradeoff-v1.json")
	var fixture struct {
		Provenance struct {
			OfficialBenchmark     bool `json:"official_benchmark"`
			CustomerData          bool `json:"customer_data"`
			LiveGold              bool `json:"live_gold"`
			UsedForFitOrThreshold bool `json:"used_for_fit_or_threshold_selection"`
		} `json:"provenance"`
		Cases []struct {
			Statement string   `json:"statement"`
			Supports  []string `json:"supports"`
			Grounded  bool     `json:"grounded"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Provenance.OfficialBenchmark || fixture.Provenance.CustomerData || fixture.Provenance.LiveGold || fixture.Provenance.UsedForFitOrThreshold || len(fixture.Cases) != 16 {
		t.Fatalf("tradeoff fixture is not isolated: %+v cases=%d", fixture.Provenance, len(fixture.Cases))
	}
	scorer, err := NewDefaultScorer()
	if err != nil {
		t.Fatal(err)
	}
	var grounded, falseAnswers, acceptedGrounded, abstainedGrounded, acceptedFalse, abstainedFalse int
	for _, item := range fixture.Cases {
		result, err := Evaluate(context.Background(), scorer, Request{Claims: []Claim{{Statement: item.Statement, Supports: item.Supports}}}, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		accepted := MeetsDefaultThreshold(result)
		if item.Grounded {
			grounded++
			if accepted {
				acceptedGrounded++
			} else {
				abstainedGrounded++
			}
		} else {
			falseAnswers++
			if accepted {
				acceptedFalse++
			} else {
				abstainedFalse++
			}
		}
	}
	if grounded != 8 || falseAnswers != 8 || acceptedGrounded != 6 || abstainedGrounded != 2 || acceptedFalse != 1 || abstainedFalse != 7 {
		t.Fatalf("tradeoff counts drifted: grounded=%d/%d false=%d/%d", acceptedGrounded, abstainedGrounded, acceptedFalse, abstainedFalse)
	}
	falseAbstentionRate := float64(abstainedGrounded) / float64(grounded)
	falseGroundedRate := float64(acceptedFalse) / float64(falseAnswers)
	if math.Abs(falseAbstentionRate-0.25) > 1e-12 || math.Abs(falseGroundedRate-0.125) > 1e-12 {
		t.Fatalf("tradeoff rates drifted: false_abstention=%v false_grounded=%v", falseAbstentionRate, falseGroundedRate)
	}
}

func TestProductShapedTradeoffReportMatchesFixtureAndSchema(t *testing.T) {
	if !tradeoffArtifactAvailable("docs/stages/stage-09/evidence/factual-consistency-tradeoff-v1.json") ||
		!tradeoffArtifactAvailable("docs/stages/stage-09/evidence/factual-consistency-tradeoff-v1.schema.json") {
		t.Skip("optional stage-09 evidence artifact is not present in this standalone checkout")
	}
	var fixture struct {
		Cases []struct {
			Statement string   `json:"statement"`
			Supports  []string `json:"supports"`
			Grounded  bool     `json:"grounded"`
		} `json:"cases"`
	}
	var report struct {
		SchemaVersion string `json:"schema_version"`
		Issue         int    `json:"issue"`
		Dataset       struct {
			Cases                 int  `json:"cases"`
			OfficialBenchmark     bool `json:"official_benchmark"`
			CustomerData          bool `json:"customer_data"`
			LiveGold              bool `json:"live_gold"`
			UsedForFitOrThreshold bool `json:"used_for_fit_or_threshold_selection"`
		} `json:"dataset"`
		Threshold int `json:"threshold_per_mille"`
		Tradeoff  struct {
			GroundedCases       int     `json:"grounded_cases"`
			FalseAnswerCases    int     `json:"false_answer_cases"`
			AcceptedGrounded    int     `json:"accepted_grounded"`
			AbstainedGrounded   int     `json:"abstained_grounded"`
			AcceptedFalse       int     `json:"accepted_false_answers"`
			AbstainedFalse      int     `json:"abstained_false_answers"`
			FalseAbstentionRate float64 `json:"false_abstention_rate"`
			FalseGroundedRate   float64 `json:"false_grounded_rate"`
		} `json:"tradeoff"`
	}
	if err := json.Unmarshal(readTradeoffArtifact(t, "services/brain/internal/factualconsistency/testdata/product-shaped-tradeoff-v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readTradeoffArtifact(t, "docs/stages/stage-09/evidence/factual-consistency-tradeoff-v1.json"), &report); err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(readTradeoffArtifact(t, "docs/stages/stage-09/evidence/factual-consistency-tradeoff-v1.schema.json"), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("tradeoff schema is not a closed object: %#v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tradeoff schema properties missing: %#v", schema)
	}
	for _, key := range []string{"schema_version", "issue", "dataset", "threshold_per_mille", "tradeoff", "limitations"} {
		if _, ok := properties[key]; !ok {
			t.Fatalf("tradeoff schema omits required property %q", key)
		}
	}
	grounded, falseAnswers, acceptedGrounded, abstainedGrounded, acceptedFalse, abstainedFalse := 0, 0, 0, 0, 0, 0
	scorer, err := NewDefaultScorer()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		result, err := Evaluate(context.Background(), scorer, Request{Claims: []Claim{{Statement: item.Statement, Supports: item.Supports}}}, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if item.Grounded {
			grounded++
			if MeetsDefaultThreshold(result) {
				acceptedGrounded++
			} else {
				abstainedGrounded++
			}
		} else {
			falseAnswers++
			if MeetsDefaultThreshold(result) {
				acceptedFalse++
			} else {
				abstainedFalse++
			}
		}
	}
	if report.SchemaVersion != "stage09.factual_consistency_tradeoff.v1" || report.Issue != 313 || report.Dataset.Cases != len(fixture.Cases) || report.Dataset.OfficialBenchmark || report.Dataset.CustomerData || report.Dataset.LiveGold || report.Dataset.UsedForFitOrThreshold || report.Threshold != 778 {
		t.Fatalf("report metadata violates its schema or provenance: %+v", report)
	}
	if report.Tradeoff.GroundedCases != grounded || report.Tradeoff.FalseAnswerCases != falseAnswers || report.Tradeoff.AcceptedGrounded != acceptedGrounded || report.Tradeoff.AbstainedGrounded != abstainedGrounded || report.Tradeoff.AcceptedFalse != acceptedFalse || report.Tradeoff.AbstainedFalse != abstainedFalse {
		t.Fatalf("report counts do not recompute from fixture: %+v", report.Tradeoff)
	}
	if math.Abs(report.Tradeoff.FalseAbstentionRate-float64(abstainedGrounded)/float64(grounded)) > 1e-12 || math.Abs(report.Tradeoff.FalseGroundedRate-float64(acceptedFalse)/float64(falseAnswers)) > 1e-12 {
		t.Fatalf("report rates do not recompute from fixture: %+v", report.Tradeoff)
	}
}

func tradeoffArtifactAvailable(relative string) bool {
	paths := []string{relative, filepath.Join("..", "..", "..", "..", relative)}
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		paths = append([]string{filepath.Join(root, os.Getenv("TEST_WORKSPACE"), relative)}, paths...)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func readTradeoffArtifact(t *testing.T, relative string) []byte {
	t.Helper()
	paths := []string{relative, filepath.Join("..", "..", "..", "..", relative)}
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		paths = append([]string{filepath.Join(root, os.Getenv("TEST_WORKSPACE"), relative)}, paths...)
	}
	for _, path := range paths {
		if encoded, err := os.ReadFile(path); err == nil {
			return encoded
		}
	}
	t.Fatalf("unable to locate tradeoff artifact %q", relative)
	return nil
}
