package factualconsistency

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testCalibration() LexicalCalibration {
	bins := []CalibrationBin{{MaxRawPerMille: 250, ScorePerMille: 100}, {MaxRawPerMille: 750, ScorePerMille: 600}, {MaxRawPerMille: 1000, ScorePerMille: 900}}
	datasetDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return LexicalCalibration{
		ID: "fixture-v1", DatasetDigest: datasetDigest, DecisionPerMille: 600,
		Digest: CalibrationArtifactDigest("fixture-v1", datasetDigest, 600, bins), Bins: bins,
	}
}

func TestEvaluateProducesCalibratedProvenance(t *testing.T) {
	scorer, err := NewLexicalScorer(testCalibration())
	if err != nil {
		t.Fatal(err)
	}
	result, err := Evaluate(context.Background(), scorer, Request{Claims: []Claim{{
		Statement: "the deployment window is fifteen minutes",
		Supports:  []string{"The deployment window is fifteen minutes."},
	}}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusScored || result.ScorePerMille != 900 ||
		result.EvaluatedClaimCount != 1 || result.TotalClaimCount != 1 || result.Provenance == nil ||
		result.Provenance.CalibrationID != "fixture-v1" || result.Provenance.CalibrationDigest == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestEvaluateUnknownAndAbstentionAreNotNumericZero(t *testing.T) {
	unknown, err := Evaluate(context.Background(), nil, Request{Claims: []Claim{{Statement: "claim", Supports: []string{"support"}}}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Status != StatusUnknown || unknown.Reason != ReasonScorerUnavailable || unknown.TotalClaimCount != 1 || unknown.Provenance != nil {
		t.Fatalf("unknown = %#v", unknown)
	}
	abstained := Abstained()
	if abstained.Status != StatusAbstained || abstained.Reason != ReasonAnswerAbstained || abstained.TotalClaimCount != 0 {
		t.Fatalf("abstained = %#v", abstained)
	}
}

func TestEvaluateBoundsAndFailuresFailClosed(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTotalBytes = 4
	request := Request{Claims: []Claim{{Statement: "claim", Supports: []string{"support"}}}}
	result, err := Evaluate(context.Background(), panicScorer{}, request, limits)
	if err != nil || result.Status != StatusUnknown || result.Reason != ReasonBudgetExceeded {
		t.Fatalf("over budget = %#v, %v", result, err)
	}
	result, err = Evaluate(context.Background(), errorScorer{}, request, DefaultLimits())
	if err != nil || result.Status != StatusUnknown || result.Reason != ReasonScorerFailed {
		t.Fatalf("failure = %#v, %v", result, err)
	}
	result, err = Evaluate(context.Background(), panicScorer{}, request, DefaultLimits())
	if err != nil || result.Status != StatusUnknown || result.Reason != ReasonScorerFailed {
		t.Fatalf("panic = %#v, %v", result, err)
	}
	result, err = Evaluate(context.Background(), invalidScorer{}, request, DefaultLimits())
	if err != nil || result.Status != StatusUnknown || result.Reason != ReasonInvalidResult {
		t.Fatalf("invalid = %#v, %v", result, err)
	}
}

func TestEvaluateTimeoutAndCallerCancellation(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = time.Millisecond
	request := Request{Claims: []Claim{{Statement: "claim", Supports: []string{"support"}}}}
	result, err := Evaluate(context.Background(), blockingScorer{}, request, limits)
	if err != nil || result.Reason != ReasonScorerFailed {
		t.Fatalf("timeout = %#v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Evaluate(ctx, nil, request, limits); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestCalibrationRejectsUnpinnedOrNonMonotonicBins(t *testing.T) {
	calibration := testCalibration()
	calibration.Digest = "0"
	if _, err := NewLexicalScorer(calibration); err == nil {
		t.Fatal("mismatched calibration digest accepted")
	}
	calibration = testCalibration()
	calibration.Bins[1].ScorePerMille = 50
	calibration.Digest = CalibrationArtifactDigest(
		calibration.ID, calibration.DatasetDigest, calibration.DecisionPerMille, calibration.Bins,
	)
	if _, err := NewLexicalScorer(calibration); err == nil {
		t.Fatal("non-monotonic calibration accepted")
	}
}

func BenchmarkDefaultScorer(b *testing.B) {
	scorer, err := NewDefaultScorer()
	if err != nil {
		b.Fatal(err)
	}
	request := Request{Claims: []Claim{{
		Statement: "Audit logs are retained for 90 days",
		Supports:  []string{"The retention policy keeps audit logs for 90 days."},
	}}}
	limits := DefaultLimits()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, scoreErr := Evaluate(context.Background(), scorer, request, limits)
		if scoreErr != nil || result.Status != StatusScored {
			b.Fatalf("result=%#v err=%v", result, scoreErr)
		}
	}
}

type panicScorer struct{}

func (panicScorer) Score(context.Context, Request) (Result, error) { panic("must not run") }

type errorScorer struct{}

func (errorScorer) Score(context.Context, Request) (Result, error) {
	return Result{}, errors.New("private")
}

type invalidScorer struct{}

func (invalidScorer) Score(context.Context, Request) (Result, error) {
	return Result{Status: StatusScored, ScorePerMille: 1001}, nil
}

type blockingScorer struct{}

func (blockingScorer) Score(ctx context.Context, _ Request) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}
