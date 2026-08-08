package projections

import (
	"fmt"
	"sort"
	"time"
)

// ReceiptOperation separates first index publication from later updates while
// retaining the v1 freshness/deletion/permission SLO dimensions.
type ReceiptOperation string

const (
	ReceiptIndex      ReceiptOperation = "index"
	ReceiptUpdate     ReceiptOperation = "update"
	ReceiptDelete     ReceiptOperation = "delete"
	ReceiptPermission ReceiptOperation = "permission_change"
)

func (o ReceiptOperation) valid() bool {
	switch o {
	case ReceiptIndex, ReceiptUpdate, ReceiptDelete, ReceiptPermission:
		return true
	default:
		return false
	}
}

// ReceiptSurfaces returns the deterministic answer-path surface order used by
// receipt verification and adversarial fixtures.
func ReceiptSurfaces() []ProjectionKind {
	return []ProjectionKind{
		ProjectionLexical,
		ProjectionDense,
		ProjectionGraph,
		ProjectionClaims,
		ProjectionCache,
		ProjectionAnswer,
	}
}

func (k ProjectionKind) receiptSurface() bool {
	for _, surface := range ReceiptSurfaces() {
		if k == surface {
			return true
		}
	}
	return false
}

// ReceiptSLO is one source-specific practical propagation budget. Index and
// update are reported separately even though both inherit the v1 freshness
// target for their underlying projection lane.
type ReceiptSLO struct {
	SourceID  string           `json:"source_id"`
	Surface   ProjectionKind   `json:"surface"`
	Operation ReceiptOperation `json:"operation"`
	TargetP50 time.Duration    `json:"target_p50"`
	TargetP95 time.Duration    `json:"target_p95"`
}

func (s ReceiptSLO) validate() error {
	if s.SourceID == "" {
		return fmt.Errorf("projections: receipt slo requires source id")
	}
	if !s.Surface.receiptSurface() {
		return fmt.Errorf("projections: unknown receipt surface %q", s.Surface)
	}
	if !s.Operation.valid() {
		return fmt.Errorf("projections: unknown receipt operation %q", s.Operation)
	}
	if s.TargetP95 <= 0 || s.TargetP50 < 0 || s.TargetP50 > s.TargetP95 {
		return fmt.Errorf("projections: invalid receipt targets for %s/%s/%s", s.SourceID, s.Surface, s.Operation)
	}
	return nil
}

func validateReceiptSLOs(slos []ReceiptSLO) error {
	seen := make(map[[3]string]bool, len(slos))
	deleteSurfaces := make(map[string]map[ProjectionKind]bool)
	for _, slo := range slos {
		if err := slo.validate(); err != nil {
			return err
		}
		key := [3]string{slo.SourceID, string(slo.Surface), string(slo.Operation)}
		if seen[key] {
			return fmt.Errorf("projections: duplicate receipt slo for %s/%s/%s", key[0], key[1], key[2])
		}
		seen[key] = true
		if slo.Operation == ReceiptDelete {
			if deleteSurfaces[slo.SourceID] == nil {
				deleteSurfaces[slo.SourceID] = make(map[ProjectionKind]bool, len(ReceiptSurfaces()))
			}
			deleteSurfaces[slo.SourceID][slo.Surface] = true
		}
	}
	for sourceID, surfaces := range deleteSurfaces {
		for _, required := range ReceiptSurfaces() {
			if !surfaces[required] {
				return fmt.Errorf("projections: incomplete delete receipt matrix for source %q: missing %s", sourceID, required)
			}
		}
	}
	return nil
}

