package hosted

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

func TestFactualConsistencyCalibrationEvidenceReceipt(t *testing.T) {
	var schema map[string]any
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/factual-consistency-calibration-v1.schema.json", &schema, false)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["additionalProperties"] != false {
		t.Fatalf("calibration receipt schema must be draft 2020-12 and closed: %#v", schema)
	}
	var receiptValue any
	if err := json.Unmarshal(readQualityEvidenceFile(t, "docs/stages/stage-09/evidence/factual-consistency-calibration-v1.json"), &receiptValue); err != nil {
		t.Fatal(err)
	}
	if err := validateQualityJSONSchema(schema, receiptValue, "$"); err != nil {
		t.Fatalf("factual-consistency receipt violates schema: %v", err)
	}

	var receipt struct {
		Issue   int    `json:"issue"`
		Scope   string `json:"claim_scope"`
		Dataset struct {
			SHA256            string `json:"sha256"`
			OfficialBenchmark bool   `json:"official_benchmark"`
			CustomerData      bool   `json:"customer_data"`
			LiveGold          bool   `json:"live_gold"`
			FitCases          int    `json:"fit_cases"`
			HoldoutCases      int    `json:"holdout_cases"`
		} `json:"dataset"`
		Artifact struct {
			CalibrationDigest string `json:"calibration_digest"`
			DecisionPerMille  uint32 `json:"decision_per_mille"`
		} `json:"artifact"`
		Metrics struct {
			Brier               float64 `json:"brier"`
			ECE                 float64 `json:"ece"`
			UngroundedClaimRate float64 `json:"ungrounded_claim_rate"`
			AbstentionPrecision float64 `json:"abstention_precision"`
			AbstentionRecall    float64 `json:"abstention_recall"`
		} `json:"holdout_metrics"`
		Bounds struct {
			ScorerTimeoutMS             int     `json:"scorer_timeout_ms"`
			ScorerPasses                int     `json:"scorer_passes"`
			MaxScorerInvocations        int     `json:"max_scorer_invocations"`
			DefaultModelCalls           int     `json:"default_model_calls"`
			MaxOptionalRepairModelCalls int     `json:"max_optional_repair_model_calls"`
			ExternalScorerCostUSD       float64 `json:"external_scorer_cost_usd"`
		} `json:"runtime_bounds"`
		Limitations []string `json:"limitations"`
	}
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/factual-consistency-calibration-v1.json", &receipt, false)
	datasetBytes := readQualityEvidenceFile(t, "services/brain/internal/factualconsistency/testdata/calibration-v1.json")
	digest := sha256.Sum256(datasetBytes)
	if got := hex.EncodeToString(digest[:]); got != receipt.Dataset.SHA256 || got != factualconsistency.DefaultDatasetDigest {
		t.Fatalf("dataset digest got=%s receipt=%s runtime=%s", got, receipt.Dataset.SHA256, factualconsistency.DefaultDatasetDigest)
	}
	if receipt.Issue != 313 || receipt.Dataset.OfficialBenchmark || receipt.Dataset.CustomerData || receipt.Dataset.LiveGold ||
		receipt.Dataset.FitCases != 30 || receipt.Dataset.HoldoutCases != 16 ||
		receipt.Artifact.CalibrationDigest != factualconsistency.DefaultCalibration().Digest ||
		receipt.Artifact.DecisionPerMille != factualconsistency.DefaultDecisionPerMille {
		t.Fatalf("calibration identity or isolation drifted: %+v", receipt)
	}
	for name, pair := range map[string][2]float64{
		"brier":                {receipt.Metrics.Brier, 0.10807675},
		"ece":                  {receipt.Metrics.ECE, 0.10975},
		"ungrounded":           {receipt.Metrics.UngroundedClaimRate, 0.125},
		"abstention_precision": {receipt.Metrics.AbstentionPrecision, 0.875},
		"abstention_recall":    {receipt.Metrics.AbstentionRecall, 0.875},
	} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Errorf("%s=%v want=%v", name, pair[0], pair[1])
		}
	}
	if receipt.Bounds.ScorerTimeoutMS != 50 || receipt.Bounds.ScorerPasses != 1 || receipt.Bounds.MaxScorerInvocations != 3 ||
		receipt.Bounds.DefaultModelCalls != 0 || receipt.Bounds.MaxOptionalRepairModelCalls != 1 ||
		receipt.Bounds.ExternalScorerCostUSD != 0 {
		t.Fatalf("runtime cost/latency bounds drifted: %+v", receipt.Bounds)
	}
	limitations := strings.ToLower(strings.Join(receipt.Limitations, " ") + " " + receipt.Scope)
	for _, required := range []string{"not production", "synthetic", "12.5%", "not semantic entailment", "official"} {
		if !strings.Contains(limitations, required) {
			t.Fatalf("evidence omits limitation %q: %s", required, limitations)
		}
	}
}
