package projections

import (
	"fmt"
	"sort"
	"time"
)

// This file defines the frozen v1 propagation SLO contract for rebuildable
// projections (issue #316) and its offline verification helpers. The contract
// is documented in docs/specs/brain/PROJECTION-SLOS.md; DefaultSLOs is the
// machine-readable source of the target values and is locked by test against
// the documented table. Nothing here talks to live infrastructure: callers
// collect Samples from receipts and verify them deterministically.

// ProjectionKind names one rebuildable projection surface the brain serves.
// Projections are derived artifacts, never authority (ADR 0002 / ADR 0020).
type ProjectionKind string

const (
	// ProjectionLexical is the generation-pinned lexical/occurrence projection
	// published atomically with each complete generation.
	ProjectionLexical ProjectionKind = "lexical"
	// ProjectionOntology is the generation-scoped ontology-edge sidecar
	// persisted by GraphRepository in this package.
	ProjectionOntology ProjectionKind = "ontology"
	// ProjectionDense is the generation-scoped dense-vector sidecar persisted
	// by SQLDenseStore in this package.
	ProjectionDense ProjectionKind = "dense"
	// ProjectionGraph is the answer-path graph surface. ProjectionOntology is
	// retained as the persisted sidecar name used by the original v1 contract.
	ProjectionGraph ProjectionKind = "graph"
	// ProjectionClaims is the verified-claim materialization surface.
	ProjectionClaims ProjectionKind = "claims"
	// ProjectionCache is the generation-scoped retrieval/answer cache surface.
	ProjectionCache ProjectionKind = "cache"
	// ProjectionAnswer is the final answer/citation emission surface.
	ProjectionAnswer ProjectionKind = "answer"
)

func (k ProjectionKind) valid() bool {
	switch k {
	case ProjectionLexical, ProjectionOntology, ProjectionDense:
		return true
	default:
		return false
	}
}

// PropagationKind names one propagation dimension a projection SLO bounds.
type PropagationKind string

const (
	// PropagationFreshness measures canonical publication of a complete
	// generation to the projection verifiably serving that generation.
	PropagationFreshness PropagationKind = "freshness"
	// PropagationDeletion measures a canonical tombstone append to residual
	// derived data verifiably purged from the projection. Read-side denial is
	// immediate at authority (OURO-SEC-008) and is an invariant, not a latency
	// distribution; the SLO bounds only derived residue.
	PropagationDeletion PropagationKind = "deletion"
	// PropagationPermission measures a canonical ACL epoch bump to every
	// projection read path enforcing the new epoch. Each query reauthorizes at
	// query, hydrate, and emit (OURO-SEC-010), so the bound is the longest
	// in-flight read that could still emit under the previous epoch.
	PropagationPermission PropagationKind = "permission_change"
)

func (k PropagationKind) valid() bool {
	switch k {
	case PropagationFreshness, PropagationDeletion, PropagationPermission:
		return true
	default:
		return false
	}
}

// SLO bounds one propagation dimension of one projection. TargetP95 is
// required; TargetP50 optionally bounds the median and zero disables it.
type SLO struct {
	Projection  ProjectionKind
	Propagation PropagationKind
	TargetP50   time.Duration
	TargetP95   time.Duration
}

func (s SLO) validate() error {
	if !s.Projection.valid() {
		return fmt.Errorf("projections: unknown projection kind %q", s.Projection)
	}
	if !s.Propagation.valid() {
		return fmt.Errorf("projections: unknown propagation kind %q", s.Propagation)
	}
	if s.TargetP95 <= 0 {
		return fmt.Errorf("projections: slo %s/%s requires positive p95 target", s.Projection, s.Propagation)
	}
	if s.TargetP50 < 0 || s.TargetP50 > s.TargetP95 {
		return fmt.Errorf("projections: slo %s/%s p50 target must be within [0, p95]", s.Projection, s.Propagation)
	}
	return nil
}

// DefaultSLOs returns the frozen v1 propagation targets per projection, as
// documented in docs/specs/brain/PROJECTION-SLOS.md. Freshness targets align
// with docs/reference/performance-targets.yaml (cold exact/lexical <= 60s,
// semantic/graph <= 20m); deletion targets bound residual purge, which rides
// the same publication machinery; permission targets bound the longest
// in-flight read under the superseded ACL epoch.
func DefaultSLOs() []SLO {
	return []SLO{
		{Projection: ProjectionLexical, Propagation: PropagationFreshness, TargetP50: 10 * time.Second, TargetP95: 60 * time.Second},
		{Projection: ProjectionLexical, Propagation: PropagationDeletion, TargetP95: 60 * time.Second},
		{Projection: ProjectionLexical, Propagation: PropagationPermission, TargetP95: 5 * time.Second},
		{Projection: ProjectionOntology, Propagation: PropagationFreshness, TargetP50: 5 * time.Minute, TargetP95: 20 * time.Minute},
		{Projection: ProjectionOntology, Propagation: PropagationDeletion, TargetP95: 20 * time.Minute},
		{Projection: ProjectionOntology, Propagation: PropagationPermission, TargetP95: 5 * time.Second},
		{Projection: ProjectionDense, Propagation: PropagationFreshness, TargetP50: 5 * time.Minute, TargetP95: 20 * time.Minute},
		{Projection: ProjectionDense, Propagation: PropagationDeletion, TargetP95: 20 * time.Minute},
		{Projection: ProjectionDense, Propagation: PropagationPermission, TargetP95: 5 * time.Second},
	}
}