// DefaultReceiptSLOs expands the frozen v1 targets into source-specific
// answer-path receipt budgets. Graph and claims inherit the ontology lane;
// cache and answer inherit the lexical lane. This adds no new infrastructure
// or achieved-SLO claim: it defines how existing publication receipts are
// measured end to end.
func DefaultReceiptSLOs(sourceID string) []ReceiptSLO {
	base := make(map[[2]string]SLO, len(DefaultSLOs()))
	for _, slo := range DefaultSLOs() {
		base[[2]string{string(slo.Projection), string(slo.Propagation)}] = slo
	}
	projectionForSurface := map[ProjectionKind]ProjectionKind{
		ProjectionLexical: ProjectionLexical,
		ProjectionDense:   ProjectionDense,
		ProjectionGraph:   ProjectionOntology,
		ProjectionClaims:  ProjectionOntology,
		ProjectionCache:   ProjectionLexical,
		ProjectionAnswer:  ProjectionLexical,
	}
	operations := []ReceiptOperation{ReceiptIndex, ReceiptUpdate, ReceiptDelete, ReceiptPermission}
	slos := make([]ReceiptSLO, 0, len(ReceiptSurfaces())*len(operations))
	for _, surface := range ReceiptSurfaces() {
		for _, operation := range operations {
			propagation := PropagationFreshness
			switch operation {
			case ReceiptDelete:
				propagation = PropagationDeletion
			case ReceiptPermission:
				propagation = PropagationPermission
			}
			parent := base[[2]string{string(projectionForSurface[surface]), string(propagation)}]
			slos = append(slos, ReceiptSLO{
				SourceID: sourceID, Surface: surface, Operation: operation,
				TargetP50: parent.TargetP50, TargetP95: parent.TargetP95,
			})
		}
	}
	return slos
}

// PropagationEvent is a canonical source event against which projection
// receipts are checked. GenerationAt is always present; deletion and
// permission events additionally carry their authority timestamp.
type PropagationEvent struct {
	EventID             string           `json:"event_id"`
	SourceID            string           `json:"source_id"`
	Operation           ReceiptOperation `json:"operation"`
	GenerationID        string           `json:"generation_id"`
	GenerationAt        time.Time        `json:"generation_at"`
	TombstoneAt         time.Time        `json:"tombstone_at,omitempty"`
	PermissionChangedAt time.Time        `json:"permission_changed_at,omitempty"`
	ACLEpoch            uint64           `json:"acl_epoch,omitempty"`
}

func (e PropagationEvent) occurredAt() time.Time {
	switch e.Operation {
	case ReceiptDelete:
		return e.TombstoneAt
	case ReceiptPermission:
		return e.PermissionChangedAt
	default:
		return e.GenerationAt
	}
}

func (e PropagationEvent) validate() error {
	if e.EventID == "" || e.SourceID == "" || e.GenerationID == "" || !e.Operation.valid() || e.GenerationAt.IsZero() {
		return fmt.Errorf("projections: malformed propagation event %q", e.EventID)
	}
	switch e.Operation {
	case ReceiptIndex, ReceiptUpdate:
		if !e.TombstoneAt.IsZero() || !e.PermissionChangedAt.IsZero() || e.ACLEpoch != 0 {
			return fmt.Errorf("projections: malformed generation event %q", e.EventID)
		}
	case ReceiptDelete:
		if e.TombstoneAt.IsZero() || e.TombstoneAt.Before(e.GenerationAt) || !e.PermissionChangedAt.IsZero() || e.ACLEpoch != 0 {
			return fmt.Errorf("projections: malformed tombstone event %q", e.EventID)
		}
	case ReceiptPermission:
		if e.PermissionChangedAt.IsZero() || e.PermissionChangedAt.Before(e.GenerationAt) || !e.TombstoneAt.IsZero() || e.ACLEpoch == 0 {
			return fmt.Errorf("projections: malformed permission event %q", e.EventID)
		}
	}
	return nil
}

// PropagationReceipt is a non-payload-bearing observation from one source and
// answer-path surface. Exact canonical timestamps prevent a retry or stale
// worker from claiming a different generation, tombstone, or ACL event.
type PropagationReceipt struct {
	ReceiptID           string           `json:"receipt_id"`
	EventID             string           `json:"event_id"`
	SourceID            string           `json:"source_id"`
	Surface             ProjectionKind   `json:"surface"`
	Operation           ReceiptOperation `json:"operation"`
	GenerationID        string           `json:"generation_id"`
	CurrentGenerationID string           `json:"current_generation_id"`
	GenerationAt        time.Time        `json:"generation_at"`
	TombstoneAt         time.Time        `json:"tombstone_at,omitempty"`
	PermissionChangedAt time.Time        `json:"permission_changed_at,omitempty"`
	ReflectedAt         time.Time        `json:"reflected_at"`
	ACLEpoch            uint64           `json:"acl_epoch,omitempty"`
	Attempt             int              `json:"attempt"`
	Succeeded           bool             `json:"succeeded"`
	TombstoneComplete   bool             `json:"tombstone_complete"`
}

