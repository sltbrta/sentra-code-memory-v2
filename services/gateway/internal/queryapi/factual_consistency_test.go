package queryapi

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

func TestFactualConsistencyContractDistinguishesScoreAbstentionAndUnknown(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	scored := &contractsv1.FactualConsistencyScore{
		Status:              contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_SCORED,
		ScorePerMille:       0,
		EvaluatedClaimCount: 1,
		TotalClaimCount:     1,
		Provenance: &contractsv1.FactualConsistencyProvenance{
			ScorerId: "fixture", ScorerVersion: "v1", CalibrationId: "fixture-v1",
			CalibrationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("a", 64)},
		},
	}
	abstained := &contractsv1.FactualConsistencyScore{
		Status: contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_ABSTAINED,
		Reason: contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_ANSWER_ABSTAINED,
	}
	unknown := &contractsv1.FactualConsistencyScore{
		Status:          contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN,
		Reason:          contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_SCORER_UNAVAILABLE,
		TotalClaimCount: 1,
	}
	for name, message := range map[string]proto.Message{"scored zero": scored, "abstained": abstained, "unknown": unknown} {
		if err := validator.Validate(message); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}

	invalidUnknown := proto.Clone(unknown).(*contractsv1.FactualConsistencyScore)
	invalidUnknown.ScorePerMille = 1
	if err := validator.Validate(invalidUnknown); err == nil {
		t.Fatal("unknown accepted as a numeric score")
	}
	invalidScored := proto.Clone(scored).(*contractsv1.FactualConsistencyScore)
	invalidScored.Provenance = nil
	if err := validator.Validate(invalidScored); err == nil {
		t.Fatal("score without calibration provenance accepted")
	}
}