// Evidence is the receipt state backing one propagation sample at measurement
// time. Verification fails closed on it: a sample whose receipt is pinned to a
// superseded generation, or whose receipt record is tombstoned, can never
// prove compliance. For deletion samples the *subject* record is tombstoned by
// construction; Tombstoned here refers to the measurement receipt itself,
// which must remain readable.
type Evidence struct {
	// GenerationID is the complete generation the observation was pinned to.
	GenerationID string
	// CurrentGenerationID is the source's current complete generation at
	// observation time. A mismatch marks the sample stale.
	CurrentGenerationID string
	// Tombstoned reports whether the receipt evidence record backing this
	// sample was tombstoned when the sample was read.
	Tombstoned bool
}

// Sample is one observed propagation: the canonical authority event time and
// the time the projection verifiably reflected it.
type Sample struct {
	Projection  ProjectionKind
	Propagation PropagationKind
	// EventAt is the canonical event: generation publication, tombstone
	// append, or ACL epoch bump.
	EventAt time.Time
	// ReflectedAt is when the projection verifiably reflected the event.
	ReflectedAt time.Time
	Evidence    Evidence
}

// admissible reports whether the sample may count as compliance evidence.
// Every rejection is fail-closed: rejected samples are counted but can never
// prove an SLO met.
func (s Sample) admissible() bool {
	if s.EventAt.IsZero() || s.ReflectedAt.IsZero() || s.ReflectedAt.Before(s.EventAt) {
		return false
	}
	if s.Evidence.GenerationID == "" || s.Evidence.CurrentGenerationID == "" {
		return false
	}
	if s.Evidence.GenerationID != s.Evidence.CurrentGenerationID {
		return false
	}
	return !s.Evidence.Tombstoned
}

// Verdict is the outcome of verifying one SLO against admissible samples.
type Verdict string

const (
	// VerdictMet marks admissible samples whose percentiles are within target.
	VerdictMet Verdict = "met"
	// VerdictMissed marks admissible samples whose percentiles exceed target.
	VerdictMissed Verdict = "missed"
	// VerdictUnverified marks an SLO with zero admissible samples. Unverified
	// is never compliant: absence of evidence fails closed.
	VerdictUnverified Verdict = "unverified"
)

// Measurement reports one SLO's verification: sample admission counts, the
// observed p50/p95 propagation lags (zero when unverified), and the verdict.
type Measurement struct {
	SLO             SLO
	AdmittedSamples int
	RejectedSamples int
	P50             time.Duration
	P95             time.Duration
	Verdict         Verdict
}

// Verify checks every SLO against the samples for its (projection,
// propagation) dimension and returns one Measurement per SLO in input order.
// It rejects malformed or duplicate SLO definitions outright; sample problems
// never error, they fail closed into rejection counts and verdicts. Samples
// matching no SLO are ignored.
func Verify(slos []SLO, samples []Sample) ([]Measurement, error) {
	seen := make(map[SLO]bool, len(slos))
	for _, slo := range slos {
		if err := slo.validate(); err != nil {
			return nil, err
		}
		key := SLO{Projection: slo.Projection, Propagation: slo.Propagation}
		if seen[key] {
			return nil, fmt.Errorf("projections: duplicate slo for %s/%s", slo.Projection, slo.Propagation)
		}
		seen[key] = true
	}
	measurements := make([]Measurement, 0, len(slos))
	for _, slo := range slos {
		measurement := Measurement{SLO: slo, Verdict: VerdictUnverified}
		var lags []time.Duration
		for _, sample := range samples {
			if sample.Projection != slo.Projection || sample.Propagation != slo.Propagation {
				continue
			}
			if !sample.admissible() {
				measurement.RejectedSamples++
				continue
			}
			measurement.AdmittedSamples++
			lags = append(lags, sample.ReflectedAt.Sub(sample.EventAt))
		}
		if len(lags) > 0 {
			sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
			measurement.P50 = percentile(lags, 50)
			measurement.P95 = percentile(lags, 95)
			measurement.Verdict = VerdictMet
			if measurement.P95 > slo.TargetP95 {
				measurement.Verdict = VerdictMissed
			}
			if slo.TargetP50 > 0 && measurement.P50 > slo.TargetP50 {
				measurement.Verdict = VerdictMissed
			}
		}
		measurements = append(measurements, measurement)
	}
	return measurements, nil
}

// Compliant reports whether every measurement met its SLO. It fails closed:
// an empty measurement set, a missed SLO, or an unverified SLO is never
// compliant.
func Compliant(measurements []Measurement) bool {
	if len(measurements) == 0 {
		return false
	}
	for _, measurement := range measurements {
		if measurement.Verdict != VerdictMet {
			return false
		}
	}
	return true
}

// percentile returns the nearest-rank percentile of sorted non-empty lags.
// Percentiles outside [1, 100] are clamped explicitly for deterministic callers.
func percentile(sorted []time.Duration, p int) time.Duration {
	if p < 1 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	rank := (len(sorted)*p + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
