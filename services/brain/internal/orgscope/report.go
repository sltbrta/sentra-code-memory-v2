package orgscope

import (
	"sort"
	"time"
)

// ComplianceReport summarizes caller-recorded hermetic probes within
// EvidenceScope. Zero rates are meaningful only with non-zero probe counts;
// this is not a full issue-acceptance or production certification. NonClaims
// list important surfaces this slice does not measure.
type ComplianceReport struct {
	EvidenceScope         string        `json:"evidence_scope"`
	TenantID              string        `json:"tenant_id"`
	ProbesRun             int           `json:"probes_run"`
	UnauthorizedCitations int           `json:"unauthorized_citations"`
	LeakRate              float64       `json:"leak_rate"`
	StaleGrantProbes      int           `json:"stale_grant_probes"`
	StaleGrantHits        int           `json:"stale_grant_hits"`
	StaleGrantRate        float64       `json:"stale_grant_rate"`
	ErasureRequests       int           `json:"erasure_requests"`
	ErasureComplete       int           `json:"erasure_complete"`
	ErasureCompletionRate float64       `json:"erasure_completion_rate"`
	ErasureSLO            time.Duration `json:"erasure_slo"`
	ErasureP95            time.Duration `json:"erasure_p95"`
	ErasureSLOMet         bool          `json:"erasure_slo_met"`
	RestoreProbes         int           `json:"restore_probes"`
	RestoreCorrect        bool          `json:"restore_correct"`
	NonClaims             []string      `json:"non_claims"`
}

// ReportCard accumulates adversarial probe outcomes into a ComplianceReport.
type ReportCard struct {
	tenantID       string
	probes         int
	unauthorized   int
	staleProbes    int
	staleHits      int
	erasureReqs    int
	erasureOK      int
	erasureSLO     time.Duration
	erasureLags    []time.Duration
	restoreProbes  int
	restoreFailed  bool
	restoreChecked bool
}

// NewReportCard starts an empty compliance measurement for one tenant.
func NewReportCard(tenantID string) *ReportCard {
	return &ReportCard{tenantID: tenantID, erasureSLO: time.Minute}
}

// SetErasureSLO sets the deterministic local completion target. Non-positive
// values fail closed in the final report.
func (r *ReportCard) SetErasureSLO(target time.Duration) { r.erasureSLO = target }

// RecordProbe scores one red-team retrieval: any citation whose item id is in
// the forbidden set counts as an unauthorized citation (leak).
func (r *ReportCard) RecordProbe(citations []Citation, forbidden map[string]bool) {
	r.probes++
	for _, c := range citations {
		if forbidden[c.ItemID] {
			r.unauthorized++
		}
	}
}

// RecordStaleGrantProbe scores one post-revocation retrieval attempt; hit
// means revoked access still returned citations.
func (r *ReportCard) RecordStaleGrantProbe(hit bool) {
	r.staleProbes++
	if hit {
		r.staleHits++
	}
}

// RecordErasure scores one erasure receipt against a projection scan.
func (r *ReportCard) RecordErasure(receipt ErasureReceipt, leaks LeakReport) {
	r.erasureReqs++
	validEvidence := r.validErasureEvidence(receipt, leaks)
	if validEvidence {
		r.erasureLags = append(r.erasureLags, receipt.CompletedAt.Sub(receipt.StartedAt))
	}
	if validEvidence && receipt.Complete {
		r.erasureOK++
	}
}

func (r *ReportCard) validErasureEvidence(receipt ErasureReceipt, leaks LeakReport) bool {
	if receipt.TenantID != r.tenantID || receipt.Coverage != LocalStoreErasureCoverage ||
		receipt.Receipt.TenantID != r.tenantID || receipt.Receipt.Kind != ReceiptErasure ||
		receipt.Receipt.Subject != "store" || receipt.Receipt.Object != joinIDs(receipt.ItemIDs) ||
		len(receipt.ItemIDs) == 0 || leaks.Checked != len(receipt.ItemIDs) || len(leaks.Leaks) != 0 ||
		!equalStrings(receipt.ItemIDs, leaks.ItemIDs) ||
		receipt.StartedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) {
		return false
	}
	for _, projection := range localErasureProjections {
		if count, ok := receipt.Projections[projection]; !ok || count < 0 {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RecordRestore scores one caller-supplied local restore/rebuild resurrection
// probe. It does not attest backup provenance or operator authorization.
func (r *ReportCard) RecordRestore(correct bool) {
	r.restoreProbes++
	r.restoreChecked = true
	if !correct {
		r.restoreFailed = true
	}
}

// Build returns the final report with honest non-claims.
func (r *ReportCard) Build() ComplianceReport {
	report := ComplianceReport{
		EvidenceScope:         LocalStoreErasureCoverage,
		TenantID:              r.tenantID,
		ProbesRun:             r.probes,
		UnauthorizedCitations: r.unauthorized,
		StaleGrantProbes:      r.staleProbes,
		StaleGrantHits:        r.staleHits,
		ErasureRequests:       r.erasureReqs,
		ErasureComplete:       r.erasureOK,
		ErasureSLO:            r.erasureSLO,
		RestoreProbes:         r.restoreProbes,
		RestoreCorrect:        r.restoreChecked && !r.restoreFailed,
		NonClaims: []string{
			"live_postgres_rls",
			"openfga_cloud_tuples",
			"scim_http_endpoint",
			"kms_crypto_shred",
			"production_memory_ontology_graph_claims_integration",
			"multi_region_backup",
			"caller_owned_backup_artifact_deletion",
			"fresh_store_pre_erasure_backup_protection_without_external_erasure_ledger",
			"backup_restore_rebuild_operator_authorization",
			"audit_identifier_and_digest_deletion",
			"production_erasure_slo",
		},
	}
	if r.probes > 0 {
		report.LeakRate = float64(r.unauthorized) / float64(r.probes)
	}
	if r.staleProbes > 0 {
		report.StaleGrantRate = float64(r.staleHits) / float64(r.staleProbes)
	}
	if r.erasureReqs > 0 {
		report.ErasureCompletionRate = float64(r.erasureOK) / float64(r.erasureReqs)
	}
	if len(r.erasureLags) > 0 {
		lags := append([]time.Duration(nil), r.erasureLags...)
		sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
		report.ErasureP95 = lags[(len(lags)*95+99)/100-1]
		report.ErasureSLOMet = r.erasureSLO > 0 && report.ErasureP95 <= r.erasureSLO &&
			len(lags) == r.erasureReqs && r.erasureOK == r.erasureReqs
	}
	return report
}
