package projections

import (
	"testing"
	"time"
)

// fixtureBase is a fixed observation origin so every sample is reproducible.
var fixtureBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// currentEvidence is the admissible receipt fixture: pinned to the current
// complete generation and not tombstoned.
func currentEvidence() Evidence {
	return Evidence{GenerationID: "gen-7", CurrentGenerationID: "gen-7"}
}

// staleEvidence is the superseded-pin fixture that must be rejected.
func staleEvidence() Evidence {
	return Evidence{GenerationID: "gen-6", CurrentGenerationID: "gen-7"}
}

// tombstonedEvidence is the deleted-receipt fixture that must be rejected.
func tombstonedEvidence() Evidence {
	return Evidence{GenerationID: "gen-7", CurrentGenerationID: "gen-7", Tombstoned: true}
}

// fixtureSample builds one propagation sample with the given lag and evidence.
func fixtureSample(projection ProjectionKind, propagation PropagationKind, lag time.Duration, evidence Evidence) Sample {
	return Sample{
		Projection:  projection,
		Propagation: propagation,
		EventAt:     fixtureBase,
		ReflectedAt: fixtureBase.Add(lag),
		Evidence:    evidence,
	}
}

// fixtureSamples builds n admissible samples with lags lag(1..n).
func fixtureSamples(projection ProjectionKind, propagation PropagationKind, n int, lag func(i int) time.Duration) []Sample {
	samples := make([]Sample, 0, n)
	for i := 1; i <= n; i++ {
		samples = append(samples, fixtureSample(projection, propagation, lag(i), currentEvidence()))
	}
	return samples
}

func measurementFor(t *testing.T, measurements []Measurement, projection ProjectionKind, propagation PropagationKind) Measurement {
	t.Helper()
	for _, m := range measurements {
		if m.SLO.Projection == projection && m.SLO.Propagation == propagation {
			return m
		}
	}
	t.Fatalf("no measurement for %s/%s", projection, propagation)
	return Measurement{}
}

// TestDefaultSLOsMatchSpec locks the machine-readable defaults to the table
// in docs/specs/brain/PROJECTION-SLOS.md: full 3x3 coverage with the exact
// documented targets.
func TestDefaultSLOsMatchSpec(t *testing.T) {
	t.Parallel()
	want := map[[2]string][2]time.Duration{
		{"lexical", "freshness"}:          {10 * time.Second, 60 * time.Second},
		{"lexical", "deletion"}:           {0, 60 * time.Second},
		{"lexical", "permission_change"}:  {0, 5 * time.Second},
		{"ontology", "freshness"}:         {5 * time.Minute, 20 * time.Minute},
		{"ontology", "deletion"}:          {0, 20 * time.Minute},
		{"ontology", "permission_change"}: {0, 5 * time.Second},
		{"dense", "freshness"}:            {5 * time.Minute, 20 * time.Minute},
		{"dense", "deletion"}:             {0, 20 * time.Minute},
		{"dense", "permission_change"}:    {0, 5 * time.Second},
	}
	slos := DefaultSLOs()
	if len(slos) != len(want) {
		t.Fatalf("DefaultSLOs = %d entries, want %d", len(slos), len(want))
	}
	for _, slo := range slos {
		targets, ok := want[[2]string{string(slo.Projection), string(slo.Propagation)}]
		if !ok {
			t.Fatalf("undocumented slo %s/%s", slo.Projection, slo.Propagation)
		}
		if slo.TargetP50 != targets[0] || slo.TargetP95 != targets[1] {
			t.Fatalf("%s/%s targets = %v/%v, want %v/%v",
				slo.Projection, slo.Propagation, slo.TargetP50, slo.TargetP95, targets[0], targets[1])
		}
	}
	if _, err := Verify(slos, nil); err != nil {
		t.Fatalf("DefaultSLOs must validate: %v", err)
	}
}

