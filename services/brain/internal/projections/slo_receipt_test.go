package projections

import (
	"reflect"
	"testing"
	"time"
)

var receiptFixtureBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func receiptEvent(id string, operation ReceiptOperation, generation string, at time.Time) PropagationEvent {
	event := PropagationEvent{
		EventID:      id,
		SourceID:     "source-a",
		Operation:    operation,
		GenerationID: generation,
		GenerationAt: at,
	}
	switch operation {
	case ReceiptDelete:
		event.TombstoneAt = at.Add(time.Minute)
	case ReceiptPermission:
		event.PermissionChangedAt = at.Add(time.Minute)
		event.ACLEpoch = 2
	}
	return event
}

func successfulReceipt(event PropagationEvent, surface ProjectionKind, attempt int, lag time.Duration) PropagationReceipt {
	return PropagationReceipt{
		ReceiptID:           event.EventID + "-" + string(surface) + "-" + time.Duration(attempt).String(),
		EventID:             event.EventID,
		SourceID:            event.SourceID,
		Surface:             surface,
		Operation:           event.Operation,
		GenerationID:        event.GenerationID,
		CurrentGenerationID: event.GenerationID,
		GenerationAt:        event.GenerationAt,
		TombstoneAt:         event.TombstoneAt,
		PermissionChangedAt: event.PermissionChangedAt,
		ReflectedAt:         event.occurredAt().Add(lag),
		ACLEpoch:            event.ACLEpoch,
		Attempt:             attempt,
		Succeeded:           true,
		TombstoneComplete:   event.Operation == ReceiptDelete,
	}
}

func receiptsFor(event PropagationEvent, lag time.Duration) []PropagationReceipt {
	receipts := make([]PropagationReceipt, 0, len(ReceiptSurfaces()))
	for _, surface := range ReceiptSurfaces() {
		receipts = append(receipts, successfulReceipt(event, surface, 1, lag))
	}
	return receipts
}

func practicalFixture() ([]PropagationEvent, []PropagationReceipt) {
	index := receiptEvent("index-1", ReceiptIndex, "gen-1", receiptFixtureBase)
	update := receiptEvent("update-1", ReceiptUpdate, "gen-2", receiptFixtureBase.Add(2*time.Minute))
	permission := receiptEvent("permission-1", ReceiptPermission, "gen-2", receiptFixtureBase.Add(3*time.Minute))
	events := []PropagationEvent{index, update, permission}
	var receipts []PropagationReceipt
	for _, event := range events {
		receipts = append(receipts, receiptsFor(event, time.Second)...)
	}
	return events, receipts
}

func TestReceiptIdentityAndACLEpochValidationFailClosed(t *testing.T) {
	events, receipts := practicalFixture()
	slos := DefaultReceiptSLOs("source-a")
	if _, err := RunPropagationDrill(slos, events, append(receipts, receipts[0])); err == nil {
		t.Fatal("duplicate receipt identity must be rejected")
	}
	badPermission := append([]PropagationReceipt(nil), receipts...)
	for i := range badPermission {
		if badPermission[i].Operation == ReceiptPermission {
			badPermission[i].ACLEpoch++
		}
	}
	report, err := RunPropagationDrill(slos, events, badPermission)
	if err != nil {
		t.Fatal(err)
	}
	if got := receiptMeasurementFor(t, report, ProjectionClaims, ReceiptPermission); got.Verdict != VerdictMissed {
		t.Fatalf("mismatched ACL epoch verdict = %+v", got)
	}
	badGeneration := append([]PropagationReceipt(nil), receipts...)
	for i := range badGeneration {
		if badGeneration[i].Operation == ReceiptUpdate {
			badGeneration[i].ACLEpoch = 2
		}
	}
	if report, err := RunPropagationDrill(slos, events, badGeneration); err != nil {
		t.Fatal(err)
	} else if got := receiptMeasurementFor(t, report, ProjectionClaims, ReceiptUpdate); got.Verdict != VerdictMissed {
		t.Fatalf("non-permission ACL epoch verdict = %+v", got)
	}
}

