package queryapi

import (
	"errors"
	"time"
)

var (
	// ErrRequestDenied marks an authenticated-context failure: an unmapped or
	// malformed peer identity, or a body identity that does not match the
	// authenticated peer. The transport maps it to the static request-denied
	// shape; no port is ever invoked.
	ErrRequestDenied = errors.New("queryapi: request denied")
	// ErrInvalidRequest marks a message that fails decoding or Protovalidate
	// before authority work. The transport maps it to the static
	// request-malformed shape.
	ErrInvalidRequest = errors.New("queryapi: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract
	// validation — a defect in a composed port or in this package. The
	// transport maps it to the static response-invalid shape.
	ErrInvalidResponse = errors.New("queryapi: invalid response")
	// ErrUnknownScope is the non-disclosing port failure for an unknown,
	// unauthorized, or revoked source, generation, or history scope. Ports
	// must return it without existence detail; the handler maps it to the
	// static not_found_or_denied outcome.
	ErrUnknownScope = errors.New("queryapi: unknown scope")
	// ErrIdempotencyConflict marks a reused idempotency key bound to a
	// different request digest. The handler maps it to the static
	// not_found_or_denied outcome without mutation.
	ErrIdempotencyConflict = errors.New("queryapi: idempotency conflict")
	// ErrUnknownAdmission marks resolution or completion of a key that was
	// never admitted. The handler maps it to the static not_found_or_denied
	// outcome.
	ErrUnknownAdmission = errors.New("queryapi: unknown admitted query")
	// ErrCompletionConflict marks a second, differing completion for one
	// admitted query. The handler maps it to the static not_found_or_denied
	// outcome.
	ErrCompletionConflict = errors.New("queryapi: completion conflict")
	// ErrInvalidConfiguration marks an incomplete or malformed handler
	// configuration at construction time.
	ErrInvalidConfiguration = errors.New("queryapi: invalid configuration")
	// errPortFailure marks any other port failure the response contract has
	// no shape for; the transport maps it to its static denial. It never
	// wraps port error text.
	errPortFailure = errors.New("queryapi: port failure")
	// errFactsUnavailable marks a source-catalog facts failure while building
	// an Ask success; the caller maps it to the static not_found_or_denied
	// outcome.
	errFactsUnavailable = errors.New("queryapi: generation facts unavailable")
)

// Action names one authorization checkpoint, mirroring the engine's funnel:
// query precedes any corpus read, hydrate precedes canonical byte access, and
// emit precedes result emission.
type Action string

const (
	// ActionQuery authorizes the principal's relationship before admission.
	ActionQuery Action = "query"
	// ActionHydrate reauthorizes before hydrating private payloads.
	ActionHydrate Action = "hydrate"
	// ActionEmit reauthorizes immediately before emission.
	ActionEmit Action = "emit"
)

// Decision records one current authorization outcome and its epoch.
type Decision struct {
	Allowed bool
	Epoch   uint64
}

// Principal is the authenticated gateway peer identity, derived exclusively
// from the peer the transport authenticated — never from request bodies.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// EngineQuery is one admitted grounded question pinned to one generation,
// mirroring the engine's Query shape field for field.
type EngineQuery struct {
	QueryID        string
	Principal      Principal
	SourceID       string
	GenerationID   string
	Text           string
	Freshness      string
	IdempotencyKey string
}

// EngineAnswer is the bounded grounded result: answered, partial, or
// abstained, mirroring the engine's Answer shape field for field.
type EngineAnswer struct {
	QueryID            string
	Status             string
	Prose              string
	Claims             []EngineClaim
	DegradedReasons    []string
	TokenUsage         uint64
	FactualConsistency EngineFactualConsistency
}

// EngineFactualConsistency is the domain-neutral calibrated score disclosure.
type EngineFactualConsistency struct {
	Status              string
	ScorePerMille       uint32
	Reason              string
	Provenance          *EngineFactualConsistencyProvenance
	EvaluatedClaimCount uint32
	TotalClaimCount     uint32
}

// EngineFactualConsistencyProvenance pins scorer and calibration identities.
type EngineFactualConsistencyProvenance struct {
	ScorerID          string
	ScorerVersion     string
	CalibrationID     string
	CalibrationDigest string
}

// EngineClaim is one bounded material assertion and its verified support.
type EngineClaim struct {
	ClaimID            string
	Statement          string
	Citations          []EngineCitation
	ConfidencePerMille uint32
}

// EngineCitation binds one claim to exact committed-code evidence.
type EngineCitation struct {
	EvidenceID           string
	SourceRevisionID     string
	GitOID               string
	Path                 string
	StartLine            uint32
	StartColumn          uint32
	EndLine              uint32
	EndColumn            uint32
	SupportingTextDigest string
}

// EngineFreshness pins one complete generation and discloses its state,
// mirroring the engine's Freshness shape field for field.
type EngineFreshness struct {
	GenerationID    string
	Sequence        uint64
	CommitOID       string
	TreeOID         string
	GenerationState string
	State           string
	ACLEpoch        uint64
	ObservedAt      time.Time
}

// EngineCoverage discloses canonical versus indexed revision counts.
type EngineCoverage struct {
	CanonicalRevisionCount uint64
	IndexedRevisionCount   uint64
}

// EngineResult is the complete engine output for one Ask: answer plus
// freshness, coverage, and projection disclosures, mirroring the engine's
// Result shape field for field.
type EngineResult struct {
	Answer     EngineAnswer
	Freshness  EngineFreshness
	Coverage   EngineCoverage
	Projection string
}

// EngineStatus is the authorized GetStatus view for one source.
type EngineStatus struct {
	SourceID   string
	Freshness  EngineFreshness
	Coverage   EngineCoverage
	Projection string
}

// Admission is one Ask admission against the conversation store.
type Admission struct {
	Principal      Principal
	SourceID       string
	GenerationID   string
	Text           string
	Freshness      string
	IdempotencyKey string
}

// AdmissionResult is the durable admission disposition; Replayed marks an
// exact idempotent retry that committed nothing.
type AdmissionResult struct {
	QueryID    string
	UserTurnID string
	Replayed   bool
}

// Completion is the exactly-once assistant outcome for one admitted query:
// Result for an active completion, Failed for a visibly interrupted one.
type Completion struct {
	Tenant         string
	PrincipalID    string
	IdempotencyKey string
	Result         *EngineResult
	Failed         bool
}

// CompletionResult is the durable completion disposition.
type CompletionResult struct {
	AssistantTurnID string
	Sequence        uint64
	Replayed        bool
}

// Resolution resolves one admitted key to its original outcome: Completed
// reports whether the exactly-once completion exists, Status distinguishes
// active from failed, and Result carries the active completion's outcome.
type Resolution struct {
	QueryID         string
	UserTurnID      string
	SessionID       string
	Completed       bool
	Status          string
	Result          *EngineResult
	AssistantTurnID string
}

// HistoryTurn is one hydrated private history entry: a user turn carries
// Text, an active assistant turn carries Answer, a failed one neither.
type HistoryTurn struct {
	TurnID       string
	SessionID    string
	Sequence     uint64
	Role         string
	Status       string
	OccurredAtMs int64
	Text         string
	Answer       *EngineAnswer
}

// HistoryPage is one bounded history page with its opaque cursor.
type HistoryPage struct {
	Turns      []HistoryTurn
	NextCursor string
}

// SourceFacts is the authorized non-sensitive provenance for one source.
type SourceFacts struct {
	SourceID     string
	RepositoryID string
	BrainID      string
	State        string
	Current      *GenerationFacts
}

// GenerationFacts is the contract-visible metadata of one published
// generation the engine's freshness disclosure does not carry: snapshot
// identity, policy digest, and the five P5 lane readiness records.
type GenerationFacts struct {
	GenerationID    string
	Sequence        uint64
	SnapshotID      string
	CommitOID       string
	TreeOID         string
	PolicyDigest    string
	State           string
	Readiness       []LaneFacts
	SourceWatermark uint64
}

// LaneFacts reports one P5 language lane's publication disposition.
type LaneFacts struct {
	Language   string
	Coverage   string
	ReasonCode string
}

// SourcePage is one bounded authorized source page with its opaque cursor.
type SourcePage struct {
	Sources    []SourceFacts
	NextCursor string
}