func TestVerifyMetReportsPercentiles(t *testing.T) {
	t.Parallel()
	slo := SLO{Projection: ProjectionLexical, Propagation: PropagationFreshness, TargetP50: 10 * time.Second, TargetP95: 60 * time.Second}
	// Lags 1s..20s: nearest-rank p50 = 10s, p95 = 19s.
	samples := fixtureSamples(ProjectionLexical, PropagationFreshness, 20, func(i int) time.Duration {
		return time.Duration(i) * time.Second
	})
	measurements, err := Verify([]SLO{slo}, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	m := measurements[0]
	if m.Verdict != VerdictMet {
		t.Fatalf("verdict = %s, want met", m.Verdict)
	}
	if m.AdmittedSamples != 20 || m.RejectedSamples != 0 {
		t.Fatalf("admitted/rejected = %d/%d", m.AdmittedSamples, m.RejectedSamples)
	}
	if m.P50 != 10*time.Second {
		t.Fatalf("p50 = %v, want 10s", m.P50)
	}
	if m.P95 != 19*time.Second {
		t.Fatalf("p95 = %v, want 19s", m.P95)
	}
	if !Compliant(measurements) {
		t.Fatal("all-met measurements must be compliant")
	}
}

func TestVerifyMissesTailTarget(t *testing.T) {
	t.Parallel()
	slo := SLO{Projection: ProjectionDense, Propagation: PropagationDeletion, TargetP95: 20 * time.Minute}
	// 18 fast purges and one 25m straggler: nearest-rank p95 over 19 samples
	// is the maximum, 25m > 20m.
	samples := fixtureSamples(ProjectionDense, PropagationDeletion, 18, func(int) time.Duration {
		return time.Minute
	})
	samples = append(samples, fixtureSample(ProjectionDense, PropagationDeletion, 25*time.Minute, currentEvidence()))
	measurements, err := Verify([]SLO{slo}, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if measurements[0].Verdict != VerdictMissed {
		t.Fatalf("verdict = %s, want missed", measurements[0].Verdict)
	}
	if measurements[0].P95 != 25*time.Minute {
		t.Fatalf("p95 = %v, want 25m", measurements[0].P95)
	}
	if Compliant(measurements) {
		t.Fatal("missed slo must not be compliant")
	}
}

func TestVerifyMissesMedianTarget(t *testing.T) {
	t.Parallel()
	slo := SLO{Projection: ProjectionOntology, Propagation: PropagationFreshness, TargetP50: 5 * time.Minute, TargetP95: 20 * time.Minute}
	// Every lag 10m: p95 within target, p50 breaches the median bound.
	samples := fixtureSamples(ProjectionOntology, PropagationFreshness, 10, func(int) time.Duration {
		return 10 * time.Minute
	})
	measurements, err := Verify([]SLO{slo}, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if measurements[0].Verdict != VerdictMissed {
		t.Fatalf("verdict = %s, want missed", measurements[0].Verdict)
	}
}

// TestVerifyFailsClosedOnInadmissibleEvidence proves stale pins, tombstoned
// receipts, inverted timestamps, and missing timestamps can never prove an
// SLO met: with only inadmissible samples the SLO stays unverified, and
// unverified is never compliant.
func TestVerifyFailsClosedOnInadmissibleEvidence(t *testing.T) {
	t.Parallel()
	slo := SLO{Projection: ProjectionLexical, Propagation: PropagationPermission, TargetP95: 5 * time.Second}
	inverted := fixtureSample(ProjectionLexical, PropagationPermission, time.Second, currentEvidence())
	inverted.EventAt, inverted.ReflectedAt = inverted.ReflectedAt, inverted.EventAt
	zeroReflected := fixtureSample(ProjectionLexical, PropagationPermission, time.Second, currentEvidence())
	zeroReflected.ReflectedAt = time.Time{}
	unpinned := fixtureSample(ProjectionLexical, PropagationPermission, time.Second, Evidence{})
	samples := []Sample{
		fixtureSample(ProjectionLexical, PropagationPermission, time.Second, staleEvidence()),
		fixtureSample(ProjectionLexical, PropagationPermission, time.Second, tombstonedEvidence()),
		inverted,
		zeroReflected,
		unpinned,
	}
	measurements, err := Verify([]SLO{slo}, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	m := measurements[0]
	if m.Verdict != VerdictUnverified {
		t.Fatalf("verdict = %s, want unverified", m.Verdict)
	}
	if m.AdmittedSamples != 0 || m.RejectedSamples != 5 {
		t.Fatalf("admitted/rejected = %d/%d, want 0/5", m.AdmittedSamples, m.RejectedSamples)
	}
	if m.P50 != 0 || m.P95 != 0 {
		t.Fatalf("unverified percentiles = %v/%v, want zero", m.P50, m.P95)
	}
	if Compliant(measurements) {
		t.Fatal("unverified slo must not be compliant")
	}
}

// TestVerifyRejectedSamplesNeverWidenCompliance mixes admissible in-target
// samples with inadmissible out-of-target ones: the rejected samples are
// counted, excluded from percentiles, and cannot flip the verdict either way.
func TestVerifyRejectedSamplesNeverWidenCompliance(t *testing.T) {
	t.Parallel()
	slo := SLO{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP95: 20 * time.Minute}
	samples := fixtureSamples(ProjectionDense, PropagationFreshness, 5, func(int) time.Duration {
		return time.Minute
	})
	samples = append(samples, fixtureSample(ProjectionDense, PropagationFreshness, 3*time.Hour, staleEvidence()))
	measurements, err := Verify([]SLO{slo}, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	m := measurements[0]
	if m.Verdict != VerdictMet || m.AdmittedSamples != 5 || m.RejectedSamples != 1 {
		t.Fatalf("measurement = %+v", m)
	}
	if m.P95 != time.Minute {
		t.Fatalf("p95 = %v, want 1m (stale straggler excluded)", m.P95)
	}
}

func TestVerifyScopesSamplesToDimension(t *testing.T) {
	t.Parallel()
	slos := []SLO{
		{Projection: ProjectionLexical, Propagation: PropagationFreshness, TargetP95: 60 * time.Second},
		{Projection: ProjectionLexical, Propagation: PropagationDeletion, TargetP95: 60 * time.Second},
	}
	samples := fixtureSamples(ProjectionLexical, PropagationFreshness, 3, func(int) time.Duration {
		return time.Second
	})
	// Other-dimension and other-projection samples must not leak in.
	samples = append(samples,
		fixtureSample(ProjectionLexical, PropagationDeletion, 2*time.Hour, currentEvidence()),
		fixtureSample(ProjectionDense, PropagationFreshness, 2*time.Hour, currentEvidence()),
	)
	measurements, err := Verify(slos, samples)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	freshness := measurementFor(t, measurements, ProjectionLexical, PropagationFreshness)
	if freshness.Verdict != VerdictMet || freshness.AdmittedSamples != 3 {
		t.Fatalf("freshness = %+v", freshness)
	}
	deletion := measurementFor(t, measurements, ProjectionLexical, PropagationDeletion)
	if deletion.Verdict != VerdictMissed || deletion.AdmittedSamples != 1 {
		t.Fatalf("deletion = %+v", deletion)
	}
	if Compliant(measurements) {
		t.Fatal("one missed slo must fail the report")
	}
}

func TestVerifyRejectsMalformedSLOs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		slos []SLO
	}{
		{"unknown projection", []SLO{{Projection: "graph", Propagation: PropagationFreshness, TargetP95: time.Second}}},
		{"unknown propagation", []SLO{{Projection: ProjectionDense, Propagation: "rebuild", TargetP95: time.Second}}},
		{"zero p95", []SLO{{Projection: ProjectionDense, Propagation: PropagationFreshness}}},
		{"p50 above p95", []SLO{{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP50: 2 * time.Second, TargetP95: time.Second}}},
		{"negative p50", []SLO{{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP50: -time.Second, TargetP95: time.Second}}},
		{"duplicate dimension", []SLO{
			{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP95: time.Second},
			{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP95: 2 * time.Second},
		}},
	}
	for _, tc := range cases {
		if _, err := Verify(tc.slos, nil); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

func TestCompliantFailsClosedOnEmptyReport(t *testing.T) {
	t.Parallel()
	if Compliant(nil) {
		t.Fatal("empty report must not be compliant")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	t.Parallel()
	sorted := []time.Duration{time.Second}
	if got := percentile(sorted, 50); got != time.Second {
		t.Fatalf("single p50 = %v", got)
	}
	if got := percentile(sorted, 95); got != time.Second {
		t.Fatalf("single p95 = %v", got)
	}
	two := []time.Duration{time.Second, 2 * time.Second}
	if got := percentile(two, 50); got != time.Second {
		t.Fatalf("two p50 = %v, want 1s", got)
	}
	if got := percentile(two, 95); got != 2*time.Second {
		t.Fatalf("two p95 = %v, want 2s", got)
	}
	if got := percentile(two, 0); got != time.Second {
		t.Fatalf("clamped p0 = %v, want 1s", got)
	}
	if got := percentile(two, 101); got != 2*time.Second {
		t.Fatalf("clamped p101 = %v, want 2s", got)
	}
}