func (r PropagationReceipt) matches(event PropagationEvent, surface ProjectionKind) bool {
	return r.EventID == event.EventID && r.SourceID == event.SourceID && r.Surface == surface && r.Operation == event.Operation
}

func (r PropagationReceipt) admissible(event PropagationEvent) bool {
	if r.ReceiptID == "" || r.Attempt < 1 || !r.Succeeded || !r.Surface.receiptSurface() ||
		r.GenerationID != event.GenerationID || r.CurrentGenerationID != event.GenerationID ||
		!r.GenerationAt.Equal(event.GenerationAt) || !r.TombstoneAt.Equal(event.TombstoneAt) ||
		!r.PermissionChangedAt.Equal(event.PermissionChangedAt) || r.ReflectedAt.IsZero() ||
		r.ReflectedAt.Before(event.occurredAt()) {
		return false
	}
	if event.Operation == ReceiptDelete && !r.TombstoneComplete {
		return false
	}
	if event.Operation == ReceiptPermission && r.ACLEpoch != event.ACLEpoch {
		return false
	}
	if event.Operation != ReceiptPermission && r.ACLEpoch != 0 {
		return false
	}
	return true
}

// ReceiptMeasurement reports source/surface/operation propagation and retry
// evidence. AdmittedEvents count canonical events once; retries never inflate
// percentiles.
type ReceiptMeasurement struct {
	SLO              ReceiptSLO    `json:"slo"`
	ExpectedEvents   int           `json:"expected_events"`
	AdmittedEvents   int           `json:"admitted_events"`
	MissingEvents    int           `json:"missing_events"`
	RejectedReceipts int           `json:"rejected_receipts"`
	RetryReceipts    int           `json:"retry_receipts"`
	P50              time.Duration `json:"p50"`
	P95              time.Duration `json:"p95"`
	Verdict          Verdict       `json:"verdict"`
}

// TombstoneReport reports completeness across every expected deletion event
// and surface. Compliance requires Complete == Expected, not merely a rounded
// percentage.
type TombstoneReport struct {
	Expected            int     `json:"expected"`
	Complete            int     `json:"complete"`
	CompletenessPercent float64 `json:"completeness_percent"`
}

// PropagationDrillReport is the deterministic result of an offline receipt
// drill. It contains no source payload or cited text.
type PropagationDrillReport struct {
	Measurements []ReceiptMeasurement `json:"measurements"`
	Tombstones   TombstoneReport      `json:"tombstones"`
}