func TestDefaultReceiptSLOsCoverEverySourceSurfaceOperation(t *testing.T) {
	t.Parallel()
	slos := DefaultReceiptSLOs("source-a")
	if len(slos) != len(ReceiptSurfaces())*4 {
		t.Fatalf("DefaultReceiptSLOs = %d, want %d", len(slos), len(ReceiptSurfaces())*4)
	}
	seen := map[[2]string]ReceiptSLO{}
	for _, slo := range slos {
		if slo.SourceID != "source-a" || !slo.Surface.receiptSurface() || !slo.Operation.valid() {
			t.Fatalf("invalid default receipt SLO: %+v", slo)
		}
		key := [2]string{string(slo.Surface), string(slo.Operation)}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate default receipt SLO: %v", key)
		}
		seen[key] = slo
	}
	for key, want := range map[[2]string][2]time.Duration{
		{"lexical", "index"}:           {10 * time.Second, 60 * time.Second},
		{"graph", "update"}:            {5 * time.Minute, 20 * time.Minute},
		{"claims", "delete"}:           {0, 20 * time.Minute},
		{"cache", "permission_change"}: {0, 5 * time.Second},
		{"answer", "update"}:           {10 * time.Second, 60 * time.Second},
	} {
		got := seen[key]
		if got.TargetP50 != want[0] || got.TargetP95 != want[1] {
			t.Fatalf("%v targets = %v/%v, want %v/%v", key, got.TargetP50, got.TargetP95, want[0], want[1])
		}
	}
}

