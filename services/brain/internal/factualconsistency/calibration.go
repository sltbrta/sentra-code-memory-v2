package factualconsistency

import "fmt"

const (
	// DefaultCalibrationID identifies a repository-authored, non-official,
	// synthetic calibration population. It is not an EnterpriseRAG, product,
	// customer, or production certification population.
	DefaultCalibrationID = "fc-synthetic-nonofficial-v1"
	// DefaultDatasetDigest is SHA-256 over testdata/calibration-v1.json exactly
	// as checked in. The serving binary contains this identity, fitted bins, and
	// threshold only; it contains no labels or case text.
	DefaultDatasetDigest = "cf31569fb15ac958c8ca931b114440fdc2d98b5f3989a9a835090730dd906e09"
	// DefaultDecisionPerMille is the fit-only threshold selected under the
	// declared 3:1 false-grounded:false-abstention loss. The repository holdout
	// is used only for reporting, never for selection.
	DefaultDecisionPerMille uint32 = 778
)

var defaultCalibrationBins = []CalibrationBin{
	{MaxRawPerMille: 399, ScorePerMille: 143},
	{MaxRawPerMille: 599, ScorePerMille: 375},
	{MaxRawPerMille: 799, ScorePerMille: 778},
	{MaxRawPerMille: 1000, ScorePerMille: 857},
}

// DefaultCalibration returns a defensive copy of the pinned fitted artifact.
func DefaultCalibration() LexicalCalibration {
	bins := append([]CalibrationBin(nil), defaultCalibrationBins...)
	return LexicalCalibration{
		ID:               DefaultCalibrationID,
		DatasetDigest:    DefaultDatasetDigest,
		DecisionPerMille: DefaultDecisionPerMille,
		Digest: CalibrationArtifactDigest(
			DefaultCalibrationID, DefaultDatasetDigest, DefaultDecisionPerMille, bins,
		),
		Bins: bins,
	}
}

// NewDefaultScorer constructs the calibrated deterministic serving scorer.
func NewDefaultScorer() (*LexicalScorer, error) {
	if DefaultDatasetDigest == "DATASET_DIGEST_PENDING" {
		return nil, fmt.Errorf("factualconsistency: calibration dataset digest is not pinned")
	}
	return NewLexicalScorer(DefaultCalibration())
}

// MeetsDefaultThreshold is the only score-to-action threshold used by hosted.
// A numeric estimate never bypasses the stricter citation/atom faithfulness
// floor; it can only force repair or abstention.
func MeetsDefaultThreshold(result Result) bool {
	return result.Status == StatusScored && result.ScorePerMille >= DefaultDecisionPerMille
}
