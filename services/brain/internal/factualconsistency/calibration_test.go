package factualconsistency

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"testing"
)

//go:embed testdata/calibration-v1.json
var calibrationDatasetBytes []byte

type calibrationDataset struct {
	SchemaVersion string `json:"schema_version"`
	DatasetID     string `json:"dataset_id"`
	Provenance    struct {
		OfficialBenchmark bool   `json:"official_benchmark"`
		CustomerData      bool   `json:"customer_data"`
		LiveGold          bool   `json:"live_gold"`
		ScorerID          string `json:"scorer_id"`
		ScorerVersion     string `json:"scorer_version"`
	} `json:"provenance"`
	FitLoss struct {
		FalseGroundedCost   int `json:"false_grounded_cost"`
		FalseAbstentionCost int `json:"false_abstention_cost"`
	} `json:"fit_loss"`
	BinMaxima []uint32 `json:"bin_maxima"`
	Cases     []struct {
		ID           string   `json:"id"`
		Split        string   `json:"split"`
		Statement    string   `json:"statement"`
		Supports     []string `json:"supports"`
		Grounded     bool     `json:"grounded"`
		Perturbation string   `json:"perturbation"`
	} `json:"cases"`
}

type calibrationObservation struct {
	raw      uint32
	grounded bool
}

func loadCalibrationDataset(t *testing.T) calibrationDataset {
	t.Helper()
	var dataset calibrationDataset
	if err := json.Unmarshal(calibrationDatasetBytes, &dataset); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(calibrationDatasetBytes)
	if got := hex.EncodeToString(digest[:]); got != DefaultDatasetDigest {
		t.Fatalf("dataset digest=%s want=%s", got, DefaultDatasetDigest)
	}
	if dataset.SchemaVersion != "ouroboros.factual_consistency.calibration_dataset.v1" ||
		dataset.DatasetID != DefaultCalibrationID || dataset.Provenance.OfficialBenchmark ||
		dataset.Provenance.CustomerData || dataset.Provenance.LiveGold ||
		dataset.Provenance.ScorerID != "bounded_lexical_support" || dataset.Provenance.ScorerVersion != "v2" {
		t.Fatalf("invalid non-official provenance: %#v", dataset)
	}
	return dataset
}

// TestDefaultCalibrationIsFitOnlyAndHoldoutMetricsAreReproducible proves that
// fitted bins and the serving threshold use only fit rows. Holdout rows are
// consumed afterward solely to calculate the reported metrics.
func TestDefaultCalibrationIsFitOnlyAndHoldoutMetricsAreReproducible(t *testing.T) {
	dataset := loadCalibrationDataset(t)
	seen := map[string]bool{}
	var fit, holdout []calibrationObservation
	for _, item := range dataset.Cases {
		if item.ID == "" || seen[item.ID] || item.Statement == "" || len(item.Supports) == 0 || item.Perturbation == "" {
			t.Fatalf("invalid or duplicate case: %#v", item)
		}
		seen[item.ID] = true
		raw := RawPerMille(Request{Claims: []Claim{{Statement: item.Statement, Supports: item.Supports}}})
		observation := calibrationObservation{raw: raw, grounded: item.Grounded}
		switch item.Split {
		case "fit":
			fit = append(fit, observation)
		case "holdout":
			holdout = append(holdout, observation)
		default:
			t.Fatalf("case %s has unbounded split %q", item.ID, item.Split)
		}
	}
	if len(fit) != 30 || len(holdout) != 16 {
		t.Fatalf("split sizes fit=%d holdout=%d", len(fit), len(holdout))
	}

	bins := fitLaplaceBins(fit, dataset.BinMaxima)
	wantBins := DefaultCalibration().Bins
	if len(bins) != len(wantBins) {
		t.Fatalf("fit bins=%v want=%v", bins, wantBins)
	}
	for i := range bins {
		if bins[i] != wantBins[i] {
			t.Fatalf("fit bin[%d]=%v want=%v", i, bins[i], wantBins[i])
		}
	}
	threshold := selectFitThreshold(fit, bins, dataset.FitLoss.FalseGroundedCost, dataset.FitLoss.FalseAbstentionCost)
	if threshold != DefaultDecisionPerMille {
		t.Fatalf("fit threshold=%d want=%d", threshold, DefaultDecisionPerMille)
	}

	metrics := calibrationMetrics(holdout, bins, threshold)
	assertNear(t, "brier", metrics.brier, 0.10807675)
	assertNear(t, "ece", metrics.ece, 0.10975)
	assertNear(t, "ungrounded_claim_rate", metrics.ungroundedClaimRate, 0.125)
	assertNear(t, "abstention_precision", metrics.abstentionPrecision, 0.875)
	assertNear(t, "abstention_recall", metrics.abstentionRecall, 0.875)
}