// RunPropagationDrill verifies update/delete/ACL/retry/stale-worker and partial
// failure fixtures without touching live infrastructure.
func RunPropagationDrill(slos []ReceiptSLO, events []PropagationEvent, receipts []PropagationReceipt) (PropagationDrillReport, error) {
	if err := validateReceiptSLOs(slos); err != nil {
		return PropagationDrillReport{}, err
	}
	seenReceipt := make(map[string]struct{}, len(receipts))
	for _, receipt := range receipts {
		if receipt.ReceiptID == "" {
			return PropagationDrillReport{}, fmt.Errorf("projections: receipt identity is required")
		}
		if _, duplicate := seenReceipt[receipt.ReceiptID]; duplicate {
			return PropagationDrillReport{}, fmt.Errorf("projections: duplicate receipt identity %q", receipt.ReceiptID)
		}
		seenReceipt[receipt.ReceiptID] = struct{}{}
	}
	seenEvent := make(map[[2]string]bool, len(events))
	for _, event := range events {
		if err := event.validate(); err != nil {
			return PropagationDrillReport{}, err
		}
		key := [2]string{event.SourceID, event.EventID}
		if seenEvent[key] {
			return PropagationDrillReport{}, fmt.Errorf("projections: duplicate propagation event %q for source %q", event.EventID, event.SourceID)
		}
		seenEvent[key] = true
	}

	report := PropagationDrillReport{Measurements: make([]ReceiptMeasurement, 0, len(slos))}
	for _, slo := range slos {
		measurement := ReceiptMeasurement{SLO: slo, Verdict: VerdictUnverified}
		var lags []time.Duration
		for _, event := range events {
			if event.SourceID != slo.SourceID || event.Operation != slo.Operation {
				continue
			}
			measurement.ExpectedEvents++
			matched := 0
			var reflectedAt time.Time
			for _, receipt := range receipts {
				if !receipt.matches(event, slo.Surface) {
					continue
				}
				matched++
				if !receipt.admissible(event) {
					measurement.RejectedReceipts++
					continue
				}
				if reflectedAt.IsZero() || receipt.ReflectedAt.Before(reflectedAt) {
					reflectedAt = receipt.ReflectedAt
				}
			}
			if matched > 1 {
				measurement.RetryReceipts += matched - 1
			}
			if reflectedAt.IsZero() {
				measurement.MissingEvents++
				continue
			}
			measurement.AdmittedEvents++
			lags = append(lags, reflectedAt.Sub(event.occurredAt()))
		}
		if measurement.ExpectedEvents > 0 {
			measurement.Verdict = VerdictMissed
		}
		if len(lags) > 0 {
			sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
			measurement.P50 = percentile(lags, 50)
			measurement.P95 = percentile(lags, 95)
			if measurement.MissingEvents == 0 && measurement.P95 <= slo.TargetP95 &&
				(slo.TargetP50 == 0 || measurement.P50 <= slo.TargetP50) {
				measurement.Verdict = VerdictMet
			}
		}
		if slo.Operation == ReceiptDelete {
			report.Tombstones.Expected += measurement.ExpectedEvents
			report.Tombstones.Complete += measurement.AdmittedEvents
		}
		report.Measurements = append(report.Measurements, measurement)
	}
	if report.Tombstones.Expected > 0 {
		report.Tombstones.CompletenessPercent = 100 * float64(report.Tombstones.Complete) / float64(report.Tombstones.Expected)
	}
	return report, nil
}

// ReceiptCompliant fails closed on empty, missed, or unverified measurements,
// a missing/partial six-surface delete matrix, inconsistent tombstone totals,
// and anything below exact tombstone completeness.
func ReceiptCompliant(report PropagationDrillReport) bool {
	if len(report.Measurements) == 0 {
		return false
	}
	slos := make([]ReceiptSLO, 0, len(report.Measurements))
	sources := make(map[string]bool)
	deleteSurfaces := make(map[string]map[ProjectionKind]bool)
	expectedTombstones := 0
	completeTombstones := 0
	for _, measurement := range report.Measurements {
		if measurement.Verdict != VerdictMet {
			return false
		}
		slo := measurement.SLO
		slos = append(slos, slo)
		sources[slo.SourceID] = true
		if slo.Operation == ReceiptDelete {
			if deleteSurfaces[slo.SourceID] == nil {
				deleteSurfaces[slo.SourceID] = make(map[ProjectionKind]bool, len(ReceiptSurfaces()))
			}
			deleteSurfaces[slo.SourceID][slo.Surface] = true
			expectedTombstones += measurement.ExpectedEvents
			completeTombstones += measurement.AdmittedEvents
		}
	}
	if validateReceiptSLOs(slos) != nil {
		return false
	}
	for sourceID := range sources {
		if len(deleteSurfaces[sourceID]) != len(ReceiptSurfaces()) {
			return false
		}
	}
	if report.Tombstones.Expected != expectedTombstones || report.Tombstones.Complete != completeTombstones {
		return false
	}
	return report.Tombstones.Expected > 0 && report.Tombstones.Complete == report.Tombstones.Expected
}

// AdmissionReason is one bounded, payload-free evidence admission outcome.
type AdmissionReason string

const (
	AdmissionAllowed         AdmissionReason = "allowed"
	AdmissionInvalid         AdmissionReason = "invalid"
	AdmissionStaleGeneration AdmissionReason = "stale_generation"
	AdmissionStaleProjection AdmissionReason = "stale_projection"
	AdmissionStalePermission AdmissionReason = "stale_permission"
	AdmissionRemoved         AdmissionReason = "removed"
)

