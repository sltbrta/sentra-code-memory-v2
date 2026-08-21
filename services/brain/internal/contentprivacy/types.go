package contentprivacy

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrInvalid reports malformed input or policy without echoing content.
	ErrInvalid = errors.New("contentprivacy: invalid")
	// ErrDenied uniformly covers absent, quarantined, tombstoned, expired, and
	// unauthorized content.
	ErrDenied = errors.New("contentprivacy: not_found_or_denied")
	// ErrConflict reports reuse of a live scoped content identifier.
	ErrConflict = errors.New("contentprivacy: immutable conflict")
	// ErrDetector reports a detector failure after the configured fail-closed
	// quarantine or tombstone action has been committed.
	ErrDetector = errors.New("contentprivacy: detector failed closed")
	// ErrComposition reports that the production projection adapter is absent
	// or incomplete. It never includes caller or publisher payload details.
	ErrComposition = errors.New("contentprivacy: production composition unavailable")
	// ErrPublish reports a projection sink failure without forwarding an error
	// that could contain sensitive backend or payload details.
	ErrPublish = errors.New("contentprivacy: sanitized projection publish failed")
)

// ScopeKind is the visibility tier at which a content policy is selected.
type ScopeKind string

const (
	ScopeIndividual ScopeKind = "individual"
	ScopeTeam       ScopeKind = "team"
	ScopeCompany    ScopeKind = "company"
)

// Scope identifies the policy and reveal boundary for one content item.
type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id,omitempty"`
}

// Key returns the canonical scope key. Callers must not use it as authority.
func (s Scope) Key() string {
	if s.Kind == ScopeCompany {
		return string(ScopeCompany)
	}
	return string(s.Kind) + ":" + s.ID
}

// Class is a locally supported PII or secret detector class.
type Class string

const (
	ClassEmail              Class = "pii.email"
	ClassPhone              Class = "pii.phone"
	ClassSSN                Class = "pii.ssn"
	ClassCreditCard         Class = "pii.credit_card"
	ClassAPIKey             Class = "secret.api_key"
	ClassBearerToken        Class = "secret.bearer_token"
	ClassPrivateKey         Class = "secret.private_key"
	ClassPasswordAssignment Class = "secret.password_assignment"
)

// Action is the strongest disposition required by a finding. Clean content is
// published; detected content may only be redacted, quarantined, or tombstoned.
type Action string

const (
	ActionRedact     Action = "redact"
	ActionQuarantine Action = "quarantine"
	ActionTombstone  Action = "tombstone"
)

// Status is the current content lifecycle state.
type Status string

const (
	StatusClean       Status = "clean"
	StatusRedacted    Status = "redacted"
	StatusQuarantined Status = "quarantined"
	StatusTombstoned  Status = "tombstoned"
)

// ScopePolicy configures detection and lifecycle behavior for one scope kind.
// Classes absent from Actions are not inspected by the bounded local detector.
type ScopePolicy struct {
	Actions         map[Class]Action `json:"actions"`
	DetectorFailure Action           `json:"detector_failure"`
	Retention       time.Duration    `json:"retention"`
	AllowReveal     bool             `json:"allow_reveal"`
}

// Policy is an immutable, explicitly versioned policy set.
type Policy struct {
	ID              string                    `json:"id"`
	Version         string                    `json:"version"`
	MaxContentBytes int                       `json:"max_content_bytes"`
	MaxFindings     int                       `json:"max_findings"`
	Scopes          map[ScopeKind]ScopePolicy `json:"scopes"`
}

// Finding is a content-free detector result. Start and End are byte offsets in
// Surface; raw matched values are intentionally never retained in receipts.
type Finding struct {
	Class   Class  `json:"class"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Surface string `json:"surface,omitempty"`
}

// Detector finds only the requested classes. Implementations must return byte
// offsets into text. Guard validates every result and fails closed on drift.
type Detector interface {
	Detect(text string, classes []Class) ([]Finding, error)
}

// Claim is one query-facing derived statement.
type Claim struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Citation is an exact byte range in Content. Quote must equal that range on
// admission. Projection drops a citation if any part of its range is redacted;
// retained quotes are regenerated only from wholly non-sensitive ranges.
type Citation struct {
	ID    string `json:"id"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Quote string `json:"quote"`
}

// Input contains source and derived text at the content-privacy boundary.
// There is intentionally no benchmark-gold field. Blind is opaque run metadata
// copied through unchanged and never influences detection or disposition.
type Input struct {
	TenantID  string     `json:"tenant_id"`
	ID        string     `json:"id"`
	Scope     Scope      `json:"scope"`
	Content   string     `json:"content"`
	Claims    []Claim    `json:"claims,omitempty"`
	Citations []Citation `json:"citations,omitempty"`
	Blind     bool       `json:"blind,omitempty"`
}