func TestRunPropagationDrillReportsP50P95Deterministically(t *testing.T) {
	t.Parallel()
	slo := ReceiptSLO{
		SourceID: "source-a", Surface: ProjectionLexical, Operation: ReceiptUpdate,
		TargetP50: 10 * time.Second, TargetP95: 60 * time.Second,
	}
	var events []PropagationEvent
	var receipts []PropagationReceipt
	for i := 1; i <= 20; i++ {
		event := receiptEvent("update-"+time.Duration(i).String(), ReceiptUpdate, "gen-2", receiptFixtureBase)
		events = append(events, event)
		receipts = append(receipts, successfulReceipt(event, ProjectionLexical, 1, time.Duration(i)*time.Second))
	}
	first, err := RunPropagationDrill([]ReceiptSLO{slo}, events, receipts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunPropagationDrill([]ReceiptSLO{slo}, events, receipts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("drill is not deterministic:\nfirst=%+v\nsecond=%+v", first, second)
	}
	measurement := first.Measurements[0]
	if measurement.P50 != 10*time.Second || measurement.P95 != 19*time.Second || measurement.Verdict != VerdictMet {
		t.Fatalf("measurement = %+v", measurement)
	}
}

func TestRunPropagationDrillRejectsCrossSourceAndTimestampForgery(t *testing.T) {
	t.Parallel()
	event := receiptEvent("update-1", ReceiptUpdate, "gen-2", receiptFixtureBase)
	slo := ReceiptSLO{
		SourceID: "source-a", Surface: ProjectionLexical, Operation: ReceiptUpdate,
		TargetP95: time.Minute,
	}
	wrongSource := successfulReceipt(event, ProjectionLexical, 1, time.Second)
	wrongSource.SourceID = "source-b"
	forgedTimestamp := successfulReceipt(event, ProjectionLexical, 2, time.Second)
	forgedTimestamp.GenerationAt = forgedTimestamp.GenerationAt.Add(time.Second)
	report, err := RunPropagationDrill([]ReceiptSLO{slo}, []PropagationEvent{event}, []PropagationReceipt{wrongSource, forgedTimestamp})
	if err != nil {
		t.Fatal(err)
	}
	measurement := report.Measurements[0]
	if measurement.Verdict != VerdictMissed || measurement.MissingEvents != 1 || measurement.RejectedReceipts != 1 {
		t.Fatalf("forged receipt measurement = %+v", measurement)
	}
}

func TestRunPropagationDrillRequiresCompleteTombstonesOnEverySurface(t *testing.T) {
	t.Parallel()
	event := receiptEvent("delete-1", ReceiptDelete, "gen-2", receiptFixtureBase)
	slos := DefaultReceiptSLOs("source-a")
	deletionSLOs := slos[:0]
	for _, slo := range slos {
		if slo.Operation == ReceiptDelete {
			deletionSLOs = append(deletionSLOs, slo)
		}
	}
	receipts := receiptsFor(event, time.Second)
	report, err := RunPropagationDrill(deletionSLOs, []PropagationEvent{event}, receipts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tombstones.Expected != 6 || report.Tombstones.Complete != 6 || report.Tombstones.CompletenessPercent != 100 || !ReceiptCompliant(report) {
		t.Fatalf("complete tombstone report = %+v", report)
	}

	receipts[len(receipts)-1].TombstoneComplete = false
	report, err = RunPropagationDrill(deletionSLOs, []PropagationEvent{event}, receipts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tombstones.Expected != 6 || report.Tombstones.Complete != 5 || report.Tombstones.CompletenessPercent >= 100 || ReceiptCompliant(report) {
		t.Fatalf("partial tombstone report = %+v", report)
	}

	if _, err := RunPropagationDrill(deletionSLOs[:len(deletionSLOs)-1], []PropagationEvent{event}, receipts); err == nil {
		t.Fatal("partial delete SLO matrix must be rejected before reporting completeness")
	}
	partialMatrix := PropagationDrillReport{
		Tombstones: TombstoneReport{Expected: 5, Complete: 5, CompletenessPercent: 100},
	}
	for _, slo := range deletionSLOs[:len(deletionSLOs)-1] {
		partialMatrix.Measurements = append(partialMatrix.Measurements, ReceiptMeasurement{
			SLO: slo, ExpectedEvents: 1, AdmittedEvents: 1, Verdict: VerdictMet,
		})
	}
	if ReceiptCompliant(partialMatrix) {
		t.Fatalf("partial delete matrix reported compliant at 100%%: %+v", partialMatrix)
	}
}

func TestAdversarialRetryAndStaleWorkerDoNotWidenAdmission(t *testing.T) {
	t.Parallel()
	events, receipts := practicalFixture()
	update := events[1]
	failed := successfulReceipt(update, ProjectionDense, 1, time.Millisecond)
	failed.Succeeded = false
	retry := successfulReceipt(update, ProjectionDense, 2, 2*time.Second)
	stale := successfulReceipt(update, ProjectionDense, 3, time.Millisecond)
	stale.GenerationID = "gen-1"
	stale.CurrentGenerationID = "gen-2"
	for i := range receipts {
		if receipts[i].EventID == update.EventID && receipts[i].Surface == ProjectionDense {
			receipts = append(receipts[:i], receipts[i+1:]...)
			break
		}
	}
	receipts = append(receipts, failed, retry, stale)

	report, err := RunPropagationDrill(DefaultReceiptSLOs("source-a"), events, receipts)
	if err != nil {
		t.Fatal(err)
	}
	measurement := receiptMeasurementFor(t, report, ProjectionDense, ReceiptUpdate)
	if measurement.Verdict != VerdictMet || measurement.RejectedReceipts != 2 || measurement.RetryReceipts != 2 || measurement.AdmittedEvents != 1 {
		t.Fatalf("retry measurement = %+v", measurement)
	}
	if indexed := receiptMeasurementFor(t, report, ProjectionLexical, ReceiptIndex); indexed.Verdict != VerdictMet || indexed.AdmittedEvents != 1 {
		t.Fatalf("initial index drill = %+v", indexed)
	}
	admission := AdmitEvidence(DefaultReceiptSLOs("source-a"), events, receipts, EvidenceAdmissionRequest{
		SourceID: "source-a", GenerationID: "gen-2", ACLEpoch: 2,
		At: receiptFixtureBase.Add(10 * time.Minute),
	})
	if !admission.Allowed {
		t.Fatalf("recovered retry should admit current evidence: %+v", admission)
	}

	allFailed := append([]PropagationReceipt(nil), receipts...)
	for i := range allFailed {
		if allFailed[i].EventID == update.EventID && allFailed[i].Surface == ProjectionDense {
			allFailed[i].Succeeded = false
		}
	}
	failedReport, err := RunPropagationDrill(DefaultReceiptSLOs("source-a"), events, allFailed)
	if err != nil {
		t.Fatal(err)
	}
	failedMeasurement := receiptMeasurementFor(t, failedReport, ProjectionDense, ReceiptUpdate)
	if failedMeasurement.Verdict != VerdictMissed || failedMeasurement.MissingEvents != 1 ||
		failedMeasurement.RejectedReceipts != 3 || failedMeasurement.RetryReceipts != 2 {
		t.Fatalf("all-failed retry drill = %+v", failedMeasurement)
	}
	admission = AdmitEvidence(DefaultReceiptSLOs("source-a"), events, allFailed, EvidenceAdmissionRequest{
		SourceID: "source-a", GenerationID: "gen-2", ACLEpoch: 2,
		At: receiptFixtureBase.Add(10 * time.Minute),
	})
	if admission.Allowed || admission.Reason != AdmissionStaleProjection {
		t.Fatalf("all-failed retry widened evidence admission: %+v", admission)
	}
}

func TestAdversarialDeleteACLRevocationAndPartialFailureFailClosed(t *testing.T) {
	t.Parallel()
	slos := DefaultReceiptSLOs("source-a")
	events, receipts := practicalFixture()
	request := EvidenceAdmissionRequest{
		SourceID: "source-a", GenerationID: "gen-2", ACLEpoch: 2,
		At: receiptFixtureBase.Add(10 * time.Minute),
	}

	partial := append([]PropagationReceipt(nil), receipts...)
	for i, receipt := range partial {
		if receipt.EventID == "update-1" && receipt.Surface == ProjectionAnswer {
			partial = append(partial[:i], partial[i+1:]...)
			break
		}
	}
	partialReport, err := RunPropagationDrill(slos, events, partial)
	if err != nil {
		t.Fatal(err)
	}
	partialMeasurement := receiptMeasurementFor(t, partialReport, ProjectionAnswer, ReceiptUpdate)
	if partialMeasurement.Verdict != VerdictMissed || partialMeasurement.MissingEvents != 1 {
		t.Fatalf("partial projection drill = %+v", partialMeasurement)
	}
	if admission := AdmitEvidence(slos, events, partial, request); admission.Allowed || admission.Reason != AdmissionStaleProjection {
		t.Fatalf("partial projection failure admitted evidence: %+v", admission)
	}

	late := append([]PropagationReceipt(nil), receipts...)
	for i := range late {
		if late[i].EventID == "update-1" && late[i].Surface == ProjectionAnswer {
			late[i].ReflectedAt = events[1].occurredAt().Add(61 * time.Second)
		}
	}
	if admission := AdmitEvidence(slos, events, late, request); admission.Allowed || admission.Reason != AdmissionStaleProjection {
		t.Fatalf("beyond-budget projection admitted evidence: %+v", admission)
	}

	aclReceipts := append([]PropagationReceipt(nil), receipts...)
	for i, receipt := range aclReceipts {
		if receipt.EventID == "permission-1" && receipt.Surface == ProjectionClaims {
			aclReceipts = append(aclReceipts[:i], aclReceipts[i+1:]...)
			break
		}
	}
	aclReport, err := RunPropagationDrill(slos, events, aclReceipts)
	if err != nil {
		t.Fatal(err)
	}
	aclMeasurement := receiptMeasurementFor(t, aclReport, ProjectionClaims, ReceiptPermission)
	if aclMeasurement.Verdict != VerdictMissed || aclMeasurement.MissingEvents != 1 {
		t.Fatalf("ACL revocation drill = %+v", aclMeasurement)
	}
	if admission := AdmitEvidence(slos, events, aclReceipts, request); admission.Allowed || admission.Reason != AdmissionStalePermission {
		t.Fatalf("partial ACL propagation admitted evidence: %+v", admission)
	}

	deletion := receiptEvent("delete-1", ReceiptDelete, "gen-2", receiptFixtureBase.Add(4*time.Minute))
	deletedEvents := append(append([]PropagationEvent(nil), events...), deletion)
	if admission := AdmitEvidence(slos, deletedEvents, receipts, request); admission.Allowed || admission.Reason != AdmissionRemoved {
		t.Fatalf("canonical tombstone did not deny immediately: %+v", admission)
	}
}

func receiptMeasurementFor(t *testing.T, report PropagationDrillReport, surface ProjectionKind, operation ReceiptOperation) ReceiptMeasurement {
	t.Helper()
	for _, measurement := range report.Measurements {
		if measurement.SLO.Surface == surface && measurement.SLO.Operation == operation {
			return measurement
		}
	}
	t.Fatalf("missing measurement for %s/%s", surface, operation)
	return ReceiptMeasurement{}
}