// EvidenceAdmissionRequest asks whether a generation may contribute evidence
// to claims or answers at one deterministic observation time.
type EvidenceAdmissionRequest struct {
	SourceID         string
	GenerationID     string
	ACLEpoch         uint64
	At               time.Time
	RequiredSurfaces []ProjectionKind
}

// EvidenceAdmission is fail-closed: Allowed is true only for the bounded
// AdmissionAllowed reason.
type EvidenceAdmission struct {
	Allowed bool            `json:"allowed"`
	Reason  AdmissionReason `json:"reason"`
}

// AdmitEvidence requires current, within-budget generation and ACL receipts on
// every requested answer-path surface. A canonical tombstone denies
// immediately even when projection purge receipts are incomplete.
func AdmitEvidence(slos []ReceiptSLO, events []PropagationEvent, receipts []PropagationReceipt, request EvidenceAdmissionRequest) EvidenceAdmission {
	deny := func(reason AdmissionReason) EvidenceAdmission { return EvidenceAdmission{Reason: reason} }
	if request.SourceID == "" || request.GenerationID == "" || request.At.IsZero() {
		return deny(AdmissionInvalid)
	}
	surfaces := append([]ProjectionKind(nil), request.RequiredSurfaces...)
	if len(surfaces) == 0 {
		surfaces = ReceiptSurfaces()
	}
	for _, surface := range surfaces {
		if !surface.receiptSurface() {
			return deny(AdmissionInvalid)
		}
	}
	if validateReceiptSLOs(slos) != nil {
		return deny(AdmissionInvalid)
	}

	var generation *PropagationEvent
	var permission *PropagationEvent
	for i := range events {
		event := &events[i]
		if event.SourceID != request.SourceID {
			continue
		}
		if event.validate() != nil {
			return deny(AdmissionInvalid)
		}
		if event.occurredAt().After(request.At) {
			continue
		}
		switch event.Operation {
		case ReceiptIndex, ReceiptUpdate:
			if generation == nil || event.occurredAt().After(generation.occurredAt()) ||
				event.occurredAt().Equal(generation.occurredAt()) && event.EventID > generation.EventID {
				generation = event
			}
		case ReceiptDelete:
			if event.GenerationID == request.GenerationID {
				return deny(AdmissionRemoved)
			}
		case ReceiptPermission:
			if permission == nil || event.occurredAt().After(permission.occurredAt()) ||
				event.occurredAt().Equal(permission.occurredAt()) && event.EventID > permission.EventID {
				permission = event
			}
		}
	}
	if generation == nil || generation.GenerationID != request.GenerationID {
		return deny(AdmissionStaleGeneration)
	}
	for _, surface := range surfaces {
		if !receiptWithinBudget(slos, receipts, *generation, surface, request.At) {
			return deny(AdmissionStaleProjection)
		}
	}
	if permission != nil {
		if permission.GenerationID != request.GenerationID || request.ACLEpoch < permission.ACLEpoch {
			return deny(AdmissionStalePermission)
		}
		for _, surface := range surfaces {
			if !receiptWithinBudget(slos, receipts, *permission, surface, request.At) {
				return deny(AdmissionStalePermission)
			}
		}
	}
	return EvidenceAdmission{Allowed: true, Reason: AdmissionAllowed}
}

func receiptWithinBudget(slos []ReceiptSLO, receipts []PropagationReceipt, event PropagationEvent, surface ProjectionKind, at time.Time) bool {
	var budget time.Duration
	for _, slo := range slos {
		if slo.SourceID == event.SourceID && slo.Surface == surface && slo.Operation == event.Operation {
			budget = slo.TargetP95
			break
		}
	}
	if budget <= 0 {
		return false
	}
	for _, receipt := range receipts {
		if receipt.matches(event, surface) && receipt.admissible(event) && !receipt.ReflectedAt.After(at) &&
			receipt.ReflectedAt.Sub(event.occurredAt()) <= budget {
			return true
		}
	}
	return false
}