// Projection is the only ordinary read surface exposed by Guard. IndexText and
// CacheText are derived from redacted Content, claims are inspected separately,
// and citations overlapping a detected content span are omitted.
type Projection struct {
	TenantID     string     `json:"tenant_id"`
	ID           string     `json:"id"`
	Scope        Scope      `json:"scope"`
	Content      string     `json:"content"`
	IndexText    string     `json:"index_text"`
	CacheText    string     `json:"cache_text"`
	Claims       []Claim    `json:"claims,omitempty"`
	Citations    []Citation `json:"citations,omitempty"`
	Blind        bool       `json:"blind,omitempty"`
	PolicyID     string     `json:"policy_id"`
	Version      string     `json:"policy_version"`
	PolicyDigest string     `json:"policy_digest"`
}

// Decision is the admission outcome. Quarantine and tombstone decisions have
// no Projection.
type Decision struct {
	Status     Status      `json:"status"`
	Findings   []Finding   `json:"findings,omitempty"`
	Projection *Projection `json:"projection,omitempty"`
	Receipt    Receipt     `json:"receipt"`
}

// Receipt is an append-only, content-free policy/lifecycle record.
type Receipt struct {
	Seq           uint64    `json:"seq"`
	Kind          string    `json:"kind"`
	TenantID      string    `json:"tenant_id,omitempty"`
	ContentID     string    `json:"content_id,omitempty"`
	ScopeKey      string    `json:"scope_key,omitempty"`
	Status        Status    `json:"status,omitempty"`
	Classes       []Class   `json:"classes,omitempty"`
	PolicyID      string    `json:"policy_id"`
	PolicyVersion string    `json:"policy_version"`
	PolicyDigest  string    `json:"policy_digest"`
	At            time.Time `json:"at"`
}

const (
	ReceiptPolicyInstall      = "policy.install"
	ReceiptContentClean       = "content.clean"
	ReceiptContentRedact      = "content.redact"
	ReceiptContentQuarantine  = "content.quarantine"
	ReceiptContentTombstone   = "content.tombstone"
	ReceiptDetectorQuarantine = "detector.quarantine"
	ReceiptDetectorTombstone  = "detector.tombstone"
	ReceiptRetentionTombstone = "retention.tombstone"
	ReceiptManualTombstone    = "manual.tombstone"
	ReceiptAuthorizedReveal   = "content.reveal"
	// ReceiptPersistFailed replaces a receipt's kind when the durable append
	// failed. The decision it records still happened; what did not happen is
	// the record of it surviving a restart, and a caller reading the log needs
	// to see the gap rather than infer it from a missing sequence number.
	ReceiptPersistFailed = "receipt.persist_failed"
)

// RevealRequest is reauthorized for every raw-content reveal.
type RevealRequest struct {
	TenantID  string    `json:"tenant_id"`
	ContentID string    `json:"content_id"`
	Scope     Scope     `json:"scope"`
	Principal string    `json:"principal"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// RevealAuthorizer is the external, current authority for exceptional raw
// reads. A nil authorizer always denies.
type RevealAuthorizer interface {
	AuthorizeReveal(RevealRequest) error
}

// RevealAuthorizerFunc adapts a function to RevealAuthorizer.
type RevealAuthorizerFunc func(RevealRequest) error

func (f RevealAuthorizerFunc) AuthorizeReveal(r RevealRequest) error { return f(r) }

// Revealed is an authorized copy of the original content surfaces.
type Revealed struct {
	Content   string     `json:"content"`
	Claims    []Claim    `json:"claims,omitempty"`
	Citations []Citation `json:"citations,omitempty"`
	Receipt   Receipt    `json:"receipt"`
}

// Tombstone is the retained non-content authority blocking resurrection.
type Tombstone struct {
	TenantID  string    `json:"tenant_id"`
	ContentID string    `json:"content_id"`
	ScopeKey  string    `json:"scope_key"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// ProjectionPublisher is the production-facing sink port. It receives only a
// projection constructed by Guard's validated admission path, never
// caller-provided raw surfaces. Implementations must treat the stable tenant,
// scope, and content identity as an idempotency key because an error can leave
// external publication uncertain and cause an exact retry.
type ProjectionPublisher interface {
	PublishProjection(context.Context, Projection) error
}

// ProjectionPublisherFunc adapts a function to ProjectionPublisher.
type ProjectionPublisherFunc func(context.Context, Projection) error

func (f ProjectionPublisherFunc) PublishProjection(ctx context.Context, projection Projection) error {
	return f(ctx, projection)
}
