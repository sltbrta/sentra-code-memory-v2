package factualconsistency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// CalibrationBin maps an inclusive raw token-support ceiling to a calibrated
// factual-consistency estimate. Bins must cover [0,1000] monotonically.
type CalibrationBin struct {
	MaxRawPerMille uint32
	ScorePerMille  uint32
}

// LexicalCalibration is an immutable, caller-supplied offline calibration.
// Production composition must supply a calibration artifact appropriate to
// its own scorer population; this package ships no universal calibration.
type LexicalCalibration struct {
	ID               string
	DatasetDigest    string
	DecisionPerMille uint32
	Digest           string
	Bins             []CalibrationBin
}

// LexicalScorer is a deterministic calibration harness. Its raw statistic is
// the mean fraction of unique claim tokens found in citation support. It is
// suitable for fixtures and calibration plumbing, not semantic entailment.
type LexicalScorer struct {
	calibration LexicalCalibration
}

// NewLexicalScorer validates monotonic bins and binds them to their canonical
// SHA-256 digest. A mismatched digest cannot produce a score.
func NewLexicalScorer(calibration LexicalCalibration) (*LexicalScorer, error) {
	if calibration.ID == "" || len(calibration.ID) > 128 ||
		len(calibration.DatasetDigest) != 64 || calibration.DecisionPerMille > 1000 ||
		len(calibration.Bins) == 0 {
		return nil, fmt.Errorf("factualconsistency: invalid calibration")
	}
	previousMax := uint32(0)
	previousScore := uint32(0)
	for index, bin := range calibration.Bins {
		if bin.MaxRawPerMille > 1000 || bin.ScorePerMille > 1000 ||
			(index > 0 && bin.MaxRawPerMille <= previousMax) ||
			(index > 0 && bin.ScorePerMille < previousScore) {
			return nil, fmt.Errorf("factualconsistency: invalid calibration bins")
		}
		previousMax, previousScore = bin.MaxRawPerMille, bin.ScorePerMille
	}
	if calibration.Bins[len(calibration.Bins)-1].MaxRawPerMille != 1000 ||
		calibration.Digest != CalibrationArtifactDigest(
			calibration.ID, calibration.DatasetDigest, calibration.DecisionPerMille, calibration.Bins,
		) {
		return nil, fmt.Errorf("factualconsistency: calibration digest mismatch")
	}
	copyCalibration := calibration
	copyCalibration.Bins = append([]CalibrationBin(nil), calibration.Bins...)
	return &LexicalScorer{calibration: copyCalibration}, nil
}

// CalibrationArtifactDigest binds the fitted bins and serving threshold to the
// exact non-official dataset bytes used to derive them. Runtime scoring embeds
// this digest and the fitted artifact only; labels and benchmark gold are not
// linked into the serving binary.
func CalibrationArtifactDigest(id, datasetDigest string, decisionPerMille uint32, bins []CalibrationBin) string {
	ordered := append([]CalibrationBin(nil), bins...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].MaxRawPerMille < ordered[j].MaxRawPerMille })
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "ouroboros.factual-consistency.lexical-calibration.v2\x00%s\x00%s\x00%d", id, datasetDigest, decisionPerMille)
	for _, bin := range ordered {
		_, _ = fmt.Fprintf(hasher, "\x00%d:%d", bin.MaxRawPerMille, bin.ScorePerMille)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// Score calculates one bounded deterministic raw statistic and applies the
// pinned calibration bins. Evaluate owns all public bounds and validation.
func (s *LexicalScorer) Score(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	rawMean := RawPerMille(request)
	score := s.calibration.Bins[len(s.calibration.Bins)-1].ScorePerMille
	for _, bin := range s.calibration.Bins {
		if rawMean <= bin.MaxRawPerMille {
			score = bin.ScorePerMille
			break
		}
	}
	count := uint32(len(request.Claims))
	return Result{
		Status: StatusScored, ScorePerMille: score,
		Provenance: &Provenance{
			ScorerID: "bounded_lexical_support", ScorerVersion: "v2",
			CalibrationID: s.calibration.ID, CalibrationDigest: s.calibration.Digest,
		},
		EvaluatedClaimCount: count, TotalClaimCount: count,
	}, nil
}

// RawPerMille returns the deterministic pre-calibration statistic. It is
// exported for the offline calibration verifier; serving callers should use
// Evaluate so malformed and over-budget inputs fail closed.
func RawPerMille(request Request) uint32 {
	if len(request.Claims) == 0 {
		return 0
	}
	var rawTotal uint64
	for _, claim := range request.Claims {
		claimTokens := tokens(claim.Statement)
		supportTokens := make(map[string]struct{})
		for _, support := range claim.Supports {
			for token := range tokens(support) {
				supportTokens[token] = struct{}{}
			}
		}
		matched := 0
		for token := range claimTokens {
			if _, found := supportTokens[token]; found {
				matched++
			}
		}
		raw := uint32(0)
		if len(claimTokens) > 0 {
			raw = uint32(matched * 1000 / len(claimTokens))
		}
		rawTotal += uint64(raw)
	}
	return uint32(rawTotal / uint64(len(request.Claims)))
}

func tokens(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsNumber(character)
	}) {
		if field != "" {
			result[field] = struct{}{}
		}
	}
	return result
}
