package outcomes

import "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"

// AuthorityClass mirrors the frozen evidence AuthorityClass enum for admission.
type AuthorityClass string

const (
	// AuthorityMachineObservation is the only admitted outcome class.
	AuthorityMachineObservation AuthorityClass = "machine_observation"
	// AuthorityModelProposal is always rejected for outcome admission.
	AuthorityModelProposal AuthorityClass = "model_proposal"
	// AuthorityDirectSource is not an outcome class (evidence is separate).
	AuthorityDirectSource AuthorityClass = "direct_source"
	// AuthorityDerivedSummary is not admitted as an outcome fact.
	AuthorityDerivedSummary AuthorityClass = "derived_summary"
)

// ForbiddenKeys are fields that must never appear in a sanitized outcome bundle.
var ForbiddenKeys = []string{
	"prompt",
	"prompts",
	"raw_trace",
	"raw_traces",
	"rawTrace",
	"secret",
	"secrets",
	"token",
	"password",
	"api_key",
	"apiKey",
	"authorization",
	"source_bytes",
	"sourceBytes",
	"model_proposal",
	"modelProposal",
	"completion",
	"raw_source",
}

// EvidenceRef is a sanitized observation reference (digest + opaque id only).
type EvidenceRef struct {
	// EvidenceID is an opaque evidence identity.
	EvidenceID string
	// Digest pins immutable observation bytes.
	Digest contracts.Digest
	// AuthorityClass must itself be machine observation when present.
	AuthorityClass AuthorityClass
}

// AdmitRequest carries one sanitized outcome bundle for admission.
type AdmitRequest struct {
	// Tenant scopes the admission.
	Tenant string
	// Principal is the authenticated actor (cross-check only).
	Principal string
	// FactID is the opaque outcome-fact identity.
	FactID string
	// AuthorityClass must be machine_observation.
	AuthorityClass AuthorityClass
	// OutcomeBundle is the sanitized observation payload (no prompts/secrets/raw).
	OutcomeBundle []byte
	// DraftPrReceiptDigest pins the admitted draft-PR receipt projection.
	DraftPrReceiptDigest contracts.Digest
	// Evidence lists sanitized observation refs only.
	Evidence []EvidenceRef
	// RawTraceSeparated must be true; raw traces stay outside the bundle.
	RawTraceSeparated bool
	// IdempotencyKey distinguishes exact retries from conflicts.
	IdempotencyKey string
}

// AdmittedFact is the durable admission result.
type AdmittedFact struct {
	// FactID is the opaque outcome-fact identity.
	FactID string
	// AuthorityClass is always machine_observation.
	AuthorityClass AuthorityClass
	// OutcomeBundleDigest pins the sanitized bundle bytes.
	OutcomeBundleDigest contracts.Digest
	// DraftPrReceiptDigest pins the draft-PR receipt.
	DraftPrReceiptDigest contracts.Digest
	// Evidence lists admitted observation refs.
	Evidence []EvidenceRef
	// RawTraceSeparated is always true on success.
	RawTraceSeparated bool
	// Receipt is the non-sensitive admission receipt.
	Receipt contracts.Receipt
	// Replayed is true when an exact prior admission is returned.
	Replayed bool
}

// RawTraceRecord is retained separately under restricted scope. It is never
// admitted through Admit — the type exists only to document the separation.
type RawTraceRecord struct {
	// TraceID is an opaque restricted-scope identity.
	TraceID string
	// Scope is the original restricted ACL scope (not elevated).
	Scope string
	// Digest pins raw trace bytes in the encrypted vault.
	Digest contracts.Digest
	// SeparatedFromOutcome must be true.
	SeparatedFromOutcome bool
}