func fitLaplaceBins(observations []calibrationObservation, maxima []uint32) []CalibrationBin {
	bins := make([]CalibrationBin, len(maxima))
	for i, maximum := range maxima {
		count, grounded := 0, 0
		minimum := uint32(0)
		if i > 0 {
			minimum = maxima[i-1] + 1
		}
		for _, observation := range observations {
			if observation.raw < minimum || observation.raw > maximum {
				continue
			}
			count++
			if observation.grounded {
				grounded++
			}
		}
		// Beta(1,1) posterior mean, rounded to the nearest per-mille.
		score := uint32(math.Round(float64(grounded+1) * 1000 / float64(count+2)))
		bins[i] = CalibrationBin{MaxRawPerMille: maximum, ScorePerMille: score}
	}
	return bins
}

func selectFitThreshold(observations []calibrationObservation, bins []CalibrationBin, falseGroundedCost, falseAbstentionCost int) uint32 {
	candidates := []uint32{1001}
	for _, bin := range bins {
		candidates = append(candidates, bin.ScorePerMille)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	best, bestLoss := uint32(1001), int(^uint(0)>>1)
	for _, threshold := range candidates {
		loss := 0
		for _, observation := range observations {
			score := scoreForRaw(observation.raw, bins)
			accepted := score >= threshold
			if accepted && !observation.grounded {
				loss += falseGroundedCost
			} else if !accepted && observation.grounded {
				loss += falseAbstentionCost
			}
		}
		if loss < bestLoss {
			best, bestLoss = threshold, loss
		}
	}
	return best
}

type reportedCalibrationMetrics struct {
	brier, ece, ungroundedClaimRate, abstentionPrecision, abstentionRecall float64
}

func calibrationMetrics(observations []calibrationObservation, bins []CalibrationBin, threshold uint32) reportedCalibrationMetrics {
	var result reportedCalibrationMetrics
	type bucket struct {
		count, grounded int
		score           float64
	}
	buckets := map[uint32]*bucket{}
	accepted, acceptedUngrounded := 0, 0
	abstained, abstainedUngrounded, totalUngrounded := 0, 0, 0
	for _, observation := range observations {
		scorePerMille := scoreForRaw(observation.raw, bins)
		score := float64(scorePerMille) / 1000
		label := float64(0)
		if observation.grounded {
			label = 1
		} else {
			totalUngrounded++
		}
		result.brier += (score - label) * (score - label)
		entry := buckets[scorePerMille]
		if entry == nil {
			entry = &bucket{score: score}
			buckets[scorePerMille] = entry
		}
		entry.count++
		if observation.grounded {
			entry.grounded++
		}
		if scorePerMille >= threshold {
			accepted++
			if !observation.grounded {
				acceptedUngrounded++
			}
		} else {
			abstained++
			if !observation.grounded {
				abstainedUngrounded++
			}
		}
	}
	n := float64(len(observations))
	result.brier /= n
	for _, entry := range buckets {
		result.ece += float64(entry.count) / n * math.Abs(entry.score-float64(entry.grounded)/float64(entry.count))
	}
	result.ungroundedClaimRate = float64(acceptedUngrounded) / float64(accepted)
	result.abstentionPrecision = float64(abstainedUngrounded) / float64(abstained)
	result.abstentionRecall = float64(abstainedUngrounded) / float64(totalUngrounded)
	return result
}

func scoreForRaw(raw uint32, bins []CalibrationBin) uint32 {
	for _, bin := range bins {
		if raw <= bin.MaxRawPerMille {
			return bin.ScorePerMille
		}
	}
	return bins[len(bins)-1].ScorePerMille
}

func assertNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s=%.12f want=%.12f", name, got, want)
	}
}
